package client

import (
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
	"github.com/garindra/meja/internal/theme"
)

func TestVerticalPaneBorderOwnershipSplitsTopAndBottom(t *testing.T) {
	placements := []protocol.PanePlacement{
		{PaneID: 0, Rect: protocol.Rect{Width: 4, Height: 4}},
		{PaneID: 1, Rect: protocol.Rect{X: 5, Width: 4, Height: 4}},
	}
	for row := 0; row < 4; row++ {
		_, got := focusedPaneBorderCells(placements, 0)[borderPoint{column: 4, row: row}]
		if want := row < 2; got != want {
			t.Fatalf("left pane owns border row %d = %v, want %v", row, got, want)
		}
		_, got = focusedPaneBorderCells(placements, 1)[borderPoint{column: 4, row: row}]
		if want := row >= 2; got != want {
			t.Fatalf("right pane owns border row %d = %v, want %v", row, got, want)
		}
	}
}

func TestHorizontalPaneBorderOwnershipSplitsLeftAndRight(t *testing.T) {
	placements := []protocol.PanePlacement{
		{PaneID: 0, Rect: protocol.Rect{Width: 4, Height: 2}},
		{PaneID: 1, Rect: protocol.Rect{Y: 3, Width: 4, Height: 2}},
	}
	for column := 0; column < 4; column++ {
		_, got := focusedPaneBorderCells(placements, 0)[borderPoint{column: column, row: 2}]
		if want := column < 2; got != want {
			t.Fatalf("top pane owns border column %d = %v, want %v", column, got, want)
		}
		_, got = focusedPaneBorderCells(placements, 1)[borderPoint{column: column, row: 2}]
		if want := column >= 2; got != want {
			t.Fatalf("bottom pane owns border column %d = %v, want %v", column, got, want)
		}
	}
}

func TestCornerPaneOwnsPartsOfBothAdjacentBorders(t *testing.T) {
	placements := []protocol.PanePlacement{
		{PaneID: 0, Rect: protocol.Rect{Width: 4, Height: 5}},
		{PaneID: 1, Rect: protocol.Rect{X: 5, Width: 4, Height: 2}},
		{PaneID: 2, Rect: protocol.Rect{X: 5, Y: 3, Width: 4, Height: 2}},
	}
	border := focusedPaneBorderCells(placements, 1)
	for row := 0; row < 2; row++ {
		if _, ok := border[borderPoint{column: 4, row: row}]; !ok {
			t.Fatalf("top-right pane did not own vertical border row %d", row)
		}
	}
	for column := 5; column < 9; column++ {
		if _, ok := border[borderPoint{column: column, row: 2}]; !ok {
			t.Fatalf("top-right pane did not own horizontal border column %d", column)
		}
	}
	if _, ok := border[borderPoint{column: 4, row: 2}]; !ok {
		t.Fatal("top-right pane did not own the adjoining wire junction")
	}
	if got := paneBorderRune(placements, 4, 2); got != '├' {
		t.Fatalf("corner split junction = %q, want %q", got, '├')
	}
}

func TestFourPaneWireIntersectionPhysicallyMeets(t *testing.T) {
	placements := []protocol.PanePlacement{
		{PaneID: 0, Rect: protocol.Rect{Width: 2, Height: 2}},
		{PaneID: 1, Rect: protocol.Rect{X: 3, Width: 2, Height: 2}},
		{PaneID: 2, Rect: protocol.Rect{Y: 3, Width: 2, Height: 2}},
		{PaneID: 3, Rect: protocol.Rect{X: 3, Y: 3, Width: 2, Height: 2}},
	}
	if got := paneBorderRune(placements, 2, 2); got != '┼' {
		t.Fatalf("four-pane intersection = %q, want %q", got, '┼')
	}
}

func TestSameRevisionFocusChangeRecolorsPaneBorders(t *testing.T) {
	s := newScanoutState(true)
	s.cols, s.rows = 9, 5
	s.layout = protocol.ClientLayout{
		WindowID: 1, LayoutRevision: 1, FocusedPaneID: 0,
		Panes: []protocol.PanePlacement{
			{PaneID: 0, Slot: 0, Rect: protocol.Rect{Width: 4, Height: 4}},
			{PaneID: 1, Slot: 1, Rect: protocol.Rect{X: 5, Width: 4, Height: 4}},
		},
	}
	next := s.layout
	next.FocusedPaneID = 1
	if _, err := s.acceptLayout(next); err != nil {
		t.Fatal(err)
	}
	accent := sgrForStyle(protocol.Style{FG: theme.AccentColor(), BG: protocol.Color{Mode: "default"}})
	if output := string(s.takeANSI()); !strings.Contains(output, accent) {
		t.Fatalf("focus change did not emit accent-colored border: %q", output)
	}
}
