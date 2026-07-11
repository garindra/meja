package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	osuser "os/user"
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
)

type Target struct {
	Username        string
	Hostname        string
	HasExplicitUser bool
}

type Config struct {
	Target       Target
	Port         int
	PortSet      bool
	CAFile       string
	IdentityFile string
	Cwd          string
	Argv         []string
	Stdin        *os.File
	Stdout       io.Writer
	Stderr       io.Writer
}

type runtimeState struct {
	mu           sync.Mutex
	ui           *render.ClientState
	stdout       io.Writer
	inputBlocked bool
}

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
		MaxIdleTimeout:  quicMaxIdleTimeout,
		KeepAlivePeriod: quicKeepAlivePeriod,
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
	ui := &runtimeState{ui: render.NewClientState(), stdout: cfg.Stdout}
	outputReady := make(chan struct{}, 1)
	sessionDone := make(chan error, 2)
	go acceptOutputStream(ctx, conn, ui, mgmtFrames, outputReady, sessionDone)

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
	var restoreOnce sync.Once
	restoreTerminal := func() {
		restoreOnce.Do(func() {
			_, _ = io.WriteString(cfg.Stdout, "\x1b[?25h\x1b[0m")
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

func acceptOutputStream(ctx context.Context, conn quic.Connection, ui *runtimeState, mgmtFrames chan<- protocol.Frame, outputReady chan<- struct{}, sessionDone chan<- error) {
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
	outputReady <- struct{}{}

	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				sessionDone <- nil
				return
			}
			sessionDone <- fmt.Errorf("read output frame: %w", err)
			return
		}
		needsRedraw := false
		requestSnapshot := false
		var snapshotPaneID uint64
		ui.with(func(state *render.ClientState) {
			switch frame.Type {
			case protocol.MsgBindRenderStream:
				msg, err := protocol.DecodeBindRenderStream(frame.Payload)
				if err == nil {
					state.ApplyBind(msg)
					needsRedraw = true
				}
			case protocol.MsgReplacePane:
				msg, err := protocol.DecodeReplacePane(frame.Payload)
				if err == nil {
					if !state.ApplyReplace(msg) {
						requestSnapshot = true
						snapshotPaneID = state.FocusedPaneID
						return
					}
					needsRedraw = true
				}
			case protocol.MsgDefineStyle:
				msg, err := protocol.DecodeDefineStyle(frame.Payload)
				if err == nil {
					if !state.DefineStyle(msg) {
						requestSnapshot = true
						snapshotPaneID = state.FocusedPaneID
					}
				}
			case protocol.MsgSetRun:
				msg, err := protocol.DecodeSetRun(frame.Payload)
				if err == nil {
					if !state.ApplySetRun(msg) {
						requestSnapshot = true
						snapshotPaneID = state.FocusedPaneID
						return
					}
					needsRedraw = true
				}
			case protocol.MsgSetCursor:
				msg, err := protocol.DecodeSetCursor(frame.Payload)
				if err == nil {
					if !state.ApplySetCursor(msg) {
						requestSnapshot = true
						snapshotPaneID = state.FocusedPaneID
						return
					}
					needsRedraw = true
				}
			case protocol.MsgSetCursorVisible:
				msg, err := protocol.DecodeSetCursorVisible(frame.Payload)
				if err == nil {
					if !state.ApplySetCursorVisible(msg) {
						requestSnapshot = true
						snapshotPaneID = state.FocusedPaneID
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
			if exited.ExitCode == 0 {
				sessionDone <- nil
				return
			}
			sessionDone <- fmt.Errorf("remote pane exited with code %d signal %s", exited.ExitCode, exited.Signal)
			return
		case protocol.MsgBindRenderStream, protocol.MsgReplacePane, protocol.MsgDefineStyle, protocol.MsgSetRun, protocol.MsgSetCursor, protocol.MsgSetCursorVisible:
			if frame.Type == protocol.MsgBindRenderStream {
				ui.setInputBlocked(false)
			}
			if needsRedraw {
				if err := ui.redraw(); err != nil {
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
		case protocol.MsgWindowList:
			msg, err := protocol.DecodeWindowList(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			ui.with(func(state *render.ClientState) { state.ApplyWindowList(msg) })
			if err := ui.redraw(); err != nil {
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
			if err := ui.redraw(); err != nil {
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
			if err := ui.redraw(); err != nil {
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
	prefix := false
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				inputs, mgmts, detach := processInputByte(&prefix, b, ui, cfg)
				if detach {
					done <- errDetachRequested
					return
				}
				for _, payload := range inputs {
					if waitErr := ui.waitForInputReady(ctx); waitErr != nil {
						if ctx.Err() != nil {
							return
						}
						errs <- waitErr
						return
					}
					if sendErr := sendInputBytes(inputFrames, ui.activePaneID(), payload); sendErr != nil {
						errs <- sendErr
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

func processInputByte(prefix *bool, b byte, ui *runtimeState, cfg Config) ([][]byte, []protocol.Frame, bool) {
	if *prefix {
		*prefix = false
		switch b {
		case prefixByte:
			return [][]byte{{prefixByte}}, nil, false
		case 'c':
			payload, _ := protocol.EncodeCreateWindow(nil, protocol.CreateWindow{Cwd: cfg.Cwd, Argv: cfg.Argv})
			frame := protocol.Frame{Type: protocol.MsgCreateWindow, Payload: payload}
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
			payload, _ := protocol.EncodeCloseWindow(nil, protocol.CloseWindow{WindowID: ui.activeWindowID()})
			frame := protocol.Frame{Type: protocol.MsgCloseWindow, Payload: payload}
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
	}
	if b == prefixByte {
		*prefix = true
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
			if err := ui.redraw(); err != nil {
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

func (r *runtimeState) redraw() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.stdout.Write(render.RenderANSI(r.ui))
	return err
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
