package render

import (
	"strings"
	"tali/internal/protocol"
	"testing"
)

func testRenderState() *ClientState {
	s := NewClientState()
	s.SetTerminalSize(8, 4)
	s.ApplyWindowLayout(protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, FocusedPaneID: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 8, Height: 3}}}})
	return s
}

func TestSteadyStateANSIUsesDamageOnly(t *testing.T) {
	s := testRenderState()
	s.SetWritePosition(0, protocol.SetWritePosition{Row: 1, Column: 2})
	s.WriteText(0, protocol.WriteText{CellWidth: 1, Text: []byte("x")})
	_ = RenderANSI(s)
	s.SetWritePosition(0, protocol.SetWritePosition{Row: 1, Column: 3})
	s.WriteText(0, protocol.WriteText{CellWidth: 1, Text: []byte("y")})
	out := string(RenderANSI(s))
	if strings.Contains(out, "\x1b[2J") || !strings.Contains(out, "y") {
		t.Fatalf("damage output=%q", out)
	}
}
func TestUnchangedANSIEmitsNothing(t *testing.T) {
	s := testRenderState()
	_ = RenderANSI(s)
	if out := RenderANSI(s); len(out) != 0 {
		t.Fatalf("unchanged output=%q", out)
	}
}
func TestFillMarksOnlyRequestedColumns(t *testing.T) {
	s := testRenderState()
	_ = RenderANSI(s)
	s.SetWritePosition(0, protocol.SetWritePosition{Row: 0, Column: 2})
	if !s.Fill(0, protocol.Fill{Columns: 3, Rune: '-', Width: 1}) {
		t.Fatal("fill rejected")
	}
	out := string(RenderANSI(s))
	if !strings.Contains(out, "---") {
		t.Fatalf("fill output=%q", out)
	}
}
