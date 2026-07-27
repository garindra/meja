package server

import "testing"

func TestWindowSnapshotDeeplyClonesRecursiveLayout(t *testing.T) {
	d := newCommandTestDaemon(t)
	state := NewSessionState(1)
	state.daemon = d
	window := &Window{
		ID: 10,
		Layout: &SplitLayout{
			Direction: SplitVertical,
			Ratio:     400,
			First:     &PaneLayout{PaneID: 11},
			Second: &SplitLayout{
				Direction: SplitHorizontal,
				Ratio:     600,
				First:     &PaneLayout{PaneID: 12},
				Second:    &PaneLayout{PaneID: 13},
			},
		},
	}
	state.Windows[window.ID] = window
	d.sessions[state.ID] = state
	d.windows[window.ID] = window

	snapshot := cloneWindow(window)
	if snapshot == nil {
		t.Fatal("window snapshot is nil")
	}
	snapshotRoot := snapshot.Layout.(*SplitLayout)
	snapshotNested := snapshotRoot.Second.(*SplitLayout)

	d.call(func() {
		canonicalRoot := window.Layout.(*SplitLayout)
		canonicalRoot.Ratio = 700
		canonicalRoot.Second.(*SplitLayout).First.(*PaneLayout).PaneID = 99
	})
	if snapshotRoot.Ratio != 400 || snapshotNested.First.(*PaneLayout).PaneID != 12 {
		t.Fatalf("old snapshot changed with canonical layout: %#v", snapshot.Layout)
	}

	snapshotRoot.Ratio = 250
	snapshotNested.Second.(*PaneLayout).PaneID = 101
	d.call(func() {
		canonicalRoot := window.Layout.(*SplitLayout)
		if canonicalRoot.Ratio != 700 {
			t.Fatalf("snapshot mutation changed canonical ratio to %d", canonicalRoot.Ratio)
		}
		if got := canonicalRoot.Second.(*SplitLayout).Second.(*PaneLayout).PaneID; got != 13 {
			t.Fatalf("snapshot mutation changed canonical pane to %d", got)
		}
	})
}

func TestClientViewCollectionsDoNotAliasCanonicalOrLaterProjection(t *testing.T) {
	state := NewSessionState(1)
	client := clientForState(state)
	first := &Pane{ID: testAddPaneID(state), terminal: newTerminal(80, 23)}
	createTestWindow(state, first)

	var initial ViewTransition
	state.daemon.call(func() {
		initial = state.daemon.prepareViewTransitionNow(
			viewTransitionAttach,
			testClientIdentity(client),
			state,
		)
	})
	if len(initial.Projection.View.Layout.Panes) != 1 || len(initial.Projection.View.Panes) != 1 ||
		len(initial.Projection.View.NavigationPanes) != 1 {
		t.Fatalf("initial projection = %#v", initial.Projection.View)
	}
	initial.Projection.View.Layout.Panes[0].PaneID = 999
	initial.Projection.View.Panes[0].Placement.PaneID = 998
	initial.Projection.View.NavigationPanes[0].PaneID = 997
	initial.Projection.View.paneByID[first.ID] = nil

	var next ViewTransition
	state.daemon.call(func() {
		next = state.daemon.prepareViewTransitionNow(
			viewTransitionAttach,
			testClientIdentity(client),
			state,
		)
	})
	if got := next.Projection.View.Layout.Panes[0].PaneID; got != first.ID {
		t.Fatalf("snapshot layout mutation changed later projection pane to %d", got)
	}
	if got := next.Projection.View.Panes[0].Placement.PaneID; got != first.ID {
		t.Fatalf("resolved-view mutation changed later projection pane to %d", got)
	}
	if got := next.Projection.View.NavigationPanes[0].PaneID; got != first.ID {
		t.Fatalf("navigation-view mutation changed later projection pane to %d", got)
	}
	if got := next.Projection.View.Pane(first.ID); got != first {
		t.Fatalf("pane index mutation changed later projection pane to %#v", got)
	}
}
