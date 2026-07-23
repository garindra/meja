package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

// projectTestClientState is for passive model tests which assert SessionState
// and ClientState without starting transport or pane actors. Integration tests
// use ClientInstance.applyViewTransition instead.
func projectTestClientState(client *ClientInstance, transition PreparedViewTransition) error {
	return client.commitProjectionPlan(transition.Projection)
}

func installTestCurrentProjection(client *ClientInstance) error {
	if client == nil || client.Daemon == nil {
		return nil
	}
	state := client.sessionState()
	if state == nil {
		return errSessionUnavailable
	}
	var transition PreparedViewTransition
	client.Daemon.call(func() {
		_, _ = prepareClientWindowGeometryNow(client, state, state.ActiveWindowID)
		transition = client.Daemon.prepareViewTransitionNow(viewTransitionAttach, client, state, true)
	})
	return projectTestClientState(client, transition)
}

func selectTestWindow(client *ClientInstance, windowID uint64) error {
	transition, err := client.Daemon.selectWindow(client.AttachmentID, client.sessionID, windowID)
	if err != nil {
		return err
	}
	return projectTestClientState(client, transition)
}

func resizeTestActiveWindow(client *ClientInstance, cols, rows uint16) (ClientProjectionPlan, error) {
	transition, err := client.Daemon.resizeClientView(client, cols, rows)
	if err != nil {
		return transition.Projection, err
	}
	for _, resize := range transition.PaneResizes {
		if resize.Pane.commands == nil && resize.Pane.terminal != nil {
			if err := resize.Pane.resize(uint16(resize.Rect.Width), uint16(resize.Rect.Height)); err != nil {
				return transition.Projection, err
			}
		}
	}
	return transition.Projection, projectTestClientState(client, transition)
}

func setLeasedTestClient(t *testing.T, state *SessionState, client *ClientInstance, generation uint64) {
	t.Helper()
	if state == nil || client == nil {
		t.Fatal("leased test client requires state and client")
	}
	if client.AttachmentID == 0 {
		client.AttachmentID = 1
	}
	setTestClient(state, client)
	var windowID uint64
	state.daemon.call(func() {
		windowID = state.ActiveWindowID
		if windowID == 0 {
			ids := state.orderedWindowIDs()
			if len(ids) > 0 {
				windowID = ids[0]
			}
		}
		if windowID == 0 {
			return
		}
		if state.daemon.windowLeases == nil {
			state.daemon.windowLeases = make(map[uint64]*WindowViewLease)
		}
		state.daemon.windowLeases[windowID] = &WindowViewLease{
			WindowID: windowID, SessionID: state.ID,
			AttachmentID: client.AttachmentID, Generation: generation,
		}
	})
	if windowID == 0 {
		t.Fatal("leased test client requires an active window")
	}
	client.ViewLeaseWindowID = windowID
	client.ViewLeaseGeneration = generation
	if err := installTestCurrentProjection(client); err != nil {
		t.Fatalf("install initial leased projection: %v", err)
	}
}

var testClientByState sync.Map

func clientForState(state *SessionState) *ClientInstance {
	if state == nil {
		return nil
	}
	if current, ok := testClientByState.Load(state); ok {
		return current.(*ClientInstance)
	}
	if current := state.attachedClient(); current != nil {
		testClientByState.Store(state, current)
		return current
	}
	client := &ClientInstance{
		sessionID:      state.ID,
		Daemon:         state.daemon,
		shell:          defaultShell(),
		lifetimeDone:   make(chan struct{}),
		statusCommands: make(chan statusCommand, 64),
		events:         make(chan func() error, 64),
		layouts:        make(map[uint64]protocol.WindowLayout),
		heldKeys:       make(map[frontendHeldKey]uint64),
	}
	if state.daemon == nil {
		state.daemon = testDaemonForState(state)
	}
	client.Daemon = state.daemon
	client.clientState = &ClientState{}
	// Production registration publishes the singleton group into the daemon's
	// authoritative indexes before any panes or windows are inserted. Tests
	// must preserve that shape or daemon-posted lifecycle events silently stop
	// at an index lookup that direct SessionState adapters bypass.
	state.daemon.ensureSessionGroupInActor(state)
	if state.daemon.clients == nil {
		state.daemon.clients = make(map[uint64]*ClientInstance)
	}
	if state.daemon.sessions == nil {
		state.daemon.sessions = make(map[uint64]*SessionState)
	}
	state.daemon.ensureSessionGroupInActor(state)
	state.daemon.sessions[state.ID] = state
	state.daemon.sessionIndex.Store(state.ID, state)
	if client.statusCommands == nil {
		client.statusCommands = make(chan statusCommand, 64)
	}
	if client.lifetimeDone == nil {
		client.lifetimeDone = make(chan struct{})
	}
	if client.events == nil {
		client.events = make(chan func() error, 64)
	}
	state.daemon.clients[state.ID] = client
	state.daemon.clientIndex.Store(state.ID, client)
	// A newly registered production client is not projected until its first
	// window exists. Keep the fixture on that same boundary instead of asking
	// the plan builder to tolerate an impossible empty view.
	if len(state.Windows) > 0 {
		if err := installTestCurrentProjection(client); err != nil {
			panic(err)
		}
	}
	client.startStatusOutput()
	testClientByState.Store(state, client)
	return client
}

func newStandaloneClient(state *SessionState) *ClientState {
	client := clientForState(state)
	if client == nil {
		return nil
	}
	return client.ensureClientState()
}

func executeTestClientCommand(client *ClientInstance, argv []string) (bool, error) {
	return client.executeAttachedCommand(argv)
}

// testAddPaneID keeps fixture identity allocation on the daemon, matching
// production ownership.
func testAddPaneID(s *SessionState) uint64 {
	if s == nil {
		panic("nil test session")
	}
	if s.daemon == nil {
		clientForState(s)
	}
	id, err := s.daemon.allocatePaneID()
	if err != nil {
		panic(err)
	}
	return id
}

func syncTestProjection(t *testing.T, state *SessionState) {
	t.Helper()
	client := clientForState(state)
	if client == nil {
		t.Fatal("test projection requires a client instance")
	}
	if err := installTestCurrentProjection(client); err != nil {
		t.Fatalf("install test projection: %v", err)
	}
}

func focusTestSessionPane(s *SessionState, paneID uint64) (*Window, *ClientState, error) {
	client := clientForState(s)
	window, plan, err := s.daemon.focusClientPane(client, paneID)
	if err != nil {
		return nil, nil, err
	}
	if err := client.commitProjectionPlan(plan); err != nil {
		return nil, nil, err
	}
	return window, cloneClientState(client.clientState), nil
}

func selectTestSessionWindow(s *SessionState, windowID uint64) (*Window, *ClientState, error) {
	client := clientForState(s)
	if err := selectTestWindow(client, windowID); err != nil {
		return nil, nil, err
	}
	return cloneWindow(s.Windows[windowID]), cloneClientState(client.clientState), nil
}

func testActivePane(s *SessionState) (*Pane, *ClientState) {
	client := clientForState(s)
	return client.activePane(), client.snapshotClient()
}

func testActiveWindow(s *SessionState) (*Window, *ClientState) {
	client := clientForState(s)
	return client.activeWindow(), client.snapshotClient()
}

func resolveTestInputTarget(s *SessionState, paneID uint64) (*Pane, *ClientState, bool) {
	client := clientForState(s)
	pane := client.activePane()
	matched := pane != nil && client.clientState.FocusedPaneID == paneID
	return pane, client.snapshotClient(), matched
}

func testWindowLayout(s *SessionState) (protocol.WindowLayout, error) {
	return clientForState(s).windowLayout()
}

func testRenderBindings(s *SessionState) ([]RenderBinding, *ClientState) {
	client := clientForState(s)
	return client.renderBindings(), client.snapshotClient()
}

func snapshotTestClient(s *SessionState) *ClientState {
	return clientForState(s).snapshotClient()
}

func setTestClientSize(s *SessionState, cols, rows uint16) *ClientState {
	client := clientForState(s)
	client.terminalCols.Store(uint32(cols))
	client.terminalRows.Store(uint32(rows))
	if _, err := resizeTestActiveWindow(client, cols, rows); err != nil && s.ActiveWindowID != 0 {
		return nil
	}
	client.clientState.TerminalCols = cols
	client.clientState.TerminalRows = rows
	return client.snapshotClient()
}

func createTestWindow(s *SessionState, pane *Pane) (*Window, *ClientState) {
	client := clientForState(s)
	cols, rows := client.clientState.TerminalCols, client.clientState.TerminalRows
	if cols == 0 || rows == 0 {
		paneCols, paneRows := pane.TerminalSize()
		cols, rows = uint16(paneCols), uint16(paneRows)
	}
	window, transition, err := s.daemon.createClientWindow(client, pane, cols, rows)
	if err != nil {
		return nil, nil
	}
	if err := projectTestClientState(client, transition); err != nil {
		return nil, nil
	}
	return window, client.snapshotClient()
}

func resizeTestSessionActiveWindow(s *SessionState, cols, rows uint16) error {
	_, err := resizeTestActiveWindow(clientForState(s), cols, rows)
	return err
}

func toggleTestZoom(s *SessionState) (*Window, *ClientState, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.toggleClientZoom(client)
	if err != nil {
		return nil, nil, false, err
	}
	if err := projectTestClientState(client, transition); err != nil {
		return nil, nil, false, err
	}
	return window, cloneClientState(client.clientState), changed, nil
}

func splitTestFocusedPane(s *SessionState, pane *Pane, direction SplitDirection) (*Window, *ClientState, error) {
	client := clientForState(s)
	window, transition, err := s.daemon.splitClientPane(client, pane, direction)
	if err != nil {
		return nil, nil, err
	}
	if err := projectTestClientState(client, transition); err != nil {
		return nil, nil, err
	}
	return window, cloneClientState(client.clientState), nil
}

func cycleTestWindowLayout(s *SessionState) (*Window, *ClientState, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.cycleClientLayout(client)
	if err != nil {
		return nil, nil, false, err
	}
	if err := projectTestClientState(client, transition); err != nil {
		return nil, nil, false, err
	}
	return window, cloneClientState(client.clientState), changed, nil
}

func resizeTestFocusedPane(s *SessionState, direction PaneResizeDirection, amount int) (*Window, *ClientState, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.resizeClientPane(client, direction, amount)
	if err != nil {
		return nil, nil, false, err
	}
	if err := projectTestClientState(client, transition); err != nil {
		return nil, nil, false, err
	}
	return window, cloneClientState(client.clientState), changed, nil
}

func swapTestFocusedPane(s *SessionState, direction PaneSwapDirection) (*Window, *ClientState, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.swapClientPane(client, direction)
	if err != nil {
		return nil, nil, false, err
	}
	if err := projectTestClientState(client, transition); err != nil {
		return nil, nil, false, err
	}
	return window, cloneClientState(client.clientState), changed, nil
}

func closeTestFocusedPane(s *SessionState) (*Pane, *Window, *ClientState, bool, uint64, bool, error) {
	client := clientForState(s)
	pane := client.activePane()
	if pane == nil {
		return nil, nil, nil, false, 0, false, errors.New("client has no active pane")
	}
	result, err := s.daemon.removeClientPane(client, pane.ID)
	if err != nil {
		return nil, nil, nil, false, 0, false, err
	}
	if !result.FinalPane {
		if err := projectTestClientState(client, result.Transition); err != nil {
			return nil, nil, nil, false, 0, false, err
		}
	}
	return result.Pane, result.Window, cloneClientState(client.clientState), result.WindowClosed, result.ClosedWindowID, result.FinalPane, nil
}

func removeTestPane(s *SessionState, paneID uint64) (*Window, *ClientState, bool, bool, error) {
	client := clientForState(s)
	result, err := s.daemon.removeClientPane(client, paneID)
	if err != nil {
		return nil, nil, false, false, err
	}
	if result.Removed && !result.FinalPane {
		if err := projectTestClientState(client, result.Transition); err != nil {
			return nil, nil, false, false, err
		}
	}
	return result.Window, cloneClientState(client.clientState), result.FinalPane, result.Removed, nil
}

func setTestClient(state *SessionState, client *ClientInstance) {
	if state == nil {
		return
	}
	if state.daemon == nil {
		if client != nil && client.Daemon != nil {
			state.daemon = client.Daemon
		} else {
			state.daemon = testDaemonForState(state)
		}
	}
	if state.daemon.clients == nil {
		state.daemon.clients = make(map[uint64]*ClientInstance)
	}
	if state.daemon.sessions == nil {
		state.daemon.sessions = make(map[uint64]*SessionState)
	}
	state.daemon.sessions[state.ID] = state
	state.daemon.sessionIndex.Store(state.ID, state)
	if client == nil {
		delete(state.daemon.clients, state.ID)
		state.daemon.clientIndex.Delete(state.ID)
		testClientByState.Delete(state)
		return
	}
	client.sessionID = state.ID
	if client.clientState == nil {
		client.clientState = &ClientState{}
	}
	client.terminalCols.Store(uint32(client.clientState.TerminalCols))
	client.terminalRows.Store(uint32(client.clientState.TerminalRows))
	if client.Daemon == nil {
		client.Daemon = state.daemon
	}
	if client.statusCommands == nil {
		client.statusCommands = make(chan statusCommand, 64)
	}
	if client.lifetimeDone == nil {
		client.lifetimeDone = make(chan struct{})
	}
	if client.events == nil {
		client.events = make(chan func() error, 64)
	}
	state.daemon.clients[state.ID] = client
	state.daemon.clientIndex.Store(state.ID, client)
	testClientByState.Store(state, client)
	_ = installTestCurrentProjection(client)
	client.startStatusOutput()
}

func testDaemonForState(state *SessionState) *Daemon {
	return &Daemon{
		clients:                  make(map[uint64]*ClientInstance),
		sessions:                 make(map[uint64]*SessionState),
		panes:                    make(map[uint64]*Pane),
		names:                    make(map[string]*SessionState),
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
		nextWindowID:             1,
	}
}

func testClientOf(state *SessionState) *ClientInstance {
	if state == nil || state.daemon == nil {
		return nil
	}
	if client, ok := state.daemon.clientIndex.Load(state.ID); ok {
		return client.(*ClientInstance)
	}
	return state.daemon.clients[state.ID]
}

func testDaemonSession(d *Daemon, id uint64) *SessionState {
	var state *SessionState
	d.call(func() { state = d.sessions[id] })
	return state
}

func flushTestSessionPersistence(ctx context.Context, state *SessionState, directory string) (string, error) {
	if state.persistenceRecord() == nil {
		return "", nil
	}
	clone := cloneSessionPersistence(*state.persistenceRecord())
	update := persistenceSnapshot{persistence: &clone}
	for name := range state.obsoletePersistenceSet() {
		if update.persistence.Name != name {
			update.obsoleteNames = append(update.obsoleteNames, name)
		}
	}
	return flushPersistenceSnapshot(ctx, directory, update)
}

func newFrontendTestClient(state *SessionState) *ClientInstance {
	client := newClientInstance(nil, nil)
	setTestClient(state, client)
	return client
}

func runStateOperation(state *SessionState, run func() error) error {
	if state != nil && state.daemon == nil {
		state.daemon = testDaemonForState(state)
	}
	if state != nil && state.daemon != nil && state.daemon.requests != nil {
		var err error
		state.daemon.call(func() { err = run() })
		return err
	}
	return run()
}

func stopState(state *SessionState) {
	if state != nil && state.daemon != nil && state.daemon.persistenceStarted.Load() {
		state.daemon.stopPersistence()
		<-state.daemon.persistenceDone
	}
}
