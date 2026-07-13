package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/user"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"tali/internal/control"
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
	ControlPath      string
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

type connectionHandler struct {
	state       *sessionState
	shell       string
	mgmtFrames  chan protocol.Frame
	defaultCwd  string
	defaultArgv []string
}

func (c *connectionHandler) coordinate(run func() error) error {
	return c.state.coordinate(func() error {
		if c.state.currentManagementFrames() != c.mgmtFrames {
			return nil
		}
		return run()
	})
}

func handleSession(ctx context.Context, d *daemon, conn quic.Connection) error {
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

	first, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read session attachment: %w", err)
	}
	var s *sessionState
	var resumeEncoded string
	var generation uint64
	responseType := protocol.MsgSessionAttachOK
	switch first.Type {
	case protocol.MsgSessionAttach:
		attach, decodeErr := protocol.DecodeSessionAttach(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		if attach.Version != protocol.ProtocolVersion {
			return errors.New("unsupported session protocol version")
		}
		s, err = d.attach(attach.SessionID, attach.Token)
		if err == nil {
			resumeToken, tokenErr := control.NewToken()
			if tokenErr != nil {
				return tokenErr
			}
			s.attachMu.Lock()
			s.generation++
			resumeEncoded = control.EncodeToken(resumeToken)
			s.resumeTokens = map[string]uint64{resumeEncoded: s.generation}
			generation = s.generation
			s.attachMu.Unlock()
		}
	case protocol.MsgSessionResume:
		resume, decodeErr := protocol.DecodeSessionResume(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		if resume.Version != protocol.ProtocolVersion {
			return errors.New("unsupported session protocol version")
		}
		s, resumeEncoded, generation, err = d.resume(resume.SessionID, resume.ResumeToken, resume.Generation)
		responseType = protocol.MsgSessionResumeOK
	default:
		return fmt.Errorf("expected session attachment, got message type %d", first.Type)
	}
	if err != nil {
		_ = sendEncodedDirect(mgmtStream, protocol.MsgSessionAttachFailed, protocol.SessionAttachFailed{Reason: "session attachment rejected"}, protocol.EncodeSessionAttachFailed)
		return err
	}
	mgmtFrames := make(chan protocol.Frame, 64)
	writerErrs := make(chan error, 4)
	mgmtWriteMu := &sync.Mutex{}
	go writeStream(mgmtStream, mgmtFrames, writerErrs, mgmtWriteMu)
	defer close(mgmtFrames)
	d.activate(s, conn)

	if responseType == protocol.MsgSessionResumeOK {
		if err := sendEncoded(mgmtFrames, protocol.MsgSessionResumeOK, protocol.SessionResumeOK{Version: protocol.ProtocolVersion, SessionID: s.sessionID, ResumeToken: resumeEncoded, Generation: generation}, protocol.EncodeSessionResumeOK); err != nil {
			return err
		}
	} else if err := sendEncoded(mgmtFrames, protocol.MsgSessionAttachOK, protocol.SessionAttachOK{Version: protocol.ProtocolVersion, SessionID: s.sessionID, ResumeToken: resumeEncoded, Generation: generation}, protocol.EncodeSessionAttachOK); err != nil {
		return err
	}
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve daemon user: %w", err)
	}
	shell := loginShellForUser(current)

	outputStreams := make(map[int]io.Writer, int(protocol.MaxRenderSlots))
	for slot := 0; slot < int(protocol.MaxRenderSlots); slot++ {
		outputStream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			return fmt.Errorf("open output stream %d: %w", slot, err)
		}
		openPayload, err := protocol.EncodeStreamOpen(nil, protocol.StreamOpen{StreamType: protocol.StreamTypePaneOutput, Slot: uint8(slot)})
		if err != nil {
			return err
		}
		if err := protocol.NewEncoder(outputStream).WriteFrame(protocol.Frame{Type: protocol.MsgOpenPaneOutputStream, Payload: openPayload}); err != nil {
			return err
		}
		outputStreams[slot] = outputStream
	}

	handler := &connectionHandler{
		state:      s,
		shell:      shell,
		mgmtFrames: mgmtFrames,
	}
	s.attachConnection(mgmtFrames, outputStreams)
	defer func() {
		_ = conn.CloseWithError(0, "")
		_ = handler.state.detachStreams(outputStreams)
		s.detachConnection(mgmtFrames)
	}()
	s.session.EnsureClient(clientID0)

	createPane, err := expectDecoded(mgmtDecoder, protocol.MsgCreatePane, protocol.DecodeCreatePane)
	if err != nil {
		return fmt.Errorf("read create pane: %w", err)
	}
	handler.defaultCwd = createPane.Cwd
	handler.defaultArgv = append([]string(nil), createPane.Argv...)
	if err := s.coordinate(func() error {
		s.session.SetClientSize(clientID0, createPane.Cols, createPane.Rows)
		if !s.session.HasWindows() {
			initialPane, _, _, err := handler.createWindow(createPane.Cwd, createPane.Argv, createPane.Cols, createPane.Rows)
			if err != nil {
				return err
			}
			if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: initialPane.ID}, protocol.EncodePaneCreated); err != nil {
				return err
			}
			if err := handler.state.publishStatusBar(); err != nil {
				return err
			}
			if err := handler.state.publishWindowLayout(); err != nil {
				return err
			}
			handler.startPane(initialPane)
			return handler.state.publishBindingsAndSnapshots(nil)
		}

		handoff := handler.state.beginOutputHandoff()
		s.session.ResizeAll(createPane.Cols, createPane.Rows)
		_, pane, _, err := s.session.ReattachClient(clientID0)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: pane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := handler.state.publishStatusBar(); err != nil {
			return err
		}
		if err := handler.state.publishWindowLayout(); err != nil {
			return err
		}
		return handler.state.publishBindingsAndSnapshots(handoff)
	}); err != nil {
		return err
	}

	mgmtErrs := make(chan error, 1)
	inputErrs := make(chan error, 1)
	go handler.handleManagement(mgmtDecoder, mgmtErrs)
	go handler.handleInput(inputDecoder, inputErrs)

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

func (c *connectionHandler) startPane(pane *Pane) {
	updates := make(chan terminal.Update, 256)
	pane.renderCommands = make(chan paneRenderCommand, 2)
	pane.rendererDone = make(chan struct{})
	go c.state.runPaneRenderer(pane, updates)
	go relayPTYToTerminal(pane, updates)
	go func() {
		_ = pane.Process.Wait()
		_ = pane.PTY.Close()
		_ = c.handlePaneProcessExit(pane.ID)
	}()
}

func (p *Pane) attachOutput(stream io.Writer, refresh func(*renderOutput) error) error {
	if p.renderCommands == nil {
		if refresh == nil {
			return nil
		}
		return refresh(newRenderOutput(stream))
	}
	select {
	case p.renderCommands <- paneRenderCommand{attach: stream, refresh: refresh}:
		return nil
	case <-p.rendererDone:
		return nil
	}
}

func (p *Pane) detachOutput(stream io.Writer) error {
	if p.renderCommands == nil {
		return nil
	}
	return p.sendRenderCommand(paneRenderCommand{detach: stream})
}

func (p *Pane) releaseOutput(slot int, done chan<- int) {
	if p.renderCommands == nil {
		done <- slot
		return
	}
	release := &paneOutputRelease{slot: slot, done: done, acked: make(chan struct{})}
	select {
	case p.renderCommands <- paneRenderCommand{release: release}:
		go func() {
			select {
			case <-p.rendererDone:
				release.acknowledge()
			case <-release.acked:
			}
		}()
	case <-p.rendererDone:
		release.acknowledge()
	}
}

func (p *Pane) applyRender(render func(*renderOutput) error) error {
	if p.renderCommands == nil {
		return nil
	}
	return p.sendRenderCommand(paneRenderCommand{apply: render})
}

func (p *Pane) sendRenderCommand(command paneRenderCommand) error {
	done := make(chan error, 1)
	command.done = done
	select {
	case p.renderCommands <- command:
	case <-p.rendererDone:
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-p.rendererDone:
		return nil
	}
}

func (c *connectionHandler) handlePaneProcessExit(paneID uint64) error {
	return c.state.coordinate(func() error { return c.handlePaneProcessExitNow(paneID) })
}

func (c *connectionHandler) handlePaneProcessExitNow(paneID uint64) error {
	if c.state.session.Pane(paneID) == nil {
		return nil
	}
	handoff := c.state.beginOutputHandoff()
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
	if err := c.state.publishStatusBar(); err != nil {
		return err
	}
	if window == nil || clientState == nil {
		return nil
	}
	c.resizeSessionToClient(clientState)
	if err := c.state.publishWindowLayout(); err != nil {
		return err
	}
	return c.state.publishBindingsAndSnapshots(handoff)
}

func (c *connectionHandler) createWindow(cwd string, argv []string, cols, rows uint16) (*Pane, *Window, *ClientState, error) {
	paneID := c.state.session.AddPaneID()
	pane, err := StartPane(paneID, paneRequest{
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

func (c *connectionHandler) createWindowSize() (uint16, uint16, error) {
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

func (c *connectionHandler) resizeSessionToClient(clientState *ClientState) {
	if clientState == nil || clientState.TerminalCols == 0 || clientState.TerminalRows == 0 {
		return
	}
	c.state.session.ResizeAll(clientState.TerminalCols, clientState.TerminalRows)
}

func (c *connectionHandler) handleManagement(decoder *protocol.Decoder, done chan<- error) {
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

func (c *connectionHandler) handleInput(decoder *protocol.Decoder, done chan<- error) {
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

func (c *connectionHandler) resizeClient(cols, rows uint16) error {
	return c.coordinate(func() error { return c.resizeClientNow(cols, rows) })
}

func (c *connectionHandler) resizeClientNow(cols, rows uint16) error {
	handoff := c.state.beginOutputHandoff()
	c.state.session.ClearHistoryViews()
	c.state.session.SetClientSize(clientID0, cols, rows)
	c.state.session.ResizeAll(cols, rows)
	if err := c.state.publishStatusBar(); err != nil {
		return err
	}
	if err := c.state.publishWindowLayout(); err != nil {
		return err
	}
	return c.state.publishVisibleSnapshots(handoff)
}

func (c *connectionHandler) handleServerInputEvent(event serverInputEvent) (bool, error) {
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
		return false, c.coordinate(func() error {
			if _, _, err := c.state.session.FocusPaneDirection(clientID0, event.Direction); err != nil {
				return err
			}
			return c.state.publishWindowLayout()
		})
	case serverCommandBeginPrompt:
		return false, c.commandBeginRenameWindowPrompt()
	case serverCommandPrompt:
		return false, c.handlePromptEvent(event)
	}
	return false, nil
}

func (c *connectionHandler) commandBeginRenameWindowPrompt() error {
	if _, err := c.state.session.BeginRenameWindowPrompt(clientID0); err != nil {
		return err
	}
	return c.state.publishStatusBar()
}

func (c *connectionHandler) handlePromptEvent(event serverInputEvent) error {
	switch event.PromptAction {
	case PromptActionChanged, PromptActionCancel:
		return c.state.publishStatusBar()
	case PromptActionSubmit:
		switch event.PromptKind {
		case PromptKindRenameWindow:
			if _, err := c.state.session.RenameWindow(event.PromptWindowID, event.PromptText); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported prompt kind %d", event.PromptKind)
		}
		return c.state.publishStatusBar()
	default:
		return nil
	}
}

func (c *connectionHandler) commandCreateWindow() error {
	return c.coordinate(c.commandCreateWindowNow)
}

func (c *connectionHandler) commandCreateWindowNow() error {
	handoff := c.state.beginOutputHandoff()
	cols, rows, err := c.createWindowSize()
	if err != nil {
		return err
	}
	pane, _, _, err := c.createWindow(c.defaultCwd, c.defaultArgv, cols, rows)
	if err != nil {
		return err
	}
	if err := c.state.publishStatusBar(); err != nil {
		return err
	}
	if err := c.state.publishWindowLayout(); err != nil {
		return err
	}
	c.startPane(pane)
	if err := c.state.publishBindingsAndSnapshots(handoff); err != nil {
		return err
	}
	return nil
}

func (c *connectionHandler) commandSelectWindow(windowID uint64) error {
	return c.coordinate(func() error { return c.commandSelectWindowNow(windowID) })
}

func (c *connectionHandler) commandSelectWindowNow(windowID uint64) error {
	handoff := c.state.beginOutputHandoff()
	if _, _, err := c.state.session.SelectWindow(clientID0, windowID); err != nil {
		return err
	}
	if err := c.state.publishStatusBar(); err != nil {
		return err
	}
	if err := c.state.publishWindowLayout(); err != nil {
		return err
	}
	return c.state.publishBindingsAndSnapshots(handoff)
}

func (c *connectionHandler) commandSplit(direction SplitDirection) error {
	return c.coordinate(func() error { return c.commandSplitNow(direction) })
}

func (c *connectionHandler) commandSplitNow(direction SplitDirection) error {
	activePane, clientState := c.state.session.ActivePane(clientID0)
	if activePane == nil || clientState == nil {
		return nil
	}
	if err := c.state.session.CanSplitFocusedPane(clientID0); err != nil {
		handoff := c.state.beginOutputHandoff()
		return c.state.publishVisibleSnapshots(handoff)
	}
	paneID := c.state.session.AddPaneID()
	cols, rows := activePane.TerminalSize()
	newPane, err := StartPane(paneID, paneRequest{Cols: uint16(cols), Rows: uint16(rows), Shell: c.shell})
	if err != nil {
		return fmt.Errorf("start split pane: %w", err)
	}
	handoff := c.state.beginOutputHandoff()
	_, clientState, err = c.state.session.SplitFocusedPane(clientID0, newPane, direction)
	if err != nil {
		_ = terminatePane(newPane)
		return err
	}
	c.state.session.ResizeAll(clientState.TerminalCols, clientState.TerminalRows)
	if err := c.state.publishWindowLayout(); err != nil {
		return err
	}
	c.startPane(newPane)
	if err := c.state.publishBindingsAndSnapshots(handoff); err != nil {
		return err
	}
	return nil
}

func (c *connectionHandler) commandEnterHistory() error {
	return c.coordinate(c.commandEnterHistoryNow)
}

func (c *connectionHandler) commandEnterHistoryNow() error {
	pane, clientState := c.state.session.ActivePane(clientID0)
	if pane == nil || clientState == nil {
		return nil
	}
	if c.state.session.IsHistoryPane(clientID0, pane.ID) {
		return nil
	}
	handoff := c.state.beginOutputHandoff()
	if err := c.state.session.InstallHistoryView(clientID0, pane.ID, captureHistorySnapshot(pane)); err != nil {
		return err
	}
	return c.state.publishBindingsAndSnapshots(handoff)
}

func (c *connectionHandler) commandClosePane() error {
	return c.coordinate(c.commandClosePaneNow)
}

func (c *connectionHandler) commandClosePaneNow() error {
	handoff := c.state.beginOutputHandoff()
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
	if err := c.state.publishStatusBar(); err != nil {
		return err
	}
	if window != nil && clientState != nil {
		c.resizeSessionToClient(clientState)
		if err := c.state.publishWindowLayout(); err != nil {
			return err
		}
		return c.state.publishBindingsAndSnapshots(handoff)
	}
	return nil
}

func (c *connectionHandler) handleHistoryInput(pane *Pane, data []byte) error {
	return c.coordinate(func() error { return c.handleHistoryInputNow(pane, data) })
}

func (c *connectionHandler) handleHistoryInputNow(pane *Pane, data []byte) error {
	for len(data) > 0 {
		direction, count, exit, consumed := decodeHistoryInput(data)
		if consumed <= 0 {
			consumed = 1
		}
		data = data[min(consumed, len(data)):]
		if exit {
			handoff := c.state.beginOutputHandoff()
			if bindings, ok := c.state.session.exitHistoryAndRebuild(clientID0, pane.ID); ok {
				return c.state.finishOutputHandoff(handoff, bindings)
			}
			return nil
		}
		if count < 0 {
			if c.state.session.jumpHistory(clientID0, pane.ID, count == -1) {
				window := c.state.windowForPane(pane.ID)
				view := c.state.session.HistoryView(clientID0, pane.ID)
				if window != nil && view != nil {
					if err := c.state.sendHistorySnapshotSerialized(pane, view); err != nil {
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
			if err := c.state.emitHistoryMove(pane, move); err != nil {
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

func terminatePane(pane *Pane) error {
	if pane == nil {
		return nil
	}
	if pane.Process != nil && pane.Process.Process != nil {
		_ = pane.Process.Process.Signal(syscall.SIGHUP)
	}
	if pane.PTY != nil {
		_ = pane.PTY.Close()
	}
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

func sendEncodedDirect[T any](w io.Writer, msgType uint64, msg T, encode func([]byte, T) ([]byte, error)) error {
	// Kept separate from the asynchronous stream writer for pre-attachment
	// failures, where no session state may be touched.
	payload, err := encode(nil, msg)
	if err != nil {
		return err
	}
	return protocol.NewEncoder(w).WriteFrame(protocol.Frame{Type: msgType, Payload: payload})
}

func writeStream(stream io.Writer, frames <-chan protocol.Frame, errs chan<- error, writeMu *sync.Mutex) {
	enc := protocol.NewEncoder(stream)
	for frame := range frames {
		writeMu.Lock()
		if err := enc.WriteFrame(frame); err != nil {
			writeMu.Unlock()
			errs <- fmt.Errorf("write frame type %d: %w", frame.Type, err)
			return
		}
		writeMu.Unlock()
	}
}
