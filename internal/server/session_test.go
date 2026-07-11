package server

import "testing"

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
}

func TestSessionSplitCreatesNewPaneAndBindings(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	window, clientState := s.CreateWindow(pane0, 0)
	if windowPrimaryPaneID(window) != pane0.ID || clientState.FocusedPaneID != pane0.ID {
		t.Fatalf("initial window = %#v client=%#v", window, clientState)
	}

	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	window, clientState, err := s.SplitFocusedPane(0, pane1)
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

func TestWindowLayoutAndFocusReuseVisiblePanes(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1); err != nil {
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
	if _, _, err := s.SplitFocusedPane(0, pane1); err != nil {
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
	if _, _, err := s.SplitFocusedPane(0, pane1); err != nil {
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
