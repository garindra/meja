package render

import (
	"tali/internal/protocol"
	"testing"
)

func BenchmarkIncrementalTextRender(b *testing.B) {
	s := NewClientState()
	s.SetTerminalSize(120, 40)
	s.ApplyWindowLayout(protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, FocusedPaneID: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 120, Height: 39}}}})
	_ = RenderANSI(s)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetWritePosition(0, protocol.SetWritePosition{Row: 10, Column: i % 100})
		s.WriteText(0, protocol.WriteText{CellWidth: 1, Text: []byte("x")})
		_ = RenderANSI(s)
	}
}
