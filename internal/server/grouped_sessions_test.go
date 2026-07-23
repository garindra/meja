package server

import (
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func groupedTestDaemon() *Daemon {
	return &Daemon{
		nextID:                   1,
		nextPaneID:               1,
		nextWindowID:             1,
		nextGroupID:              1,
		sessions:                 make(map[uint64]*SessionState),
		names:                    make(map[string]*SessionState),
		groups:                   make(map[uint64]*GroupState),
		panes:                    make(map[uint64]*Pane),
		windowLeases:             make(map[uint64]*WindowViewLease),
		processObserver:          NewProcessObserver(),
		processObservations:      make(map[uint64]ProcessObservation),
		processSaveCandidates:    make(map[uint64]processSaveCandidate),
		sessionPersistions:       make(map[uint64]*SessionPersistence),
		persistenceGroups:        make(map[uint64]*GroupState),
		obsoletePersistenceNames: make(map[uint64]map[string]struct{}),
		persistenceNow:           make(chan struct{}, 1),
		persistenceStop:          make(chan struct{}),
		persistenceDone:          make(chan struct{}),
		persistenceUpdates:       make(chan persistenceSnapshot, 1),
	}
}

func groupedTestSession(d *Daemon, id uint64, name string) *SessionState {
	s := newSession(id, name)
	s.daemon = d
	d.sessions[id] = s
	d.names[name] = s
	d.ensureSessionGroupInActor(s)
	if d.nextID <= id {
		d.nextID = id + 1
	}
	return s
}

func TestGroupedSessionsShareExecutionGraphAndLinks(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	newStandaloneClient(base)
	setTestClientSize(base, 80, 23)
	pane := &Pane{ID: 1, Title: "shell", terminal: newTerminal(80, 23)}
	createTestWindow(base, pane)

	mirror := groupedTestSession(d, 2, "mirror")
	if err := d.groupSession(base, mirror); err != nil {
		t.Fatal(err)
	}
	if base.GroupID != mirror.GroupID || len(base.group.SessionIDs) != 2 {
		t.Fatalf("group membership base=%d mirror=%d members=%v", base.GroupID, mirror.GroupID, base.group.SessionIDs)
	}
	if base.Windows[1] != mirror.Windows[1] || base.Panes[1] != mirror.Panes[1] {
		t.Fatal("mirror did not receive the canonical window and pane identities")
	}
	if len(mirror.Links) != 1 || mirror.Links[0].WindowID != 1 {
		t.Fatalf("mirror links = %#v", mirror.Links)
	}
}

func TestGroupedSessionCreationCLIDoesNotCreatePane(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	defer d.disconnectActiveClients()
	baseResult := d.executeCommand(protocol.CommandRequest{
		Args:             []string{"new-session", "-d", "-s", "base", "--", "/bin/sleep", "30"},
		WorkingDirectory: t.TempDir(), TerminalCols: 80, TerminalRows: 23,
	})
	if baseResult.exitCode != 0 {
		t.Fatalf("base creation failed: %#v", baseResult)
	}
	base := d.sessionByName("base")
	basePane, _ := testActivePane(base)
	result := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new-session", "-d", "-P", "-F", "#{session_id}:#{pane_id}", "-t", "base", "-s", "mirror"},
	})
	if result.exitCode != 0 || string(result.stdout) != "2:1\n" {
		t.Fatalf("mirror creation result = %#v", result)
	}
	mirror := d.sessionByName("mirror")
	mirrorPane, _ := testActivePane(mirror)
	if mirrorPane != basePane || len(mirror.Panes) != 1 || len(mirror.Windows) != 1 {
		t.Fatalf("mirror graph pane=%p base=%p panes=%d windows=%d", mirrorPane, basePane, len(mirror.Panes), len(mirror.Windows))
	}
}

func TestGroupedSessionsCanViewDifferentWindowsButLeaseConflictsAreAtomic(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	setTestClientSize(base, 80, 23)
	createTestWindow(base, &Pane{ID: 1, terminal: newTerminal(80, 23)})
	createTestWindow(base, &Pane{ID: 2, terminal: newTerminal(80, 23)})
	mirror := groupedTestSession(d, 2, "mirror")
	if err := d.groupSession(base, mirror); err != nil {
		t.Fatal(err)
	}
	first := &WindowViewLease{WindowID: 1, SessionID: base.ID, AttachmentID: 10, Generation: 1}
	second := &WindowViewLease{WindowID: 2, SessionID: mirror.ID, AttachmentID: 11, Generation: 1}
	d.windowLeases[1] = first
	d.windowLeases[2] = second
	if err := d.validateWindowView(10, first.WindowID, first.Generation); err != nil {
		t.Fatal(err)
	}
	if err := d.validateWindowView(11, 1, first.Generation); err == nil {
		t.Fatal("stale generation was accepted")
	}
	base.ActiveWindowID = first.WindowID
	setTestClient(base, &ClientInstance{
		Daemon:              d,
		AttachmentID:        10,
		ViewLeaseWindowID:   first.WindowID,
		ViewLeaseGeneration: first.Generation,
	})
	mirror.ActiveWindowID = second.WindowID
	setTestClient(mirror, &ClientInstance{
		Daemon:              d,
		AttachmentID:        11,
		ViewLeaseWindowID:   second.WindowID,
		ViewLeaseGeneration: second.Generation,
	})
	if _, err := d.selectWindow(11, mirror.ID, first.WindowID); err == nil {
		t.Fatal("expected a conflicting window lease")
	}
	if mirror.ActiveWindowID != second.WindowID || testClientOf(mirror).ViewLeaseWindowID != second.WindowID {
		t.Fatalf("failed mirror selection changed active=%d lease=%d", mirror.ActiveWindowID, testClientOf(mirror).ViewLeaseWindowID)
	}
	baseClient := testClientOf(base).clientState
	oldActive := baseClient.ActiveWindowID
	if _, _, err := selectTestSessionWindow(base, 2); err == nil {
		t.Fatal("selection of an occupied window unexpectedly succeeded")
	}
	if baseClient.ActiveWindowID != oldActive || testClientOf(base).ViewLeaseWindowID != first.WindowID {
		t.Fatalf("failed selection changed view active=%d lease=%d", baseClient.ActiveWindowID, testClientOf(base).ViewLeaseWindowID)
	}
}

func TestGroupedWindowLeaseConflictReportsDisplayIndexForEveryCommandOrigin(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	setTestClientSize(base, 80, 23)
	first, _ := createTestWindow(base, &Pane{ID: 1, terminal: newTerminal(80, 23)})
	second, _ := createTestWindow(base, &Pane{ID: 2, terminal: newTerminal(80, 23)})
	mirror := groupedTestSession(d, 2, "mirror")
	if err := d.groupSession(base, mirror); err != nil {
		t.Fatal(err)
	}

	baseClient := &ClientInstance{Daemon: d, AttachmentID: 10, sessionID: base.ID}
	mirrorClient := &ClientInstance{Daemon: d, AttachmentID: 11, sessionID: mirror.ID}
	setTestClient(base, baseClient)
	setTestClient(mirror, mirrorClient)
	base.ActiveWindowID = second.ID
	mirror.ActiveWindowID = first.ID
	d.windowLeases[second.ID] = &WindowViewLease{
		WindowID: second.ID, SessionID: base.ID, AttachmentID: baseClient.AttachmentID, Generation: 1,
	}
	d.windowLeases[first.ID] = &WindowViewLease{
		WindowID: first.ID, SessionID: mirror.ID, AttachmentID: mirrorClient.AttachmentID, Generation: 1,
	}

	if second.ID == uint64(second.DisplayIndex) {
		t.Fatalf("fixture requires distinct internal ID and display index, both were %d", second.ID)
	}
	want := `window 1 is currently viewed by session "base"`

	t.Run("attached status prompt", func(t *testing.T) {
		_, err := d.commandEngine().run(mirrorClient.commandContext(), []string{"select-window", "-t", ":1"})
		if err == nil || err.Error() != want {
			t.Fatalf("attached conflict = %v, want %q", err, want)
		}
	})

	t.Run("external CLI", func(t *testing.T) {
		result := d.executeCommand(protocol.CommandRequest{Args: []string{"select-window", "-t", "mirror:1"}})
		if result.exitCode != 1 || strings.TrimSpace(string(result.stderr)) != want {
			t.Fatalf("external conflict = %#v, want stderr %q", result, want)
		}
	})
}

func TestGroupedSessionViewsKeepFocusIndependent(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	setTestClientSize(base, 80, 23)
	createTestWindow(base, &Pane{ID: 1, terminal: newTerminal(80, 23)})
	splitTestFocusedPane(base, &Pane{ID: 2, terminal: newTerminal(80, 23)}, SplitVertical)
	mirror := groupedTestSession(d, 2, "mirror")
	if err := d.groupSession(base, mirror); err != nil {
		t.Fatal(err)
	}
	focusTestSessionPane(base, 2)
	focusTestSessionPane(mirror, 1)
	if base.WindowViews[1].FocusedPaneID != 2 || mirror.WindowViews[1].FocusedPaneID != 1 {
		t.Fatalf("independent views base=%#v mirror=%#v", base.WindowViews[1], mirror.WindowViews[1])
	}
}

func TestGroupedNewWindowLinksEverySessionButActivatesOnlyInvoker(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	setTestClientSize(base, 80, 23)
	createTestWindow(base, &Pane{ID: 1, terminal: newTerminal(80, 23)})
	mirror := groupedTestSession(d, 2, "mirror")
	if err := d.groupSession(base, mirror); err != nil {
		t.Fatal(err)
	}
	oldMirrorWindow := mirror.ActiveWindowID
	createTestWindow(base, &Pane{ID: 2, terminal: newTerminal(80, 23)})
	if len(base.Links) != 2 || len(mirror.Links) != 2 || base.Windows[2] != mirror.Windows[2] {
		t.Fatalf("synchronized links base=%#v mirror=%#v", base.Links, mirror.Links)
	}
	if base.ActiveWindowID == oldMirrorWindow || mirror.ActiveWindowID != oldMirrorWindow {
		t.Fatalf("active windows base=%d mirror=%d oldMirror=%d", base.ActiveWindowID, mirror.ActiveWindowID, oldMirrorWindow)
	}
}

func TestGroupedResizeChangesOnlyViewedWindow(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	newStandaloneClient(base)
	setTestClientSize(base, 80, 23)
	createTestWindow(base, &Pane{ID: 1, terminal: newTerminal(80, 23)})
	createTestWindow(base, &Pane{ID: 2, terminal: newTerminal(80, 23)})
	first := base.Windows[1]
	second := base.Windows[2]
	firstRevision, secondRevision := first.LayoutRevision, second.LayoutRevision
	if _, _, err := selectTestSessionWindow(base, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := resizeTestSessionActiveWindow(base, 120, 30); err != nil {
		t.Fatal(err)
	}
	if first.Cols != 120 || first.Rows != 30 || first.LayoutRevision == firstRevision {
		t.Fatalf("active window dimensions/revision = %dx%d/%d", first.Cols, first.Rows, first.LayoutRevision)
	}
	if second.Cols == 120 || second.Rows == 30 || second.LayoutRevision != secondRevision {
		t.Fatalf("unviewed window changed dimensions/revision = %dx%d/%d", second.Cols, second.Rows, second.LayoutRevision)
	}
}

func TestKillingOneGroupedSessionPreservesExecutionGraph(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	setTestClientSize(base, 80, 23)
	createTestWindow(base, &Pane{ID: 1, terminal: newTerminal(80, 23)})
	mirror := groupedTestSession(d, 2, "mirror")
	if err := d.groupSession(base, mirror); err != nil {
		t.Fatal(err)
	}
	if err := d.shutdownSession(mirror); err != nil {
		t.Fatal(err)
	}
	if d.sessionByName("mirror") != nil || d.sessionByName("base") == nil {
		t.Fatal("killing mirror changed the wrong session registry entries")
	}
	if base.Panes[1] == nil || base.Windows[1] == nil || len(base.group.SessionIDs) != 1 {
		t.Fatal("shared graph did not survive removal of one session")
	}
}

func TestProjectionPlanCarriesBindingsAndRejectsStaleRevision(t *testing.T) {
	d := groupedTestDaemon()
	session := groupedTestSession(d, 1, "view")
	setTestClientSize(session, 80, 23)
	createTestWindow(session, &Pane{ID: 1, terminal: newTerminal(80, 23)})
	client := &ClientInstance{Daemon: d, sessionID: session.ID, AttachmentID: 7, clientState: &ClientState{}}
	setTestClient(session, client)
	d.windowLeases[1] = &WindowViewLease{WindowID: 1, SessionID: session.ID, AttachmentID: client.AttachmentID, Generation: 4}
	client.ViewLeaseWindowID = 1
	client.ViewLeaseGeneration = 4
	session.ActiveWindowID = 1
	var transition PreparedViewTransition
	d.call(func() { transition = d.prepareViewTransitionNow(viewTransitionAttach, client, session, true) })
	plan := transition.Projection
	if plan.SessionID != session.ID || plan.WindowID != 1 || len(plan.Bindings) != 1 || plan.ViewLeaseGeneration != 4 {
		t.Fatalf("projection plan = %#v", plan)
	}
	if err := client.commitProjectionPlan(plan); err != nil {
		t.Fatal(err)
	}
	if len(client.clientState.RenderBindings) != 1 || client.clientState.RenderBindings[0].PaneID != 1 {
		t.Fatalf("client bindings = %#v", client.clientState.RenderBindings)
	}
	plan.ProjectionRevision--
	if err := client.commitProjectionPlan(plan); err == nil {
		t.Fatal("stale projection revision was accepted")
	}
}

func TestFocusOnlyProjectionPreservesRenderedLayoutRevision(t *testing.T) {
	client := &ClientInstance{clientState: &ClientState{}}
	client.highestLayoutRevision.Store(7)
	client.projectionRevision.Store(10)

	plan := ClientProjectionPlan{
		ProjectionRevision: 11,
		LayoutRevision:     99,
		FocusedPaneID:      2,
		FullSnapshot:       false,
	}
	if err := client.commitProjectionPlan(plan); err != nil {
		t.Fatal(err)
	}
	if got := client.highestLayoutRevision.Load(); got != 7 {
		t.Fatalf("focus-only projection changed rendered revision to %d, want 7", got)
	}
	if got := client.clientState.FocusedPaneID; got != 2 {
		t.Fatalf("focus-only projection did not install pane focus: got %d", got)
	}
}
