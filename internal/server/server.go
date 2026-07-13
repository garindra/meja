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
	renderIdleFlush     = time.Millisecond
	renderMaxBatchAge   = 10 * time.Millisecond
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
	ctx             context.Context
	state           *sessionState
	unixUser        *user.User
	shell           string
	mgmtFrames      chan protocol.Frame
	outputFrames    map[int]chan protocol.Frame
	defaultCwd      string
	defaultArgv     []string
	installedStyles map[int]map[uint32]protocol.Style
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
		ctx:             ctx,
		state:           s,
		unixUser:        unixUser,
		shell:           shell,
		mgmtFrames:      mgmtFrames,
		outputFrames:    outputFrames,
		installedStyles: make(map[int]map[uint32]protocol.Style),
	}
	s.session.EnsureClient(clientID0)

	createPane, err := expectDecoded(mgmtDecoder, protocol.MsgCreatePane, protocol.DecodeCreatePane)
	if err != nil {
		return fmt.Errorf("read create pane: %w", err)
	}
	ctrl.defaultCwd = createPane.Cwd
	ctrl.defaultArgv = append([]string(nil), createPane.Argv...)
	s.session.SetClientSize(clientID0, createPane.Cols, createPane.Rows)
	if !s.session.HasWindows() {
		initialPane, _, _, err := ctrl.createWindow(createPane.Cwd, createPane.Argv, createPane.Cols, createPane.Rows)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: initialPane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := ctrl.publishStatusBar(); err != nil {
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
		_, pane, _, err := s.session.ReattachClient(clientID0)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: pane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := ctrl.publishStatusBar(); err != nil {
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
	updates := make(chan terminal.Update, 256)
	go c.publishTerminalUpdates(pane, updates)
	go c.relayPTYToTerminal(pane, updates)
	go func() {
		_ = pane.Process.Wait()
		_ = pane.PTY.Close()
		_ = c.handlePaneProcessExit(pane.ID)
	}()
}

func (c *controller) handlePaneProcessExit(paneID uint64) error {
	window, clientState, finalPane, removed, err := c.state.session.RemovePane(paneID, clientID0)
	if err != nil || !removed {
		return err
	}
	if finalPane {
		cols, rows := uint16(80), uint16(24)
		if clientState != nil && clientState.TerminalCols > 0 && clientState.TerminalRows > 0 {
			cols, rows = clientState.TerminalCols, clientState.TerminalRows
		}
		newPane, replacement, nextClient, createErr := c.createWindow(c.defaultCwd, c.defaultArgv, cols, rows)
		if createErr != nil {
			return createErr
		}
		c.startPane(newPane)
		window, clientState = replacement, nextClient
	}
	if err := c.publishStatusBar(); err != nil {
		return err
	}
	if window == nil || clientState == nil {
		return nil
	}
	c.resizeSessionToClient(clientState)
	if err := c.publishWindowLayout(); err != nil {
		return err
	}
	return c.publishBindingsAndSnapshots()
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
			for index := 0; index < len(msg.Data); index++ {
				if c.state.session.ActivePrompt(clientID0) != nil {
					consumed, events, terminated := c.state.session.ConsumePromptInput(clientID0, msg.Data[index:])
					for _, event := range events {
						detach, err := c.handleServerInputEvent(event)
						if err != nil {
							done <- err
							return
						}
						if detach {
							done <- nil
							return
						}
					}
					if consumed > 0 {
						index += consumed - 1
						if terminated {
							break
						}
						continue
					}
				}
				pane, _ := c.state.session.ActivePane(clientID0)
				if pane == nil {
					break
				}
				if c.state.session.IsHistoryPane(clientID0, pane.ID) {
					if err := c.handleHistoryInput(pane, msg.Data[index:]); err != nil {
						done <- err
						return
					}
					break
				}
				if translated, consumed, ok := translateApplicationCursor(msg.Data[index:], c.state.session.InputIsNormal(clientID0) && pane.UsesApplicationCursorKeys()); ok {
					if _, err := pane.WriteInput(translated); err != nil {
						done <- fmt.Errorf("write application cursor input: %w", err)
						return
					}
					index += consumed - 1
					continue
				}
				b := msg.Data[index]
				event := c.state.session.ConsumeInputByte(clientID0, b)
				detach, err := c.handleServerInputEvent(event)
				if err != nil {
					done <- err
					return
				}
				if detach {
					done <- nil
					return
				}
			}
		case protocol.MsgResizePane:
			msg, err := protocol.DecodeResizePane(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane, clientState := c.state.session.ActivePane(clientID0)
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
	if err := c.publishStatusBar(); err != nil {
		return err
	}
	if err := c.publishWindowLayout(); err != nil {
		return err
	}
	return c.publishVisibleSnapshotsLocked()
}

func (c *controller) handleServerInputEvent(event serverInputEvent) (bool, error) {
	switch event.Command {
	case serverCommandNone:
		return false, nil
	case serverCommandLiteral:
		pane, _ := c.state.session.ActivePane(clientID0)
		if pane == nil {
			return false, nil
		}
		_, err := pane.WriteInput([]byte{event.Byte})
		if err != nil {
			return false, fmt.Errorf("write pty: %w", err)
		}
		return false, nil
	case serverCommandCreateWindow:
		return false, c.commandCreateWindow()
	case serverCommandSplitVertical:
		return false, c.commandSplit(SplitVertical)
	case serverCommandSplitHorizontal:
		return false, c.commandSplit(SplitHorizontal)
	case serverCommandDetach:
		return true, nil
	case serverCommandNextWindow:
		if id, ok := c.state.session.RelativeWindowID(clientID0, 1); ok {
			return false, c.commandSelectWindow(id)
		}
	case serverCommandPreviousWindow:
		if id, ok := c.state.session.RelativeWindowID(clientID0, -1); ok {
			return false, c.commandSelectWindow(id)
		}
	case serverCommandLastWindow:
		if id, ok := c.state.session.LastWindowID(clientID0); ok {
			return false, c.commandSelectWindow(id)
		}
	case serverCommandSelectIndex:
		if id, ok := c.state.session.WindowIDByIndex(event.Index); ok {
			return false, c.commandSelectWindow(id)
		}
	case serverCommandClosePane:
		return false, c.commandClosePane()
	case serverCommandEnterHistory:
		return false, c.commandEnterHistory()
	case serverCommandFocusDirection:
		_, _, err := c.state.session.FocusPaneDirection(clientID0, event.Direction)
		if err != nil {
			return false, err
		}
		return false, c.publishWindowLayout()
	case serverCommandBeginPrompt:
		return false, c.commandBeginRenameWindowPrompt()
	case serverCommandPrompt:
		return false, c.handlePromptEvent(event)
	}
	return false, nil
}

func (c *controller) commandBeginRenameWindowPrompt() error {
	if _, err := c.state.session.BeginRenameWindowPrompt(clientID0); err != nil {
		return err
	}
	return c.publishStatusBar()
}

func (c *controller) handlePromptEvent(event serverInputEvent) error {
	switch event.PromptAction {
	case PromptActionChanged, PromptActionCancel:
		return c.publishStatusBar()
	case PromptActionSubmit:
		switch event.PromptKind {
		case PromptKindRenameWindow:
			if _, err := c.state.session.RenameWindow(event.PromptWindowID, event.PromptText); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported prompt kind %d", event.PromptKind)
		}
		return c.publishStatusBar()
	default:
		return nil
	}
}

func (c *controller) commandCreateWindow() error {
	cols, rows, err := c.createWindowSize()
	if err != nil {
		return err
	}
	pane, _, _, err := c.createWindow(c.defaultCwd, c.defaultArgv, cols, rows)
	if err != nil {
		return err
	}
	if err := c.publishStatusBar(); err != nil {
		return err
	}
	if err := c.publishWindowLayout(); err != nil {
		return err
	}
	if err := c.publishBindingsAndSnapshots(); err != nil {
		return err
	}
	c.startPane(pane)
	return nil
}

func (c *controller) commandSelectWindow(windowID uint64) error {
	if _, _, err := c.state.session.SelectWindow(clientID0, windowID); err != nil {
		return err
	}
	if err := c.publishStatusBar(); err != nil {
		return err
	}
	if err := c.publishWindowLayout(); err != nil {
		return err
	}
	return c.publishBindingsAndSnapshots()
}

func (c *controller) commandSplit(direction SplitDirection) error {
	activePane, clientState := c.state.session.ActivePane(clientID0)
	if activePane == nil || clientState == nil {
		return nil
	}
	if err := c.state.session.CanSplitFocusedPane(clientID0); err != nil {
		return c.publishVisibleSnapshots()
	}
	paneID := c.state.session.AddPaneID()
	cols, rows := activePane.TerminalSize()
	newPane, err := StartPane(c.unixUser, paneID, paneRequest{Cols: uint16(cols), Rows: uint16(rows), Shell: c.shell})
	if err != nil {
		return fmt.Errorf("start split pane: %w", err)
	}
	_, clientState, err = c.state.session.SplitFocusedPane(clientID0, newPane, direction)
	if err != nil {
		_ = terminatePane(newPane)
		return err
	}
	c.state.session.ResizeAll(clientState.TerminalCols, clientState.TerminalRows)
	if err := c.publishWindowLayout(); err != nil {
		return err
	}
	if err := c.publishBindingsAndSnapshots(); err != nil {
		return err
	}
	c.startPane(newPane)
	return nil
}

func (c *controller) commandEnterHistory() error {
	pane, clientState := c.state.session.ActivePane(clientID0)
	if pane == nil || clientState == nil {
		return nil
	}
	if !c.state.session.IsHistoryPane(clientID0, pane.ID) {
		if err := c.state.session.InstallHistoryView(clientID0, pane.ID, captureHistorySnapshot(pane)); err != nil {
			return err
		}
	}
	return c.publishBindingsAndSnapshots()
}

func (c *controller) commandClosePane() error {
	closedPane, window, clientState, windowClosed, _, autoCreate, err := c.state.session.CloseFocusedPane(clientID0)
	if err != nil {
		return err
	}
	_ = terminatePane(closedPane)
	if windowClosed && autoCreate {
		cols, rows := uint16(80), uint16(24)
		if clientState != nil && clientState.TerminalCols > 0 && clientState.TerminalRows > 0 {
			cols, rows = clientState.TerminalCols, clientState.TerminalRows
		}
		pane, replacement, nextClient, err := c.createWindow(c.defaultCwd, c.defaultArgv, cols, rows)
		if err != nil {
			return err
		}
		c.startPane(pane)
		window, clientState = replacement, nextClient
	}
	if err := c.publishStatusBar(); err != nil {
		return err
	}
	if window != nil && clientState != nil {
		c.resizeSessionToClient(clientState)
		if err := c.publishWindowLayout(); err != nil {
			return err
		}
		return c.publishBindingsAndSnapshots()
	}
	return nil
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
					if err := c.sendHistorySnapshotSerialized(binding, pane, view); err != nil {
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
		if err := sendEncoded(outputFrames, protocol.MsgScroll, protocol.Scroll{Delta: move.Delta}, protocol.EncodeScroll); err != nil {
			return err
		}
	}
	runs := historyMoveRuns(view, move)
	compiler := newDisplayCompiler(outputFrames, styleDefinitionsMap(view.Snapshot.Styles))
	for _, run := range runs {
		if err := compiler.writeCells(run.Row, run.Column, run.Cells); err != nil {
			return err
		}
	}
	if err := sendEncoded(outputFrames, protocol.MsgCursorUpdate, protocol.CursorUpdate{Cursor: move.Cursor, Visible: true}, protocol.EncodeCursorUpdate); err != nil {
		return err
	}
	return sendEncoded(outputFrames, protocol.MsgPresent, protocol.Present{}, protocol.EncodePresent)
}

type displayCellRun struct {
	Row, Column int
	Cells       []protocol.Cell
}

func historyMoveRuns(view *HistoryView, move historyMove) []displayCellRun {
	if move.Delta == 0 {
		return nil
	}
	snapshot := view.Snapshot
	runs := make([]displayCellRun, 0, 2)
	if move.Delta > 0 {
		cells := append([]protocol.Cell(nil), snapshot.Rows[view.ViewTop].Cells...)
		overlayHistoryCounter(cells, snapshot.Cols, move.NewCounter, snapshot.CounterStyle)
		runs = append(runs, displayCellRun{Row: 0, Cells: cells})
		if snapshot.ViewportRows > 1 {
			runs = append(runs, historyCounterRun(view, 1, move.OldCounter, ""))
		}
		return runs
	}
	bottom := snapshot.ViewportRows - 1
	rowIndex := view.ViewTop + bottom
	runs = append(runs, displayCellRun{Row: bottom, Cells: append([]protocol.Cell(nil), snapshot.Rows[rowIndex].Cells...)})
	runs = append(runs, historyCounterRun(view, 0, move.OldCounter, move.NewCounter))
	return runs
}

func historyCounterRun(view *HistoryView, viewportRow int, oldLabel, newLabel string) displayCellRun {
	snapshot := view.Snapshot
	width := max(len(oldLabel), len(newLabel))
	start := max(0, snapshot.Cols-width)
	rowIndex := view.ViewTop + viewportRow
	cells := append([]protocol.Cell(nil), snapshot.Rows[rowIndex].Cells[start:]...)
	if newLabel != "" {
		overlayHistoryCounter(cells, len(cells), newLabel, snapshot.CounterStyle)
	}
	return displayCellRun{Row: viewportRow, Column: start, Cells: cells}
}

func normalizedRune(r rune) rune {
	if r == 0 {
		return ' '
	}
	return r
}

func styleDefinitionsMap(defs []protocol.StyleDefinition) map[uint32]protocol.Style {
	styles := make(map[uint32]protocol.Style, len(defs))
	for _, def := range defs {
		styles[def.ID] = def.Style
	}
	return styles
}

func (c *controller) relayPTYToTerminal(pane *Pane, updates chan<- terminal.Update) {
	defer close(updates)
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
			update.Replies = nil
			updates <- update
		}
		if err != nil {
			return
		}
	}
}

func (c *controller) publishTerminalUpdates(pane *Pane, updates <-chan terminal.Update) {
	var aggregate terminal.Update
	var idle, maxAge *time.Timer
	var idleC, maxC <-chan time.Time
	pending := false
	stop := func(timer *time.Timer) {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	flush := func() bool {
		if !pending {
			return true
		}
		stop(idle)
		stop(maxAge)
		idle, idleC, maxAge, maxC = nil, nil, nil, nil
		current := aggregate
		aggregate = terminal.Update{}
		pending = false
		return c.emitTerminalUpdate(pane, current) == nil
	}
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				flush()
				return
			}
			mergeTerminalUpdate(&aggregate, update)
			if !pending {
				pending = true
				maxAge = time.NewTimer(renderMaxBatchAge)
				maxC = maxAge.C
			}
			stop(idle)
			idle = time.NewTimer(renderIdleFlush)
			idleC = idle.C
		case <-idleC:
			if !flush() {
				return
			}
		case <-maxC:
			if !flush() {
				return
			}
		}
	}
}

func mergeTerminalUpdate(dst *terminal.Update, src terminal.Update) {
	if dst.DirtyRows == nil {
		dst.DirtyRows = make(map[int]struct{})
	}
	if dst.DirtySpans == nil {
		dst.DirtySpans = make(map[int]terminal.DirtySpan)
	}
	if dst.DefinedStyles == nil {
		dst.DefinedStyles = make(map[uint32]terminal.Style)
	}
	for row := range src.DirtyRows {
		dst.DirtyRows[row] = struct{}{}
	}
	for row, span := range src.DirtySpans {
		current, ok := dst.DirtySpans[row]
		if !ok {
			dst.DirtySpans[row] = span
			continue
		}
		if span.Start < current.Start {
			current.Start = span.Start
		}
		if span.End > current.End {
			current.End = span.End
		}
		dst.DirtySpans[row] = current
	}
	for id, style := range src.DefinedStyles {
		dst.DefinedStyles[id] = style
	}
	dst.FullRedraw = dst.FullRedraw || src.FullRedraw
	dst.CursorChanged = dst.CursorChanged || src.CursorChanged
	dst.VisibleChange = dst.VisibleChange || src.VisibleChange
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
		return c.sendFullRender(binding, pane)
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()

	rows := make([]int, 0, len(update.DirtySpans))
	for row := range update.DirtySpans {
		rows = append(rows, row)
	}
	sort.Ints(rows)
	runs := make([]displayCellRun, 0, len(rows))
	for _, row := range rows {
		span := update.DirtySpans[row]
		start := row*pane.Terminal.Cols + span.Start
		end := row*pane.Terminal.Cols + span.End
		runs = append(runs, displayCellRun{
			Row:    row,
			Column: span.Start,
			Cells:  append([]protocol.Cell(nil), pane.Terminal.Cells[start:end]...),
		})
	}
	neededStyles := make(map[uint32]struct{})
	for id := range update.DefinedStyles {
		neededStyles[id] = struct{}{}
	}
	for _, run := range runs {
		for _, cell := range run.Cells {
			neededStyles[cell.StyleID] = struct{}{}
		}
	}
	styleByID := make(map[uint32]protocol.Style)
	for _, def := range pane.Terminal.SnapshotStyles() {
		styleByID[def.ID] = def.Style
	}
	styleIDs := make([]int, 0, len(neededStyles))
	for id := range neededStyles {
		styleIDs = append(styleIDs, int(id))
	}
	sort.Ints(styleIDs)
	styles := make([]protocol.StyleDefinition, 0, len(styleIDs))
	for _, rawID := range styleIDs {
		id := uint32(rawID)
		style, ok := styleByID[id]
		if !ok {
			return fmt.Errorf("terminal style %d is undefined", id)
		}
		styles = append(styles, protocol.StyleDefinition{ID: id, Style: style})
	}
	if len(styles) == 0 && len(runs) == 0 && !update.CursorChanged && !update.VisibleChange {
		return nil
	}
	for _, def := range styles {
		if err := c.installStyle(binding.Slot, outputFrames, def.ID, def.Style); err != nil {
			return err
		}
	}
	compiler := newDisplayCompiler(outputFrames, styleByID)
	for _, run := range runs {
		if err := compiler.writeCells(run.Row, run.Column, run.Cells); err != nil {
			return err
		}
	}
	if update.CursorChanged || update.VisibleChange {
		if err := sendEncoded(outputFrames, protocol.MsgCursorUpdate, protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY}, Visible: pane.Terminal.CursorVisible}, protocol.EncodeCursorUpdate); err != nil {
			return err
		}
	}
	return sendEncoded(outputFrames, protocol.MsgPresent, protocol.Present{}, protocol.EncodePresent)
}

func (c *controller) sendFullRender(binding RenderBinding, pane *Pane) error {
	outputFrames := c.outputFrames[binding.Slot]
	if outputFrames == nil {
		return fmt.Errorf("missing output stream for slot %d", binding.Slot)
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	styleDefs := pane.Terminal.SnapshotStyles()
	styleByID := make(map[uint32]protocol.Style, len(styleDefs))
	for _, def := range styleDefs {
		styleByID[def.ID] = def.Style
		if err := c.installStyle(binding.Slot, outputFrames, def.ID, def.Style); err != nil {
			return err
		}
	}
	compiler := newDisplayCompiler(outputFrames, styleByID)
	for row := 0; row < pane.Terminal.Rows; row++ {
		start := row * pane.Terminal.Cols
		if err := compiler.writeCells(row, 0, pane.Terminal.Cells[start:start+pane.Terminal.Cols]); err != nil {
			return err
		}
	}
	if err := sendEncoded(outputFrames, protocol.MsgCursorUpdate, protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY}, Visible: pane.Terminal.CursorVisible}, protocol.EncodeCursorUpdate); err != nil {
		return err
	}
	return sendEncoded(outputFrames, protocol.MsgPresent, protocol.Present{}, protocol.EncodePresent)
}

func (c *controller) sendCurrentViewSnapshot(binding RenderBinding, pane *Pane) error {
	if view := c.state.session.HistoryView(clientID0, pane.ID); view != nil {
		return c.sendHistorySnapshot(binding, pane, view)
	}
	return c.sendFullRender(binding, pane)
}

func (c *controller) sendHistorySnapshot(binding RenderBinding, pane *Pane, view *HistoryView) error {
	outputFrames := c.outputFrames[binding.Slot]
	if outputFrames == nil {
		return fmt.Errorf("missing output stream for slot %d", binding.Slot)
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	snapshot := view.Snapshot
	for _, def := range snapshot.Styles {
		if err := c.installStyle(binding.Slot, outputFrames, def.ID, def.Style); err != nil {
			return err
		}
	}
	cells := historyViewport(view)
	compiler := newDisplayCompiler(outputFrames, styleDefinitionsMap(snapshot.Styles))
	for row := 0; row < snapshot.ViewportRows; row++ {
		start := row * snapshot.Cols
		if err := compiler.writeCells(row, 0, cells[start:start+snapshot.Cols]); err != nil {
			return err
		}
	}
	if err := sendEncoded(outputFrames, protocol.MsgCursorUpdate, protocol.CursorUpdate{Cursor: protocol.Cursor{X: min(view.CursorCol, snapshot.Cols-1), Y: view.CursorRow - view.ViewTop}, Visible: true}, protocol.EncodeCursorUpdate); err != nil {
		return err
	}
	return sendEncoded(outputFrames, protocol.MsgPresent, protocol.Present{}, protocol.EncodePresent)
}

func (c *controller) installStyle(slot int, ch chan<- protocol.Frame, id uint32, style protocol.Style) error {
	if c.installedStyles == nil {
		c.installedStyles = make(map[int]map[uint32]protocol.Style)
	}
	styles := c.installedStyles[slot]
	if styles == nil {
		styles = make(map[uint32]protocol.Style)
		c.installedStyles[slot] = styles
	}
	if installed, ok := styles[id]; ok && installed == style {
		return nil
	}
	if err := sendEncoded(ch, protocol.MsgStyleInstall, protocol.StyleInstall{ID: id, Style: style}, protocol.EncodeStyleInstall); err != nil {
		return err
	}
	styles[id] = style
	return nil
}

func (c *controller) resetInstalledStyles(slot int) { delete(c.installedStyles, slot) }

func (c *controller) sendHistorySnapshotSerialized(binding RenderBinding, pane *Pane, view *HistoryView) error {
	c.state.renderMu.Lock()
	defer c.state.renderMu.Unlock()
	return c.sendHistorySnapshot(binding, pane, view)
}

func (c *controller) publishStatusBar() error {
	client := c.state.session.SnapshotClient(clientID0)
	if client == nil || client.TerminalCols == 0 {
		return nil
	}
	width := int(client.TerminalCols)
	cells := make([]protocol.Cell, width)
	for i := range cells {
		cells[i] = protocol.Cell{Rune: ' ', StyleID: 0, Width: 1}
	}
	style := protocol.Style{
		FG: protocol.Color{Mode: "default"},
		BG: protocol.Color{Mode: "rgb", R: 42, G: 99, B: 158},
	}
	text := ""
	if prompt := c.state.session.ActivePrompt(clientID0); prompt != nil {
		style = protocol.Style{
			FG: protocol.Color{Mode: "indexed", Index: 0},
			BG: protocol.Color{Mode: "indexed", Index: 3},
		}
		text = prompt.Label + string(prompt.Text)
	} else {
		list := c.state.session.WindowStatuses(clientID0)
		text = fmt.Sprintf("[%d] ", c.state.session.ID)
		for _, window := range list {
			marker := ' '
			if window.Active {
				marker = '*'
			}
			text += fmt.Sprintf("%d:%s%c ", window.Index, window.Title, marker)
		}
	}
	column := 0
	for _, r := range text {
		if column >= len(cells) {
			break
		}
		cells[column].Rune = r
		column++
	}
	return sendEncoded(c.mgmtFrames, protocol.MsgStatusBar, protocol.StatusBar{
		Cols:   width,
		Cells:  cells,
		Styles: []protocol.StyleDefinition{{ID: 0, Style: style}},
	}, protocol.EncodeStatusBar)
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
		if err := sendEncoded(outputFrames, protocol.MsgRelayoutBarrier, protocol.RelayoutBarrier{LayoutRevision: window.LayoutRevision}, protocol.EncodeRelayoutBarrier); err != nil {
			return err
		}
		c.resetInstalledStyles(binding.Slot)
		if err := c.sendCurrentViewSnapshot(binding, pane); err != nil {
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
		outputFrames := c.outputFrames[binding.Slot]
		if outputFrames == nil {
			return fmt.Errorf("missing output stream for slot %d", binding.Slot)
		}
		if err := sendEncoded(outputFrames, protocol.MsgRelayoutBarrier, protocol.RelayoutBarrier{LayoutRevision: window.LayoutRevision}, protocol.EncodeRelayoutBarrier); err != nil {
			return err
		}
		c.resetInstalledStyles(binding.Slot)
		if err := c.sendCurrentViewSnapshot(binding, pane); err != nil {
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
