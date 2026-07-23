package server

import "testing"

func TestPresetLayoutsComputeExpectedShapes(t *testing.T) {
	paneIDs := []uint64{1, 2, 3, 4, 5}
	rect := Rect{Width: 120, Height: 40}

	tests := []struct {
		name  string
		build func() LayoutNode
		check func(*testing.T, map[uint64]Rect)
	}{
		{
			name: "even horizontal",
			build: func() LayoutNode {
				return buildPresetLayout(paneIDs, 1, layoutPresetEvenHorizontal)
			},
			check: func(t *testing.T, placements map[uint64]Rect) {
				for paneID, placement := range placements {
					if placement.Y != 0 || placement.Height != rect.Height {
						t.Errorf("pane %d placement = %#v, want a full-height horizontal row", paneID, placement)
					}
				}
			},
		},
		{
			name: "even vertical",
			build: func() LayoutNode {
				return buildPresetLayout(paneIDs, 1, layoutPresetEvenVertical)
			},
			check: func(t *testing.T, placements map[uint64]Rect) {
				for paneID, placement := range placements {
					if placement.X != 0 || placement.Width != rect.Width {
						t.Errorf("pane %d placement = %#v, want a full-width vertical column", paneID, placement)
					}
				}
			},
		},
		{
			name: "main horizontal",
			build: func() LayoutNode {
				return buildPresetLayout(paneIDs, 3, layoutPresetMainHorizontal)
			},
			check: func(t *testing.T, placements map[uint64]Rect) {
				if got := placements[3]; got.X != 0 || got.Y != 0 || got.Width != rect.Width {
					t.Errorf("main pane placement = %#v, want full-width top pane", got)
				}
				for paneID, placement := range placements {
					if paneID != 3 && placement.Y <= placements[3].Y {
						t.Errorf("secondary pane %d placement = %#v, want below main pane", paneID, placement)
					}
				}
			},
		},
		{
			name: "main vertical",
			build: func() LayoutNode {
				return buildPresetLayout(paneIDs, 3, layoutPresetMainVertical)
			},
			check: func(t *testing.T, placements map[uint64]Rect) {
				if got := placements[3]; got.X != 0 || got.Y != 0 || got.Height != rect.Height {
					t.Errorf("main pane placement = %#v, want full-height left pane", got)
				}
				for paneID, placement := range placements {
					if paneID != 3 && placement.X <= placements[3].X {
						t.Errorf("secondary pane %d placement = %#v, want right of main pane", paneID, placement)
					}
				}
			},
		},
		{
			name: "tiled",
			build: func() LayoutNode {
				return buildPresetLayout(paneIDs, 1, layoutPresetTiled)
			},
			check: func(t *testing.T, placements map[uint64]Rect) {
				rows := map[int]bool{}
				columns := map[int]bool{}
				for paneID, placement := range placements {
					if placement.Width <= 0 || placement.Height <= 0 {
						t.Errorf("pane %d has invalid placement = %#v", paneID, placement)
					}
					rows[placement.Y] = true
					columns[placement.X] = true
				}
				if len(rows) < 2 || len(columns) < 2 {
					t.Errorf("tiled placements = %#v, want multiple rows and columns", placements)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			placements := test.build().Compute(rect)
			byPane := make(map[uint64]Rect, len(placements))
			for _, placement := range placements {
				byPane[placement.PaneID] = placement.Rect
			}
			if len(byPane) != len(paneIDs) {
				t.Fatalf("placements = %#v, want one placement for every pane", placements)
			}
			test.check(t, byPane)
		})
	}
}

func TestCycleWindowLayoutAdvancesAndUsesFocusedPaneAsMain(t *testing.T) {
	s := NewSessionState(1)
	t.Cleanup(func() { stopState(s) })
	client := newStandaloneClient(s)
	client.TerminalCols, client.TerminalRows = 120, 40
	first := &Pane{ID: testAddPaneID(s), terminal: newTerminal(120, 40)}
	window, _ := createTestWindow(s, first)
	second := &Pane{ID: testAddPaneID(s), terminal: newTerminal(120, 40)}
	if _, _, err := splitTestFocusedPane(s, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	client.FocusedPaneID = second.ID

	for want := 0; want < layoutPresetCount; want++ {
		if _, _, changed, err := cycleTestWindowLayout(s); err != nil || !changed {
			t.Fatalf("cycle %d changed=%v err=%v", want, changed, err)
		}
		if got := s.Windows[window.ID].layoutCycleIndex; got != want {
			t.Fatalf("cycle index = %d, want %d", got, want)
		}
		if want == layoutPresetMainVertical {
			placements := s.Windows[window.ID].Layout.Compute(Rect{Width: 120, Height: 40})
			if len(placements) != 2 || placements[0].PaneID != second.ID || placements[0].Rect.X != 0 || placements[0].Rect.Width != 71 {
				t.Fatalf("main vertical placements = %#v, want focused pane first and wide", placements)
			}
		}
	}
}
