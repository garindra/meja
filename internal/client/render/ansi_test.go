package render

import (
	"strings"
	"testing"

	"tali/internal/protocol"
)

func TestTabBarShowsActiveMarkerAndSessionID(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(20, 5)
	state.SessionID = 7
	state.Windows = []protocol.WindowInfo{
		{WindowID: 1, PaneID: 1, Index: 0, Title: "bash", Active: true},
		{WindowID: 2, PaneID: 2, Index: 1, Title: "logs"},
	}
	state.ActiveWindowID = 1
	got := string(RenderANSI(state))
	if !strings.Contains(got, "[7]") {
		t.Fatalf("RenderANSI() missing active session id prefix: %q", got)
	}
	if !strings.Contains(got, "[7] 0:bash* ") {
		t.Fatalf("RenderANSI() missing active tab marker: %q", got)
	}
}

func TestRenderANSIComposesMultiplePaneGridsAndBorder(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(9, 4)
	state.SessionID = 7
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{X: 0, Y: 0, Width: 4, Height: 3}},
			{PaneID: 11, Rect: protocol.Rect{X: 5, Y: 0, Width: 4, Height: 3}},
		},
	})
	state.ApplyBind(protocol.BindRenderStream{Slot: 0, PaneID: 10, BindingGeneration: 1})
	state.ApplyBind(protocol.BindRenderStream{Slot: 1, PaneID: 11, BindingGeneration: 2})
	state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            10,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              4,
		Rows:              3,
		Cells:             repeatedCells("ABCDWXYZ1234"),
		Styles:            []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}}},
	})
	state.ApplyReplace(1, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            11,
		BindingGeneration: 2,
		Generation:        1,
		Cols:              4,
		Rows:              3,
		Cells:             repeatedCells("efghijklmnop"),
		Styles:            []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}}},
	})

	got := string(RenderANSI(state))
	if !strings.Contains(got, "ABCD│efgh") {
		t.Fatalf("RenderANSI() missing first composed row: %q", got)
	}
	if !strings.Contains(got, "WXYZ│ijkl") {
		t.Fatalf("RenderANSI() missing second composed row: %q", got)
	}
}

func TestTabBarTruncatesWithoutWrapping(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(10, 3)
	state.SessionID = 7
	state.Windows = []protocol.WindowInfo{
		{WindowID: 1, PaneID: 1, Index: 0, Title: "verylongtitle", Active: true},
	}
	state.ActiveWindowID = 1
	bar := renderTabBar(state)
	if len(bar) < 10 {
		t.Fatalf("tab bar too short: %d", len(bar))
	}
	if strings.Contains(bar, "\n") {
		t.Fatalf("tab bar wrapped: %q", bar)
	}
}

func TestRenderANSIDoesNotClearScreenOnSteadyStateRedraw(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(4, 3)
	state.SessionID = 7
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 1})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 0, Width: 4, Height: 2}},
		},
	})
	state.ApplyBind(protocol.BindRenderStream{Slot: 0, PaneID: 1, BindingGeneration: 1})
	state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            1,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              4,
		Rows:              2,
		Cells:             repeatedCells("abcdefgh"),
		Styles:            []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}}},
	})

	first := string(RenderANSI(state))
	if !strings.Contains(first, "\x1b[H\x1b[2J") {
		t.Fatalf("first render missing clear: %q", first)
	}
	second := string(RenderANSI(state))
	if strings.Contains(second, "\x1b[H\x1b[2J") {
		t.Fatalf("steady-state redraw unexpectedly clears screen: %q", second)
	}
	if second != "" {
		t.Fatalf("unchanged steady-state redraw emitted output: %q", second)
	}
}

func TestRenderANSIEmitsOnlyReportedCellRun(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(4, 3)
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 1})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 0, Width: 4, Height: 2}},
		},
	})
	state.ApplyBind(protocol.BindRenderStream{Slot: 0, PaneID: 1, BindingGeneration: 1})
	state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            1,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              4,
		Rows:              2,
		Cells:             repeatedCells("abcdefgh"),
	})
	_ = RenderANSI(state)

	if !state.ApplyPaneUpdate(0, protocol.PaneUpdate{
		BindingGeneration: 1,
		BaseGeneration:    1,
		Generation:        2,
		Runs: []protocol.CellRun{{
			Row:    0,
			Column: 1,
			Cells:  repeatedCells("Z"),
		}},
	}) {
		t.Fatal("ApplyPaneUpdate() rejected valid damage")
	}
	got := string(RenderANSI(state))
	if !strings.Contains(got, "\x1b[1;2H") || !strings.Contains(got, "Z") {
		t.Fatalf("incremental render missing damaged cell: %q", got)
	}
	if strings.Contains(got, "abcd") || strings.Contains(got, "efgh") {
		t.Fatalf("incremental render emitted an undamaged row: %q", got)
	}
	if strings.Contains(got, "\x1b[H\x1b[2J") {
		t.Fatalf("incremental render cleared the screen: %q", got)
	}
}

func TestRenderANSIDoesNotDetectUnreportedGridMutation(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(4, 3)
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 1, Rect: protocol.Rect{Width: 4, Height: 2}},
		},
	})
	state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            1,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              4,
		Rows:              2,
		Cells:             repeatedCells("abcdefgh"),
	})
	_ = RenderANSI(state)

	state.Panes[1].Grid.Cells[0].Rune = 'Z'
	if got := RenderANSI(state); len(got) != 0 {
		t.Fatalf("unreported mutation produced terminal output: %q", got)
	}
}

func TestRenderANSIPreservesDisjointDamageSpans(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(8, 3)
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 1, Rect: protocol.Rect{Width: 8, Height: 2}},
		},
	})
	state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            1,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              8,
		Rows:              2,
		Cells:             repeatedCells("abcdefghABCDEFGH"),
	})
	_ = RenderANSI(state)

	state.Panes[1].Grid.Cells[0].Rune = 'X'
	state.Panes[1].Grid.Cells[7].Rune = 'Y'
	state.markDamageRect(protocol.Rect{Width: 1, Height: 1})
	state.markDamageRect(protocol.Rect{X: 7, Width: 1, Height: 1})
	got := string(RenderANSI(state))
	if !strings.Contains(got, "\x1b[1;1H") || !strings.Contains(got, "\x1b[1;8H") {
		t.Fatalf("disjoint damage positions missing: %q", got)
	}
	if strings.Contains(got, "bcdefg") {
		t.Fatalf("disjoint damage widened across unchanged cells: %q", got)
	}
}

func TestPaneFocusChangeDoesNotRedrawTabBar(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(9, 3)
	state.Windows = []protocol.WindowInfo{{WindowID: 1, Index: 0, Title: "bash", Active: true}}
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{Width: 4, Height: 2}},
			{PaneID: 11, Rect: protocol.Rect{X: 5, Width: 4, Height: 2}},
		},
	})
	state.ApplyReplace(0, protocol.ReplacePane{WindowID: 1, PaneID: 10, BindingGeneration: 1, Cols: 4, Rows: 2, Cells: repeatedCells("abcdefgh")})
	state.ApplyReplace(1, protocol.ReplacePane{WindowID: 1, PaneID: 11, BindingGeneration: 2, Cols: 4, Rows: 2, Cells: repeatedCells("ABCDEFGH")})
	_ = RenderANSI(state)

	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 11})
	got := string(RenderANSI(state))
	if strings.Contains(got, "0:bash") {
		t.Fatalf("pane focus change redrew unchanged tab bar: %q", got)
	}
}

func TestRenderANSIRepaintsEntireReplacementPaneWithoutDiffing(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(4, 3)
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 1})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 0, Width: 4, Height: 2}},
		},
	})
	snapshot := protocol.ReplacePane{
		WindowID:          1,
		PaneID:            1,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              4,
		Rows:              2,
		Cells:             repeatedCells("abcdefgh"),
	}
	state.ApplyReplace(0, snapshot)
	_ = RenderANSI(state)

	snapshot.Generation++
	state.ApplyReplace(0, snapshot)
	got := string(RenderANSI(state))
	if !strings.Contains(got, "abcd") || !strings.Contains(got, "efgh") {
		t.Fatalf("replacement snapshot was diffed instead of fully repainted: %q", got)
	}
}

func repeatedCells(s string) []protocol.Cell {
	out := make([]protocol.Cell, 0, len(s))
	for _, r := range s {
		out = append(out, protocol.Cell{Rune: r, Width: 1})
	}
	return out
}
