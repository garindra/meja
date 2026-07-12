package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	osuser "os/user"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/term"

	"tali/internal/auth"
	"tali/internal/client/render"
	"tali/internal/protocol"
	"tali/internal/sshconfig"
)

const prefixByte = 0x02

var errDetachRequested = errors.New("detach requested")

const (
	quicMaxIdleTimeout  = 60 * time.Second
	quicKeepAlivePeriod = 10 * time.Second
	clientPingPeriod    = 15 * time.Second
	redrawCoalesceDelay = 2 * time.Millisecond
	incomingBurstWindow = 50 * time.Millisecond
)

type Target struct {
	Username        string
	Hostname        string
	HasExplicitUser bool
}

type Config struct {
	Target             Target
	Port               int
	PortSet            bool
	CAFile             string
	IdentityFile       string
	DebugRender        bool
	DebugRenderLogPath string
	Cwd                string
	Argv               []string
	Stdin              *os.File
	Stdout             io.Writer
	Stderr             io.Writer
}

type runtimeState struct {
	mu             sync.Mutex
	ui             *render.ClientState
	stdout         io.Writer
	stderr         io.Writer
	inputBlocked   bool
	redrawCh       chan struct{}
	debugRender    bool
	redrawRequests uint64
	redrawWrites   uint64

	incomingMu              sync.Mutex
	incomingBurstStarted    time.Time
	incomingBurstTimer      *time.Timer
	incomingClosed          bool
	incomingWireBytes       uint64
	incomingPayloadBytes    uint64
	incomingCommandCount    uint64
	incomingMessageTypeHits map[uint64]uint64
}

type prefixState uint8

const (
	prefixIdle prefixState = iota
	prefixActive
	prefixArrowESC
	prefixArrowCSI
)

func ParseTarget(raw string) (Target, error) {
	parsed, err := sshconfig.ParseTarget(raw)
	if err != nil {
		return Target{}, fmt.Errorf("invalid target %q: %w", raw, err)
	}
	return Target{
		Username:        parsed.Username,
		Hostname:        parsed.Host,
		HasExplicitUser: parsed.HasExplicitUser,
	}, nil
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Stdin == nil {
		return errors.New("stdin is required")
	}
	if cfg.Stdout == nil {
		return errors.New("stdout is required")
	}
	renderLog := cfg.Stderr
	if cfg.DebugRenderLogPath != "" {
		f, err := os.Create(cfg.DebugRenderLogPath)
		if err != nil {
			return fmt.Errorf("open render log: %w", err)
		}
		defer f.Close()
		renderLog = f
	}

	localUser, err := currentUsername()
	if err != nil {
		return err
	}
	resolved, err := sshconfig.Resolve(sshconfig.ParsedTarget{
		Host:            cfg.Target.Hostname,
		Username:        cfg.Target.Username,
		HasExplicitUser: cfg.Target.HasExplicitUser,
	}, sshconfig.ResolveOptions{
		ExplicitIdentityFile: cfg.IdentityFile,
		ExplicitPort:         cfg.Port,
		ExplicitPortSet:      cfg.PortSet,
		LocalUsername:        localUser,
	})
	if err != nil {
		return err
	}
	identity, err := auth.SelectIdentity(auth.SelectOptions{
		IdentityFiles:  resolved.IdentityFiles,
		IdentitiesOnly: resolved.IdentitiesOnly,
	})
	if err != nil {
		return err
	}
	tlsConfig, err := loadTLSConfig(cfg.CAFile, resolved.Hostname)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(resolved.Hostname, fmt.Sprintf("%d", resolved.Port))
	conn, err := quic.DialAddr(ctx, addr, tlsConfig, &quic.Config{
		MaxIdleTimeout:     quicMaxIdleTimeout,
		KeepAlivePeriod:    quicKeepAlivePeriod,
		MaxIncomingStreams: int64(protocol.MaxRenderSlots),
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.CloseWithError(0, "")

	mgmtStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open management stream: %w", err)
	}
	inputStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open input stream: %w", err)
	}

	mgmtFrames := make(chan protocol.Frame, 64)
	inputFrames := make(chan protocol.Frame, 64)
	streamErrs := make(chan error, 8)
	go writeFrames(mgmtStream, mgmtFrames, streamErrs)
	go writeFrames(inputStream, inputFrames, streamErrs)
	defer close(mgmtFrames)
	defer close(inputFrames)

	if err := enqueueEncoded(mgmtFrames, protocol.MsgOpenManagementStream, protocol.StreamOpen{StreamType: protocol.StreamTypeManagement}, protocol.EncodeStreamOpen); err != nil {
		return err
	}
	if err := enqueueEncoded(mgmtFrames, protocol.MsgClientHello, protocol.ClientHello{Version: 1}, protocol.EncodeClientHello); err != nil {
		return err
	}
	if err := enqueueEncoded(inputFrames, protocol.MsgOpenInputStream, protocol.StreamOpen{StreamType: protocol.StreamTypeInput}, protocol.EncodeStreamOpen); err != nil {
		return err
	}

	mgmtDecoder := protocol.NewDecoder(mgmtStream, protocol.DefaultMaxFrameSize)
	ui := &runtimeState{
		ui:          render.NewClientState(),
		stdout:      cfg.Stdout,
		stderr:      renderLog,
		redrawCh:    make(chan struct{}, 1),
		debugRender: cfg.DebugRender,
	}
	defer ui.closeIncomingRenderLog()
	go ui.redrawLoop(ctx, streamErrs)
	outputReady := make(chan struct{}, 1)
	sessionDone := make(chan error, 2)
	go acceptOutputStreams(ctx, conn, ui, mgmtFrames, outputReady, sessionDone)

	if err := enqueueEncoded(mgmtFrames, protocol.MsgAuthBegin, protocol.AuthBegin{
		Username:  resolved.Username,
		PublicKey: identity.AuthorizedKey(),
	}, protocol.EncodeAuthBegin); err != nil {
		return err
	}

	challengeFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth challenge: %w", err)
	}
	if challengeFrame.Type == protocol.MsgAuthFailed {
		failed, err := protocol.DecodeAuthFailed(challengeFrame.Payload)
		if err != nil {
			return err
		}
		return fmt.Errorf("authentication failed: %s", failed.Reason)
	}
	if challengeFrame.Type != protocol.MsgAuthChallenge {
		return fmt.Errorf("unexpected auth message type %d", challengeFrame.Type)
	}
	challenge, err := protocol.DecodeAuthChallenge(challengeFrame.Payload)
	if err != nil {
		return err
	}
	signature, err := auth.SignTranscript(identity.Signer, auth.BuildTranscript(
		resolved.Username,
		identity.Fingerprint(),
		challenge.ChallengeID,
		challenge.Nonce,
		challenge.ExpiresAt,
	))
	if err != nil {
		return err
	}
	if err := enqueueEncoded(mgmtFrames, protocol.MsgAuthResponse, protocol.AuthResponse{
		ChallengeID: challenge.ChallengeID,
		Signature:   signature,
	}, protocol.EncodeAuthResponse); err != nil {
		return err
	}

	authResult, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth result: %w", err)
	}
	switch authResult.Type {
	case protocol.MsgAuthOK:
		if _, err := protocol.DecodeAuthOK(authResult.Payload); err != nil {
			return err
		}
	case protocol.MsgAuthFailed:
		failed, err := protocol.DecodeAuthFailed(authResult.Payload)
		if err != nil {
			return err
		}
		return fmt.Errorf("authentication failed: %s", failed.Reason)
	default:
		return fmt.Errorf("unexpected auth result type %d", authResult.Type)
	}

	cols, rows, err := terminalSize(cfg.Stdin)
	if err != nil {
		return err
	}
	ui.with(func(state *render.ClientState) {
		state.SetTerminalSize(int(cols), int(rows))
	})

	rawState, err := term.MakeRaw(int(cfg.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	if _, err := io.WriteString(cfg.Stdout, "\x1b[?1049h\x1b[H\x1b[2J"); err != nil {
		_ = term.Restore(int(cfg.Stdin.Fd()), rawState)
		return fmt.Errorf("enter alternate screen: %w", err)
	}
	var restoreOnce sync.Once
	restoreTerminal := func() {
		restoreOnce.Do(func() {
			_, _ = io.WriteString(cfg.Stdout, "\x1b[?25h\x1b[0m\x1b[?1049l")
			_ = term.Restore(int(cfg.Stdin.Fd()), rawState)
		})
	}
	defer restoreTerminal()

	restoreSignals := make(chan os.Signal, 1)
	signal.Notify(restoreSignals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(restoreSignals)
	go func() {
		select {
		case <-ctx.Done():
		case <-restoreSignals:
			restoreTerminal()
		}
	}()

	if err := enqueueEncoded(mgmtFrames, protocol.MsgCreatePane, protocol.CreatePane{
		Cwd:  cfg.Cwd,
		Argv: cfg.Argv,
		Cols: cols,
		Rows: uint16(ui.drawableRows()),
	}, protocol.EncodeCreatePane); err != nil {
		return err
	}

	createdFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read pane created: %w", err)
	}
	if createdFrame.Type != protocol.MsgPaneCreated {
		return fmt.Errorf("unexpected pane message type %d", createdFrame.Type)
	}
	created, err := protocol.DecodePaneCreated(createdFrame.Payload)
	if err != nil {
		return fmt.Errorf("decode pane created: %w", err)
	}
	ui.with(func(state *render.ClientState) {
		state.FocusedPaneID = created.PaneID
	})
	ui.setInputBlocked(true)

	select {
	case <-outputReady:
	case err := <-sessionDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	mgmtDone := make(chan error, 1)
	go managementLoop(mgmtDecoder, ui, mgmtDone)

	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	clientDone := make(chan error, 1)
	go forwardInput(copyCtx, cfg.Stdin, inputFrames, mgmtFrames, ui, cfg, streamErrs, clientDone)
	go forwardResize(copyCtx, cfg.Stdin, inputFrames, ui, streamErrs)
	go sendPeriodicPing(copyCtx, mgmtFrames, streamErrs)

	for {
		select {
		case err := <-streamErrs:
			if err != nil {
				return err
			}
		case err := <-sessionDone:
			return err
		case err := <-mgmtDone:
			return err
		case err := <-clientDone:
			if errors.Is(err, errDetachRequested) {
				return nil
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func loadTLSConfig(caFile, serverName string) (*tls.Config, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("append CA file: no certificates found")
		}
	}
	return &tls.Config{
		RootCAs:    roots,
		NextProtos: []string{protocol.ALPN},
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}, nil
}

func acceptOutputStreams(ctx context.Context, conn quic.Connection, ui *runtimeState, mgmtFrames chan<- protocol.Frame, outputReady chan<- struct{}, sessionDone chan<- error) {
	for i := 0; i < int(protocol.MaxRenderSlots); i++ {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			sessionDone <- fmt.Errorf("accept output stream: %w", err)
			return
		}
		decoder := protocol.NewDecoder(stream, protocol.DefaultMaxFrameSize)
		openFrame, err := decoder.ReadFrame()
		if err != nil {
			sessionDone <- fmt.Errorf("read output stream open: %w", err)
			return
		}
		if openFrame.Type != protocol.MsgOpenPaneOutputStream {
			sessionDone <- fmt.Errorf("unexpected output stream opener %d", openFrame.Type)
			return
		}
		open, err := protocol.DecodeStreamOpen(openFrame.Payload)
		if err != nil {
			sessionDone <- err
			return
		}
		if int(open.Slot) != i {
			sessionDone <- fmt.Errorf("unexpected output stream slot %d, want %d", open.Slot, i)
			return
		}
		go readOutputStream(open.Slot, decoder, ui, mgmtFrames, sessionDone)
	}
	outputReady <- struct{}{}
}

func readOutputStream(slot uint8, decoder *protocol.Decoder, ui *runtimeState, mgmtFrames chan<- protocol.Frame, sessionDone chan<- error) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			sessionDone <- fmt.Errorf("read output frame: %w", err)
			return
		}
		ui.recordIncomingRenderFrame(frame)
		needsRedraw := false
		requestSnapshot := false
		var snapshotPaneID uint64
		ui.with(func(state *render.ClientState) {
			switch frame.Type {
			case protocol.MsgBindRenderStream:
				msg, err := protocol.DecodeBindRenderStream(frame.Payload)
				if err == nil {
					if msg.Slot != slot {
						return
					}
					state.ApplyBind(msg)
					snapshotPaneID = msg.PaneID
				}
			case protocol.MsgReplacePane:
				msg, err := protocol.DecodeReplacePane(frame.Payload)
				if err == nil {
					accepted, presented := state.ApplyReplaceResult(slot, msg)
					if !accepted {
						requestSnapshot = true
						snapshotPaneID = msg.PaneID
						return
					}
					needsRedraw = presented
				}
			case protocol.MsgPaneUpdate:
				msg, err := protocol.DecodePaneUpdate(frame.Payload)
				if err == nil {
					accepted, presented := state.ApplyPaneUpdateResult(slot, msg)
					if !accepted {
						requestSnapshot = true
						snapshotPaneID = state.RenderSlots[slot]
						return
					}
					needsRedraw = presented
				}
			case protocol.MsgScrollPane:
				msg, err := protocol.DecodeScrollPane(frame.Payload)
				if err == nil {
					if !state.ApplyScrollPane(slot, msg.Delta) {
						requestSnapshot = true
						snapshotPaneID = state.RenderSlots[slot]
						return
					}
				}
			case protocol.MsgDefineStyle:
				msg, err := protocol.DecodeDefineStyle(frame.Payload)
				if err == nil {
					if !state.DefineStyle(slot, msg) {
						requestSnapshot = true
						snapshotPaneID = state.RenderSlots[slot]
					}
				}
			case protocol.MsgSetRun:
				msg, err := protocol.DecodeSetRun(frame.Payload)
				if err == nil {
					if !state.ApplySetRun(slot, msg) {
						requestSnapshot = true
						snapshotPaneID = state.RenderSlots[slot]
						return
					}
					needsRedraw = true
				}
			case protocol.MsgSetCursor:
				msg, err := protocol.DecodeSetCursor(frame.Payload)
				if err == nil {
					if !state.ApplySetCursor(slot, msg) {
						requestSnapshot = true
						snapshotPaneID = state.RenderSlots[slot]
						return
					}
					needsRedraw = true
				}
			case protocol.MsgSetCursorVisible:
				msg, err := protocol.DecodeSetCursorVisible(frame.Payload)
				if err == nil {
					if !state.ApplySetCursorVisible(slot, msg) {
						requestSnapshot = true
						snapshotPaneID = state.RenderSlots[slot]
						return
					}
					needsRedraw = true
				}
			}
		})

		if requestSnapshot {
			_ = enqueueEncoded(mgmtFrames, protocol.MsgRequestPaneSnapshot, protocol.RequestPaneSnapshot{PaneID: snapshotPaneID}, protocol.EncodeRequestPaneSnapshot)
			continue
		}
		switch frame.Type {
		case protocol.MsgPaneExited:
			exited, err := protocol.DecodePaneExited(frame.Payload)
			if err != nil {
				sessionDone <- err
				return
			}
			if exited.ExitCode != 0 {
				sessionDone <- fmt.Errorf("remote pane exited with code %d signal %s", exited.ExitCode, exited.Signal)
				return
			}
		case protocol.MsgBindRenderStream, protocol.MsgReplacePane, protocol.MsgPaneUpdate, protocol.MsgScrollPane, protocol.MsgDefineStyle, protocol.MsgSetRun, protocol.MsgSetCursor, protocol.MsgSetCursorVisible:
			if frame.Type == protocol.MsgReplacePane && needsRedraw {
				ui.setInputBlocked(false)
			}
			if needsRedraw {
				if err := ui.requestRedraw(fmt.Sprintf("output-stream slot=%d msg=%d", slot, frame.Type)); err != nil {
					sessionDone <- err
					return
				}
			}
		default:
			sessionDone <- fmt.Errorf("unexpected output message %d", frame.Type)
			return
		}
	}
}

func managementLoop(decoder *protocol.Decoder, ui *runtimeState, done chan<- error) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			done <- fmt.Errorf("read management frame: %w", err)
			return
		}
		switch frame.Type {
		case protocol.MsgWindowLayout:
			msg, err := protocol.DecodeWindowLayout(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			presented := false
			ui.with(func(state *render.ClientState) { presented = state.ApplyWindowLayout(msg) })
			if presented {
				ui.setInputBlocked(false)
				if err := ui.requestRedraw("window-layout-snapshot"); err != nil {
					done <- err
					return
				}
			}
		case protocol.MsgWindowList:
			msg, err := protocol.DecodeWindowList(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			ui.with(func(state *render.ClientState) { state.ApplyWindowList(msg) })
			if err := ui.requestRedraw("window-list"); err != nil {
				done <- err
				return
			}
		case protocol.MsgWindowCreated:
			msg, err := protocol.DecodeWindowCreated(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			ui.with(func(state *render.ClientState) {
				if msg.Window.Active {
					state.ApplyWindowSelected(protocol.WindowSelected{
						WindowID: msg.Window.WindowID,
						PaneID:   msg.Window.PaneID,
					})
				}
			})
		case protocol.MsgWindowSelected:
			msg, err := protocol.DecodeWindowSelected(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			ui.with(func(state *render.ClientState) { state.ApplyWindowSelected(msg) })
			if err := ui.requestRedraw("window-selected"); err != nil {
				done <- err
				return
			}
		case protocol.MsgWindowClosed:
			msg, err := protocol.DecodeWindowClosed(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			ui.with(func(state *render.ClientState) { state.ApplyWindowClosed(msg.WindowID) })
			if err := ui.requestRedraw("window-closed"); err != nil {
				done <- err
				return
			}
		case protocol.MsgWindowTitleChanged:
			// WINDOW_LIST carries the canonical metadata; ignore these for now.
		case protocol.MsgPong:
			if _, err := protocol.DecodePong(frame.Payload); err != nil {
				done <- err
				return
			}
		case protocol.MsgPaneExited:
			exited, err := protocol.DecodePaneExited(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			if exited.ExitCode != 0 {
				done <- fmt.Errorf("remote pane exited with code %d signal %s", exited.ExitCode, exited.Signal)
				return
			}
			done <- nil
			return
		default:
		}
	}
}

func forwardInput(ctx context.Context, stdin *os.File, inputFrames, mgmtFrames chan<- protocol.Frame, ui *runtimeState, cfg Config, errs chan<- error, done chan<- error) {
	buf := make([]byte, 4096)
	pending := make([]byte, 0, len(buf))
	prefix := prefixIdle
	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		if waitErr := ui.waitForInputReady(ctx); waitErr != nil {
			return waitErr
		}
		if sendErr := sendInputBytes(inputFrames, ui.activePaneID(), append([]byte(nil), pending...)); sendErr != nil {
			return sendErr
		}
		pending = pending[:0]
		return nil
	}
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				inputs, mgmts, detach := processInputByte(&prefix, b, ui, cfg)
				if detach {
					if flushErr := flushPending(); flushErr != nil {
						if ctx.Err() != nil {
							return
						}
						errs <- flushErr
						return
					}
					done <- errDetachRequested
					return
				}
				for _, payload := range inputs {
					pending = append(pending, payload...)
				}
				if len(mgmts) > 0 {
					if flushErr := flushPending(); flushErr != nil {
						if ctx.Err() != nil {
							return
						}
						errs <- flushErr
						return
					}
				}
				for _, msg := range mgmts {
					if sendErr := enqueueFrame(mgmtFrames, msg); sendErr != nil {
						errs <- sendErr
						return
					}
				}
			}
			if flushErr := flushPending(); flushErr != nil {
				if ctx.Err() != nil {
					return
				}
				errs <- flushErr
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return
			}
			errs <- fmt.Errorf("read stdin: %w", err)
			return
		}
	}
}

func processInputByte(prefix *prefixState, b byte, ui *runtimeState, cfg Config) ([][]byte, []protocol.Frame, bool) {
	switch *prefix {
	case prefixActive:
		if b == 0x1b {
			*prefix = prefixArrowESC
			return nil, nil, false
		}
		*prefix = prefixIdle
		switch b {
		case prefixByte:
			return [][]byte{{prefixByte}}, nil, false
		case 'c':
			payload, _ := protocol.EncodeCreateWindow(nil, protocol.CreateWindow{Cwd: cfg.Cwd, Argv: cfg.Argv})
			frame := protocol.Frame{Type: protocol.MsgCreateWindow, Payload: payload}
			ui.setInputBlocked(true)
			return nil, []protocol.Frame{frame}, false
		case '%':
			payload, _ := protocol.EncodeCreateSplit(nil, protocol.CreateSplit{PaneID: ui.activePaneID(), Direction: protocol.SplitVertical})
			frame := protocol.Frame{Type: protocol.MsgCreateSplit, Payload: payload}
			ui.setInputBlocked(true)
			return nil, []protocol.Frame{frame}, false
		case '"':
			payload, _ := protocol.EncodeCreateSplit(nil, protocol.CreateSplit{PaneID: ui.activePaneID(), Direction: protocol.SplitHorizontal})
			frame := protocol.Frame{Type: protocol.MsgCreateSplit, Payload: payload}
			ui.setInputBlocked(true)
			return nil, []protocol.Frame{frame}, false
		case 'd':
			return nil, nil, true
		case 'n':
			if windowID, ok := ui.nextWindowID(); ok {
				payload, _ := protocol.EncodeSelectWindow(nil, protocol.SelectWindow{WindowID: windowID})
				frame := protocol.Frame{Type: protocol.MsgSelectWindow, Payload: payload}
				ui.setInputBlocked(true)
				return nil, []protocol.Frame{frame}, false
			}
			return nil, nil, false
		case 'p':
			if windowID, ok := ui.previousWindowID(); ok {
				payload, _ := protocol.EncodeSelectWindow(nil, protocol.SelectWindow{WindowID: windowID})
				frame := protocol.Frame{Type: protocol.MsgSelectWindow, Payload: payload}
				ui.setInputBlocked(true)
				return nil, []protocol.Frame{frame}, false
			}
			return nil, nil, false
		case 'l':
			if windowID, ok := ui.lastWindowID(); ok {
				payload, _ := protocol.EncodeSelectWindow(nil, protocol.SelectWindow{WindowID: windowID})
				frame := protocol.Frame{Type: protocol.MsgSelectWindow, Payload: payload}
				ui.setInputBlocked(true)
				return nil, []protocol.Frame{frame}, false
			}
			return nil, nil, false
		case 'x':
			payload, _ := protocol.EncodeClosePane(nil, protocol.ClosePane{PaneID: ui.activePaneID()})
			frame := protocol.Frame{Type: protocol.MsgClosePane, Payload: payload}
			ui.setInputBlocked(true)
			return nil, []protocol.Frame{frame}, false
		case '[':
			payload, _ := protocol.EncodeEnterHistory(nil, protocol.EnterHistory{PaneID: ui.activePaneID()})
			frame := protocol.Frame{Type: protocol.MsgEnterHistory, Payload: payload}
			ui.setInputBlocked(true)
			return nil, []protocol.Frame{frame}, false
		default:
			if b >= '0' && b <= '9' {
				if windowID, ok := ui.windowIDByIndex(int(b - '0')); ok {
					payload, _ := protocol.EncodeSelectWindow(nil, protocol.SelectWindow{WindowID: windowID})
					frame := protocol.Frame{Type: protocol.MsgSelectWindow, Payload: payload}
					ui.setInputBlocked(true)
					return nil, []protocol.Frame{frame}, false
				}
			}
			return nil, nil, false
		}
	case prefixArrowESC:
		if b == '[' {
			*prefix = prefixArrowCSI
			return nil, nil, false
		}
		*prefix = prefixIdle
		return nil, nil, false
	case prefixArrowCSI:
		*prefix = prefixIdle
		switch b {
		case 'A', 'B', 'C', 'D':
			if paneID, ok := ui.focusablePaneID(b); ok {
				payload, _ := protocol.EncodeFocusPane(nil, protocol.FocusPane{PaneID: paneID})
				frame := protocol.Frame{Type: protocol.MsgFocusPane, Payload: payload}
				return nil, []protocol.Frame{frame}, false
			}
		}
		return nil, nil, false
	}
	if b == prefixByte {
		*prefix = prefixActive
		return nil, nil, false
	}
	return [][]byte{{b}}, nil, false
}

func forwardResize(ctx context.Context, tty *os.File, inputFrames chan<- protocol.Frame, ui *runtimeState, errs chan<- error) {
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, syscall.SIGWINCH)
	defer signal.Stop(sigch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigch:
			cols, rows, err := terminalSize(tty)
			if err != nil {
				errs <- err
				return
			}
			ui.with(func(state *render.ClientState) { state.SetTerminalSize(int(cols), int(rows)) })
			if sendErr := enqueueEncoded(inputFrames, protocol.MsgResizePane, protocol.ResizePane{
				PaneID: ui.activePaneID(),
				Cols:   cols,
				Rows:   uint16(ui.drawableRows()),
			}, protocol.EncodeResizePane); sendErr != nil {
				errs <- sendErr
				return
			}
			if err := ui.requestRedraw("resize"); err != nil {
				errs <- err
				return
			}
		}
	}
}

func sendPeriodicPing(ctx context.Context, mgmtFrames chan<- protocol.Frame, errs chan<- error) {
	ticker := time.NewTicker(clientPingPeriod)
	defer ticker.Stop()

	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seq++
			if err := enqueueEncoded(mgmtFrames, protocol.MsgPing, protocol.Ping{
				Seq:           seq,
				SentUnixMilli: time.Now().UnixMilli(),
			}, protocol.EncodePing); err != nil {
				if ctx.Err() != nil {
					return
				}
				errs <- err
				return
			}
		}
	}
}

func terminalSize(f *os.File) (uint16, uint16, error) {
	cols, rows, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0, fmt.Errorf("get terminal size: %w", err)
	}
	return uint16(cols), uint16(rows), nil
}

func writeFrames(stream io.Writer, frames <-chan protocol.Frame, errs chan<- error) {
	enc := protocol.NewEncoder(stream)
	for frame := range frames {
		if err := enc.WriteFrame(frame); err != nil {
			errs <- fmt.Errorf("write frame type %d: %w", frame.Type, err)
			return
		}
	}
}

func sendInputBytes(ch chan<- protocol.Frame, paneID uint64, data []byte) error {
	return enqueueEncoded(ch, protocol.MsgInputBytes, protocol.InputBytes{PaneID: paneID, Data: data}, protocol.EncodeInputBytes)
}

func enqueueFrame(ch chan<- protocol.Frame, frame protocol.Frame) error {
	defer func() { recover() }()
	ch <- frame
	return nil
}

func enqueueEncoded[T any](ch chan<- protocol.Frame, msgType uint64, v T, encode func([]byte, T) ([]byte, error)) error {
	payload, err := encode(nil, v)
	if err != nil {
		return err
	}
	defer func() { recover() }()
	ch <- protocol.Frame{Type: msgType, Payload: payload}
	return nil
}

func currentUsername() (string, error) {
	current, err := osuser.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current username: %w", err)
	}
	return strings.TrimSpace(current.Username), nil
}

func (r *runtimeState) with(fn func(*render.ClientState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.ui)
}

func (r *runtimeState) redrawLoop(ctx context.Context, errs chan<- error) {
	if r.redrawCh == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.redrawCh:
		}

		r.logRenderf("redraw flush")
		if err := r.redraw(); err != nil {
			if ctx.Err() != nil {
				return
			}
			errs <- err
			return
		}

		timer := time.NewTimer(redrawCoalesceDelay)
		pending := false
		for {
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-r.redrawCh:
				pending = true
			case <-timer.C:
				if !pending {
					goto nextRedraw
				}
				pending = false
				r.logRenderf("redraw trailing flush")
				if err := r.redraw(); err != nil {
					if ctx.Err() != nil {
						return
					}
					errs <- err
					return
				}
				timer.Reset(redrawCoalesceDelay)
			}
		}
	nextRedraw:
	}
}

func (r *runtimeState) requestRedraw(reason string) error {
	r.mu.Lock()
	r.redrawRequests++
	req := r.redrawRequests
	r.mu.Unlock()
	r.logRenderf("redraw request #%d: %s", req, reason)
	if r.redrawCh == nil {
		return r.redraw()
	}
	select {
	case r.redrawCh <- struct{}{}:
		r.logRenderf("redraw queued #%d", req)
	default:
		r.logRenderf("redraw coalesced #%d", req)
	}
	return nil
}

func (r *runtimeState) redraw() error {
	r.mu.Lock()
	buf := render.RenderANSI(r.ui)
	r.redrawWrites++
	writeNo := r.redrawWrites
	r.mu.Unlock()
	r.logRenderf("redraw write #%d bytes=%d", writeNo, len(buf))
	_, err := r.stdout.Write(buf)
	return err
}

func (r *runtimeState) logRenderf(format string, args ...any) {
	if !r.debugRender || r.stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(r.stderr, "tali render: "+format+"\n", args...)
}

func (r *runtimeState) recordIncomingRenderFrame(frame protocol.Frame) {
	if !r.debugRender || r.stderr == nil {
		return
	}

	r.incomingMu.Lock()
	if r.incomingClosed {
		r.incomingMu.Unlock()
		return
	}
	if r.incomingBurstStarted.IsZero() {
		r.incomingBurstStarted = time.Now()
		r.incomingMessageTypeHits = make(map[uint64]uint64)
		r.incomingBurstTimer = time.AfterFunc(incomingBurstWindow, r.flushIncomingRender)
	}
	r.incomingWireBytes += uint64(encodedFrameSize(frame))
	r.incomingPayloadBytes += uint64(len(frame.Payload))
	r.incomingCommandCount++
	r.incomingMessageTypeHits[frame.Type]++
	r.incomingMu.Unlock()
}

func (r *runtimeState) flushIncomingRender() {
	r.incomingMu.Lock()
	defer r.incomingMu.Unlock()
	if r.incomingBurstStarted.IsZero() {
		return
	}
	if r.incomingBurstTimer != nil {
		r.incomingBurstTimer.Stop()
		r.incomingBurstTimer = nil
	}
	startedAt := r.incomingBurstStarted
	types := formatIncomingRenderTypes(r.incomingMessageTypeHits)
	r.logRenderf(
		"incoming burst at=%s window=%s elapsed=%s wire_bytes=%d payload_bytes=%d commands=%d types=%s",
		time.Now().Format(time.RFC3339Nano),
		incomingBurstWindow,
		time.Since(startedAt).Round(time.Millisecond),
		r.incomingWireBytes,
		r.incomingPayloadBytes,
		r.incomingCommandCount,
		types,
	)
	r.incomingBurstStarted = time.Time{}
	r.incomingWireBytes = 0
	r.incomingPayloadBytes = 0
	r.incomingCommandCount = 0
	r.incomingMessageTypeHits = nil
}

func (r *runtimeState) closeIncomingRenderLog() {
	r.incomingMu.Lock()
	r.incomingClosed = true
	r.incomingMu.Unlock()
	r.flushIncomingRender()
}

func formatIncomingRenderTypes(types map[uint64]uint64) string {
	if len(types) == 0 {
		return "none"
	}
	keys := make([]uint64, 0, len(types))
	for msgType := range types {
		keys = append(keys, msgType)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	parts := make([]string, 0, len(keys))
	for _, msgType := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", incomingRenderMessageName(msgType), types[msgType]))
	}
	return strings.Join(parts, ",")
}

func incomingRenderMessageName(msgType uint64) string {
	switch msgType {
	case protocol.MsgBindRenderStream:
		return "BindRenderStream"
	case protocol.MsgReplacePane:
		return "ReplacePane"
	case protocol.MsgPaneUpdate:
		return "PaneUpdate"
	case protocol.MsgScrollPane:
		return "ScrollPane"
	case protocol.MsgDefineStyle:
		return "DefineStyle"
	case protocol.MsgSetRun:
		return "SetRun"
	case protocol.MsgSetCursor:
		return "SetCursor"
	case protocol.MsgSetCursorVisible:
		return "SetCursorVisible"
	case protocol.MsgPaneExited:
		return "PaneExited"
	default:
		return fmt.Sprintf("Message%d", msgType)
	}
}

func encodedFrameSize(frame protocol.Frame) int {
	var buf [binary.MaxVarintLen64]byte
	typeBytes := binary.PutUvarint(buf[:], frame.Type)
	payloadBytes := binary.PutUvarint(buf[:], uint64(len(frame.Payload)))
	return typeBytes + payloadBytes + len(frame.Payload)
}

func (r *runtimeState) activePaneID() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.FocusedPaneID
}

func (r *runtimeState) activeWindowID() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.ActiveWindowID
}

func (r *runtimeState) nextWindowID() (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.NextWindowID()
}

func (r *runtimeState) previousWindowID() (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.PreviousWindowID()
}

func (r *runtimeState) lastWindowID() (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.LastActiveWindowID()
}

func (r *runtimeState) nextFocusablePaneID() (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.NextFocusablePaneID()
}

func (r *runtimeState) focusablePaneID(direction byte) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.FocusablePaneID(direction)
}

func (r *runtimeState) windowIDByIndex(index int) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.WindowIDByIndex(index)
}

func (r *runtimeState) drawableRows() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ui.DrawableRows()
}

func (r *runtimeState) setInputBlocked(blocked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputBlocked = blocked
}

func (r *runtimeState) waitForInputReady(ctx context.Context) error {
	for {
		r.mu.Lock()
		blocked := r.inputBlocked
		r.mu.Unlock()
		if !blocked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
