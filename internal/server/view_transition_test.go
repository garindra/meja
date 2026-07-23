package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestDaemonPreparesFinalClientLayoutRevision(t *testing.T) {
	state, client, _, _ := preparedTransitionFixture(t, 12, 4)
	client.identity = &ClientIdentity{lastAllocatedClientLayoutRevision: 17}
	state.daemon.clientInstances[client.identity] = client
	client.currentLayout.LayoutRevision = 17

	var transition PreparedViewTransition
	state.daemon.call(func() {
		transition = state.daemon.prepareViewTransitionNow(viewTransitionLayout, client, state)
	})
	if got := transition.Projection.Layout.LayoutRevision; got != 18 {
		t.Fatalf("prepared client layout revision = %d, want 18", got)
	}
	if got := client.identity.lastAllocatedClientLayoutRevision; got != 18 {
		t.Fatalf("identity client layout revision = %d, want 18", got)
	}

	_, err := client.prepareProjection(transition)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitTestProjection(client, transition); err != nil {
		t.Fatal(err)
	}
	if got := client.currentLayout.LayoutRevision; got != 18 {
		t.Fatalf("applied client layout revision = %d, want prepared revision 18", got)
	}
}

func preparedTransitionFixture(t *testing.T, cols, rows int) (*SessionState, *ClientInstance, *Pane, ClientProjectionPlan) {
	t.Helper()
	state := NewSessionState(0)
	pane, updates := startTestPaneRenderer(testAddPaneID(state), cols, rows)
	t.Cleanup(func() { close(updates) })
	createTestWindow(state, pane)
	client := testClientInstance(make(chan protocol.Frame, 4), map[int]*OutputLease{0: testOutputLease(0, &bytes.Buffer{})})
	attachDisplayTestClient(t, state, client)
	var plan ClientProjectionPlan
	state.daemon.call(func() { plan = state.daemon.prepareViewTransitionNow(viewTransitionLayout, client, state).Projection })
	return state, client, pane, plan
}

func TestPreparedProjectionOwnsDaemonPlanSlices(t *testing.T) {
	_, client, _, plan := preparedTransitionFixture(t, 12, 4)
	prepared, err := client.prepareProjection(PreparedViewTransition{Reason: viewTransitionLayout, Projection: plan})
	if err != nil {
		t.Fatal(err)
	}
	plan.Layout.Panes[0].Rect.Width = 1
	plan.Layout.Panes[0].Slot = 7
	if got := prepared.Plan.Layout.Panes[0].Rect.Width; got != 12 {
		t.Fatalf("prepared plan width changed through source slice: got %d, want 12", got)
	}
	if got := prepared.Panes[0].Placement.Slot; got != 0 {
		t.Fatalf("prepared pane slot changed through source slice: got %d, want 0", got)
	}
}

func TestPrepareProjectionRejectsPaneGridLayoutMismatch(t *testing.T) {
	_, client, pane, plan := preparedTransitionFixture(t, 12, 4)
	plan.Layout.Panes[0].Rect.Width = 11
	_, err := client.prepareProjection(PreparedViewTransition{Reason: viewTransitionResize, Projection: plan})
	if err == nil || !strings.Contains(err.Error(), "grid 12x4 does not match layout 11x4") {
		t.Fatalf("prepare error = %v, want pane grid/layout mismatch", err)
	}
	if got := client.appliedProjectionRevision.Load(); got >= plan.ProjectionRevision {
		t.Fatalf("invalid projection revision %d was installed; client revision=%d", plan.ProjectionRevision, got)
	}
	_ = pane
}

func TestPreparedProjectionBindsResolvedPaneWithoutGraphReread(t *testing.T) {
	state, client, pane, plan := preparedTransitionFixture(t, 12, 4)
	wire := &bytes.Buffer{}
	client.Output[0] = testOutputLease(0, wire)
	prepared, err := client.prepareProjection(PreparedViewTransition{Reason: viewTransitionSelectWindow, Projection: plan})
	if err != nil {
		t.Fatal(err)
	}

	// Once prepared, application must use the resolved pane carried in the
	// transition. Removing the concurrent lookup entry simulates graph progress
	// after the daemon transaction without invalidating the immutable result.
	state.daemon.paneIndex.Delete(pane.ID)
	defer state.daemon.paneIndex.Store(pane.ID, pane)
	if err := client.installPreparedProjection(prepared, client.beginOutputHandoffWithRemovedPanes(nil)); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	commands := decodePendingCommands(t, wire.Bytes())
	if len(commands) == 0 || commands[0].Opcode != protocol.DisplayOpcodeStartRender {
		t.Fatalf("prepared pane emitted %#v, want START_RENDER", commandOpcodes(commands))
	}
	frame := <-client.controlOut
	layout, err := protocol.DecodeClientLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 1 || layout.Panes[0].PaneID != pane.ID || layout.Panes[0].Rect.Width != 12 {
		t.Fatalf("published prepared layout = %#v", layout)
	}
}

func TestRejectedTargetTransitionDoesNotReleaseInstalledOutput(t *testing.T) {
	state := NewSessionState(0)
	pane, updates := startTestPaneRenderer(testAddPaneID(state), 12, 4)
	defer close(updates)
	createTestWindow(state, pane)
	wire := &bytes.Buffer{}
	client := testClientInstance(make(chan protocol.Frame, 4), map[int]*OutputLease{0: testOutputLease(0, wire)})
	attachDisplayTestClient(t, state, client)
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	wire.Reset()

	_, err := client.Daemon.selectWindow(client.AttachmentID, client.sessionID, 999)
	if err == nil || !strings.Contains(err.Error(), "unknown window 999") {
		t.Fatalf("transition error = %v, want unknown target", err)
	}
	updates <- []byte("x")
	syncPaneRenderer(t, pane)
	if wire.Len() == 0 {
		t.Fatal("rejected target transition released the installed pane output")
	}
}

func TestDaemonPreparesResizeAndClientAppliesPhysicalGrid(t *testing.T) {
	state := NewSessionState(0)
	pane, updates := startTestPaneRenderer(testAddPaneID(state), 20, 6)
	defer close(updates)
	createTestWindow(state, pane)
	wire := &bytes.Buffer{}
	client := testClientInstance(make(chan protocol.Frame, 8), map[int]*OutputLease{0: testOutputLease(0, wire)})
	attachDisplayTestClient(t, state, client)
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)

	client.terminalCols.Store(80)
	client.terminalRows.Store(20)
	transition, err := state.daemon.resizeClientView(client, 80, 20)
	if err != nil {
		t.Fatal(err)
	}
	if cols, rows := pane.TerminalSize(); cols != 20 || rows != 6 {
		t.Fatalf("daemon preparation changed physical pane grid to %dx%d", cols, rows)
	}
	if len(transition.PaneResizes) != 1 || transition.PaneResizes[0].Cols != 80 || transition.PaneResizes[0].Rows != 20 {
		t.Fatalf("prepared pane resizes = %#v, want one 80x20 resize", transition.PaneResizes)
	}
	if err := client.applyViewTransition(transition); err != nil {
		t.Fatal(err)
	}
	if cols, rows := pane.TerminalSize(); cols != 80 || rows != 20 {
		t.Fatalf("applied physical pane grid = %dx%d, want 80x20", cols, rows)
	}
}
