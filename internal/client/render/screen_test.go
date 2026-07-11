package render

import (
	"tali/internal/protocol"
	"testing"
)

func TestReplacePaneRoundTripAndSetRun(t *testing.T) {
	state := NewClientState()
	state.ApplyReplace(protocol.ReplacePane{
		SessionID:  0,
		WindowID:   0,
		PaneID:     0,
		Generation: 10,
		Cols:       3,
		Rows:       1,
		Cells: []protocol.Cell{
			{Rune: 'a', Width: 1},
			{Rune: 'b', Width: 1},
			{Rune: 'c', Width: 1},
		},
		Styles: []protocol.StyleDefinition{{ID: 0, Style: protocol.Style{FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}}}},
	})
	ok := state.ApplySetRun(protocol.SetRun{
		BaseGeneration: 10,
		Generation:     11,
		Row:            0,
		Column:         1,
		Cells:          []protocol.Cell{{Rune: 'Z', Width: 1}},
	})
	if !ok || state.Grid.Cells[1].Rune != 'Z' || state.Generation != 11 {
		t.Fatalf("ApplySetRun() failed: %#v", state)
	}
}

func TestGenerationMismatchRejected(t *testing.T) {
	state := NewClientState()
	state.ApplyReplace(protocol.ReplacePane{Generation: 5, Cols: 1, Rows: 1, Cells: []protocol.Cell{{Rune: 'a', Width: 1}}})
	if state.ApplySetRun(protocol.SetRun{BaseGeneration: 4, Generation: 6, Row: 0, Column: 0, Cells: []protocol.Cell{{Rune: 'b', Width: 1}}}) {
		t.Fatal("ApplySetRun() unexpectedly accepted mismatched generation")
	}
}
