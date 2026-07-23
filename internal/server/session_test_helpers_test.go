package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

// commitTestProjection is for passive model tests which assert SessionState
// and ClientInstance.currentLayout without starting transport or pane actors.
// Integration tests use ClientInstance.applyViewTransition instead.
func commitTestProjection(client *ClientInstance, transition PreparedViewTransition) error {
	if err := client.commitProjectionPlan(transition.Projection); err != nil {
		return err
	}
	client.currentLayout = transition.Projection.Layout
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
	var transition PreparedViewTransition
	client.Daemon.call(func() {
		_, _ = prepareClientWindowGeometryNow(client, state, state.ActiveWindowID)
		transition = client.Daemon.prepareViewTransitionNow(viewTransitionAttach, client, state)
	})
	return commitTestProjection(client, transition)
}

func selectTestWindow(client *ClientInstance, windowID uint64) error {
	transition, err := client.Daemon.selectWindow(client.AttachmentID, client.sessionID, windowID)
	if err != nil {
		return err
	}
	return commitTestProjection(client, transition)
}

func resizeTestActiveWindow(client *ClientInstance, cols, rows uint16) (ClientProjectionPlan, error) {
	client.terminalCols.Store(uint32(cols))
	client.terminalRows.Store(uint32(rows))
	transition, err := client.Daemon.resizeClientView(client, cols, rows)
	if err != nil {
		return transition.Projection, err
	}
	for _, resize := range transition.PaneResizes {
		if resize.Pane.commands == nil && resize.Pane.terminal != nil {
			if err := resize.Pane.resize(resize.Cols, resize.Rows); err != nil {
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

func (c *ClientInstance) setTestTerminalSize(cols, rows uint16) {
	c.terminalCols.Store(uint32(cols))
	c.terminalRows.Store(uint32(rows))
}

func (c *ClientInstance) testLayout() protocol.ClientLayout {
	return c.currentLayout
}

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
	return window, client.currentLayout, nil
}

func selectTestSessionWindow(s *SessionState, windowID uint64) (*Window, protocol.ClientLayout, error) {
	client := clientForState(s)
	if err := selectTestWindow(client, windowID); err != nil {
		return nil, protocol.ClientLayout{}, err
	}
	return cloneWindow(s.Windows[windowID]), client.currentLayout, nil
}

func testActivePane(s *SessionState) (*Pane, protocol.ClientLayout) {
	client := clientForState(s)
	return client.activePane(), client.currentLayout
}

func testActiveWindow(s *SessionState) (*Window, protocol.ClientLayout) {
	client := clientForState(s)
	return client.activeWindow(), client.currentLayout
}

func resolveTestInputTarget(s *SessionState, paneID uint64) (*Pane, protocol.ClientLayout, bool) {
	client := clientForState(s)
	pane := client.activePane()
	matched := pane != nil && client.currentLayout.FocusedPaneID == paneID
	return pane, client.currentLayout, matched
}

func testClientLayout(s *SessionState) (protocol.ClientLayout, error) {
	client := clientForState(s)
	layout := client.currentLayout
	if layout.LayoutRevision == 0 {
		return protocol.ClientLayout{}, errors.New("test client has no installed layout")
	}
	return layout, nil
}

func testClientLayoutPanes(s *SessionState) ([]protocol.PanePlacement, protocol.ClientLayout) {
	client := clientForState(s)
	return client.currentPanePlacements(), client.currentLayout
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
	return client.currentLayout
}

func createTestWindow(s *SessionState, pane *Pane) (*Window, protocol.ClientLayout) {
	client := clientForState(s)
	cols, rows := uint16(client.terminalCols.Load()), uint16(client.terminalRows.Load())
	if cols == 0 || rows == 0 {
		paneCols, paneRows := pane.TerminalSize()
		cols, rows = uint16(paneCols), uint16(paneRows)
	}
	window, transition, err := s.daemon.createClientWindow(client, pane, cols, rows)
	if err != nil {
		return nil, protocol.ClientLayout{}
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}
	}
	return window, client.currentLayout
}

func resizeTestSessionActiveWindow(s *SessionState, cols, rows uint16) error {
	_, err := resizeTestActiveWindow(clientForState(s), cols, rows)
	return err
}

func toggleTestZoom(s *SessionState) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.toggleClientZoom(client)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentLayout, changed, nil
}

func splitTestFocusedPane(s *SessionState, pane *Pane, direction SplitDirection) (*Window, protocol.ClientLayout, error) {
	client := clientForState(s)
	window, transition, err := s.daemon.splitClientPane(client, pane, direction)
	if err != nil {
		return nil, protocol.ClientLayout{}, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, err
	}
	return window, client.currentLayout, nil
}

func cycleTestWindowLayout(s *SessionState) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.cycleWindowLayout(client)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentLayout, changed, nil
}

func resizeTestFocusedPane(s *SessionState, direction PaneResizeDirection, amount int) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.resizeClientPane(client, direction, amount)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentLayout, changed, nil
}

func swapTestFocusedPane(s *SessionState, direction PaneSwapDirection) (*Window, protocol.ClientLayout, bool, error) {
	client := clientForState(s)
	window, transition, changed, err := s.daemon.swapClientPane(client, direction)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	if err := commitTestProjection(client, transition); err != nil {
		return nil, protocol.ClientLayout{}, false, err
	}
	return window, client.currentLayout, changed, nil
}

func closeTestFocusedPane(s *SessionState) (*Pane, *Window, protocol.ClientLayout, bool, uint64, bool, error) {
	client := clientForState(s)
	pane := client.activePane()
	if pane == nil {
		return nil, nil, protocol.ClientLayout{}, false, 0, false, errors.New("client has no active pane")
	}
	result, err := s.daemon.removeClientPane(client, pane.ID)
	if err != nil {
		return nil, nil, protocol.ClientLayout{}, false, 0, false, err
	}
	if !result.FinalPane {
		if err := commitTestProjection(client, result.Transition); err != nil {
			return nil, nil, protocol.ClientLayout{}, false, 0, false, err
		}
	}
	return result.Pane, result.Window, client.currentLayout, result.WindowClosed, result.ClosedWindowID, result.FinalPane, nil
}

func removeTestPane(s *SessionState, paneID uint64) (*Window, protocol.ClientLayout, bool, bool, error) {
	client := clientForState(s)
	result, err := s.daemon.removeClientPane(client, paneID)
	if err != nil {
		return nil, protocol.ClientLayout{}, false, false, err
	}
	if result.Removed && !result.FinalPane {
		if err := commitTestProjection(client, result.Transition); err != nil {
			return nil, protocol.ClientLayout{}, false, false, err
		}
	}
	return result.Window, client.currentLayout, result.FinalPane, result.Removed, nil
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
	if state.daemon.clientSessions == nil {
		state.daemon.clientSessions = make(map[*ClientIdentity]uint64)
	}
	if state.daemon.clientInstances == nil {
		state.daemon.clientInstances = make(map[*ClientIdentity]*ClientInstance)
	}
	if state.daemon.attachments == nil {
		state.daemon.attachments = make(map[uint64]*ClientIdentity)
	}
	state.daemon.sessions[state.ID] = state
	state.daemon.sessionIndex.Store(state.ID, state)
	state.daemon.ensureSessionGroupInActor(state)
	previous := state.daemon.clients[state.ID]
	if client == nil {
		delete(state.daemon.clients, state.ID)
		state.daemon.clientIndex.Delete(state.ID)
		if previous != nil && previous.identity != nil {
			if state.daemon.attachments[state.ID] == previous.identity {
				delete(state.daemon.attachments, state.ID)
			}
			if state.daemon.clientSessions[previous.identity] == state.ID {
				delete(state.daemon.clientSessions, previous.identity)
			}
			if state.daemon.clientInstances[previous.identity] == previous {
				delete(state.daemon.clientInstances, previous.identity)
			}
		}
		testClientByState.Delete(state)
		return
	}
	if previous != nil && previous != client && previous.identity != nil {
		if state.daemon.attachments[state.ID] == previous.identity {
			delete(state.daemon.attachments, state.ID)
		}
		if state.daemon.clientSessions[previous.identity] == state.ID {
			delete(state.daemon.clientSessions, previous.identity)
		}
		if state.daemon.clientInstances[previous.identity] == previous {
			delete(state.daemon.clientInstances, previous.identity)
		}
	}
	if client.identity == nil {
		client.identity = &ClientIdentity{}
	}
	if oldSessionID := state.daemon.clientSessions[client.identity]; oldSessionID != 0 && oldSessionID != state.ID {
		if state.daemon.attachments[oldSessionID] == client.identity {
			delete(state.daemon.attachments, oldSessionID)
		}
		if state.daemon.clients[oldSessionID] == client {
			delete(state.daemon.clients, oldSessionID)
			state.daemon.clientIndex.Delete(oldSessionID)
		}
	}
	client.sessionID = state.ID
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
	state.daemon.clientInstances[client.identity] = client
	state.daemon.clientSessions[client.identity] = state.ID
	state.daemon.attachments[state.ID] = client.identity
	testClientByState.Store(state, client)
	if len(state.Windows) > 0 {
		_ = installTestCurrentProjection(client)
	}
	client.startStatusOutput()
}

func testDaemonForState(state *SessionState) *Daemon {
	return &Daemon{
		clients:                  make(map[uint64]*ClientInstance),
		clientIdentities:         make(map[string]*ClientIdentity),
		clientSessions:           make(map[*ClientIdentity]uint64),
		clientInstances:          make(map[*ClientIdentity]*ClientInstance),
		clientTerminalReasons:    make(map[*ClientIdentity]string),
		attachments:              make(map[uint64]*ClientIdentity),
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
