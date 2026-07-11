package render

import (
	"testing"

	"tali/internal/protocol"
)

func BenchmarkRenderANSI(b *testing.B) {
	state := NewClientState()
	state.SetTerminalSize(80, 24)
	state.SessionID = 0
	state.ActiveWindowID = 1
	state.Windows = []protocol.WindowInfo{{WindowID: 1, PaneID: 1, Index: 0, Title: "bash", Active: true}}
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 0, Width: 80, Height: 23}},
		},
	})
	state.ApplyBind(protocol.BindRenderStream{Slot: 0, PaneID: 1, BindingGeneration: 1})
	cells := make([]protocol.Cell, 80*23)
	for i := range cells {
		cells[i] = protocol.Cell{Rune: 'x', StyleID: 0, Width: 1}
	}
	state.ApplyReplace(0, protocol.ReplacePane{
		PaneID:            1,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              80,
		Rows:              23,
		Cells:             cells,
		Styles:            []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}}},
	})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RenderANSI(state)
	}
}
