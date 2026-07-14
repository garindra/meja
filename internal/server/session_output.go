package server

import (
	"fmt"
	"io"

	"tali/internal/protocol"
)

const (
	statusNormalStyleID uint32 = 1
	statusPromptStyleID uint32 = 2
)

type statusModel struct {
	width   int
	text    string
	styleID uint32
}

type statusCommand struct {
	attach     io.Writer
	detach     bool
	connection *Connection
	model      *statusModel
	done       chan error
}

func (s *Session) sendStatusCommand(command statusCommand) error {
	if s.statusCommands == nil {
		return nil
	}
	command.done = make(chan error, 1)
	select {
	case s.statusCommands <- command:
	case <-s.operationsDone:
		return nil
	}
	select {
	case err := <-command.done:
		return err
	case <-s.operationsDone:
		return nil
	}
}

func (s *Session) attachStatusOutput(connection *Connection, stream io.Writer) error {
	return s.sendStatusCommand(statusCommand{attach: stream, connection: connection})
}

func (s *Session) detachStatusOutput(connection *Connection) error {
	return s.sendStatusCommand(statusCommand{detach: true, connection: connection})
}

func (s *Session) runStatusOutput() {
	var connection *Connection
	var output *renderOutput
	initialized := false
	var latest statusModel
	var hasLatest bool
	for {
		select {
		case command := <-s.statusCommands:
			var err error
			switch {
			case command.attach != nil:
				connection = command.connection
				output = newRenderOutput(command.attach)
				initialized = false
				if hasLatest {
					err = renderStatusModel(output, latest, true)
					initialized = err == nil
				}
			case command.detach:
				if connection == command.connection {
					connection = nil
					output = nil
					initialized = false
				}
			case command.model != nil:
				latest = *command.model
				hasLatest = true
				if output != nil {
					err = renderStatusModel(output, latest, !initialized)
					initialized = err == nil
				}
			}
			if err != nil {
				output = nil
				initialized = false
			}
			command.done <- err
		case <-s.operationsDone:
			return
		}
	}
}

func renderStatusModel(output *renderOutput, model statusModel, full bool) error {
	normal := protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "rgb", R: 42, G: 99, B: 158}}
	prompt := protocol.Style{FG: protocol.Color{Mode: "indexed", Index: 0}, BG: protocol.Color{Mode: "indexed", Index: 3}}
	if full {
		if err := installStyle(output, statusNormalStyleID, normal); err != nil {
			return err
		}
		if err := installStyle(output, statusPromptStyleID, prompt); err != nil {
			return err
		}
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 0}); err != nil {
		return err
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWriteStyle, StyleID: model.styleID}); err != nil {
		return err
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeFill, Fill: protocol.Fill{Columns: model.width, Rune: ' ', Width: 1}}); err != nil {
		return err
	}
	runes := []rune(model.text)
	if len(runes) > model.width {
		runes = runes[:model.width]
	}
	if len(runes) > 0 {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 0}); err != nil {
			return err
		}
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte(string(runes))}); err != nil {
			return err
		}
	}
	return output.present()
}

func (s *Session) publishStatusBar() error {
	client := s.SnapshotClient(clientID0)
	if client == nil || client.TerminalCols == 0 {
		return nil
	}
	width := int(client.TerminalCols)
	styleID := statusNormalStyleID
	text := ""
	if prompt := s.ActivePrompt(clientID0); prompt != nil {
		styleID = statusPromptStyleID
		text = prompt.Label + string(prompt.Text)
	} else {
		list := s.WindowStatuses(clientID0)
		if name := s.SessionName(); name != "" {
			text = fmt.Sprintf("[%s] ", name)
		} else {
			text = fmt.Sprintf("[%d] ", s.ID)
		}
		for _, window := range list {
			marker := ' '
			if window.Active {
				marker = '*'
			}
			text += fmt.Sprintf("%d:%s%c ", window.Index, window.Title, marker)
		}
	}
	return s.sendStatusCommand(statusCommand{model: &statusModel{width: width, text: text, styleID: styleID}})
}

func (s *Session) publishWindowLayout() error {
	layout, err := s.WindowLayout(clientID0)
	if err != nil {
		return err
	}
	mgmtFrames := s.currentManagementFrames()
	if mgmtFrames == nil {
		return nil
	}
	return sendEncoded(mgmtFrames, protocol.MsgWindowLayout, layout, protocol.EncodeWindowLayout)
}

type outputHandoff struct {
	released chan *OutputLease
	pending  map[int]struct{}
}

func (s *Session) windowForPane(paneID uint64) *Window {
	for _, window := range s.Windows {
		if windowHasPane(window, paneID) {
			return cloneWindow(window)
		}
	}
	return nil
}

func (s *Session) beginOutputHandoff() *outputHandoff {
	bindings, _ := s.RenderBindings(clientID0)
	handoff := &outputHandoff{
		released: make(chan *OutputLease, len(bindings)),
		pending:  make(map[int]struct{}, len(bindings)),
	}
	for _, binding := range bindings {
		pane := s.Pane(binding.PaneID)
		if pane == nil {
			continue
		}
		handoff.pending[binding.Slot] = struct{}{}
		pane.releaseOutput(handoff.released)
	}
	return handoff
}

func (s *Session) publishBindingsAndSnapshots(handoff *outputHandoff) error {
	bindings, _, _, err := s.RebuildRenderBindings(clientID0)
	if err != nil {
		return err
	}
	return s.finishOutputHandoff(handoff, bindings)
}

func (s *Session) finishOutputHandoff(handoff *outputHandoff, bindings []RenderBinding) error {
	bySlot := make(map[int]RenderBinding, len(bindings))
	for _, binding := range bindings {
		bySlot[binding.Slot] = binding
	}
	if handoff == nil {
		for _, binding := range bindings {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
		return nil
	}
	for _, binding := range bindings {
		if _, waiting := handoff.pending[binding.Slot]; !waiting {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
	}
	for range handoff.pending {
		lease := <-handoff.released
		if lease == nil {
			continue
		}
		if binding, ok := bySlot[lease.Slot]; ok {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Session) attachBinding(binding RenderBinding) error {
	pane := s.Pane(binding.PaneID)
	window := s.windowForPane(binding.PaneID)
	if pane == nil || window == nil {
		return nil
	}
	lease := s.currentOutputLease(binding.Slot)
	if lease == nil {
		return nil
	}
	view := s.HistoryView(clientID0, pane.ID)
	return pane.attachOutputMode(lease, view == nil, func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: window.LayoutRevision}); err != nil {
			return err
		}
		if err := installStyle(output, protocol.CanonicalDefaultStyleID, protocol.CanonicalDefaultStyle()); err != nil {
			return err
		}
		return sendCurrentViewSnapshot(output, pane, view)
	})
}

func (s *Session) publishVisibleSnapshots(handoff *outputHandoff) error {
	bindings, _ := s.RenderBindings(clientID0)
	return s.finishOutputHandoff(handoff, bindings)
}

func (s *Session) detachLeases(leases map[int]*OutputLease) error {
	for _, pane := range s.PanesSnapshot() {
		for _, lease := range leases {
			if err := pane.detachOutput(lease.Stream); err != nil {
				return err
			}
		}
	}
	return nil
}
