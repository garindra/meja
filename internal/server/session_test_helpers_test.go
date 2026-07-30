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
	transition.delivery.cancel()
	if err := client.commitProjectionPlan(transition.Projection); err != nil {
		return err
	}
	client.currentView = transition.Projection.View
	client.appliedProjectionRevision = transition.Projection.ProjectionRevision
	return nil
}

func installTestCurrentProjection(client *ClientInstance) error {
	if client == nil || client.Daemon == nil {
		return nil
	}
	state := testClientSession(client)
	if state == nil {
		return errSessionUnavailable
	}
	var transition ViewTransition
	client.Daemon.call(func() {
		identity := testClientIdentity(client)
		_, _ = prepareClientWindowGeometryNow(identity, state, state.ActiveWindowID)
		transition = client.Daemon.prepareViewTransitionNow(viewTransitionAttach, identity, state)
	})
	return commitTestProjection(client, transition)
}

func selectTestWindow(client *ClientInstance, windowID uint64) error {
	transition, err := client.Daemon.selectWindow(client.clientID, client.sessionID, windowID)
	if err != nil {
		return err
	}
	return commitTestProjection(client, transition)
}

func resizeTestActiveWindow(client *ClientInstance, cols, rows uint16) (ClientProjectionPlan, error) {
	client.terminalCols = cols
	client.terminalRows = rows
	transition, err := client.Daemon.resizeClientView(client.clientID, client.connection, cols, rows)
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
			ClientID: client.clientID, Generation: generation,
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
var testIdentityByClient sync.Map

func newClientInstance(d *Daemon, identity *ClientIdentity, connections ...*clientConnection) *ClientInstance {
	var connection *clientConnection
	if len(connections) > 0 {
		connection = connections[0]
	}
	admission := ClientAdmission{connection: connection}
	if identity != nil {
		admission.ClientID = identity.ID
		admission.SessionID = identity.SessionID
		admission.ResumeToken = identity.ResumeToken
		admission.Shell = identity.shell
		admission.Cols = identity.terminalCols
		admission.Rows = identity.terminalRows
	}
	client := newClientInstanceFromAdmission(d, admission)
	if identity != nil {
		testIdentityByClient.Store(client, identity)
	}
	return client
}

func testClientIdentity(client *ClientInstance) *ClientIdentity {
	if client == nil || client.Daemon == nil {
		if identity, ok := testIdentityByClient.Load(client); ok {
			return identity.(*ClientIdentity)
		}
		return nil
	}
	if identity := client.Daemon.clients[client.clientID]; identity != nil {
		return identity
	}
	if identity, ok := testIdentityByClient.Load(client); ok {
		return identity.(*ClientIdentity)
	}
	return nil
}

func (c *ClientInstance) setTestTerminalSize(cols, rows uint16) {
	c.terminalCols = cols
	c.terminalRows = rows
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
		if attached == nil || attached.ID == client.clientID {
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

func snapshotTestClientActor(client *ClientInstance) clientInstanceSnapshot {
	if client == nil {
		return clientInstanceSnapshot{}
	}
	result := make(chan clientInstanceSnapshot, 1)
	if client.commands == nil {
		command := clientInstanceCommand{Snapshot: result}
		client.runClientCommand(command)
		return <-result
	}
	client.commands <- clientInstanceCommand{Snapshot: result}
	return <-result
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

func syncTestStatus(t *testing.T, state *SessionState) {
	t.Helper()
	client := clientForState(state)
	if client == nil {
		t.Fatal("test status refresh requires a client instance")
	}
	var status clientStatusState
	state.daemon.call(func() {
		status = state.daemon.clientStatusSnapshotNow(testClientIdentity(clientForState(state)), state)
	})
	result := make(chan error, 1)
	client.postCommand(clientInstanceCommand{
		RefreshStatus: true,
		Status:        status,
		HasStatus:     true,
		Done:          result,
	})
	if err := <-result; err != nil {
		t.Fatalf("refresh test status: %v", err)
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
	return s.Panes[client.currentView.Layout.FocusedPaneID], client.currentView.Layout
}

func testActiveWindow(s *SessionState) (*Window, protocol.ClientLayout) {
	client := clientForState(s)
	return cloneWindow(s.Windows[client.currentView.Layout.WindowID]), client.currentView.Layout
}

func resolveTestInputTarget(s *SessionState, paneID uint64) (*Pane, protocol.ClientLayout, bool) {
	client := clientForState(s)
	pane := s.Panes[client.currentView.Layout.FocusedPaneID]
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
	return client.currentView.Placements(), client.currentView.Layout
}

func snapshotTestClient(s *SessionState) *clientInputState {
	state := &clientForState(s).inputState
	snapshot := *state
	snapshot.PrefixEscape = append([]byte(nil), state.PrefixEscape...)
	return &snapshot
}

func resolveTestPrompt(client *ClientInstance, submitted bool, text string) (bool, error) {
	prompt := client.ActivePrompt()
	if prompt == nil {
		return false, errors.New("test client has no active prompt")
	}
	return client.resolvePrompt(protocol.FrontendPromptResult{
		PromptID: prompt.ID, Submitted: submitted, Text: text,
	})
}

func setTestClientSize(s *SessionState, cols, rows uint16) protocol.ClientLayout {
	client := clientForState(s)
	client.terminalCols = cols
	client.terminalRows = rows
	if _, err := resizeTestActiveWindow(client, cols, rows); err != nil && s.ActiveWindowID != 0 {
		return protocol.ClientLayout{}
	}
	return client.currentView.Layout
}

func createTestWindow(s *SessionState, pane *Pane) (*Window, protocol.ClientLayout) {
	client := clientForState(s)
	cols, rows := client.terminalCols, client.terminalRows
	if cols == 0 || rows == 0 {
		paneCols, paneRows := pane.TerminalSize()
		cols, rows = uint16(paneCols), uint16(paneRows)
	}
	window, transition, err := s.daemon.createClientWindow(client.clientID, pane, cols, rows)
	if err != nil {
		return nil, protocol.ClientLayout{}
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}
	}
	var canonical *Window
	s.daemon.call(func() {
		if state := s.daemon.sessions[s.ID]; state != nil {
			canonical = state.Windows[window.ID]
		}
	})
	return canonical, client.currentView.Layout
}

func resizeTestSessionActiveWindow(s *SessionState, cols, rows uint16) error {
	_, err := resizeTestActiveWindow(clientForState(s), cols, rows)
	return err
}

func toggleTestZoom(s *SessionState) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.toggleClientZoom(client.clientID)
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
	window, transition, err := s.daemon.splitClientPane(client.clientID, pane, direction)
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
	window, transition, changed, err := s.daemon.cycleWindowLayout(client.clientID)
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
	window, transition, changed, err := s.daemon.resizeClientPane(client.clientID, direction, amount)
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
	window, transition, changed, err := s.daemon.swapClientPane(client.clientID, direction)
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
	result, err := s.daemon.removeClientPane(client.clientID, pane.ID)
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
	result, err := s.daemon.removeClientPane(client.clientID, paneID)
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
	var identity *ClientIdentity
	if client != nil {
		identity = testClientIdentity(client)
	}
	if client != nil && identity == nil {
		identity = &ClientIdentity{shell: defaultShell()}
		testIdentityByClient.Store(client, identity)
	}
	if client != nil && identity.ID == 0 {
		if state.daemon.nextClientID == 0 {
			state.daemon.nextClientID = 1
		}
		identity.ID = state.daemon.nextClientID
		client.clientID = identity.ID
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
	if previous != nil && previous != identity {
		for _, lease := range state.daemon.windowLeases {
			if lease != nil && lease.ClientID == previous.ID {
				lease.ClientID = identity.ID
				lease.Generation++
			}
		}
		previous.State = clientLifecycle{Phase: clientDetached}
	}
	if oldSessionID := identity.SessionID; oldSessionID != 0 && oldSessionID != state.ID {
		if old := state.daemon.sessions[oldSessionID]; old != nil && old.ClientID == identity.ID {
			old.ClientID = 0
		}
	}
	client.sessionID = state.ID
	identity.SessionID = state.ID
	if client.Daemon == nil {
		client.Daemon = state.daemon
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
	if !client.connection.deliveryConfigured {
		client.connection.commands = client.commands
		client.connection.done = client.lifetimeDone
		client.connection.startDeliveryWorker()
	}
	identity.State = clientLifecycle{Phase: clientActive, Active: client.connection}
	state.ClientID = identity.ID
	state.daemon.clients[identity.ID] = identity
	testClientByState.Store(state, client)
	testClientByIdentity.Store(identity, client)
	startTestClientCommandLoop(client)
	if len(state.Windows) > 0 {
		_ = installTestCurrentProjection(client)
	}
}

func testDaemonForState(state *SessionState) *Daemon {
	return &Daemon{
		clients:                  make(map[ClientID]*ClientIdentity),
		clientTokens:             make(map[string]ClientID),
		sessions:                 make(map[uint64]*SessionState),
		panes:                    make(map[uint64]*Pane),
		windows:                  make(map[uint64]*Window),
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
		persistenceStarted:       make(chan struct{}),
		persistenceUpdates:       make(chan persistenceSnapshot, 1),
		nextWindowID:             1,
	}
}

func testClientSession(client *ClientInstance) *SessionState {
	if client == nil {
		return nil
	}
	return testDaemonSession(client.Daemon, client.sessionID)
}

func testClientOf(state *SessionState) *ClientInstance {
	if state == nil || state.daemon == nil {
		return nil
	}
	if client, ok := testClientByState.Load(state); ok {
		candidate := client.(*ClientInstance)
		if attached := state.attachedClient(); attached != nil && attached.ID == candidate.clientID {
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

func testOperationSession(d *Daemon, result commandOperationResult) *SessionState {
	return testDaemonSession(d, result.sessionID)
}

func groupTestSessions(d *Daemon, base, mirror *SessionState) error {
	if d == nil || base == nil || mirror == nil {
		return errors.New("grouping requires two sessions")
	}
	var err error
	d.call(func() { err = d.groupSessionInActor(base, mirror) })
	return err
}

func validateTestWindowView(d *Daemon, clientID ClientID, windowID, generation uint64) error {
	var err error
	d.call(func() {
		lease := d.windowLeases[windowID]
		if lease == nil || lease.ClientID != clientID || lease.Generation != generation {
			err = errors.New("stale or invalid window view lease")
		}
	})
	return err
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

func captureTestSession(state *SessionState, ctx context.Context, observer ProcessObserver) (SessionCapture, error) {
	if state == nil || state.daemon == nil {
		return SessionCapture{}, errSessionUnavailable
	}
	state.daemon.call(func() {
		if state.daemon.sessions[state.ID] == nil {
			state.daemon.sessions[state.ID] = state
			state.daemon.ensureSessionGroupInActor(state)
		}
	})
	return state.daemon.captureSessionID(state.ID, ctx, observer)
}

func stopState(state *SessionState) {
	if state != nil && state.daemon != nil && state.daemon.persistenceHasStarted() {
		state.daemon.stopPersistence()
		<-state.daemon.persistenceDone
	}
}
