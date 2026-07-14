package server

import (
	"fmt"
	"io"

	"tali/internal/protocol"
)

func (s *sessionState) publishStatusBar() error {
	client := s.session.SnapshotClient(clientID0)
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
	if prompt := s.session.ActivePrompt(clientID0); prompt != nil {
		style = protocol.Style{
			FG: protocol.Color{Mode: "indexed", Index: 0},
			BG: protocol.Color{Mode: "indexed", Index: 3},
		}
		text = prompt.Label + string(prompt.Text)
	} else {
		list := s.session.WindowStatuses(clientID0)
		if name := s.session.SessionName(); name != "" {
			text = fmt.Sprintf("[%s] ", name)
		} else {
			text = fmt.Sprintf("[%d] ", s.session.ID)
		}
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
	mgmtFrames := s.currentManagementFrames()
	if mgmtFrames == nil {
		return nil
	}
	return sendEncoded(mgmtFrames, protocol.MsgStatusBar, protocol.StatusBar{
		Cols:   width,
		Cells:  cells,
		Styles: []protocol.StyleDefinition{{ID: 0, Style: style}},
	}, protocol.EncodeStatusBar)
}

func (s *sessionState) publishWindowLayout() error {
	layout, err := s.session.WindowLayout(clientID0)
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
	released chan int
	pending  map[int]struct{}
}

func (s *sessionState) beginOutputHandoff() *outputHandoff {
	bindings, _ := s.session.RenderBindings(clientID0)
	handoff := &outputHandoff{
		released: make(chan int, len(bindings)),
		pending:  make(map[int]struct{}, len(bindings)),
	}
	for _, binding := range bindings {
		pane := s.session.Pane(binding.PaneID)
		if pane == nil {
			continue
		}
		handoff.pending[binding.Slot] = struct{}{}
		pane.releaseOutput(binding.Slot, handoff.released)
	}
	return handoff
}

func (s *sessionState) publishBindingsAndSnapshots(handoff *outputHandoff) error {
	bindings, _, _, err := s.session.RebuildRenderBindings(clientID0)
	if err != nil {
		return err
	}
	return s.finishOutputHandoff(handoff, bindings)
}

func (s *sessionState) finishOutputHandoff(handoff *outputHandoff, bindings []RenderBinding) error {
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
		slot := <-handoff.released
		if binding, ok := bySlot[slot]; ok {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sessionState) attachBinding(binding RenderBinding) error {
	pane := s.session.Pane(binding.PaneID)
	window := s.windowForPane(binding.PaneID)
	if pane == nil || window == nil {
		return nil
	}
	stream := s.currentOutputStream(binding.Slot)
	if stream == nil {
		return nil
	}
	return pane.attachOutput(stream, func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: window.LayoutRevision}); err != nil {
			return err
		}
		if err := installStyle(output, protocol.CanonicalDefaultStyleID, protocol.CanonicalDefaultStyle()); err != nil {
			return err
		}
		return s.sendCurrentViewSnapshot(output, pane)
	})
}

func (s *sessionState) publishVisibleSnapshots(handoff *outputHandoff) error {
	bindings, _ := s.session.RenderBindings(clientID0)
	return s.finishOutputHandoff(handoff, bindings)
}

func (s *sessionState) detachStreams(streams map[int]io.Writer) error {
	for _, pane := range s.session.PanesSnapshot() {
		for _, stream := range streams {
			if err := pane.detachOutput(stream); err != nil {
				return err
			}
		}
	}
	return nil
}
