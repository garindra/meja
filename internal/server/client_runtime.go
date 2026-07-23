package server

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/garindra/meja/internal/protocol"
)

func (c *ClientInstance) handleControlFrame(frame protocol.Frame) (bool, error) {
	state := c.sessionState()
	if state == nil {
		return false, errors.New("client instance has no session")
	}
	if c.Daemon != nil && c.ViewLeaseWindowID != 0 && c.ViewLeaseGeneration != 0 {
		if err := c.Daemon.validateWindowView(c.AttachmentID, c.ViewLeaseWindowID, c.ViewLeaseGeneration); err != nil {
			return false, err
		}
	}
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
		if state.attachedClient() != c {
			return false, nil
		}
		var processErr error
		detach, processErr = c.handleInputBytes(msg.LayoutRevision, msg.Data)
		if processErr == nil && !detach && msg.SourceIdle {
			if event, ok := c.frontendInput.flushLoneEscape(); ok {
				detach, processErr = c.handleFrontendInputEvent(event)
			}
		}
		stopped = c.Daemon != nil && c.Daemon.isSessionStopping(state)
		if processErr != nil {
			return false, processErr
		}
		if detach || stopped {
			return true, nil
		}
	case protocol.MsgFrontendResize:
		msg, err := protocol.DecodeFrontendResize(frame.Payload)
		if err != nil {
			return false, err
		}
		if state.attachedClient() != c {
			return false, nil
		}
		if err := c.resizeClient(msg.Cols, msg.Rows); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unexpected control frame %d", frame.Type)
	}
	return false, nil
}

func (d *Daemon) startPane(state *SessionState, pane *Pane) {
	if pane == nil {
		return
	}
	pane.initializeRuntime()
	if d != nil {
		d.watchPaneProcesses(state, pane)
	}
	go pane.run()
	go relayPTYOutput(pane)
	go runPTYWriter(pane, func(error) {
		_ = terminatePane(pane)
		if d != nil {
			d.postPaneProcessExit(pane.ID)
		}
	})
	go func() {
		_ = pane.Process.Wait()
		pane.stop()
		close(pane.processDone)
		if d != nil {
			d.postPaneProcessExit(pane.ID)
		}
	}()
}

func (c *ClientInstance) createWindowSize() (uint16, uint16, error) {
	if clientState := c.snapshotClient(); clientState != nil && clientState.TerminalCols > 0 && clientState.TerminalRows > 0 {
		return clientState.TerminalCols, clientState.TerminalRows, nil
	}
	activePane := c.activePane()
	if activePane == nil {
		return 0, 0, fmt.Errorf("create window: no active pane")
	}
	cols, rows := activePane.TerminalSize()
	return uint16(cols), uint16(rows), nil
}

func (c *ClientInstance) handleInputBytes(layoutRevision uint64, data []byte) (bool, error) {
	if c == nil {
		return false, nil
	}
	// Plain legacy text remains the overwhelmingly common path, including
	// printable text while Kitty flags 1|2 are active. Preserve batching and
	// the existing prompt/prefix behavior when no escape sequence is pending.
	if c.frontendInput.state == frontendParserGround && bytes.IndexByte(data, 0x1b) < 0 {
		return c.handleLegacyInputBytes(data)
	}
	dispatch := func(event frontendInputEvent) (bool, bool, error) {
		hadPrompt := c.ActivePrompt() != nil
		detach, err := c.handleFrontendInputEvent(event)
		if err != nil || detach {
			return detach, false, err
		}
		if hadPrompt && c.ActivePrompt() == nil {
			return false, true, nil
		}
		return false, false, nil
	}
	var pendingMotion *frontendInputEvent
	events := c.coalesceFrontendWheelBursts(c.frontendInput.Feed(layoutRevision, data))
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

func (c *ClientInstance) handleLegacyInputBytes(data []byte) (bool, error) {
	pane := c.activePane()
	if pane != nil && c.InputIsNormal() && !pane.isHistoryMode() &&
		bytes.IndexByte(data, 0x02) < 0 && (!pane.InputMode().applicationCursorKeys || bytes.IndexByte(data, 0x1b) < 0) {
		if err := pane.sendInput(data); err != nil {
			return false, fmt.Errorf("write pty input: %w", err)
		}
		return false, nil
	}
	for index := 0; index < len(data); index++ {
		if c.ActivePrompt() != nil {
			consumed, events, terminated := c.ConsumePromptInput(data[index:])
			for _, event := range events {
				detach, err := c.handleServerInputEvent(event)
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
		pane := c.activePane()
		if pane == nil {
			break
		}
		if pane.isHistoryMode() {
			copied, err := pane.handleHistoryInput(data[index:])
			if err != nil || len(copied) == 0 {
				return false, err
			}
			if c.Daemon != nil {
				if _, bufferErr := c.Daemon.pasteBuffers.addAutomatic(copied); bufferErr != nil {
					return false, bufferErr
				}
			}
			return false, c.writeFrontendTerminal(osc52ClipboardWrite(copied))
		}
		if translated, consumed, ok := translateApplicationCursor(data[index:], c.InputIsNormal() && pane.InputMode().applicationCursorKeys); ok {
			if err := pane.sendInput(translated); err != nil {
				return false, fmt.Errorf("write application cursor input: %w", err)
			}
			index += consumed - 1
			continue
		}
		event := c.ConsumeInputByte(data[index])
		detach, err := c.handleServerInputEvent(event)
		if err != nil || detach {
			return detach, err
		}
	}
	return false, nil
}

func (c *ClientInstance) resizeClient(cols, rows uint16) error {
	// A pane's START_RENDER barrier describes the grid used by every following
	// paint command on that output stream. Release the current bindings before
	// changing any pane terminal grid; otherwise a widening resize can emit
	// new-grid paint through the old barrier and make the frontend reject the
	// display stream. Pane command FIFO ordering guarantees each release is
	// processed before the prepared transition applies the corresponding resize.
	if err := c.sessionState().exitAllPaneHistoryModes(); err != nil {
		return err
	}
	// The dimensions belong to this client actor. Publish them before asking
	// the daemon for its immutable projection so that the plan describes the
	// resize being applied, rather than the previous scanout.
	c.terminalCols.Store(uint32(cols))
	c.terminalRows.Store(uint32(rows))
	transition, err := c.Daemon.resizeClientView(c, cols, rows)
	if err != nil {
		return err
	}
	return c.applyViewTransition(transition)
}

func (c *ClientInstance) handleServerInputEvent(event serverInputEvent) (bool, error) {
	switch event.Command {
	case serverCommandNone:
		return false, nil
	case serverCommandLiteral:
		pane := c.activePane()
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
		detach, err := c.executeAttachedCommand(event.CommandArgs)
		if err == nil || detach {
			return detach, err
		}
		// Prefix commands are interactive UI actions. A rejected target or
		// invalid command must be reported in the status bar, not escape the
		// control-frame handler and tear down the transport.
		c.showStatusMessage(err.Error())
		return false, c.publishStatusBar()
	case serverCommandPrompt:
		return c.handlePromptEvent(event)
	case serverCommandOpenCommandPrompt:
		if _, err := c.BeginCommandPrompt(); err != nil {
			return false, err
		}
		return false, c.publishStatusBar()
	}
	return false, nil
}

func (c *ClientInstance) handlePromptEvent(event serverInputEvent) (bool, error) {
	switch event.PromptAction {
	case PromptActionChanged:
		return false, c.publishStatusBar()
	case PromptActionSubmit, PromptActionCancel:
		return c.resolvePrompt(promptResult{Submitted: event.PromptAction == PromptActionSubmit, Text: event.PromptText})
	default:
		return false, nil
	}
}

func (c *ClientInstance) runCommandPromptAnswer(result promptResult) (bool, error) {
	if !result.Submitted {
		return false, c.publishStatusBar()
	}
	argv, err := parseCommandLine(result.Text)
	if err == nil {
		var detach bool
		detach, err = c.executeAttachedCommand(argv)
		if err == nil {
			if detach {
				return true, nil
			}
			return false, c.publishStatusBar()
		}
	}
	c.showStatusMessage(err.Error())
	return false, c.publishStatusBar()
}

func (c *ClientInstance) commandEnterHistory() error {
	pane := c.activePane()
	if pane == nil {
		return nil
	}
	if pane.isHistoryMode() {
		return nil
	}
	_, err := pane.enterHistoryMode()
	return err
}
