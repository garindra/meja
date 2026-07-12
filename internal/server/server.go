package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os/user"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"tali/internal/auth"
	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

const (
	sessionID0          = 0
	clientID0           = 0
	quicMaxIdleTimeout  = 60 * time.Second
	quicKeepAlivePeriod = 10 * time.Second
)

type Config struct {
	ListenAddr       string
	CertFile         string
	KeyFile          string
	Stdout           io.Writer
	Stderr           io.Writer
	TerminalDebugLog io.Writer
}

type paneRequest struct {
	Cwd     string
	Command []string
	Cols    uint16
	Rows    uint16
	Shell   string
}

type sessionState struct {
	verifier *auth.Verifier
	session  *Session

	mu           sync.RWMutex
	outputFrames map[int]chan protocol.Frame
	renderMu     sync.Mutex
}

type controller struct {
	ctx          context.Context
	state        *sessionState
	unixUser     *user.User
	shell        string
	mgmtFrames   chan protocol.Frame
	outputFrames map[int]chan protocol.Frame
}

func Run(ctx context.Context, cfg Config) error {
	terminal.SetDebugLogger(cfg.TerminalDebugLog)
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{protocol.ALPN},
		MinVersion:   tls.VersionTLS13,
	}
	listener, err := quic.ListenAddr(cfg.ListenAddr, tlsConfig, &quic.Config{
		MaxIdleTimeout:     quicMaxIdleTimeout,
		KeepAlivePeriod:    quicKeepAlivePeriod,
		MaxIncomingStreams: int64(protocol.MaxRenderSlots),
	})
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	defer listener.Close()

	shared := &sessionState{
		verifier:     auth.NewVerifier(),
		session:      NewSession(sessionID0),
		outputFrames: map[int]chan protocol.Frame{},
	}

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		go func(conn quic.Connection) {
			if err := handleSession(ctx, shared, conn); err != nil && cfg.Stderr != nil {
				fmt.Fprintf(cfg.Stderr, "session error: %v\n", err)
			}
		}(conn)
	}
}

func handleSession(ctx context.Context, s *sessionState, conn quic.Connection) error {
	defer conn.CloseWithError(0, "")

	var err error
	mgmtStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("accept management stream: %w", err)
	}
	inputStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("accept input stream: %w", err)
	}
	mgmtDecoder := protocol.NewDecoder(mgmtStream, protocol.DefaultMaxFrameSize)
	inputDecoder := protocol.NewDecoder(inputStream, protocol.DefaultMaxFrameSize)
	if err := expectStreamOpen(mgmtDecoder, protocol.MsgOpenManagementStream, protocol.StreamTypeManagement); err != nil {
		return err
	}
	if err := expectStreamOpen(inputDecoder, protocol.MsgOpenInputStream, protocol.StreamTypeInput); err != nil {
		return err
	}

	hello, err := expectDecoded(mgmtDecoder, protocol.MsgClientHello, protocol.DecodeClientHello)
	if err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if hello.Version != 1 {
		return errors.New("unsupported client protocol version")
	}

	mgmtFrames := make(chan protocol.Frame, 64)
	writerErrs := make(chan error, 4)
	go writeStream(mgmtStream, mgmtFrames, writerErrs)
	defer close(mgmtFrames)

	authBegin, err := expectDecoded(mgmtDecoder, protocol.MsgAuthBegin, protocol.DecodeAuthBegin)
	if err != nil {
		return fmt.Errorf("read auth begin: %w", err)
	}
	beginResult, err := s.verifier.Begin(authBegin.Username, authBegin.PublicKey)
	if err != nil {
		_ = sendEncoded(mgmtFrames, protocol.MsgAuthFailed, protocol.AuthFailed{Reason: err.Error()}, protocol.EncodeAuthFailed)
		return fmt.Errorf("begin auth: %w", err)
	}
	unixUser := beginResult.User
	fingerprint := beginResult.Fingerprint
	if err := sendEncoded(mgmtFrames, protocol.MsgAuthChallenge, protocol.AuthChallenge{
		ChallengeID: beginResult.Challenge.ID,
		Nonce:       beginResult.Challenge.Nonce,
		ExpiresAt:   beginResult.Challenge.ExpiresAt,
	}, protocol.EncodeAuthChallenge); err != nil {
		return err
	}
	authResponse, err := expectDecoded(mgmtDecoder, protocol.MsgAuthResponse, protocol.DecodeAuthResponse)
	if err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	if err := s.verifier.Verify(authBegin.Username, fingerprint, authResponse.ChallengeID, authResponse.Signature); err != nil {
		_ = sendEncoded(mgmtFrames, protocol.MsgAuthFailed, protocol.AuthFailed{Reason: err.Error()}, protocol.EncodeAuthFailed)
		return fmt.Errorf("verify auth response: %w", err)
	}

	shell := loginShellForUser(unixUser)
	if err := sendEncoded(mgmtFrames, protocol.MsgAuthOK, protocol.AuthOK{
		Username: unixUser.Username,
		HomeDir:  unixUser.HomeDir,
		Shell:    shell,
	}, protocol.EncodeAuthOK); err != nil {
		return err
	}

	outputFrames := make(map[int]chan protocol.Frame, int(protocol.MaxRenderSlots))
	for slot := 0; slot < int(protocol.MaxRenderSlots); slot++ {
		outputStream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			return fmt.Errorf("open output stream %d: %w", slot, err)
		}
		frames := make(chan protocol.Frame, 256)
		outputFrames[slot] = frames
		go writeStream(outputStream, frames, writerErrs)
		defer close(frames)
		s.setOutputFrames(slot, frames)
		defer s.clearOutputFrames(slot, frames)
		if err := sendEncoded(frames, protocol.MsgOpenPaneOutputStream, protocol.StreamOpen{StreamType: protocol.StreamTypePaneOutput, Slot: uint8(slot)}, protocol.EncodeStreamOpen); err != nil {
			return err
		}
	}

	ctrl := &controller{
		ctx:          ctx,
		state:        s,
		unixUser:     unixUser,
		shell:        shell,
		mgmtFrames:   mgmtFrames,
		outputFrames: outputFrames,
	}
	s.session.EnsureClient(clientID0)

	createPane, err := expectDecoded(mgmtDecoder, protocol.MsgCreatePane, protocol.DecodeCreatePane)
	if err != nil {
		return fmt.Errorf("read create pane: %w", err)
	}
	s.session.SetClientSize(clientID0, createPane.Cols, createPane.Rows)
	if !s.session.HasWindows() {
		initialPane, window, _, err := ctrl.createWindow(createPane.Cwd, createPane.Argv, createPane.Cols, createPane.Rows)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: initialPane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := ctrl.publishWindowList(); err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: initialPane.ID}, protocol.EncodeWindowSelected); err != nil {
			return err
		}
		if err := ctrl.publishWindowLayout(); err != nil {
			return err
		}
		if err := ctrl.publishBindingsAndSnapshots(); err != nil {
			return err
		}
		ctrl.startPane(initialPane)
	} else {
		s.session.ResizeAll(createPane.Cols, createPane.Rows)
		window, pane, _, err := s.session.ReattachClient(clientID0)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: pane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := ctrl.publishWindowList(); err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
			return err
		}
		if err := ctrl.publishWindowLayout(); err != nil {
			return err
		}
		if err := ctrl.publishBindingsAndSnapshots(); err != nil {
			return err
		}
	}

	mgmtErrs := make(chan error, 1)
	inputErrs := make(chan error, 1)
	go ctrl.handleManagement(mgmtDecoder, mgmtErrs)
	go ctrl.handleInput(inputDecoder, inputErrs)

	select {
	case err := <-writerErrs:
		return err
	case err := <-mgmtErrs:
		return err
	case err := <-inputErrs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *controller) startPane(pane *Pane) {
	go c.relayPTYToTerminal(pane)
	go func() {
		_ = pane.Process.Wait()
		_ = pane.PTY.Close()
	}()
}

func (c *controller) createWindow(cwd string, argv []string, cols, rows uint16) (*Pane, *Window, *ClientState, error) {
	paneID := c.state.session.AddPaneID()
	pane, err := StartPane(c.unixUser, paneID, paneRequest{
		Cwd:     cwd,
		Command: argv,
		Cols:    cols,
		Rows:    rows,
		Shell:   c.shell,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("start pane: %w", err)
	}
	window, clientState := c.state.session.CreateWindow(pane, clientID0)
	return pane, window, clientState, nil
}

func (c *controller) createWindowSize() (uint16, uint16, error) {
	if clientState := c.state.session.SnapshotClient(clientID0); clientState != nil {
		if clientState.TerminalCols > 0 && clientState.TerminalRows > 0 {
			return clientState.TerminalCols, clientState.TerminalRows, nil
		}
	}
	activePane, _ := c.state.session.ActivePane(clientID0)
	if activePane == nil {
		return 0, 0, fmt.Errorf("create window: no active pane")
	}
	cols, rows := activePane.TerminalSize()
	return uint16(cols), uint16(rows), nil
}

func (c *controller) resizeSessionToClient(clientState *ClientState) {
	if clientState == nil || clientState.TerminalCols == 0 || clientState.TerminalRows == 0 {
		return
	}
	c.state.session.ResizeAll(clientState.TerminalCols, clientState.TerminalRows)
}

func (c *controller) handleManagement(decoder *protocol.Decoder, done chan<- error) {
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
		case protocol.MsgCreateWindow:
			msg, err := protocol.DecodeCreateWindow(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			cols, rows, err := c.createWindowSize()
			if err != nil {
				done <- err
				return
			}
			pane, window, clientState, err := c.createWindow(msg.Cwd, msg.Argv, cols, rows)
			if err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowCreated, protocol.WindowCreated{Window: c.windowInfo(window, clientState.ActiveWindowID == window.ID)}, protocol.EncodeWindowCreated); err != nil {
				done <- err
				return
			}
			if err := c.publishWindowList(); err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
				done <- err
				return
			}
			if err := c.publishWindowLayout(); err != nil {
				done <- err
				return
			}
			if err := c.publishBindingsAndSnapshots(); err != nil {
				done <- err
				return
			}
			c.startPane(pane)
		case protocol.MsgSelectWindow:
			msg, err := protocol.DecodeSelectWindow(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			window, _, err := c.state.session.SelectWindow(clientID0, msg.WindowID)
			if err != nil {
				done <- err
				return
			}
			pane, _ := c.state.session.ActivePane(clientID0)
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
				done <- err
				return
			}
			if err := c.publishWindowLayout(); err != nil {
				done <- err
				return
			}
			if err := c.publishBindingsAndSnapshots(); err != nil {
				done <- err
				return
			}
		case protocol.MsgCreateSplit:
			msg, err := protocol.DecodeCreateSplit(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			activePane, clientState := c.state.session.ActivePane(clientID0)
			if activePane == nil || clientState == nil {
				continue
			}
			targetPaneID := clientState.FocusedPaneID
			if msg.PaneID != 0 {
				targetPaneID = msg.PaneID
			}
			if targetPaneID != clientState.FocusedPaneID {
				continue
			}
			if err := c.state.session.CanSplitFocusedPane(clientID0); err != nil {
				if err := c.publishVisibleSnapshots(); err != nil {
					done <- err
					return
				}
				continue
			}
			paneID := c.state.session.AddPaneID()
			activeCols, activeRows := activePane.TerminalSize()
			newPane, err := StartPane(c.unixUser, paneID, paneRequest{
				Cols:  uint16(activeCols),
				Rows:  uint16(activeRows),
				Shell: c.shell,
			})
			if err != nil {
				done <- fmt.Errorf("start split pane: %w", err)
				return
			}
			direction := SplitVertical
			if msg.Direction == protocol.SplitHorizontal {
				direction = SplitHorizontal
			}
			window, clientState, err := c.state.session.SplitFocusedPane(clientID0, newPane, direction)
			if err != nil {
				_ = terminatePane(newPane)
				done <- err
				return
			}
			c.state.session.ResizeAll(clientState.TerminalCols, clientState.TerminalRows)
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: newPane.ID}, protocol.EncodeWindowSelected); err != nil {
				done <- err
				return
			}
			if err := c.publishWindowLayout(); err != nil {
				done <- err
				return
			}
			if err := c.publishBindingsAndSnapshots(); err != nil {
				done <- err
				return
			}
			c.startPane(newPane)
		case protocol.MsgEnterHistory:
			msg, err := protocol.DecodeEnterHistory(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane, clientState := c.state.session.ActivePane(clientID0)
			if pane == nil || clientState == nil || msg.PaneID != clientState.FocusedPaneID {
				continue
			}
			if !c.state.session.IsHistoryPane(clientID0, pane.ID) {
				snapshot := captureHistorySnapshot(pane)
				if err := c.state.session.InstallHistoryView(clientID0, pane.ID, snapshot); err != nil {
					done <- err
					return
				}
			}
			if err := c.publishBindingsAndSnapshots(); err != nil {
				done <- err
				return
			}
		case protocol.MsgFocusPane:
			msg, err := protocol.DecodeFocusPane(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			window, clientState, err := c.state.session.FocusPane(clientID0, msg.PaneID)
			if err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: clientState.FocusedPaneID}, protocol.EncodeWindowSelected); err != nil {
				done <- err
				return
			}
		case protocol.MsgCloseWindow:
			msg, err := protocol.DecodeCloseWindow(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			targetWindowID := msg.WindowID
			if targetWindowID == 0 {
				clientState := c.state.session.SnapshotClient(clientID0)
				if clientState != nil {
					targetWindowID = clientState.ActiveWindowID
				}
			}
			closed, closedPanes, replacement, pane, clientState, autoCreate, err := c.state.session.CloseWindow(clientID0, targetWindowID)
			if err != nil {
				done <- err
				return
			}
			for _, closedPane := range closedPanes {
				_ = terminatePane(closedPane)
			}
			if autoCreate {
				cols, rows := uint16(80), uint16(24)
				if clientState != nil && clientState.FocusedPaneID != 0 {
					if activePane, _ := c.state.session.ActivePane(clientID0); activePane != nil {
						activeCols, activeRows := activePane.TerminalSize()
						cols, rows = uint16(activeCols), uint16(activeRows)
					}
				}
				var err error
				pane, replacement, clientState, err = c.createWindow("", nil, cols, rows)
				if err != nil {
					done <- err
					return
				}
				c.startPane(pane)
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowClosed, protocol.WindowClosed{WindowID: closed}, protocol.EncodeWindowClosed); err != nil {
				done <- err
				return
			}
			if err := c.publishWindowList(); err != nil {
				done <- err
				return
			}
			if replacement != nil && pane != nil && clientState != nil {
				c.resizeSessionToClient(clientState)
				if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: replacement.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
					done <- err
					return
				}
				if err := c.publishWindowLayout(); err != nil {
					done <- err
					return
				}
				if err := c.publishBindingsAndSnapshots(); err != nil {
					done <- err
					return
				}
			}
		case protocol.MsgClosePane:
			_, err := protocol.DecodeClosePane(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			closedPane, window, clientState, windowClosed, closedWindowID, autoCreate, err := c.state.session.CloseFocusedPane(clientID0)
			if err != nil {
				done <- err
				return
			}
			_ = terminatePane(closedPane)
			if windowClosed {
				if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowClosed, protocol.WindowClosed{WindowID: closedWindowID}, protocol.EncodeWindowClosed); err != nil {
					done <- err
					return
				}
				if autoCreate {
					cols, rows := uint16(80), uint16(24)
					if clientState != nil && clientState.TerminalCols > 0 && clientState.TerminalRows > 0 {
						cols, rows = clientState.TerminalCols, clientState.TerminalRows
					}
					pane, replacement, nextClient, err := c.createWindow("", nil, cols, rows)
					if err != nil {
						done <- err
						return
					}
					c.startPane(pane)
					window = replacement
					clientState = nextClient
				}
				if err := c.publishWindowList(); err != nil {
					done <- err
					return
				}
			}
			if window != nil && clientState != nil {
				c.resizeSessionToClient(clientState)
				if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: clientState.FocusedPaneID}, protocol.EncodeWindowSelected); err != nil {
					done <- err
					return
				}
				if err := c.publishWindowLayout(); err != nil {
					done <- err
					return
				}
				if err := c.publishBindingsAndSnapshots(); err != nil {
					done <- err
					return
				}
			}
		case protocol.MsgListWindows:
			if err := c.publishWindowList(); err != nil {
				done <- err
				return
			}
		case protocol.MsgRequestPaneSnapshot:
			msg, err := protocol.DecodeRequestPaneSnapshot(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane := c.state.session.Panes[msg.PaneID]
			if pane == nil {
				continue
			}
			window := c.windowForPane(pane.ID)
			if window == nil {
				continue
			}
			binding, ok := c.state.session.BindingForPane(clientID0, pane.ID)
			if !ok {
				continue
			}
			if err := c.sendCurrentViewSnapshot(binding, window, pane); err != nil {
				done <- err
				return
			}
		case protocol.MsgPing:
			msg, err := protocol.DecodePing(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgPong, protocol.Pong{
				Seq:           msg.Seq,
				SentUnixMilli: msg.SentUnixMilli,
			}, protocol.EncodePong); err != nil {
				done <- err
				return
			}
		case protocol.MsgPaneExited:
			done <- nil
			return
		}
	}
}

func (c *controller) handleInput(decoder *protocol.Decoder, done chan<- error) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			done <- fmt.Errorf("read input frame: %w", err)
			return
		}
		switch frame.Type {
		case protocol.MsgInputBytes:
			msg, err := protocol.DecodeInputBytes(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane, _, _ := c.state.session.ResolveInputTarget(clientID0, msg.PaneID)
			if pane == nil {
				continue
			}
			if c.state.session.IsHistoryPane(clientID0, pane.ID) {
				if err := c.handleHistoryInput(pane, msg.Data); err != nil {
					done <- err
					return
				}
				continue
			}
			if _, err := pane.WriteInput(msg.Data); err != nil {
				done <- fmt.Errorf("write pty: %w", err)
				return
			}
		case protocol.MsgResizePane:
			msg, err := protocol.DecodeResizePane(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane, clientState, _ := c.state.session.ResolveInputTarget(clientID0, msg.PaneID)
			if pane == nil || clientState == nil {
				continue
			}
			if err := c.resizeClient(msg.Cols, msg.Rows); err != nil {
				done <- err
				return
			}
		default:
			done <- fmt.Errorf("unexpected input frame %d", frame.Type)
			return
		}
	}
}

func (c *controller) resizeClient(cols, rows uint16) error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	c.state.session.ClearHistoryViews()
	c.state.session.SetClientSize(clientID0, cols, rows)
	c.state.session.ResizeAll(cols, rows)
	if err := c.publishWindowLayout(); err != nil {
		return err
	}
	return c.publishVisibleSnapshotsLocked()
}

func (c *controller) handleHistoryInput(pane *Pane, data []byte) error {
	for len(data) > 0 {
		direction, count, exit, consumed := decodeHistoryInput(data)
		if consumed <= 0 {
			consumed = 1
		}
		data = data[min(consumed, len(data)):]
		if exit {
			c.state.renderMu.Lock()
			defer c.state.renderMu.Unlock()
			if bindings, ok := c.state.session.exitHistoryAndRebuild(clientID0, pane.ID); ok {
				return c.publishBindingSnapshotsLocked(bindings)
			}
			return nil
		}
		if count < 0 {
			if c.state.session.jumpHistory(clientID0, pane.ID, count == -1) {
				binding, ok := c.state.session.BindingForPane(clientID0, pane.ID)
				window := c.windowForPane(pane.ID)
				view := c.state.session.HistoryView(clientID0, pane.ID)
				if ok && window != nil && view != nil {
					if err := c.sendHistorySnapshotSerialized(binding, window, pane, view); err != nil {
						return err
					}
				}
			}
			continue
		}
		for i := 0; i < count; i++ {
			move, ok := c.state.session.moveHistory(clientID0, pane.ID, direction)
			if !ok {
				return nil
			}
			if !move.Changed {
				break
			}
			if err := c.emitHistoryMove(pane, move); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeHistoryInput(data []byte) (direction, count int, exit bool, consumed int) {
	if len(data) == 0 {
		return 0, 0, false, 0
	}
	switch data[0] {
	case 'q', 0x03, 0x1b:
		if len(data) >= 3 && data[0] == 0x1b && data[1] == '[' {
			switch data[2] {
			case 'A':
				return -1, 1, false, 3
			case 'B':
				return 1, 1, false, 3
			case '5', '6':
				if len(data) >= 4 && data[3] == '~' {
					direction = -1
					if data[2] == '6' {
						direction = 1
					}
					return direction, 12, false, 4
				}
			}
		}
		return 0, 0, true, 1
	case 0x15:
		return -1, 6, false, 1
	case 0x04:
		return 1, 6, false, 1
	case 'g':
		return 0, -1, false, 1
	case 'G':
		return 0, -2, false, 1
	default:
		return 0, 0, false, 1
	}
}

func (c *controller) emitHistoryMove(pane *Pane, move historyMove) error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	view := c.state.session.HistoryView(clientID0, pane.ID)
	if view == nil {
		return nil
	}
	binding, ok := c.state.session.BindingForPane(clientID0, pane.ID)
	if !ok {
		return nil
	}
	outputFrames := c.state.currentOutputFrames(binding.Slot)
	if outputFrames == nil {
		return nil
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	if move.Delta != 0 {
		if err := sendEncoded(outputFrames, protocol.MsgScrollPane, protocol.ScrollPane{Delta: move.Delta}, protocol.EncodeScrollPane); err != nil {
			return err
		}
	}
	runs := historyMoveRuns(view, move)
	base := pane.Generation
	pane.Generation++
	return sendEncoded(outputFrames, protocol.MsgPaneUpdate, protocol.PaneUpdate{
		BindingGeneration: binding.BindingGeneration,
		BaseGeneration:    base,
		Generation:        pane.Generation,
		Runs:              runs,
		CursorChanged:     true,
		Cursor:            move.Cursor,
	}, protocol.EncodePaneUpdate)
}

func historyMoveRuns(view *HistoryView, move historyMove) []protocol.CellRun {
	if move.Delta == 0 {
		return nil
	}
	snapshot := view.Snapshot
	runs := make([]protocol.CellRun, 0, 2)
	if move.Delta > 0 {
		cells := append([]protocol.Cell(nil), snapshot.Rows[view.ViewTop].Cells...)
		overlayHistoryCounter(cells, snapshot.Cols, move.NewCounter, snapshot.CounterStyle)
		runs = append(runs, protocol.CellRun{Row: 0, Cells: cells})
		if snapshot.ViewportRows > 1 {
			runs = append(runs, historyCounterRun(view, 1, move.OldCounter, ""))
		}
		return runs
	}
	bottom := snapshot.ViewportRows - 1
	rowIndex := view.ViewTop + bottom
	runs = append(runs, protocol.CellRun{Row: bottom, Cells: append([]protocol.Cell(nil), snapshot.Rows[rowIndex].Cells...)})
	runs = append(runs, historyCounterRun(view, 0, move.OldCounter, move.NewCounter))
	return runs
}

func historyCounterRun(view *HistoryView, viewportRow int, oldLabel, newLabel string) protocol.CellRun {
	snapshot := view.Snapshot
	width := max(len(oldLabel), len(newLabel))
	start := max(0, snapshot.Cols-width)
	rowIndex := view.ViewTop + viewportRow
	cells := append([]protocol.Cell(nil), snapshot.Rows[rowIndex].Cells[start:]...)
	if newLabel != "" {
		overlayHistoryCounter(cells, len(cells), newLabel, snapshot.CounterStyle)
	}
	return protocol.CellRun{Row: viewportRow, Column: start, Cells: cells}
}

func (c *controller) relayPTYToTerminal(pane *Pane) {
	buf := make([]byte, 32*1024)
	for {
		n, err := pane.PTY.Read(buf)
		if n > 0 {
			pane.terminalMu.Lock()
			update := pane.Terminal.Apply(buf[:n])
			pane.terminalMu.Unlock()
			for _, reply := range update.Replies {
				if _, err := pane.WriteInput(reply); err != nil {
					return
				}
			}
			if sendErr := c.emitTerminalUpdate(pane, update); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *controller) emitTerminalUpdate(pane *Pane, update terminal.Update) error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	if c.state.session.IsHistoryPane(clientID0, pane.ID) {
		return nil
	}
	binding, ok := c.state.session.BindingForPane(clientID0, pane.ID)
	if !ok {
		return nil
	}
	outputFrames := c.state.currentOutputFrames(binding.Slot)
	if outputFrames == nil {
		return nil
	}
	window := c.windowForPane(pane.ID)
	if window == nil {
		return nil
	}
	if update.FullRedraw {
		return c.sendReplaceSnapshot(binding, window, pane)
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()

	styleIDs := make([]int, 0, len(update.DefinedStyles))
	for id := range update.DefinedStyles {
		styleIDs = append(styleIDs, int(id))
	}
	sort.Ints(styleIDs)
	styles := make([]protocol.StyleDefinition, 0, len(styleIDs))
	for _, rawID := range styleIDs {
		id := uint32(rawID)
		styles = append(styles, protocol.StyleDefinition{ID: id, Style: update.DefinedStyles[id]})
	}

	rows := make([]int, 0, len(update.DirtySpans))
	for row := range update.DirtySpans {
		rows = append(rows, row)
	}
	sort.Ints(rows)
	runs := make([]protocol.CellRun, 0, len(rows))
	for _, row := range rows {
		span := update.DirtySpans[row]
		start := row*pane.Terminal.Cols + span.Start
		end := row*pane.Terminal.Cols + span.End
		runs = append(runs, protocol.CellRun{
			Row:    row,
			Column: span.Start,
			Cells:  append([]protocol.Cell(nil), pane.Terminal.Cells[start:end]...),
		})
	}
	if len(styles) == 0 && len(runs) == 0 && !update.CursorChanged && !update.VisibleChange {
		return nil
	}
	base := pane.Generation
	pane.Generation++
	return sendEncoded(outputFrames, protocol.MsgPaneUpdate, protocol.PaneUpdate{
		BindingGeneration:    binding.BindingGeneration,
		BaseGeneration:       base,
		Generation:           pane.Generation,
		Styles:               styles,
		Runs:                 runs,
		CursorChanged:        update.CursorChanged,
		Cursor:               protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY},
		CursorVisibleChanged: update.VisibleChange,
		CursorVisible:        pane.Terminal.CursorVisible,
	}, protocol.EncodePaneUpdate)
}

func (c *controller) sendReplaceSnapshot(binding RenderBinding, window *Window, pane *Pane) error {
	outputFrames := c.outputFrames[binding.Slot]
	if outputFrames == nil {
		return fmt.Errorf("missing output stream for slot %d", binding.Slot)
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	pane.Generation++
	styleDefs := pane.Terminal.SnapshotStyles()
	styles := make([]protocol.StyleDefinition, 0, len(styleDefs))
	for _, def := range styleDefs {
		styles = append(styles, protocol.StyleDefinition{ID: def.ID, Style: def.Style})
	}
	return sendEncoded(outputFrames, protocol.MsgReplacePane, protocol.ReplacePane{
		SessionID:         c.state.session.ID,
		WindowID:          window.ID,
		PaneID:            pane.ID,
		BindingGeneration: binding.BindingGeneration,
		Generation:        pane.Generation,
		Cols:              pane.Terminal.Cols,
		Rows:              pane.Terminal.Rows,
		Cells:             append([]protocol.Cell(nil), pane.Terminal.Cells...),
		Styles:            styles,
		Cursor:            protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY},
		CursorVisible:     pane.Terminal.CursorVisible,
	}, protocol.EncodeReplacePane)
}

func (c *controller) sendCurrentViewSnapshot(binding RenderBinding, window *Window, pane *Pane) error {
	if view := c.state.session.HistoryView(clientID0, pane.ID); view != nil {
		return c.sendHistorySnapshot(binding, window, pane, view)
	}
	return c.sendReplaceSnapshot(binding, window, pane)
}

func (c *controller) sendHistorySnapshot(binding RenderBinding, window *Window, pane *Pane, view *HistoryView) error {
	outputFrames := c.outputFrames[binding.Slot]
	if outputFrames == nil {
		return fmt.Errorf("missing output stream for slot %d", binding.Slot)
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	pane.Generation++
	snapshot := view.Snapshot
	return sendEncoded(outputFrames, protocol.MsgReplacePane, protocol.ReplacePane{
		SessionID:         c.state.session.ID,
		WindowID:          window.ID,
		PaneID:            pane.ID,
		BindingGeneration: binding.BindingGeneration,
		Generation:        pane.Generation,
		Cols:              snapshot.Cols,
		Rows:              snapshot.ViewportRows,
		Cells:             historyViewport(view),
		Styles:            append([]protocol.StyleDefinition(nil), snapshot.Styles...),
		Cursor: protocol.Cursor{
			X: min(view.CursorCol, snapshot.Cols-1),
			Y: view.CursorRow - view.ViewTop,
		},
		CursorVisible: true,
	}, protocol.EncodeReplacePane)
}

func (c *controller) sendHistorySnapshotSerialized(binding RenderBinding, window *Window, pane *Pane, view *HistoryView) error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	return c.sendHistorySnapshot(binding, window, pane, view)
}

func (c *controller) publishWindowList() error {
	return sendEncoded(c.mgmtFrames, protocol.MsgWindowList, c.state.session.WindowList(clientID0), protocol.EncodeWindowList)
}

func (c *controller) publishWindowLayout() error {
	layout, err := c.state.session.WindowLayout(clientID0)
	if err != nil {
		return err
	}
	return sendEncoded(c.mgmtFrames, protocol.MsgWindowLayout, layout, protocol.EncodeWindowLayout)
}

func (c *controller) publishBindingsAndSnapshots() error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	bindings, _, _, err := c.state.session.RebuildRenderBindings(clientID0)
	if err != nil {
		return err
	}
	return c.publishBindingSnapshotsLocked(bindings)
}

func (c *controller) publishBindingSnapshots(bindings []RenderBinding) error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	return c.publishBindingSnapshotsLocked(bindings)
}

func (c *controller) publishBindingSnapshotsLocked(bindings []RenderBinding) error {
	for _, binding := range bindings {
		pane := c.state.session.Panes[binding.PaneID]
		window := c.windowForPane(binding.PaneID)
		if pane == nil || window == nil {
			continue
		}
		outputFrames := c.outputFrames[binding.Slot]
		if outputFrames == nil {
			return fmt.Errorf("missing output stream for slot %d", binding.Slot)
		}
		if err := c.sendCurrentViewSnapshot(binding, window, pane); err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) publishVisibleSnapshots() error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	return c.publishVisibleSnapshotsLocked()
}

func (c *controller) publishVisibleSnapshotsLocked() error {
	bindings, _ := c.state.session.RenderBindings(clientID0)
	for _, binding := range bindings {
		pane := c.state.session.Panes[binding.PaneID]
		window := c.windowForPane(binding.PaneID)
		if pane == nil || window == nil {
			continue
		}
		if err := c.sendCurrentViewSnapshot(binding, window, pane); err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) windowForPane(paneID uint64) *Window {
	c.state.session.mu.RLock()
	defer c.state.session.mu.RUnlock()
	for _, window := range c.state.session.Windows {
		if windowHasPane(window, paneID) {
			cp := *window
			return &cp
		}
	}
	return nil
}

func terminatePane(pane *Pane) error {
	if pane == nil {
		return nil
	}
	if pane.Process.Process != nil {
		_ = pane.Process.Process.Signal(syscall.SIGHUP)
	}
	_ = pane.PTY.Close()
	return nil
}

func (c *controller) windowInfo(window *Window, active bool) protocol.WindowInfo {
	list := c.state.session.WindowList(clientID0)
	for _, w := range list.Windows {
		if w.WindowID == window.ID {
			w.Active = active
			return w
		}
	}
	return protocol.WindowInfo{WindowID: window.ID, PaneID: windowPrimaryPaneID(window), Title: window.Name, Active: active}
}

func expectStreamOpen(decoder *protocol.Decoder, opener uint64, streamType string) error {
	frame, err := decoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read stream opener: %w", err)
	}
	if frame.Type != opener {
		return fmt.Errorf("unexpected stream opener %d", frame.Type)
	}
	open, err := protocol.DecodeStreamOpen(frame.Payload)
	if err != nil {
		return err
	}
	if open.StreamType != streamType {
		return fmt.Errorf("unexpected stream type %q", open.StreamType)
	}
	return nil
}

func expectDecoded[T any](decoder *protocol.Decoder, msgType uint64, decode func([]byte) (T, error)) (T, error) {
	frame, err := decoder.ReadFrame()
	if err != nil {
		var zero T
		return zero, err
	}
	if frame.Type != msgType {
		var zero T
		return zero, fmt.Errorf("unexpected message type %d", frame.Type)
	}
	return decode(frame.Payload)
}

func sendEncoded[T any](ch chan<- protocol.Frame, msgType uint64, msg T, encode func([]byte, T) ([]byte, error)) error {
	payload, err := encode(nil, msg)
	if err != nil {
		return err
	}
	defer func() { recover() }()
	ch <- protocol.Frame{Type: msgType, Payload: payload}
	return nil
}

func writeStream(stream io.Writer, frames <-chan protocol.Frame, errs chan<- error) {
	enc := protocol.NewEncoder(stream)
	for frame := range frames {
		if err := enc.WriteFrame(frame); err != nil {
			errs <- fmt.Errorf("write frame type %d: %w", frame.Type, err)
			return
		}
	}
}

func (s *sessionState) setOutputFrames(slot int, outputFrames chan protocol.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputFrames[slot] = outputFrames
}

func (s *sessionState) clearOutputFrames(slot int, outputFrames chan protocol.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputFrames[slot] == outputFrames {
		delete(s.outputFrames, slot)
	}
}

func (s *sessionState) currentOutputFrames(slot int) chan protocol.Frame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outputFrames[slot]
}
