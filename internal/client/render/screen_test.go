package render

import (
	"strings"
	"tali/internal/protocol"
	"testing"
)

func TestDisplayCommandsApplyAtStreamWriteHead(t *testing.T) {
	s := NewClientState()
	s.SetTerminalSize(10, 4)
	s.ApplyWindowLayout(protocol.WindowLayout{WindowID: 1, LayoutRevision: 2, FocusedPaneID: 5, Panes: []protocol.PanePlacement{{PaneID: 5, Slot: 0, Rect: protocol.Rect{Width: 10, Height: 3}}}})
	if !s.ResetStream(0) || !s.SetWritePosition(0, protocol.SetWritePosition{Row: 1, Column: 2}) || !s.SetWriteStyle(0, protocol.SetWriteStyle{StyleID: 0}) || !s.WriteText(0, protocol.WriteText{CellWidth: 1, Text: []byte("ls")}) || !s.UpdateCursor(0, protocol.CursorUpdate{Cursor: protocol.Cursor{X: 4, Y: 1}, Visible: true}) {
		t.Fatal("display command rejected")
	}
	p := s.Panes[5]
	if got := string([]rune{p.Grid.Cells[12].Rune, p.Grid.Cells[13].Rune}); got != "ls" {
		t.Fatalf("text=%q", got)
	}
	out := string(RenderANSI(s))
	if !strings.Contains(out, "ls") {
		t.Fatalf("ANSI output=%q", out)
	}
}

func TestWideTextCreatesContinuationCell(t *testing.T) {
	s := NewClientState()
	s.ApplyWindowLayout(protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 4, Height: 1}}}})
	s.SetWritePosition(0, protocol.SetWritePosition{})
	if !s.WriteText(0, protocol.WriteText{CellWidth: 2, Text: []byte("界")}) {
		t.Fatal("wide text rejected")
	}
	cells := s.Panes[1].Grid.Cells
	if cells[0].Rune != '界' || cells[0].Width != 2 || cells[1].Width != 0 {
		t.Fatalf("cells=%#v", cells)
	}
}

func TestDefaultTextOverrideDoesNotChangeLatchedStyle(t *testing.T) {
	s := NewClientState()
	s.ApplyWindowLayout(protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 6, Height: 1}}}})
	if !s.InstallStyle(0, protocol.StyleInstall{ID: 2, Style: protocol.Style{Bold: true}}) || !s.SetWriteStyle(0, protocol.SetWriteStyle{StyleID: 2}) {
		t.Fatal("failed to install/select nondefault style")
	}
	if !s.WriteTextDefault(0, []byte("d")) || !s.WriteText(0, protocol.WriteText{CellWidth: 1, Text: []byte("c")}) {
		t.Fatal("text commands rejected")
	}
	p := s.Panes[1]
	if p.Grid.Cells[0].StyleID != 0 || p.Grid.Cells[1].StyleID != 2 {
		t.Fatalf("styles=%d,%d", p.Grid.Cells[0].StyleID, p.Grid.Cells[1].StyleID)
	}
	if got := s.Streams[0].StyleID; got != 2 {
		t.Fatalf("latched style=%d, want 2", got)
	}
}

func TestDefaultTextOverrideRejectsMissingCanonicalStyle(t *testing.T) {
	s := NewClientState()
	s.ApplyWindowLayout(protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 4, Height: 1}}}})
	delete(s.Panes[1].Styles, protocol.CanonicalDefaultStyleID)
	if s.WriteTextDefault(0, []byte("x")) {
		t.Fatal("accepted default text without canonical style 0")
	}
	if got := s.Streams[0].StyleID; got != 0 {
		t.Fatalf("stream style changed to %d", got)
	}
}
