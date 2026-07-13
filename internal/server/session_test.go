package server

import (
	"testing"

	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

func TestPaneAndSplitLayoutsComputeExpectedRects(t *testing.T) {
	single := (&PaneLayout{PaneID: 1}).Compute(Rect{X: 0, Y: 0, Width: 120, Height: 39})
	if len(single) != 1 || single[0].Rect.Width != 120 || single[0].Rect.Height != 39 {
		t.Fatalf("single pane layout = %#v", single)
	}

	split := (&SplitLayout{
		Direction: SplitVertical,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second:    &PaneLayout{PaneID: 2},
	}).Compute(Rect{X: 0, Y: 0, Width: 120, Height: 39})
	if len(split) != 2 || split[0].Rect.Width != 59 || split[1].Rect.X != 60 || split[1].Rect.Width != 60 {
		t.Fatalf("vertical split layout = %#v", split)
	}

	split = (&SplitLayout{
		Direction: SplitHorizontal,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second:    &PaneLayout{PaneID: 2},
	}).Compute(Rect{X: 0, Y: 0, Width: 120, Height: 39})
	if len(split) != 2 || split[0].Rect.Height != 19 || split[1].Rect.Y != 20 || split[1].Rect.Height != 19 {
		t.Fatalf("horizontal split layout = %#v", split)
	}
}

func TestSessionSplitCreatesNewPaneAndBindings(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	window, clientState := s.CreateWindow(pane0, 0)
	if window.ID != 1 || windowPrimaryPaneID(window) != pane0.ID || clientState.FocusedPaneID != pane0.ID {
		t.Fatalf("initial window = %#v client=%#v", window, clientState)
	}

	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	window, clientState, err := s.SplitFocusedPane(0, pane1, SplitVertical)
	if err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if _, ok := window.Layout.(*SplitLayout); !ok {
		t.Fatalf("window layout = %#v, want split", window.Layout)
	}
	if clientState.FocusedPaneID != pane1.ID || len(clientState.RenderBindings) != 2 {
		t.Fatalf("client after split = %#v", clientState)
	}
	if clientState.RenderBindings[1].PaneID != pane1.ID {
		t.Fatalf("second render slot = %#v", clientState.RenderBindings)
	}
}

func TestRecursiveMixedSplitsAndCloseCollapseOnlyParent(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "root"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "right"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("vertical SplitFocusedPane() error = %v", err)
	}
	pane2 := &Pane{ID: s.AddPaneID(), Title: "bottom-right"}
	window, clientState, err := s.SplitFocusedPane(0, pane2, SplitHorizontal)
	if err != nil {
		t.Fatalf("nested horizontal SplitFocusedPane() error = %v", err)
	}
	placements := window.Layout.Compute(Rect{Width: 120, Height: 39})
	if len(placements) != 3 || len(clientState.RenderBindings) != 3 {
		t.Fatalf("nested layout = %#v bindings=%#v", placements, clientState.RenderBindings)
	}
	if placements[0].PaneID != pane0.ID || placements[0].Rect.Width != 59 ||
		placements[1].PaneID != pane1.ID || placements[1].Rect.Height != 19 ||
		placements[2].PaneID != pane2.ID || placements[2].Rect.Y != 20 {
		t.Fatalf("nested placements = %#v", placements)
	}

	closed, window, clientState, windowClosed, _, _, err := s.CloseFocusedPane(0)
	if err != nil || windowClosed || closed != pane2 {
		t.Fatalf("CloseFocusedPane() closed=%#v windowClosed=%v err=%v", closed, windowClosed, err)
	}
	placements = window.Layout.Compute(Rect{Width: 120, Height: 39})
	if len(placements) != 2 || placements[0].PaneID != pane0.ID || placements[1].PaneID != pane1.ID || clientState.FocusedPaneID != pane1.ID {
		t.Fatalf("collapsed nested layout = %#v client=%#v", placements, clientState)
	}
}

func TestWindowRejectsNinthPane(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 240
	client.TerminalRows = 80
	pane := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(pane, 0)
	for count := 2; count <= int(protocol.MaxVisiblePanes); count++ {
		pane = &Pane{ID: s.AddPaneID()}
		if _, _, err := s.SplitFocusedPane(0, pane, SplitDirection(count%2)); err != nil {
			t.Fatalf("split %d error = %v", count, err)
		}
	}
	if err := s.CanSplitFocusedPane(0); err == nil {
		t.Fatal("CanSplitFocusedPane() allowed ninth pane")
	}
	extra := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, extra, SplitVertical); err == nil {
		t.Fatal("SplitFocusedPane() allowed ninth pane")
	}
}

func TestWindowLayoutAndFocusReuseVisiblePanes(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	layout, err := s.WindowLayout(0)
	if err != nil {
		t.Fatalf("WindowLayout() error = %v", err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Rect.Width != 59 || layout.Panes[1].Rect.Width != 60 {
		t.Fatalf("WindowLayout() = %#v", layout)
	}

	if _, clientState, err := s.FocusPane(0, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	} else if clientState.FocusedPaneID != pane0.ID {
		t.Fatalf("FocusPane() client = %#v", clientState)
	}
}

func TestResolveInputTargetUsesFocusedPaneWithinSplit(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if _, _, err := s.FocusPane(0, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	}

	pane, clientState, exact := s.ResolveInputTarget(0, pane1.ID)
	if pane == nil || clientState == nil || pane.ID != pane0.ID || exact {
		t.Fatalf("ResolveInputTarget() = pane %#v client %#v exact=%v", pane, clientState, exact)
	}
}

func TestCloseFocusedPaneCollapsesSplit(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	closedPane, window, clientState, windowClosed, _, autoCreate, err := s.CloseFocusedPane(0)
	if err != nil {
		t.Fatalf("CloseFocusedPane() error = %v", err)
	}
	if windowClosed || autoCreate || closedPane == nil || clientState.FocusedPaneID != pane0.ID {
		t.Fatalf("CloseFocusedPane() = pane %#v window %#v client %#v windowClosed=%v autoCreate=%v", closedPane, window, clientState, windowClosed, autoCreate)
	}
	if _, ok := window.Layout.(*PaneLayout); !ok {
		t.Fatalf("collapsed layout = %#v, want single pane", window.Layout)
	}
}

func TestRemovePaneCollapsesSplitAndMovesFocus(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	window, client, finalPane, removed, err := s.RemovePane(pane1.ID, 0)
	if err != nil || !removed || finalPane {
		t.Fatalf("RemovePane() removed=%v final=%v err=%v", removed, finalPane, err)
	}
	if client.FocusedPaneID != pane0.ID {
		t.Fatalf("focused pane = %d, want %d", client.FocusedPaneID, pane0.ID)
	}
	if _, ok := window.Layout.(*PaneLayout); !ok {
		t.Fatalf("collapsed layout = %#v, want single pane", window.Layout)
	}
}

func TestRemoveFinalPaneRequestsReplacement(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	pane := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane, 0)

	window, client, finalPane, removed, err := s.RemovePane(pane.ID, 0)
	if err != nil || !removed || !finalPane || window != nil || client == nil {
		t.Fatalf("RemovePane() window=%#v client=%#v removed=%v final=%v err=%v", window, client, removed, finalPane, err)
	}
	if s.HasWindows() {
		t.Fatal("session retained a window after its final pane exited")
	}
}

func TestLayoutRevisionsAreUniqueAcrossWindows(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	first := &Pane{ID: s.AddPaneID(), Title: "one"}
	w1, _ := s.CreateWindow(first, 0)
	second := &Pane{ID: s.AddPaneID(), Title: "two"}
	w2, _ := s.CreateWindow(second, 0)
	if w1.LayoutRevision == 0 || w2.LayoutRevision <= w1.LayoutRevision {
		t.Fatalf("layout revisions first=%d second=%d", w1.LayoutRevision, w2.LayoutRevision)
	}
}

func TestWindowLayoutCarriesRenderSlots(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 80
	client.TerminalRows = 23
	left := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	layout, err := s.WindowLayout(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Slot == layout.Panes[1].Slot {
		t.Fatalf("layout slots=%#v", layout.Panes)
	}
}

func TestCreateWindowSizePrefersClientDimensionsOverSplitPane(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash", Terminal: terminal.New(120, 39)}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs", Terminal: terminal.New(59, 39)}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	ctrl := &controller{state: &sessionState{session: s}}
	cols, rows, err := ctrl.createWindowSize()
	if err != nil {
		t.Fatalf("createWindowSize() error = %v", err)
	}
	if cols != 120 || rows != 39 {
		t.Fatalf("createWindowSize() = %dx%d, want 120x39", cols, rows)
	}
}

func TestWindowDisplayIndicesSurviveDeletionAndNewCreation(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	first, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "one"}, 0)
	second, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "two"}, 0)
	third, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "three"}, 0)
	if first.DisplayIndex != 0 || second.DisplayIndex != 1 || third.DisplayIndex != 2 {
		t.Fatalf("initial display indices = %d, %d, %d", first.DisplayIndex, second.DisplayIndex, third.DisplayIndex)
	}

	if _, _, err := s.SelectWindow(0, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, err := s.CloseWindow(0, first.ID); err != nil {
		t.Fatal(err)
	}
	statuses := s.WindowStatuses(0)
	if len(statuses) != 2 || statuses[0].Index != 1 || statuses[1].Index != 2 {
		t.Fatalf("statuses after deleting display index 0 = %#v", statuses)
	}
	if got, ok := s.WindowIDByIndex(0); ok || got != 0 {
		t.Fatalf("deleted display index lookup = %d, %v", got, ok)
	}
	if got, ok := s.WindowIDByIndex(2); !ok || got != third.ID {
		t.Fatalf("display index 2 lookup = %d, %v; want %d, true", got, ok, third.ID)
	}
	s.ConsumeInputByte(0, 0x02)
	event := s.ConsumeInputByte(0, '2')
	if event.Command != serverCommandSelectIndex || event.Index != 2 {
		t.Fatalf("numeric selection event = %#v", event)
	}

	fourth, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "four"}, 0)
	if fourth.DisplayIndex != 3 {
		t.Fatalf("new window display index = %d, want 3", fourth.DisplayIndex)
	}
	if got, ok := s.WindowIDByIndex(3); !ok || got != fourth.ID {
		t.Fatalf("display index 3 lookup = %d, %v; want %d, true", got, ok, fourth.ID)
	}
	if _, _, err := s.SelectWindow(0, third.ID); err != nil {
		t.Fatal(err)
	}
	if state := s.SnapshotClient(0); state.ActiveWindowID != third.ID {
		t.Fatalf("numeric-selection target = %d, want %d", state.ActiveWindowID, third.ID)
	}
}
