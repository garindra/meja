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
	state.Grid = Screen{Cols: 80, Rows: 23, Cells: make([]protocol.Cell, 80*23)}
	for i := range state.Grid.Cells {
		state.Grid.Cells[i] = protocol.Cell{Rune: 'x', StyleID: 0, Width: 1}
	}
	state.CursorVisible = true
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RenderANSI(state)
	}
}
