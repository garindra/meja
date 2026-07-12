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
	defaultCwd   string
	defaultArgv  []string
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
			for index := 0; index < len(msg.Data); index++ {
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
	}
	return false, nil
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

func (c *controller) publishStatusBar() error {
	client := c.state.session.SnapshotClient(clientID0)
	if client == nil || client.TerminalCols == 0 {
		return nil
	}
	width := int(client.TerminalCols)
	cells := make([]protocol.Cell, width)
	for i := range cells {
		cells[i] = protocol.Cell{Rune: ' ', Width: 1}
	}
	list := c.state.session.WindowStatuses(clientID0)
	text := fmt.Sprintf("[%d] ", c.state.session.ID)
	for _, window := range list {
		marker := ' '
		if window.Active {
			marker = '*'
		}
		text += fmt.Sprintf("%d:%s%c ", window.Index, window.Title, marker)
	}
	for i, r := range text {
		if i >= len(cells) {
			break
		}
		cells[i].Rune = r
	}
	return sendEncoded(c.mgmtFrames, protocol.MsgStatusBar, protocol.StatusBar{
		Cols:  width,
		Cells: cells,
		Styles: []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{
			FG: protocol.Color{Mode: "default"},
			BG: protocol.Color{Mode: "rgb", R: 42, G: 99, B: 158},
		}}},
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
