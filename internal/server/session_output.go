package server

import (
	"fmt"

	"tali/internal/protocol"
)

func (s *Session) publishStatusBar() error {
	client := s.SnapshotClient(clientID0)
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
	if prompt := s.ActivePrompt(clientID0); prompt != nil {
		style = protocol.Style{
			FG: protocol.Color{Mode: "indexed", Index: 0},
			BG: protocol.Color{Mode: "indexed", Index: 3},
		}
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
