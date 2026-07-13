package render

import (
	"strings"
	"tali/internal/protocol"
	"testing"
	"time"
)

func TestReconnectStatusPreservesExactMessageAndOrangeStyle(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(80, 24)
	_ = RenderANSI(state)
	state.SetReconnecting(true, time.Now().Add(-23*time.Second))
	output := string(RenderANSI(state))
	if strings.Contains(output, "\x1b[2J") {
		t.Fatalf("reconnect status caused a content redraw: %q", output)
	}
	if !strings.Contains(output, "tali is reconnecting... [Last contact 23 seconds ago]") {
		t.Fatalf("reconnect status missing from ANSI output: %q", output)
	}
	if !strings.Contains(output, "\x1b[0;38;2;255;165;0;49m") {
		t.Fatalf("reconnect status is not orange: %q", output)
	}
	if !strings.Contains(output, "\x1b[24;1H") {
		t.Fatalf("reconnect status is not on the status row: %q", output)
	}
}

func TestReconnectStatusClearsWithoutContentRedraw(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(20, 4)
	state.SetReconnecting(true, time.Now())
	_ = RenderANSI(state)
	state.SetReconnecting(false, time.Time{})
	output := string(RenderANSI(state))
	if strings.Contains(output, "tali is reconnecting") {
		t.Fatalf("reconnect status remained after reconnect: %q", output)
	}
}

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
