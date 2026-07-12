package render

import (
	"strings"
	"testing"

	"tali/internal/protocol"
)

func TestStatusBarRendersServerCells(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(20, 5)
	state.ApplyStatusBar(protocol.StatusBar{Cols: 20, Cells: repeatedCells("[7] 0:bash*         ")})
	got := string(RenderANSI(state))
	if !strings.Contains(got, "[7] 0:bash* ") {
		t.Fatalf("RenderANSI() missing server status cells: %q", got)
	}
}

func TestRenderANSIComposesMultiplePaneGridsAndBorder(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(9, 4)
	state.SessionID = 7
	selectTestPane(state, 1, 10)
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

func TestStatusBarNeverWraps(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(10, 3)
	state.ApplyStatusBar(protocol.StatusBar{Cols: 10, Cells: repeatedCells("0123456789")})
	got := string(RenderANSI(state))
	if strings.Contains(got, "\n") {
		t.Fatalf("status bar wrapped: %q", got)
	}
}

func TestRenderANSIDoesNotClearScreenOnSteadyStateRedraw(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(4, 3)
	state.SessionID = 7
	selectTestPane(state, 1, 1)
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
	selectTestPane(state, 1, 1)
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
	selectTestPane(state, 1, 10)
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

	selectTestPane(state, 1, 11)
	got := string(RenderANSI(state))
	if strings.Contains(got, "\x1b[3;1H") {
		t.Fatalf("pane focus change redrew unchanged status bar: %q", got)
	}
}

func TestRenderANSIRepaintsEntireReplacementPaneWithoutDiffing(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(4, 3)
	selectTestPane(state, 1, 1)
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
