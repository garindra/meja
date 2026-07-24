package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

// commitTestProjection is for passive model tests which assert SessionState
// and ClientInstance.currentView.Layout without starting transport or pane actors.
// Integration tests use ClientInstance.applyViewTransition instead.
func commitTestProjection(client *ClientInstance, transition ViewTransition) error {
	if err := client.commitProjectionPlan(transition.Projection); err != nil {
		return err
	}
	client.currentView = transition.Projection.View
	client.appliedProjectionRevision.Store(transition.Projection.ProjectionRevision)
	return nil
}

func installTestCurrentProjection(client *ClientInstance) error {
	if client == nil || client.Daemon == nil {
		return nil
	}
	state := client.sessionState()
	if state == nil {
		return errSessionUnavailable
	}
	var transition ViewTransition
	client.Daemon.call(func() {
		_, _ = prepareClientWindowGeometryNow(client.identity, state, state.ActiveWindowID)
		transition = client.Daemon.prepareViewTransitionNow(viewTransitionAttach, client.identity, state)
	})
	return commitTestProjection(client, transition)
}

func selectTestWindow(client *ClientInstance, windowID uint64) error {
	transition, err := client.Daemon.selectWindow(client.identity.ID, client.sessionID, windowID)
	if err != nil {
		return err
	}
	return commitTestProjection(client, transition)
}

func resizeTestActiveWindow(client *ClientInstance, cols, rows uint16) (ClientProjectionPlan, error) {
	client.terminalCols.Store(uint32(cols))
	client.terminalRows.Store(uint32(rows))
	transition, err := client.Daemon.resizeClientView(client.identity, cols, rows)
	if err != nil {
		return transition.Projection, err
	}
	for _, resolved := range transition.Projection.View.Panes {
		if resolved.Pane.commands == nil && resolved.Pane.terminal != nil {
			placement := resolved.Placement
			if err := resolved.Pane.resize(uint16(placement.Rect.Width), uint16(placement.Rect.Height)); err != nil {
				return transition.Projection, err
			}
		}
	}
	return transition.Projection, commitTestProjection(client, transition)
}

func setLeasedTestClient(t *testing.T, state *SessionState, client *ClientInstance, generation uint64) {
	t.Helper()
	if state == nil || client == nil {
		t.Fatal("leased test client requires state and client")
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
			ClientID: client.identity.ID, Generation: generation,
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
var testClientByIdentity sync.Map
var testCommandLoopStarted sync.Map

func (c *ClientInstance) setTestTerminalSize(cols, rows uint16) {
	c.terminalCols.Store(uint32(cols))
	c.terminalRows.Store(uint32(rows))
}

func (c *ClientInstance) testLayout() protocol.ClientLayout {
	return c.currentView.Layout
}

func clientForState(state *SessionState) *ClientInstance {
	if state == nil {
		return nil
	}
	if current, ok := testClientByState.Load(state); ok {
		client := current.(*ClientInstance)
		attached := state.attachedClient()
		if attached == nil || attached == client.identity {
			return client
		}
		testClientByState.Delete(state)
	}
	if attachment := state.attachedClient(); attachment != nil {
		if current, ok := testClientByIdentity.Load(attachment); ok {
			client := current.(*ClientInstance)
			testClientByState.Store(state, client)
			return client
		}
	}
	if state.daemon == nil {
		state.daemon = testDaemonForState(state)
	}
	client := newClientInstance(state.daemon, &ClientIdentity{})
	setTestClient(state, client)
	return client
}

func newTestClient(state *SessionState) *ClientInstance {
	return clientForState(state)
}

func startTestClientCommandLoop(client *ClientInstance) {
	if client == nil {
		return
	}
	if _, loaded := testCommandLoopStarted.LoadOrStore(client, struct{}{}); loaded {
		return
	}
	go func() {
		for command := range client.commands {
			client.runClientCommand(command)
		}
	}()
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

func focusTestSessionPane(s *SessionState, paneID uint64) (*Window, protocol.ClientLayout, error) {
	client := clientForState(s)
	window, err := client.focusPane(paneID)
	if err != nil {
		return nil, protocol.ClientLayout{}, err
	}
	return window, client.currentView.Layout, nil
}

func selectTestSessionWindow(s *SessionState, windowID uint64) (*Window, protocol.ClientLayout, error) {
	client := clientForState(s)
	if err := selectTestWindow(client, windowID); err != nil {
		return nil, protocol.ClientLayout{}, err
	}
	return cloneWindow(s.Windows[windowID]), client.currentView.Layout, nil
}

func testActivePane(s *SessionState) (*Pane, protocol.ClientLayout) {
	client := clientForState(s)
	return client.activePane(), client.currentView.Layout
}

func testActiveWindow(s *SessionState) (*Window, protocol.ClientLayout) {
	client := clientForState(s)
	return client.activeWindow(), client.currentView.Layout
}

func resolveTestInputTarget(s *SessionState, paneID uint64) (*Pane, protocol.ClientLayout, bool) {
	client := clientForState(s)
	pane := client.activePane()
	matched := pane != nil && client.currentView.Layout.FocusedPaneID == paneID
	return pane, client.currentView.Layout, matched
}

func testClientLayout(s *SessionState) (protocol.ClientLayout, error) {
	client := clientForState(s)
	layout := client.currentView.Layout
	if layout.LayoutRevision == 0 {
		return protocol.ClientLayout{}, errors.New("test client has no installed layout")
	}
	return layout, nil
}

func testClientLayoutPanes(s *SessionState) ([]protocol.PanePlacement, protocol.ClientLayout) {
	client := clientForState(s)
	return client.currentPanePlacements(), client.currentView.Layout
}

func snapshotTestClient(s *SessionState) *clientInputState {
	state := &clientForState(s).inputState
	snapshot := *state
	snapshot.PrefixEscape = append([]byte(nil), state.PrefixEscape...)
	snapshot.Prompt = clonePromptState(state.Prompt)
	return &snapshot
}

func setTestClientSize(s *SessionState, cols, rows uint16) protocol.ClientLayout {
	client := clientForState(s)
	client.terminalCols.Store(uint32(cols))
	client.terminalRows.Store(uint32(rows))
	if _, err := resizeTestActiveWindow(client, cols, rows); err != nil && s.ActiveWindowID != 0 {
		return protocol.ClientLayout{}
	}
	return client.currentView.Layout
}

func createTestWindow(s *SessionState, pane *Pane) (*Window, protocol.ClientLayout) {
	client := clientForState(s)
	cols, rows := uint16(client.terminalCols.Load()), uint16(client.terminalRows.Load())
	if cols == 0 || rows == 0 {
		paneCols, paneRows := pane.TerminalSize()
		cols, rows = uint16(paneCols), uint16(paneRows)
	}
	window, transition, err := s.daemon.createClientWindow(client.identity, pane, cols, rows)
	if err != nil {
		return nil, protocol.ClientLayout{}
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}
	}
	return window, client.currentView.Layout
}

func resizeTestSessionActiveWindow(s *SessionState, cols, rows uint16) error {
	_, err := resizeTestActiveWindow(clientForState(s), cols, rows)
	return err
}

func toggleTestZoom(s *SessionState) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.toggleClientZoom(client.identity)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentView.Layout, changed, nil
}

func splitTestFocusedPane(s *SessionState, pane *Pane, direction SplitDirection) (*Window, protocol.ClientLayout, error) {
	client := clientForState(s)
	window, transition, err := s.daemon.splitClientPane(client.identity, pane, direction)
	if err != nil {
		return nil, protocol.ClientLayout{}, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, err
	}
	return window, client.currentView.Layout, nil
}

func cycleTestWindowLayout(s *SessionState) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.cycleWindowLayout(client.identity)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentView.Layout, changed, nil
}

func resizeTestFocusedPane(s *SessionState, direction PaneResizeDirection, amount int) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.resizeClientPane(client.identity, direction, amount)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentView.Layout, changed, nil
}

func swapTestFocusedPane(s *SessionState, direction PaneSwapDirection) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.swapClientPane(client.identity, direction)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentView.Layout, changed, nil
}

func closeTestFocusedPane(s *SessionState) (*Pane, *Window, protocol.ClientLayout, bool, uint64, bool, error) {
	client := clientForState(s)
	pane := client.activePane()
	if pane == nil {
		return nil, nil, protocol.ClientLayout{}, false, 0, false, errors.New("client has no active pane")
	}
	result, err := s.daemon.removeClientPane(client.identity, pane.ID)
	if err != nil {
		return nil, nil, protocol.ClientLayout{}, false, 0, false, err
	}
	if !result.FinalPane {
		if err := commitTestProjection(client, result.Transition); err != nil {
			return nil, nil, protocol.ClientLayout{}, false, 0, false, err
		}
	}
	return result.Pane, result.Window, client.currentView.Layout, result.WindowClosed, result.ClosedWindowID, result.FinalPane, nil
}

func removeTestPane(s *SessionState, paneID uint64) (*Window, protocol.ClientLayout, bool, bool, error) {
	client := clientForState(s)
	result, err := s.daemon.removeClientPane(client.identity, paneID)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, false, err
	}
	if result.Removed && !result.FinalPane {
		if err := commitTestProjection(client, result.Transition); err != nil {
			return nil, protocol.ClientLayout{}, false, false, err
		}
	}
	return result.Window, client.currentView.Layout, result.FinalPane, result.Removed, nil
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
	if client != nil && client.identity == nil {
		client.identity = &ClientIdentity{shell: defaultShell()}
	}
	if client != nil && client.identity.ID == 0 {
		if state.daemon.nextClientID == 0 {
			state.daemon.nextClientID = 1
		}
		client.identity.ID = state.daemon.nextClientID
		state.daemon.nextClientID++
	}
	if state.daemon.clients == nil {
		state.daemon.clients = make(map[ClientID]*ClientIdentity)
	}
	if state.daemon.sessions == nil {
		state.daemon.sessions = make(map[uint64]*SessionState)
	}
	if state.daemon.windowLeases == nil {
		state.daemon.windowLeases = make(map[uint64]*WindowViewLease)
	}
	state.daemon.sessions[state.ID] = state
	state.daemon.sessionIndex.Store(state.ID, state)
	state.daemon.ensureSessionGroupInActor(state)
	previous := state.daemon.clients[state.ClientID]
	if client == nil {
		state.ClientID = 0
		if previous != nil {
			previous.State = clientLifecycle{Phase: clientDetached}
		}
		testClientByState.Delete(state)
		if previous != nil {
			testClientByIdentity.Delete(previous)
		}
		return
	}
	if previous != nil && previous != client.identity {
		for _, lease := range state.daemon.windowLeases {
			if lease != nil && lease.ClientID == previous.ID {
				lease.ClientID = client.identity.ID
				lease.Generation++
			}
		}
		previous.State = clientLifecycle{Phase: clientDetached}
	}
	if oldSessionID := client.identity.SessionID; oldSessionID != 0 && oldSessionID != state.ID {
		if old := state.daemon.sessions[oldSessionID]; old != nil && old.ClientID == client.identity.ID {
			old.ClientID = 0
		}
	}
	client.sessionID = state.ID
	client.identity.SessionID = state.ID
	if client.Daemon == nil {
		client.Daemon = state.daemon
	}
	if client.statusCommands == nil {
		client.statusCommands = make(chan statusCommand, 64)
	}
	if client.lifetimeDone == nil {
		client.lifetimeDone = make(chan struct{})
	}
	if client.commands == nil {
		client.commands = make(chan clientInstanceCommand, 64)
	}
	if client.connection == nil {
		client.connection = &clientConnection{}
	}
	client.connection.commands = client.commands
	client.connection.done = client.lifetimeDone
	client.identity.State = clientLifecycle{Phase: clientActive, Active: client.connection}
	state.ClientID = client.identity.ID
	state.daemon.clients[client.identity.ID] = client.identity
	testClientByState.Store(state, client)
	testClientByIdentity.Store(client.identity, client)
	startTestClientCommandLoop(client)
	if len(state.Windows) > 0 {
		_ = installTestCurrentProjection(client)
	}
	client.startStatusOutput()
}

func testDaemonForState(state *SessionState) *Daemon {
	return &Daemon{
		clients:                  make(map[ClientID]*ClientIdentity),
		clientTokens:             make(map[string]ClientID),
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
	if client, ok := testClientByState.Load(state); ok {
		candidate := client.(*ClientInstance)
		if state.attachedClient() == candidate.identity {
			return candidate
		}
	}
	if attachment := state.attachedClient(); attachment != nil {
		if client, ok := testClientByIdentity.Load(attachment); ok {
			return client.(*ClientInstance)
		}
	}
	return nil
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
