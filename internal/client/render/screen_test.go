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
	state.ApplyReplace(0, protocol.ReplacePane{PaneID: 10, BindingGeneration: 1, Generation: 5, Cols: 1, Rows: 1, Cells: []protocol.Cell{{Rune: 'a', Width: 1}}})
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

func TestLastActiveWindowTrackingIncludesWindowZeroToggle(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 0, PaneID: 10, Index: 0, Title: "bash"},
			{WindowID: 1, PaneID: 11, Index: 1, Title: "logs"},
		},
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 0, PaneID: 10})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 11})
	if got, ok := state.LastActiveWindowID(); !ok || got != 0 {
		t.Fatalf("LastActiveWindowID() after 0->1 = %d, %v; want 0, true", got, ok)
	}
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 0, PaneID: 10})
	if got, ok := state.LastActiveWindowID(); !ok || got != 1 {
		t.Fatalf("LastActiveWindowID() after 1->0 = %d, %v; want 1, true", got, ok)
	}
}

func TestWindowListDoesNotOverrideExplicitSelection(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 0, PaneID: 10, Index: 0, Title: "bash", Active: true},
			{WindowID: 1, PaneID: 11, Index: 1, Title: "logs"},
		},
		ActiveWindowID: 0,
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 11})
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 0, PaneID: 10, Index: 0, Title: "bash", Active: true},
			{WindowID: 1, PaneID: 11, Index: 1, Title: "logs"},
		},
		ActiveWindowID: 0,
	})
	if state.ActiveWindowID != 1 || state.FocusedPaneID != 11 {
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

func TestWindowSelectionResetsStaleSplitLayout(t *testing.T) {
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

	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 2, PaneID: 20})

	if state.Layout.WindowID != 2 || len(state.Layout.Panes) != 1 {
		t.Fatalf("reset layout = %#v", state.Layout)
	}
	if got := state.Layout.Panes[0].Rect; got.Width != 12 || got.Height != 3 || got.X != 0 || got.Y != 0 {
		t.Fatalf("provisional pane rect = %#v", got)
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
