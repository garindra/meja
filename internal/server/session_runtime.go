package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/garindra/meja/internal/protocol"
)

func (s *Session) startPane(pane *Pane) {
	pane.initializeRuntime()
	s.startProcessNameMonitor()
	go pane.run()
	go relayPTYOutput(pane)
	go runPTYWriter(pane, func(error) {
		_ = terminatePane(pane)
		_ = s.handlePaneProcessExit(pane.ID)
	})
	go func() {
		_ = pane.Process.Wait()
		pane.stop()
		_ = s.handlePaneProcessExit(pane.ID)
	}()
}

func (s *Session) handlePaneProcessExit(paneID uint64) error {
	return s.coordinate(func() error {
		err := s.handlePaneProcessExitNow(paneID)
		if err == nil && s.ended {
			s.shutdownNow()
		}
		return err
	})
}

func (s *Session) handlePaneProcessExitNow(paneID uint64) error {
	if s.Pane(paneID) == nil {
		return nil
	}
	handoff := s.beginOutputHandoff()
	window, clientState, finalPane, removed, err := s.RemovePane(paneID, clientID0)
	if err != nil || !removed {
		return err
	}
	if finalPane {
		s.ended = true
		return nil
	}
	if err := s.publishStatusBar(); err != nil {
		return err
	}
	if window == nil || clientState == nil {
		return nil
	}
	s.resizeSessionToClient(clientState)
	if err := s.publishWindowLayout(); err != nil {
		return err
	}
	return s.publishBindingsAndSnapshots(handoff)
}

func (s *Session) createWindow(c *Connection, cwd string, argv []string, cols, rows uint16) (*Pane, *Window, *ClientState, error) {
	paneID := s.AddPaneID()
	pane, err := StartPane(paneID, paneRequest{Cwd: cwd, Command: argv, Cols: cols, Rows: rows, Shell: c.shell})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("start pane: %w", err)
	}
	window, clientState := s.CreateWindow(pane, clientID0)
	return pane, window, clientState, nil
}

func (s *Session) createWindowSize() (uint16, uint16, error) {
	if clientState := s.SnapshotClient(clientID0); clientState != nil && clientState.TerminalCols > 0 && clientState.TerminalRows > 0 {
		return clientState.TerminalCols, clientState.TerminalRows, nil
	}
	activePane, _ := s.ActivePane(clientID0)
	if activePane == nil {
		return 0, 0, fmt.Errorf("create window: no active pane")
	}
	cols, rows := activePane.TerminalSize()
	return uint16(cols), uint16(rows), nil
}

func (s *Session) resizeSessionToClient(clientState *ClientState) {
	if clientState != nil && clientState.TerminalCols > 0 && clientState.TerminalRows > 0 {
		s.ResizeAll(clientState.TerminalCols, clientState.TerminalRows)
	}
}

// readInput is session-owned: it reads the physical input stream directly and
// turns its frames into session behavior. Connection only owns the stream
// handle across connection setup and replacement.
func (s *Session) readInput(c *Connection, done chan<- error) {
	s.readInputFrames(c, protocol.NewDecoder(c.Input, protocol.DefaultMaxFrameSize), done)
}

func (s *Session) readInputFrames(c *Connection, decoder *protocol.Decoder, done chan<- error) {
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
			var detach, stopped bool
			if err := s.coordinate(func() error {
				if s.connection != c {
					return nil
				}
				var processErr error
				detach, processErr = s.handleInputBytes(c, msg.Data)
				stopped = s.stopping
				return processErr
			}); err != nil {
				done <- err
				return
			}
			if detach {
				done <- nil
				return
			}
			if stopped {
				done <- nil
				return
			}
		case protocol.MsgResizePane:
			msg, err := protocol.DecodeResizePane(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			if err := s.coordinate(func() error {
				if s.connection != c {
					return nil
				}
				return s.resizeClient(c, msg.Cols, msg.Rows)
			}); err != nil {
				done <- err
				return
			}
		default:
			done <- fmt.Errorf("unexpected input frame %d", frame.Type)
			return
		}
	}
}

func (s *Session) handleInputBytes(c *Connection, data []byte) (bool, error) {
	pane, _ := s.ActivePane(clientID0)
	if pane != nil && s.InputIsNormal(clientID0) && !s.IsHistoryPane(clientID0, pane.ID) &&
		bytes.IndexByte(data, 0x02) < 0 && (!pane.UsesApplicationCursorKeys() || bytes.IndexByte(data, 0x1b) < 0) {
		if err := pane.sendInput(data); err != nil {
			return false, fmt.Errorf("write pty input: %w", err)
		}
		return false, nil
	}
	for index := 0; index < len(data); index++ {
		if s.ActivePrompt(clientID0) != nil {
			consumed, events, terminated := s.ConsumePromptInput(clientID0, data[index:])
			for _, event := range events {
				detach, err := s.handleServerInputEvent(c, event)
				if err != nil || detach {
					return detach, err
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
		pane, _ := s.ActivePane(clientID0)
		if pane == nil {
			break
		}
		if s.IsHistoryPane(clientID0, pane.ID) {
			return false, s.handleHistoryInput(pane, data[index:])
		}
		if translated, consumed, ok := translateApplicationCursor(data[index:], s.InputIsNormal(clientID0) && pane.UsesApplicationCursorKeys()); ok {
			if err := pane.sendInput(translated); err != nil {
				return false, fmt.Errorf("write application cursor input: %w", err)
			}
			index += consumed - 1
			continue
		}
		event := s.ConsumeInputByte(clientID0, data[index])
		detach, err := s.handleServerInputEvent(c, event)
		if err != nil || detach {
			return detach, err
		}
	}
	return false, nil
}

func (s *Session) resizeClient(c *Connection, cols, rows uint16) error {
	handoff := s.beginOutputHandoff()
	s.ClearHistoryViews()
	s.SetClientSize(clientID0, cols, rows)
	s.ResizeAll(cols, rows)
	if err := s.publishStatusBar(); err != nil {
		return err
	}
	if err := s.publishWindowLayout(); err != nil {
		return err
	}
	return s.publishVisibleSnapshots(handoff)
}

func (s *Session) handleServerInputEvent(c *Connection, event serverInputEvent) (bool, error) {
	switch event.Command {
	case serverCommandNone:
		return false, nil
	case serverCommandLiteral:
		pane, _ := s.ActivePane(clientID0)
		if pane == nil {
			return false, nil
		}
		if err := pane.sendInput([]byte{event.Byte}); err != nil {
			return false, fmt.Errorf("write pty: %w", err)
		}
		return false, nil
	case serverCommandCreateWindow:
		return false, s.commandCreateWindow(c)
	case serverCommandSplitVertical:
		return false, s.commandSplit(c, SplitVertical)
	case serverCommandSplitHorizontal:
		return false, s.commandSplit(c, SplitHorizontal)
	case serverCommandDetach:
		return true, nil
	case serverCommandNextWindow:
		if id, ok := s.RelativeWindowID(clientID0, 1); ok {
			return false, s.commandSelectWindow(id)
		}
	case serverCommandPreviousWindow:
		if id, ok := s.RelativeWindowID(clientID0, -1); ok {
			return false, s.commandSelectWindow(id)
		}
	case serverCommandLastWindow:
		if id, ok := s.LastWindowID(clientID0); ok {
			return false, s.commandSelectWindow(id)
		}
	case serverCommandSelectIndex:
		if id, ok := s.WindowIDByIndex(event.Index); ok {
			return false, s.commandSelectWindow(id)
		}
	case serverCommandClosePane:
		return false, s.commandClosePane(c)
	case serverCommandEnterHistory:
		return false, s.commandEnterHistory()
	case serverCommandSwapPanePrevious:
		return false, s.commandSwapPane(SwapPanePrevious)
	case serverCommandSwapPaneNext:
		return false, s.commandSwapPane(SwapPaneNext)
	case serverCommandFocusDirection:
		if _, _, err := s.FocusPaneDirection(clientID0, event.Direction); err != nil {
			return false, err
		}
		return false, s.publishWindowLayout()
	case serverCommandBeginWindowPrompt:
		return false, s.commandBeginRenameWindowPrompt()
	case serverCommandBeginSessionPrompt:
		return false, s.commandBeginRenameSessionPrompt()
	case serverCommandPrompt:
		return false, s.handlePromptEvent(c, event)
	}
	return false, nil
}

func (s *Session) commandBeginRenameWindowPrompt() error {
	if _, err := s.BeginRenameWindowPrompt(clientID0); err != nil {
		return err
	}
	return s.publishStatusBar()
}

func (s *Session) commandBeginRenameSessionPrompt() error {
	if _, err := s.BeginRenameSessionPrompt(clientID0); err != nil {
		return err
	}
	return s.publishStatusBar()
}

func (s *Session) handlePromptEvent(c *Connection, event serverInputEvent) error {
	if event.PromptKind == PromptKindConfirm &&
		(event.PromptAction == PromptActionSubmit || event.PromptAction == PromptActionCancel) {
		return s.resolvePrompt(clientID0, promptResult{
			Accepted: event.PromptAction == PromptActionSubmit && event.PromptText == "y",
			Text:     event.PromptText,
		})
	}
	switch event.PromptAction {
	case PromptActionChanged, PromptActionCancel:
		return s.publishStatusBar()
	case PromptActionSubmit:
		switch event.PromptKind {
		case PromptKindRenameWindow:
			if _, err := s.RenameWindow(event.PromptWindowID, event.PromptText); err != nil {
				return err
			}
		case PromptKindRenameSession:
			if c.Daemon == nil {
				return s.finishSessionRename(event.PromptText, true)
			}
			c.Daemon.requestSessionRename(s, s.Name, event.PromptText)
			return s.publishStatusBar()
		default:
			return fmt.Errorf("unsupported prompt kind %d", event.PromptKind)
		}
		return s.publishStatusBar()
	default:
		return nil
	}
}

func (s *Session) commandCreateWindow(c *Connection) error {
	handoff := s.beginOutputHandoff()
	cols, rows, err := s.createWindowSize()
	if err != nil {
		return err
	}
	pane, _, _, err := s.createWindow(c, s.defaultCwd, nil, cols, rows)
	if err != nil {
		return err
	}
	if err := s.publishStatusBar(); err != nil {
		return err
	}
	if err := s.publishWindowLayout(); err != nil {
		return err
	}
	s.startPane(pane)
	if err := s.publishBindingsAndSnapshots(handoff); err != nil {
		return err
	}
	return nil
}

func (s *Session) commandSelectWindow(windowID uint64) error {
	handoff := s.beginOutputHandoff()
	if _, _, err := s.SelectWindow(clientID0, windowID); err != nil {
		return err
	}
	if err := s.publishStatusBar(); err != nil {
		return err
	}
	if err := s.publishWindowLayout(); err != nil {
		return err
	}
	return s.publishBindingsAndSnapshots(handoff)
}

func (s *Session) commandSplit(c *Connection, direction SplitDirection) error {
	activePane, clientState := s.ActivePane(clientID0)
	if activePane == nil || clientState == nil {
		return nil
	}
	if err := s.CanSplitFocusedPane(clientID0); err != nil {
		handoff := s.beginOutputHandoff()
		return s.publishVisibleSnapshots(handoff)
	}
	paneID := s.AddPaneID()
	cols, rows := activePane.TerminalSize()
	newPane, err := StartPane(paneID, paneRequest{Cwd: s.defaultCwd, Cols: uint16(cols), Rows: uint16(rows), Shell: c.shell})
	if err != nil {
		return fmt.Errorf("start split pane: %w", err)
	}
	handoff := s.beginOutputHandoff()
	_, clientState, err = s.SplitFocusedPane(clientID0, newPane, direction)
	if err != nil {
		_ = terminatePane(newPane)
		return err
	}
	s.ResizeAll(clientState.TerminalCols, clientState.TerminalRows)
	if err := s.publishWindowLayout(); err != nil {
		return err
	}
	s.startPane(newPane)
	if err := s.publishBindingsAndSnapshots(handoff); err != nil {
		return err
	}
	return nil
}

func (s *Session) commandSwapPane(direction PaneSwapDirection) error {
	bindings, _ := s.RenderBindings(clientID0)
	if len(bindings) < 2 {
		return nil
	}
	handoff := s.beginOutputHandoff()
	_, clientState, changed, err := s.SwapFocusedPane(clientID0, direction)
	if err != nil {
		return err
	}
	if !changed {
		return s.publishVisibleSnapshots(handoff)
	}
	s.resizeSessionToClient(clientState)
	if err := s.publishWindowLayout(); err != nil {
		return err
	}
	return s.publishBindingsAndSnapshots(handoff)
}

func (s *Session) commandEnterHistory() error {
	pane, clientState := s.ActivePane(clientID0)
	if pane == nil || clientState == nil {
		return nil
	}
	if s.IsHistoryPane(clientID0, pane.ID) {
		return nil
	}
	handoff := s.beginOutputHandoff()
	snapshot, err := pane.captureHistorySnapshot()
	if err != nil {
		return err
	}
	if err := s.InstallHistoryView(clientID0, pane.ID, snapshot); err != nil {
		return err
	}
	return s.publishBindingsAndSnapshots(handoff)
}

func (s *Session) commandClosePane(c *Connection) error {
	return s.commandClosePaneNow(c)
}

func (s *Session) commandClosePaneNow(c *Connection) error {
	handoff := s.beginOutputHandoff()
	closedPane, window, clientState, _, _, finalPane, err := s.CloseFocusedPane(clientID0)
	if err != nil {
		return err
	}
	_ = terminatePane(closedPane)
	if finalPane {
		s.shutdownNow()
		return nil
	}
	if err := s.publishStatusBar(); err != nil {
		return err
	}
	if window != nil && clientState != nil {
		s.resizeSessionToClient(clientState)
		if err := s.publishWindowLayout(); err != nil {
			return err
		}
		return s.publishBindingsAndSnapshots(handoff)
	}
	return nil
}

func (s *Session) handleHistoryInput(pane *Pane, data []byte) error {
	for len(data) > 0 {
		direction, count, exit, consumed := decodeHistoryInput(data)
		if consumed <= 0 {
			consumed = 1
		}
		data = data[min(consumed, len(data)):]
		if exit {
			handoff := s.beginOutputHandoff()
			if bindings, ok := s.exitHistoryAndRebuild(clientID0, pane.ID); ok {
				return s.finishOutputHandoff(handoff, bindings)
			}
			return nil
		}
		if count < 0 {
			if s.jumpHistory(clientID0, pane.ID, count == -1) {
				window := s.windowForPane(pane.ID)
				view := s.HistoryView(clientID0, pane.ID)
				if window != nil && view != nil {
					if err := s.sendHistorySnapshotSerialized(pane, view); err != nil {
						return err
					}
				}
			}
			continue
		}
		for i := 0; i < count; i++ {
			move, ok := s.moveHistory(clientID0, pane.ID, direction)
			if !ok {
				return nil
			}
			if !move.Changed {
				break
			}
			if err := s.emitHistoryMove(pane, move); err != nil {
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
