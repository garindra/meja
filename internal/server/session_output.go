package server

import (
	"fmt"
	"io"

	"github.com/garindra/meja/internal/protocol"
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
	normal := protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "rgb", R: 42, G: 88, B: 170}}
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
			flags := ""
			if window.Active {
				flags += "*"
			}
			if window.Zoomed {
				flags += "Z"
			}
			if flags == "" {
				flags = " "
			}
			text += fmt.Sprintf("%d:%s%s ", window.Index, window.Title, flags)
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
		pane.releaseOutputStream(handoff.released)
	}
	return handoff
}

func (s *Session) rebindOutputsAndPublishLayout(handoff *outputHandoff) error {
	bindings, _, _, err := s.RebuildRenderBindings(clientID0)
	if err != nil {
		return err
	}
	if err := s.finishOutputHandoff(handoff, bindings); err != nil {
		return err
	}
	if err := s.publishStatusBar(); err != nil {
		return err
	}
	return s.publishWindowLayout()
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
	stillPending := make(map[int]struct{}, len(handoff.pending))
	for slot := range handoff.pending {
		stillPending[slot] = struct{}{}
	}
	for i := 0; i < len(handoff.pending); i++ {
		lease := <-handoff.released
		if lease == nil {
			continue
		}
		delete(stillPending, lease.Slot)
		if binding, ok := bySlot[lease.Slot]; ok {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
	}
	// A pane can already have lost its old connection's lease before a
	// reconnect begins. Its release then returns nil, but the handoff still
	// completed and the replacement output for that logical slot must be
	// attached. Wait for every release above, then attach those nil slots.
	for slot := range stillPending {
		if binding, ok := bySlot[slot]; ok {
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
	return pane.attachOutputStream(lease, window.LayoutRevision)
}

func (s *Session) publishVisibleSnapshots(handoff *outputHandoff) error {
	bindings, _ := s.RenderBindings(clientID0)
	return s.finishOutputHandoff(handoff, bindings)
}

func (s *Session) detachLeases(leases map[int]*OutputLease) error {
	for _, pane := range s.PanesSnapshot() {
		for _, lease := range leases {
			if err := pane.detachOutputStream(lease.Stream); err != nil {
				return err
			}
		}
	}
	return nil
}
