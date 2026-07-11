package render

import (
	"tali/internal/protocol"
	"testing"
)

func TestReplacePaneRoundTripAndSetRun(t *testing.T) {
	state := NewClientState()
	state.ApplyBind(protocol.BindRenderStream{SessionID: 0, WindowID: 0, PaneID: 0, BindingGeneration: 3})
	ok := state.ApplyReplace(protocol.ReplacePane{
		SessionID:         0,
		WindowID:          0,
		PaneID:            0,
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
	ok = state.ApplySetRun(protocol.SetRun{
		BindingGeneration: 3,
		BaseGeneration:    10,
		Generation:        11,
		Row:               0,
		Column:            1,
		Cells:             []protocol.Cell{{Rune: 'Z', Width: 1}},
	})
	if !ok || state.Grid.Cells[1].Rune != 'Z' || state.Generation != 11 {
		t.Fatalf("ApplySetRun() failed: %#v", state)
	}
}

func TestGenerationMismatchRejected(t *testing.T) {
	state := NewClientState()
	state.ApplyBind(protocol.BindRenderStream{BindingGeneration: 1})
	state.ApplyReplace(protocol.ReplacePane{BindingGeneration: 1, Generation: 5, Cols: 1, Rows: 1, Cells: []protocol.Cell{{Rune: 'a', Width: 1}}})
	if state.ApplySetRun(protocol.SetRun{BindingGeneration: 1, BaseGeneration: 4, Generation: 6, Row: 0, Column: 0, Cells: []protocol.Cell{{Rune: 'b', Width: 1}}}) {
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

func TestLastActiveWindowTracking(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 10, PaneID: 10, Index: 0, Title: "bash"},
			{WindowID: 20, PaneID: 20, Index: 1, Title: "logs"},
			{WindowID: 30, PaneID: 30, Index: 2, Title: "vim"},
		},
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 10, PaneID: 10})
	if _, ok := state.LastActiveWindowID(); ok {
		t.Fatal("LastActiveWindowID() unexpectedly reported a previous window after initial selection")
	}

	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 20, PaneID: 20})
	if got, ok := state.LastActiveWindowID(); !ok || got != 10 {
		t.Fatalf("LastActiveWindowID() after switch = %d, %v; want 10, true", got, ok)
	}

	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 30, PaneID: 30})
	if got, ok := state.LastActiveWindowID(); !ok || got != 20 {
		t.Fatalf("LastActiveWindowID() after second switch = %d, %v; want 20, true", got, ok)
	}

	state.ApplyWindowClosed(20)
	if _, ok := state.LastActiveWindowID(); ok {
		t.Fatal("LastActiveWindowID() unexpectedly kept a closed window")
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
	if !state.Windows[1].Active || state.Windows[0].Active {
		t.Fatalf("window actives not normalized after explicit selection: %#v", state.Windows)
	}
}

func TestRenderBindingDoesNotOwnTabSelection(t *testing.T) {
	state := NewClientState()
	state.ApplyWindowList(protocol.WindowList{
		Windows: []protocol.WindowInfo{
			{WindowID: 0, PaneID: 10, Index: 0, Title: "bash", Active: true},
			{WindowID: 1, PaneID: 11, Index: 1, Title: "logs"},
		},
		ActiveWindowID: 0,
	})
	state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 0, PaneID: 10})
	state.ApplyBind(protocol.BindRenderStream{
		SessionID:         7,
		WindowID:          1,
		PaneID:            11,
		BindingGeneration: 2,
	})

	ok := state.ApplyReplace(protocol.ReplacePane{
		SessionID:         7,
		WindowID:          1,
		PaneID:            11,
		BindingGeneration: 2,
		Generation:        1,
		Cols:              1,
		Rows:              1,
		Cells:             []protocol.Cell{{Rune: 'x', Width: 1}},
		Styles: []protocol.StyleDefinition{
			{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}},
		},
	})
	if !ok {
		t.Fatal("ApplyReplace() rejected matching binding")
	}
	if state.ActiveWindowID != 0 {
		t.Fatalf("render snapshot changed active tab selection: window=%d", state.ActiveWindowID)
	}
	if state.FocusedPaneID != 11 {
		t.Fatalf("render binding did not update focused pane: pane=%d", state.FocusedPaneID)
	}
}
