package render

import (
	"testing"

	"tali/internal/protocol"
)

func TestPaneStateRoundTripAndSetRun(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{X: 0, Y: 0, Width: 3, Height: 1}},
		},
	})
	state.ApplyBind(protocol.BindRenderStream{Slot: 0, SessionID: 0, WindowID: 1, PaneID: 10, BindingGeneration: 3})
	ok := state.ApplyReplace(0, protocol.ReplacePane{
		SessionID:         0,
		WindowID:          1,
		PaneID:            10,
		BindingGeneration: 3,
		Generation:        10,
		Cols:              3,
		Rows:              1,
		Cells: []protocol.Cell{
			{Rune: 'a', Width: 1},
			{Rune: 'b', Width: 1},
			{Rune: 'c', Width: 1},
		},
		Styles: []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}}},
	})
	if !ok {
		t.Fatal("ApplyReplace() rejected matching binding")
	}
	ok = state.ApplySetRun(0, protocol.SetRun{
		BindingGeneration: 3,
		BaseGeneration:    10,
		Generation:        11,
		Row:               0,
		Column:            1,
		Cells:             []protocol.Cell{{Rune: 'Z', Width: 1}},
	})
	if !ok || state.Panes[10].Grid.Cells[1].Rune != 'Z' || state.Panes[10].Generation != 11 {
		t.Fatalf("ApplySetRun() failed: %#v", state.Panes[10])
	}
}

func TestGenerationMismatchRejected(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{X: 0, Y: 0, Width: 1, Height: 1}},
		},
	})
	state.ApplyBind(protocol.BindRenderStream{Slot: 0, PaneID: 10, BindingGeneration: 1})
	state.ApplyReplace(0, protocol.ReplacePane{WindowID: 1, PaneID: 10, BindingGeneration: 1, Generation: 5, Cols: 1, Rows: 1, Cells: []protocol.Cell{{Rune: 'a', Width: 1}}})
	if state.ApplySetRun(0, protocol.SetRun{BindingGeneration: 1, BaseGeneration: 4, Generation: 6, Row: 0, Column: 0, Cells: []protocol.Cell{{Rune: 'b', Width: 1}}}) {
		t.Fatal("ApplySetRun() unexpectedly accepted mismatched generation")
	}
}

func TestWindowNavigation(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 10, PaneID: 10, Index: 0, Title: "bash", Active: true},
			{WindowID: 20, PaneID: 20, Index: 1, Title: "logs"},
			{WindowID: 30, PaneID: 30, Index: 2, Title: "vim"},
		},
		ActiveWindowID: 10,
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 10, PaneID: 10})
	if got, _ := state.NextWindowID(); got != 20 {
		t.Fatalf("NextWindowID() = %d, want 20", got)
	}
	if got, _ := state.PreviousWindowID(); got != 30 {
		t.Fatalf("PreviousWindowID() = %d, want 30", got)
	}
	if got, _ := state.WindowIDByIndex(2); got != 30 {
		t.Fatalf("WindowIDByIndex() = %d, want 30", got)
	}
}

func TestLastActiveWindowTrackingToggles(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 1, PaneID: 10, Index: 0, Title: "bash"},
			{WindowID: 2, PaneID: 11, Index: 1, Title: "logs"},
		},
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 2, PaneID: 11})
	if got, ok := state.LastActiveWindowID(); !ok || got != 1 {
		t.Fatalf("LastActiveWindowID() after 1->2 = %d, %v; want 1, true", got, ok)
	}
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	if got, ok := state.LastActiveWindowID(); !ok || got != 2 {
		t.Fatalf("LastActiveWindowID() after 2->1 = %d, %v; want 2, true", got, ok)
	}
}

func TestWindowListDoesNotOverrideExplicitSelection(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 1, PaneID: 10, Index: 0, Title: "bash", Active: true},
			{WindowID: 2, PaneID: 11, Index: 1, Title: "logs"},
		},
		ActiveWindowID: 1,
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 2, PaneID: 11})
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 1, PaneID: 10, Index: 0, Title: "bash", Active: true},
			{WindowID: 2, PaneID: 11, Index: 1, Title: "logs"},
		},
		ActiveWindowID: 1,
	})
	if state.ActiveWindowID != 2 || state.FocusedPaneID != 11 {
		t.Fatalf("window list overwrote explicit selection: window=%d pane=%d", state.ActiveWindowID, state.FocusedPaneID)
	}
}

func TestWindowLayoutStoresMultiplePaneRectsAndFocusOrder(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(12, 4)
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID:       1,
		LayoutRevision: 2,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{X: 0, Y: 0, Width: 5, Height: 3}},
			{PaneID: 11, Rect: protocol.Rect{X: 6, Y: 0, Width: 6, Height: 3}},
		},
	})
	if len(state.Panes) != 2 || state.Panes[11].Rect.X != 6 {
		t.Fatalf("ApplyWindowLayout() panes = %#v", state.Panes)
	}
	if got, ok := state.NextFocusablePaneID(); !ok || got != 11 {
		t.Fatalf("NextFocusablePaneID() = %d, %v; want 11, true", got, ok)
	}
}

func TestDirectionalFocusCanSelectPaneZero(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 1})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 0, Rect: protocol.Rect{X: 0, Y: 0, Width: 80, Height: 11}},
			{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 12, Width: 80, Height: 11}},
		},
	})
	if got, ok := state.FocusablePaneID('A'); !ok || got != 0 {
		t.Fatalf("FocusablePaneID(up) = %d, %v; want 0, true", got, ok)
	}

	state.FocusedPaneID = 2
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 0, Rect: protocol.Rect{X: 0, Y: 0, Width: 39, Height: 23}},
			{PaneID: 2, Rect: protocol.Rect{X: 40, Y: 0, Width: 40, Height: 23}},
		},
	})
	if got, ok := state.FocusablePaneID('D'); !ok || got != 0 {
		t.Fatalf("FocusablePaneID(left) = %d, %v; want 0, true", got, ok)
	}
}

func TestReplaceAcceptsNewPaneAfterShiftedPaneArrivesOnAnotherSlot(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 1})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 0, Rect: protocol.Rect{X: 0, Y: 0, Width: 80, Height: 19}},
			{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 20, Width: 80, Height: 19}},
		},
	})
	if !state.ApplyReplace(0, protocol.ReplacePane{WindowID: 1, PaneID: 0, BindingGeneration: 3, Cols: 80, Rows: 19}) ||
		!state.ApplyReplace(1, protocol.ReplacePane{WindowID: 1, PaneID: 1, BindingGeneration: 4, Cols: 80, Rows: 19}) {
		t.Fatal("initial pane bindings rejected")
	}

	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 2})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 0, Rect: protocol.Rect{X: 0, Y: 0, Width: 80, Height: 9}},
			{PaneID: 2, Rect: protocol.Rect{X: 0, Y: 10, Width: 80, Height: 9}},
			{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 20, Width: 80, Height: 19}},
		},
	})
	// Independent QUIC streams may deliver the shifted old pane before the new pane.
	if !state.ApplyReplace(2, protocol.ReplacePane{WindowID: 1, PaneID: 1, BindingGeneration: 10, Cols: 80, Rows: 19}) {
		t.Fatal("shifted pane replacement rejected")
	}
	if !state.ApplyReplace(1, protocol.ReplacePane{WindowID: 1, PaneID: 2, BindingGeneration: 9, Cols: 80, Rows: 9}) {
		t.Fatal("new pane replacement rejected after shifted pane arrived")
	}
	if got := state.RenderSlots[1]; got != 2 {
		t.Fatalf("render slot 1 = pane %d, want pane 2", got)
	}
}

func TestWindowSelectionKeepsPresentedLayoutUntilReplacement(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(12, 4)
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{X: 0, Y: 0, Width: 5, Height: 3}},
			{PaneID: 11, Rect: protocol.Rect{X: 6, Y: 0, Width: 6, Height: 3}},
		},
	})
	state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            10,
		BindingGeneration: 1,
		Generation:        1,
		Cols:              5,
		Rows:              3,
		Cells:             make([]protocol.Cell, 15),
	})

	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 2, PaneID: 20})

	if state.Layout.WindowID != 1 || len(state.Layout.Panes) != 2 {
		t.Fatalf("presented layout changed before replacement = %#v", state.Layout)
	}
	if _, bound := state.RenderSlots[0]; bound || !state.transitioningSlots[0] {
		t.Fatalf("old render slot remained active during transition: slots=%#v transitioning=%#v", state.RenderSlots, state.transitioningSlots)
	}
	accepted, presented := state.ApplyReplaceResult(0, protocol.ReplacePane{
		WindowID:          2,
		PaneID:            20,
		BindingGeneration: 3,
		Generation:        4,
		Cols:              12,
		Rows:              3,
		Cells:             make([]protocol.Cell, 36),
	})
	if !accepted || presented {
		t.Fatalf("replacement before layout = accepted %v presented %v", accepted, presented)
	}
	if !state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 2,
		Panes: []protocol.PanePlacement{
			{PaneID: 20, Rect: protocol.Rect{X: 0, Y: 0, Width: 12, Height: 3}},
		},
	}) {
		t.Fatal("layout did not present its pending replacement")
	}
	if state.Layout.WindowID != 2 || state.RenderSlots[0] != 20 || state.Panes[20].Generation != 4 {
		t.Fatalf("committed window state = layout %#v slots %#v panes %#v", state.Layout, state.RenderSlots, state.Panes)
	}
}

func TestPaneUpdateAppliesRunsStylesAndCursorAtomically(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{Width: 3, Height: 1}},
		},
	})
	if !state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            10,
		BindingGeneration: 2,
		Generation:        7,
		Cols:              3,
		Rows:              1,
		Cells:             repeatedCells("abc"),
	}) {
		t.Fatal("initial replacement failed")
	}
	style := protocol.Style{Bold: true, FG: protocol.Color{Mode: "indexed", Index: 2}}
	if !state.ApplyPaneUpdate(0, protocol.PaneUpdate{
		BindingGeneration:    2,
		BaseGeneration:       7,
		Generation:           8,
		Styles:               []protocol.StyleDefinition{{ID: 1, Style: style}},
		Runs:                 []protocol.CellRun{{Row: 0, Column: 1, Cells: []protocol.Cell{{Rune: 'Z', StyleID: 1, Width: 1}}}},
		CursorChanged:        true,
		Cursor:               protocol.Cursor{X: 2, Y: 0},
		CursorVisibleChanged: true,
		CursorVisible:        false,
	}) {
		t.Fatal("batched pane update failed")
	}
	pane := state.Panes[10]
	if pane.Grid.Cells[1].Rune != 'Z' || pane.Styles[1] != style || pane.Cursor.X != 2 || pane.CursorVisible || pane.Generation != 8 {
		t.Fatalf("pane after batch = %#v", pane)
	}
	if len(state.pendingDamageRects) == 0 || state.pendingDamageRects[len(state.pendingDamageRects)-1] != (protocol.Rect{X: 1, Width: 1, Height: 1}) {
		t.Fatalf("pane update damage = %#v", state.pendingDamageRects)
	}
}

func TestStalePaneUpdateIsDiscardedDuringWindowTransition(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{Width: 3, Height: 1}},
		},
	})
	state.ApplyReplace(0, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            10,
		BindingGeneration: 1,
		Generation:        2,
		Cols:              3,
		Rows:              1,
		Cells:             repeatedCells("old"),
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 2, PaneID: 20})

	accepted, presented := state.ApplyPaneUpdateResult(0, protocol.PaneUpdate{
		BindingGeneration: 1,
		BaseGeneration:    2,
		Generation:        3,
		Runs:              []protocol.CellRun{{Row: 0, Cells: repeatedCells("OLD")}},
	})
	if !accepted || presented {
		t.Fatalf("stale transitional update = accepted %v presented %v", accepted, presented)
	}
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 2,
		Panes: []protocol.PanePlacement{
			{PaneID: 20, Rect: protocol.Rect{Width: 3, Height: 1}},
		},
	})
	accepted, presented = state.ApplyReplaceResult(0, protocol.ReplacePane{
		WindowID:          2,
		PaneID:            20,
		BindingGeneration: 2,
		Generation:        4,
		Cols:              3,
		Rows:              1,
		Cells:             repeatedCells("new"),
	})
	if !accepted || !presented || state.transitioningSlots[0] {
		t.Fatalf("new replacement = accepted %v presented %v transitioning %#v", accepted, presented, state.transitioningSlots)
	}
}

func TestIncrementalUpdatesApplyToNonZeroPaneID(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 11})
	state.ApplyWindowLayout(protocol.WindowLayout{
		WindowID: 1,
		Panes: []protocol.PanePlacement{
			{PaneID: 10, Rect: protocol.Rect{X: 0, Y: 0, Width: 4, Height: 1}},
			{PaneID: 11, Rect: protocol.Rect{X: 5, Y: 0, Width: 4, Height: 1}},
		},
	})
	state.ApplyBind(protocol.BindRenderStream{Slot: 0, PaneID: 10, BindingGeneration: 1})
	state.ApplyBind(protocol.BindRenderStream{Slot: 1, PaneID: 11, BindingGeneration: 2})
	ok := state.ApplyReplace(1, protocol.ReplacePane{
		WindowID:          1,
		PaneID:            11,
		BindingGeneration: 2,
		Generation:        10,
		Cols:              4,
		Rows:              1,
		Cells: []protocol.Cell{
			{Rune: ' ', Width: 1},
			{Rune: ' ', Width: 1},
			{Rune: ' ', Width: 1},
			{Rune: ' ', Width: 1},
		},
		Styles: []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}}},
	})
	if !ok {
		t.Fatal("ApplyReplace() rejected matching pane 11 binding")
	}
	ok = state.ApplySetRun(1, protocol.SetRun{
		BindingGeneration: 2,
		BaseGeneration:    10,
		Generation:        11,
		Row:               0,
		Column:            0,
		Cells: []protocol.Cell{
			{Rune: 'l', Width: 1},
			{Rune: 's', Width: 1},
		},
	})
	if !ok {
		t.Fatal("ApplySetRun() rejected pane 11 incremental update")
	}
	if got := string([]rune{state.Panes[11].Grid.Cells[0].Rune, state.Panes[11].Grid.Cells[1].Rune}); got != "ls" {
		t.Fatalf("pane 11 incremental render = %q, want ls", got)
	}
}
