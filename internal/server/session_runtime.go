package server

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/garindra/meja/internal/protocol"
)

func (s *Session) handleControlFrame(client *ClientInstance, frame protocol.Frame) (bool, error) {
	switch frame.Type {
	case protocol.MsgFrontendInputBytes:
		msg, err := protocol.DecodeFrontendInputBytes(frame.Payload)
		if err != nil {
			return false, err
		}
		if msg.SourceIdle && !bytes.Equal(msg.Data, []byte{0x1b}) {
			return false, errors.New("source-idle frontend input must contain one Escape byte")
		}
		var detach, stopped bool
		if err := s.coordinate(func() error {
			if s.clientInstance != client {
				return nil
			}
			var processErr error
			detach, processErr = s.handleInputBytes(client, msg.LayoutRevision, msg.Data)
			if processErr == nil && !detach && msg.SourceIdle {
				if event, ok := client.frontendInput.flushLoneEscape(); ok {
					detach, processErr = s.handleFrontendInputEvent(client, event)
				}
			}
			stopped = s.stopping
			return processErr
		}); err != nil {
			return false, err
		}
		if detach || stopped {
			return true, nil
		}
	case protocol.MsgFrontendResize:
		msg, err := protocol.DecodeFrontendResize(frame.Payload)
		if err != nil {
			return false, err
		}
		if err := s.coordinate(func() error {
			if s.clientInstance != client {
				return nil
			}
			return s.resizeClient(client, msg.Cols, msg.Rows)
		}); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unexpected control frame %d", frame.Type)
	}
	return false, nil
}

func (s *Session) startPane(pane *Pane) {
	pane.initializeRuntime()
	s.watchPaneProcesses(pane)
	go pane.run()
	go relayPTYOutput(pane)
	go runPTYWriter(pane, func(error) {
		_ = terminatePane(pane)
		_ = s.handlePaneProcessExit(pane.ID)
	})
	go func() {
		_ = pane.Process.Wait()
		pane.stop()
		close(pane.processDone)
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
	_, clientState, finalPane, removed, err := s.RemovePane(paneID, clientID0)
	if err != nil || !removed {
		return err
	}
	if finalPane {
		s.ended = true
		return nil
	}
	s.resizeSessionToClient(clientState)
	return s.rebindOutputsAndPublishLayout(handoff)
}

func (s *Session) createWindow(cwd string, argv []string, cols, rows uint16, shell string) (*Pane, *Window, *ClientState, error) {
	paneID := s.AddPaneID()
	pane, err := StartPane(paneID, s.contextualPaneRequest(paneRequest{Cwd: cwd, Command: argv, Cols: cols, Rows: rows, Shell: shell}))
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

func (s *Session) handleInputBytes(c *ClientInstance, layoutRevision uint64, data []byte) (bool, error) {
	if c == nil {
		return false, nil
	}
	// Plain legacy text remains the overwhelmingly common path, including
	// printable text while Kitty flags 1|2 are active. Preserve batching and
	// the existing prompt/prefix behavior when no escape sequence is pending.
	if c.frontendInput.state == frontendParserGround && bytes.IndexByte(data, 0x1b) < 0 {
		return s.handleLegacyInputBytes(c, data)
	}
	dispatch := func(event frontendInputEvent) (bool, bool, error) {
		hadPrompt := s.ActivePrompt(clientID0) != nil
		detach, err := s.handleFrontendInputEvent(c, event)
		if err != nil || detach {
			return detach, false, err
		}
		if hadPrompt && s.ActivePrompt(clientID0) == nil {
			return false, true, nil
		}
		return false, false, nil
	}
	var pendingMotion *frontendInputEvent
	events := s.coalesceFrontendWheelBursts(c, c.frontendInput.Feed(layoutRevision, data))
	for _, event := range events {
		if event.Kind == frontendEventPointer && event.Pointer.Action == frontendPointerMove {
			copy := event
			pendingMotion = &copy
			continue
		}
		if pendingMotion != nil {
			if detach, stop, err := dispatch(*pendingMotion); err != nil || detach || stop {
				return detach, err
			}
			pendingMotion = nil
		}
		if detach, stop, err := dispatch(event); err != nil || detach || stop {
			return detach, err
		}
	}
	if pendingMotion != nil {
		if detach, _, err := dispatch(*pendingMotion); err != nil || detach {
			return detach, err
		}
	}
	return false, nil
}

func (s *Session) handleLegacyInputBytes(c *ClientInstance, data []byte) (bool, error) {
	pane, _ := s.ActivePane(clientID0)
	if pane != nil && s.InputIsNormal(clientID0) && !pane.isHistoryMode() &&
		bytes.IndexByte(data, 0x02) < 0 && (!pane.InputMode().applicationCursorKeys || bytes.IndexByte(data, 0x1b) < 0) {
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
		if pane.isHistoryMode() {
			copied, err := pane.handleHistoryInput(data[index:])
			if err != nil || len(copied) == 0 {
				return false, err
			}
			return false, c.writeFrontendTerminal(osc52ClipboardWrite(copied))
		}
		if translated, consumed, ok := translateApplicationCursor(data[index:], s.InputIsNormal(clientID0) && pane.InputMode().applicationCursorKeys); ok {
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

func (s *Session) resizeClient(c *ClientInstance, cols, rows uint16) error {
	handoff := s.beginOutputHandoff()
	if err := s.exitAllPaneHistoryModes(); err != nil {
		return err
	}
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

func (s *Session) handleServerInputEvent(c *ClientInstance, event serverInputEvent) (bool, error) {
	switch event.Command {
	case serverCommandNone:
		return false, nil
	case serverCommandLiteral:
		pane, _ := s.ActivePane(clientID0)
		if pane == nil {
			return false, nil
		}
		data := event.Data
		if len(data) == 0 {
			data = []byte{event.Byte}
		} else if translated, consumed, ok := translateApplicationCursor(data, pane.InputMode().applicationCursorKeys); ok && consumed == len(data) {
			data = translated
		}
		if err := pane.sendInput(data); err != nil {
			return false, fmt.Errorf("write pty: %w", err)
		}
		return false, nil
	case serverCommandExecute:
		return s.executeSessionCommand(c, event.CommandArgs)
	case serverCommandPrompt:
		return s.handlePromptEvent(c, event)
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

func (s *Session) handlePromptEvent(c *ClientInstance, event serverInputEvent) (bool, error) {
	if event.PromptKind == PromptKindConfirm &&
		(event.PromptAction == PromptActionSubmit || event.PromptAction == PromptActionCancel) {
		return false, s.resolvePrompt(clientID0, promptResult{
			Accepted: event.PromptAction == PromptActionSubmit && event.PromptText == "y",
			Text:     event.PromptText,
		})
	}
	switch event.PromptAction {
	case PromptActionChanged, PromptActionCancel:
		return false, s.publishStatusBar()
	case PromptActionSubmit:
		switch event.PromptKind {
		case PromptKindRenameWindow:
			return s.executeSessionCommand(c, []string{"rename-window", event.PromptText})
		case PromptKindRenameSession:
			return s.executeSessionCommand(c, []string{"rename-session", event.PromptText})
		case PromptKindCommand:
			argv, err := parseCommandLine(event.PromptText)
			if err == nil {
				var detach bool
				detach, err = s.executeSessionCommand(c, argv)
				if err == nil {
					if detach {
						return true, nil
					}
					return false, s.publishStatusBar()
				}
				var request *sessionSwitchRequest
				if errors.As(err, &request) {
					return false, request
				}
			}
			if err != nil {
				s.showStatusMessage(clientID0, err.Error())
				return false, s.publishStatusBar()
			}
		default:
			return false, fmt.Errorf("unsupported prompt kind %d", event.PromptKind)
		}
		return false, nil
	default:
		return false, nil
	}
}

func (s *Session) commandCreateWindow(c *ClientInstance) error {
	handoff := s.beginOutputHandoff()
	cols, rows, err := s.createWindowSize()
	if err != nil {
		return err
	}
	pane, _, _, err := s.createWindow(s.rootDir, nil, cols, rows, c.shell)
	if err != nil {
		return err
	}
	s.startPane(pane)
	if err := s.rebindOutputsAndPublishLayout(handoff); err != nil {
		return err
	}
	return nil
}

func (s *Session) commandSelectWindow(windowID uint64) error {
	handoff := s.beginOutputHandoff()
	if _, _, err := s.SelectWindow(clientID0, windowID); err != nil {
		return err
	}
	return s.rebindOutputsAndPublishLayout(handoff)
}

func (s *Session) commandSplit(c *ClientInstance, direction SplitDirection) error {
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
	cwd := s.rootDir
	newPane, err := StartPane(paneID, s.contextualPaneRequest(paneRequest{Cwd: cwd, Cols: uint16(cols), Rows: uint16(rows), Shell: c.shell}))
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
	s.startPane(newPane)
	if err := s.rebindOutputsAndPublishLayout(handoff); err != nil {
		return err
	}
	return nil
}

func (s *Session) commandNextLayout() error {
	window, _ := s.ActiveWindow(clientID0)
	if window == nil || len(window.Layout.PaneIDs()) <= 1 {
		return nil
	}
	handoff := s.beginOutputHandoff()
	_, clientState, changed, err := s.CycleWindowLayout(clientID0)
	if err != nil {
		return err
	}
	if !changed {
		return s.publishVisibleSnapshots(handoff)
	}
	s.resizeSessionToClient(clientState)
	return s.rebindOutputsAndPublishLayout(handoff)
}

func (s *Session) commandSwapPane(direction PaneSwapDirection) error {
	window, _ := s.ActiveWindow(clientID0)
	if window == nil || len(window.Layout.PaneIDs()) < 2 {
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
	return s.rebindOutputsAndPublishLayout(handoff)
}

func (s *Session) commandFocusPaneDirection(direction byte) error {
	window, _ := s.ActiveWindow(clientID0)
	if window == nil {
		return nil
	}
	zoomed := window.Zoomed
	var handoff *outputHandoff
	if zoomed {
		handoff = s.beginOutputHandoff()
	}
	_, clientState, err := s.FocusPaneDirection(clientID0, direction)
	if err != nil {
		return err
	}
	if !zoomed {
		return s.publishWindowLayout()
	}
	s.resizeSessionToClient(clientState)
	return s.rebindOutputsAndPublishLayout(handoff)
}

func (s *Session) commandToggleZoom() error {
	window, _ := s.ActiveWindow(clientID0)
	if window == nil || len(window.Layout.PaneIDs()) <= 1 {
		return nil
	}
	handoff := s.beginOutputHandoff()
	_, clientState, changed, err := s.ToggleZoom(clientID0)
	if err != nil {
		return err
	}
	if !changed {
		return s.publishVisibleSnapshots(handoff)
	}
	s.resizeSessionToClient(clientState)
	return s.rebindOutputsAndPublishLayout(handoff)
}

func (s *Session) commandResizePane(direction PaneResizeDirection, amount int) error {
	clientState := s.SnapshotClient(clientID0)
	if clientState == nil {
		return nil
	}
	handoff := s.beginOutputHandoff()
	_, clientState, changed, err := s.ResizeFocusedPane(clientID0, direction, amount)
	if err != nil {
		return err
	}
	if !changed {
		return s.publishVisibleSnapshots(handoff)
	}
	s.resizeSessionToClient(clientState)
	return s.rebindOutputsAndPublishLayout(handoff)
}

func (s *Session) commandEnterHistory() error {
	pane, _ := s.ActivePane(clientID0)
	if pane == nil {
		return nil
	}
	if pane.isHistoryMode() {
		return nil
	}
	_, err := pane.enterHistoryMode()
	return err
}

func (s *Session) commandClosePaneNow() error {
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
	if window != nil && clientState != nil {
		s.resizeSessionToClient(clientState)
		return s.rebindOutputsAndPublishLayout(handoff)
	}
	return s.publishStatusBar()
}
