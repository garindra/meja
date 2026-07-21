package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

const (
	sessionID0 = 0
	clientID0  = 0
)

type sessionOperation struct {
	run  func() error
	done chan error
}

func (s *Session) coordinate(run func() error) error {
	if s.operations == nil {
		return run()
	}
	// Prefer the terminal state deterministically. A select between a closed
	// operationsDone channel and a writable buffered mailbox may otherwise
	// enqueue work after the actor has stopped.
	select {
	case <-s.operationsDone:
		return nil
	default:
	}
	done := make(chan error, 1)
	select {
	case s.operations <- sessionOperation{run: run, done: done}:
	case <-s.operationsDone:
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-s.operationsDone:
		return nil
	}
}

func (s *Session) coordinateClientInstance(client *ClientInstance, run func() error) error {
	return s.coordinate(func() error {
		if s.clientInstance != client {
			return nil
		}
		return run()
	})
}

func (s *Session) runOperations() {
	for {
		select {
		case operation := <-s.operations:
			err := operation.run()
			if operation.done != nil {
				operation.done <- err
			}
			if s.stopping {
				s.stopOperations()
				return
			}
		case batch := <-s.processObservationUpdates:
			_ = s.applyMonitoredProcessObservations(batch)
		case <-s.operationsDone:
			return
		}
	}
}

func (s *Session) post(run func() error) {
	if s.operations == nil {
		_ = run()
		return
	}
	select {
	case <-s.operationsDone:
		return
	default:
	}
	select {
	case s.operations <- sessionOperation{run: run}:
	case <-s.operationsDone:
	}
}

func (s *Session) stopOperations() {
	s.stopOnce.Do(func() { close(s.operationsDone) })
}

func isSessionReplacedClose(err error) bool {
	var applicationErr *quic.ApplicationError
	return errors.As(err, &applicationErr) && applicationErr.ErrorCode == protocol.SessionReplacedErrorCode
}

// shutdownNow runs only on the session actor. The Session releases its own
// live resources, tells the Daemon to forget it, and lets runOperations exit
// after replying to the operation that caused the shutdown.
func (s *Session) shutdownNow() {
	if s.stopping {
		return
	}
	s.stopping = true
	s.ended = true
	if s.processMonitor != nil {
		s.processMonitor.DropSession(s.ID)
	}
	client := s.clientInstance
	s.clientInstance = nil
	// A session exit is terminal for its assigned client instance. Close the
	// active transport cleanly so the client exits instead of reconnecting.
	if client != nil && client.QUIC != nil {
		_ = client.QUIC.CloseWithError(0, "")
	}
	if s.daemon != nil {
		s.daemon.sessionExited(s)
	}
}

func (s *Session) shutdown() error {
	return s.shutdownWithTimeouts(defaultPaneTerminationTimeouts)
}

func (s *Session) shutdownWithTimeouts(timeouts paneTerminationTimeouts) error {
	s.shutdownOnce.Do(func() {
		var panes []*Pane
		s.shutdownErr = s.coordinate(func() error {
			if s.stopping {
				return nil
			}
			panes = make([]*Pane, 0, len(s.Panes))
			for _, pane := range s.Panes {
				panes = append(panes, pane)
			}
			s.shutdownNow()
			return nil
		})
		<-s.operationsDone
		if remaining := terminatePanesAndWait(panes, timeouts); len(remaining) > 0 {
			cleanupErr := fmt.Errorf("%d pane process(es) did not exit before the shutdown deadline", len(remaining))
			if s.daemon != nil {
				s.daemon.logf("meja server: shut down session %d: %v\n", s.ID, cleanupErr)
			}
			s.shutdownErr = errors.Join(s.shutdownErr, cleanupErr)
		}
		// Persistence stops independently when operationsDone closes. Wait after
		// pane cleanup so any in-flight write overlaps the termination grace period.
		if s.persistenceStarted.Load() {
			<-s.persistenceDone
		}
	})
	return s.shutdownErr
}

func (s *Session) attachClientInstance(client *ClientInstance, cols, rows uint16) error {
	return s.coordinate(func() error {
		previous := s.clientInstance
		if previous != nil && previous != client && previous.QUIC != nil {
			_ = previous.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "session attached elsewhere")
		}
		s.clientInstance = client
		if client != nil && client.StatusOutput != nil {
			if err := s.attachStatusOutput(client, client.StatusOutput); err != nil {
				return err
			}
		}
		// Layout revisions are session-local, while the client scanout survives a
		// live session switch and treats them as monotonic. Advance this session's
		// counter past everything already published on the retained transport.
		if client != nil {
			if floor := client.highestLayoutRevision.Load(); s.NextLayoutRevision < floor {
				s.NextLayoutRevision = floor
			}
		}
		s.EnsureClient(clientID0)
		if cols == 0 || rows == 0 {
			pane, _ := s.ActivePane(clientID0)
			if pane == nil {
				return errors.New("session has no active pane")
			}
			paneCols, paneRows := pane.TerminalSize()
			cols, rows = uint16(paneCols), uint16(paneRows)
		}
		handoff := s.beginOutputHandoff()
		s.SetClientSize(clientID0, cols, rows)
		s.ResizeAll(cols, rows)
		if _, _, _, err := s.ReattachClient(clientID0); err != nil {
			return err
		}
		return s.rebindOutputsAndPublishLayout(handoff)
	})
}

func (s *Session) detachClientInstance() {
	_ = s.coordinate(func() error {
		client := s.clientInstance
		if client == nil || (!client.detaching.Load() && !client.switching.Load()) {
			return nil
		}
		var detachErr error
		for _, pane := range s.Panes {
			if err := pane.cancelHistorySelection(); err != nil && detachErr == nil {
				detachErr = err
			}
		}
		if err := s.detachStatusOutput(client); err != nil {
			detachErr = err
		}
		if err := s.detachLeases(client.outputLeases()); err != nil {
			if detachErr == nil {
				detachErr = err
			}
		}
		// The ownership move must complete even when cleaning up an output
		// stream reports an error; transport teardown will handle that stream.
		s.clientInstance = nil
		return detachErr
	})
}

func (s *Session) currentOutputLease(slot int) *OutputLease {
	if s.clientInstance == nil || slot < 0 || slot >= len(s.clientInstance.Output) {
		return nil
	}
	return s.clientInstance.Output[slot]
}

func (s *Session) isAttached() bool {
	attached := false
	_ = s.coordinate(func() error {
		attached = s.clientInstance != nil
		return nil
	})
	return attached
}

func (s *Session) info() (name string, attached bool) {
	_ = s.coordinate(func() error {
		name = s.Name
		attached = s.clientInstance != nil
		return nil
	})
	return name, attached
}

// Session is the authority for one persistent terminal workspace. Its actor
// owns terminal state, clients, windows, and pane membership. It borrows the
// attached ClientInstance, and panes remain alive across transport replacement
// or a future client-instance session switch.
type Session struct {
	ID      uint64
	Name    string
	Windows map[uint64]*Window
	Panes   map[uint64]*Pane
	Clients map[uint64]*ClientState

	NextWindowID       uint64
	NextPaneID         uint64
	NextLayoutRevision uint64

	daemon                    *Daemon
	clientInstance            *ClientInstance
	rootDir                   string
	operations                chan sessionOperation
	operationsDone            chan struct{}
	statusCommands            chan statusCommand
	stopOnce                  sync.Once
	shutdownOnce              sync.Once
	shutdownErr               error
	processObserver           ProcessObserver
	processMonitor            *ProcessMonitor
	processSaveCandidates     map[uint64]processSaveCandidate
	processObservationUpdates chan monitoredProcessBatch
	persistenceOnce           sync.Once
	persistenceNow            chan struct{}
	persistenceDone           chan struct{}
	persistenceStarted        atomic.Bool
	sessionPersistence        *SessionPersistence
	obsoletePersistenceNames  map[string]struct{}
	promptContinuations       map[uint64]promptContinuation
	nextStatusMessageID       uint64
	statusMessageDuration     time.Duration
	stopping                  bool
	ended                     bool
}

type Window struct {
	ID               uint64
	DisplayIndex     int
	Name             string
	AutomaticName    bool
	ActivePaneID     uint64
	Zoomed           bool
	ZoomedPaneID     uint64
	Layout           LayoutNode
	LayoutRevision   uint64
	layoutCycleIndex int
}

func (w *Window) clearZoom() {
	if w == nil {
		return
	}
	w.Zoomed = false
	w.ZoomedPaneID = 0
}

type PromptKind uint8

const (
	PromptKindRenameWindow PromptKind = iota + 1
	PromptKindRenameSession
	PromptKindConfirm
	PromptKindCommand
)

type PromptAction uint8

const (
	PromptActionNone PromptAction = iota
	PromptActionChanged
	PromptActionSubmit
	PromptActionCancel
)

type PromptState struct {
	Kind          PromptKind
	Action        PromptAction
	Label         string
	Text          []rune
	Cursor        int
	pendingUTF8   []byte
	PendingEscape []byte
}

type promptResult struct {
	Accepted bool
	Text     string
}

type promptContinuation func(promptResult) error

type RenderBinding struct {
	Slot   int
	PaneID uint64
}

type PaneSwapDirection int8

const (
	SwapPanePrevious PaneSwapDirection = -1
	SwapPaneNext     PaneSwapDirection = 1
)

type ClientState struct {
	ID             uint64
	SessionID      uint64
	ActiveWindowID uint64
	FocusedPaneID  uint64
	FocusX2        int
	FocusY2        int
	HasFocusPoint  bool
	TerminalCols   uint16
	TerminalRows   uint16

	RenderBindings    []RenderBinding
	InputState        serverInputState
	PrefixEscape      []byte
	ResizeRepeatUntil time.Time
	Prompt            *PromptState
	StatusMessage     string
	statusMessageID   uint64
	LastWindowID      uint64
	HasLastWindow     bool
}

func (c *ClientState) setFocusedPane(paneID uint64) {
	c.FocusedPaneID = paneID
	c.HasFocusPoint = false
}

func NewSession(id uint64) *Session {
	session := &Session{
		ID:                        id,
		Windows:                   map[uint64]*Window{},
		Panes:                     map[uint64]*Pane{},
		Clients:                   map[uint64]*ClientState{},
		NextWindowID:              1,
		operations:                make(chan sessionOperation, 64),
		operationsDone:            make(chan struct{}),
		statusCommands:            make(chan statusCommand, 64),
		processObserver:           NewProcessObserver(),
		processSaveCandidates:     make(map[uint64]processSaveCandidate),
		processObservationUpdates: make(chan monitoredProcessBatch, 8),
		persistenceNow:            make(chan struct{}, 1),
		persistenceDone:           make(chan struct{}),
		obsoletePersistenceNames:  make(map[string]struct{}),
		promptContinuations:       make(map[uint64]promptContinuation),
	}
	go session.runOperations()
	go session.runStatusOutput()
	return session
}

func (s *Session) contextualPaneRequest(request paneRequest) paneRequest {
	request.MejaSessionTarget = strconv.FormatUint(s.ID, 10)
	if s.daemon != nil {
		request.MejaSocket = s.daemon.controlPath
	}
	return request
}

func (s *Session) nextLayoutRevisionLocked() uint64 {
	s.NextLayoutRevision++
	return s.NextLayoutRevision
}

func (s *Session) NewClient(id uint64) *ClientState {
	client := &ClientState{ID: id, SessionID: s.ID}
	s.Clients[id] = client
	return client
}

func (s *Session) EnsureClient(id uint64) *ClientState {
	client := s.ensureClientLocked(id)
	return cloneClientState(client)
}

func (s *Session) ensureClientLocked(id uint64) *ClientState {
	if client := s.Clients[id]; client != nil {
		s.ensureClientFocusLocked(client)
		return client
	}
	client := &ClientState{ID: id, SessionID: s.ID}
	s.ensureClientFocusLocked(client)
	s.Clients[id] = client
	return client
}

func (s *Session) BeginPrompt(clientID uint64, kind PromptKind, label, initial string) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, fmt.Errorf("client %d has no active window", clientID)
	}
	text := []rune(initial)
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{
		Kind:   kind,
		Label:  label,
		Text:   text,
		Cursor: len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) BeginRenameWindowPrompt(clientID uint64) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, fmt.Errorf("client %d has no active window", clientID)
	}
	text := []rune(window.Name)
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{
		Kind:   PromptKindRenameWindow,
		Label:  "(rename-window) ",
		Text:   text,
		Cursor: len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) BeginRenameSessionPrompt(clientID uint64) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	text := []rune(s.Name)
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{
		Kind:   PromptKindRenameSession,
		Label:  "(rename-session) ",
		Text:   text,
		Cursor: len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) BeginCommandPrompt(clientID uint64) (*PromptState, error) {
	return s.BeginPrompt(clientID, PromptKindCommand, ":", "")
}

func (s *Session) showStatusMessage(clientID uint64, message string) {
	client := s.Clients[clientID]
	if client == nil {
		return
	}
	s.nextStatusMessageID++
	client.StatusMessage = message
	client.statusMessageID = s.nextStatusMessageID
	messageID := client.statusMessageID
	duration := s.statusMessageDuration
	if duration <= 0 {
		duration = time.Second
	}
	time.AfterFunc(duration, func() {
		s.post(func() error {
			client := s.Clients[clientID]
			if client == nil || client.statusMessageID != messageID {
				return nil
			}
			client.StatusMessage = ""
			return s.publishStatusBar()
		})
	})
}

func (s *Session) beginConfirmationPrompt(clientID uint64, label string, continuation promptContinuation) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{Kind: PromptKindConfirm, Label: label}
	if continuation == nil {
		delete(s.promptContinuations, clientID)
	} else {
		s.promptContinuations[clientID] = continuation
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) resolvePrompt(clientID uint64, result promptResult) error {
	continuation := s.promptContinuations[clientID]
	delete(s.promptContinuations, clientID)
	if continuation == nil {
		return s.publishStatusBar()
	}
	return continuation(result)
}

func (s *Session) finishSessionRename(name string, accepted bool) error {
	client := s.Clients[clientID0]
	if accepted {
		changed := s.Name != name
		s.Name = name
		if changed {
			s.markSessionChangedForPersistence()
		}
		if client != nil && client.Prompt != nil && client.Prompt.Kind == PromptKindRenameSession {
			client.Prompt = nil
		}
	} else if client != nil && client.Prompt != nil && client.Prompt.Kind == PromptKindRenameSession {
		client.Prompt.Action = PromptActionNone
	}
	return s.publishStatusBar()
}

func (s *Session) markSessionChangedForPersistence() {
	s.persistSessionForPersistence()
}

func (s *Session) markWindowChangedForPersistence(windowID uint64) {
	s.persistWindowForPersistence(windowID)
}

func (s *Session) setRoot(root string) {
	root = filepath.Clean(root)
	if root == s.rootDir {
		return
	}
	s.rootDir = root
	s.markSessionChangedForPersistence()
}

func (s *Session) SessionName() string {
	return s.Name
}

func (s *Session) setSessionName(name string) {
	s.Name = name
}

func (s *Session) ActivePrompt(clientID uint64) *PromptState {
	client := s.Clients[clientID]
	if client == nil {
		return nil
	}
	return clonePromptState(client.Prompt)
}

func (s *Session) ensureClientFocusLocked(client *ClientState) {
	if len(s.Windows) == 0 {
		return
	}
	changed := false
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		ids := s.windowIDsLocked()
		window = s.Windows[ids[0]]
		changed = client.ActiveWindowID != window.ID
		client.ActiveWindowID = window.ID
	}
	activePaneID := windowActivePaneID(window)
	changed = changed || window.ActivePaneID != activePaneID
	window.ActivePaneID = activePaneID
	if !windowHasPane(window, client.FocusedPaneID) {
		client.setFocusedPane(window.ActivePaneID)
	}
	if changed {
		s.markSessionChangedForPersistence()
	}
}

func (s *Session) SetClientSize(clientID uint64, cols, rows uint16) *ClientState {
	client := s.ensureClientLocked(clientID)
	if client.TerminalCols != cols || client.TerminalRows != rows {
		for _, window := range s.Windows {
			window.LayoutRevision = s.nextLayoutRevisionLocked()
		}
		client.TerminalCols = cols
		client.TerminalRows = rows
	}
	return cloneClientState(client)
}

func (s *Session) CreateWindow(pane *Pane, activateFor uint64) (*Window, *ClientState) {
	if s.NextWindowID == 0 {
		s.NextWindowID = 1
	}
	windowID := s.NextWindowID
	s.NextWindowID++
	displayIndex := s.lowestAvailableWindowDisplayIndex()
	window := &Window{
		ID:               windowID,
		DisplayIndex:     displayIndex,
		Name:             pane.Title,
		AutomaticName:    true,
		ActivePaneID:     pane.ID,
		Layout:           &PaneLayout{PaneID: pane.ID},
		LayoutRevision:   s.nextLayoutRevisionLocked(),
		layoutCycleIndex: layoutPresetCustom,
	}
	s.Windows[windowID] = window
	s.Panes[pane.ID] = pane
	client := s.ensureClientLocked(activateFor)
	if client.ActiveWindowID != window.ID {
		client.LastWindowID = client.ActiveWindowID
		client.HasLastWindow = client.LastWindowID != 0
	}
	client.ActiveWindowID = windowID
	client.setFocusedPane(pane.ID)
	s.rebuildBindingsLocked(client, window)
	s.markSessionChangedForPersistence()
	return window, cloneClientState(client)
}

func (s *Session) HasWindows() bool {
	return len(s.Windows) > 0
}

func (s *Session) PanesSnapshot() []*Pane {
	panes := make([]*Pane, 0, len(s.Panes))
	for _, pane := range s.Panes {
		panes = append(panes, pane)
	}
	return panes
}

func (s *Session) Pane(id uint64) *Pane {
	return s.Panes[id]
}

func (s *Session) ReattachClient(clientID uint64) (*Window, *Pane, *ClientState, error) {
	client := s.ensureClientLocked(clientID)
	if len(s.Windows) == 0 {
		return nil, nil, nil, fmt.Errorf("session has no windows")
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		ids := s.windowIDsLocked()
		window = s.Windows[ids[0]]
	}
	s.selectWindowLocked(client, window)
	pane := s.Panes[client.FocusedPaneID]
	return cloneWindow(window), pane, cloneClientState(client), nil
}

func (s *Session) AddPaneID() uint64 {
	id := s.NextPaneID
	s.NextPaneID++
	return id
}

func (s *Session) lowestAvailableWindowDisplayIndex() int {
	used := make(map[int]struct{}, len(s.Windows))
	for _, window := range s.Windows {
		used[window.DisplayIndex] = struct{}{}
	}
	for index := 0; ; index++ {
		if _, ok := used[index]; !ok {
			return index
		}
	}
}

func (s *Session) ActivePane(clientID uint64) (*Pane, *ClientState) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	return s.Panes[client.FocusedPaneID], cloneClientState(client)
}

func (s *Session) ActiveWindow(clientID uint64) (*Window, *ClientState) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	window := s.Windows[client.ActiveWindowID]
	return cloneWindow(window), cloneClientState(client)
}

func (s *Session) ResolveInputTarget(clientID, requestedPaneID uint64) (*Pane, *ClientState, bool) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false
	}
	pane := s.Panes[client.FocusedPaneID]
	if pane == nil {
		return nil, cloneClientState(client), false
	}
	return pane, cloneClientState(client), client.FocusedPaneID == requestedPaneID
}

func (s *Session) SelectWindow(clientID, windowID uint64) (*Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[windowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", windowID)
	}
	s.selectWindowLocked(client, window)
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) selectWindowLocked(client *ClientState, window *Window) {
	changed := client.ActiveWindowID != window.ID
	if client.ActiveWindowID != window.ID {
		if previous := s.Windows[client.ActiveWindowID]; previous != nil && windowHasPane(previous, client.FocusedPaneID) {
			changed = changed || previous.ActivePaneID != client.FocusedPaneID
			previous.ActivePaneID = client.FocusedPaneID
		}
		client.LastWindowID = client.ActiveWindowID
		client.HasLastWindow = client.LastWindowID != 0
	}
	client.ActiveWindowID = window.ID
	activePaneID := windowActivePaneID(window)
	changed = changed || window.ActivePaneID != activePaneID
	window.ActivePaneID = activePaneID
	client.setFocusedPane(window.ActivePaneID)
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	s.rebuildBindingsLocked(client, window)
	if changed {
		s.markSessionChangedForPersistence()
	}
}

func (s *Session) RenameWindow(windowID uint64, name string) (*Window, error) {
	window := s.Windows[windowID]
	if window == nil {
		return nil, fmt.Errorf("unknown window %d", windowID)
	}
	// Empty names are valid; normal status projection remains well-formed.
	changed := window.Name != name || window.AutomaticName
	window.Name = name
	window.AutomaticName = false
	if changed {
		s.markWindowChangedForPersistence(windowID)
	}
	return cloneWindow(window), nil
}

func (s *Session) FocusPane(clientID, paneID uint64) (*Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if !windowHasPane(window, paneID) {
		return nil, nil, fmt.Errorf("pane %d not visible in window %d", paneID, window.ID)
	}
	if window.Zoomed && window.ZoomedPaneID != paneID {
		window.clearZoom()
		window.LayoutRevision = s.nextLayoutRevisionLocked()
		s.rebuildBindingsLocked(client, window)
	}
	changed := window.ActivePaneID != paneID
	window.ActivePaneID = paneID
	client.setFocusedPane(paneID)
	if changed {
		s.markWindowChangedForPersistence(window.ID)
	}
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) ToggleZoom(clientID uint64) (*Window, *ClientState, bool, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, false, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if len(window.Layout.PaneIDs()) <= 1 {
		return cloneWindow(window), cloneClientState(client), false, nil
	}
	activePaneChanged := false
	if window.Zoomed {
		window.clearZoom()
	} else {
		if !windowHasPane(window, client.FocusedPaneID) {
			return nil, nil, false, fmt.Errorf("focused pane %d not in window %d", client.FocusedPaneID, window.ID)
		}
		window.Zoomed = true
		window.ZoomedPaneID = client.FocusedPaneID
		activePaneChanged = window.ActivePaneID != client.FocusedPaneID
		window.ActivePaneID = client.FocusedPaneID
	}
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	s.rebuildBindingsLocked(client, window)
	if activePaneChanged {
		s.markWindowChangedForPersistence(window.ID)
	}
	return cloneWindow(window), cloneClientState(client), true, nil
}

func (s *Session) CycleWindowLayout(clientID uint64) (*Window, *ClientState, bool, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, false, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	paneIDs := window.Layout.PaneIDs()
	if len(paneIDs) <= 1 {
		return cloneWindow(window), cloneClientState(client), false, nil
	}

	next := 0
	if window.layoutCycleIndex >= 0 {
		next = (window.layoutCycleIndex + 1) % layoutPresetCount
	}
	window.Layout = buildPresetLayout(paneIDs, client.FocusedPaneID, next)
	window.layoutCycleIndex = next
	window.clearZoom()
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	s.rebuildBindingsLocked(client, window)
	s.markWindowChangedForPersistence(window.ID)
	return cloneWindow(window), cloneClientState(client), true, nil
}

func (s *Session) ResizeFocusedPane(clientID uint64, direction PaneResizeDirection, amount int) (*Window, *ClientState, bool, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, false, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if direction > ResizePaneRight {
		return nil, nil, false, fmt.Errorf("invalid pane resize direction %d", direction)
	}
	if amount <= 0 {
		return nil, nil, false, fmt.Errorf("pane resize amount must be positive")
	}
	unzoomed := window.Zoomed
	window.clearZoom()
	resized := ResizePaneBoundary(window.Layout, client.FocusedPaneID, direction, amount, Rect{
		Width:  int(client.TerminalCols),
		Height: int(client.TerminalRows),
	})
	if !resized && !unzoomed {
		return cloneWindow(window), cloneClientState(client), false, nil
	}
	window.layoutCycleIndex = layoutPresetCustom
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	s.rebuildBindingsLocked(client, window)
	if resized {
		s.markWindowChangedForPersistence(window.ID)
	}
	return cloneWindow(window), cloneClientState(client), true, nil
}

func (s *Session) SplitFocusedPane(clientID uint64, pane *Pane, direction SplitDirection) (*Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if len(window.Layout.PaneIDs()) >= int(protocol.MaxVisiblePanes) {
		return nil, nil, fmt.Errorf("window %d has reached the %d-pane limit", window.ID, protocol.MaxVisiblePanes)
	}
	if !windowHasPane(window, client.FocusedPaneID) {
		return nil, nil, fmt.Errorf("focused pane %d not in window %d", client.FocusedPaneID, window.ID)
	}
	if direction != SplitVertical && direction != SplitHorizontal {
		return nil, nil, fmt.Errorf("invalid split direction %d", direction)
	}
	window.clearZoom()
	window.layoutCycleIndex = layoutPresetCustom
	updated, replaced := replacePaneWithSplit(window.Layout, client.FocusedPaneID, pane.ID, direction)
	if !replaced {
		return nil, nil, fmt.Errorf("focused pane %d not found in layout", client.FocusedPaneID)
	}
	s.Panes[pane.ID] = pane
	window.Layout = updated
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	window.ActivePaneID = pane.ID
	client.setFocusedPane(pane.ID)
	s.rebuildBindingsLocked(client, window)
	s.markWindowChangedForPersistence(window.ID)
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) SwapFocusedPane(clientID uint64, direction PaneSwapDirection) (*Window, *ClientState, bool, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, false, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if direction != SwapPanePrevious && direction != SwapPaneNext {
		return nil, nil, false, fmt.Errorf("invalid pane swap direction %d", direction)
	}
	window.clearZoom()

	s.rebuildBindingsLocked(client, window)
	if len(client.RenderBindings) < 2 {
		return cloneWindow(window), cloneClientState(client), false, nil
	}
	current := -1
	for i, binding := range client.RenderBindings {
		if binding.PaneID == client.FocusedPaneID {
			current = i
			break
		}
	}
	if current < 0 {
		return cloneWindow(window), cloneClientState(client), false, nil
	}
	target := (current + int(direction) + len(client.RenderBindings)) % len(client.RenderBindings)
	targetPaneID := client.RenderBindings[target].PaneID
	if !swapLayoutPanes(window.Layout, client.FocusedPaneID, targetPaneID) {
		return nil, nil, false, fmt.Errorf("swap panes %d and %d in window %d", client.FocusedPaneID, targetPaneID, window.ID)
	}
	window.layoutCycleIndex = layoutPresetCustom
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	client.setFocusedPane(client.FocusedPaneID)
	s.rebuildBindingsLocked(client, window)
	s.markWindowChangedForPersistence(window.ID)
	return cloneWindow(window), cloneClientState(client), true, nil
}

func (s *Session) CanSplitFocusedPane(clientID uint64) error {
	client := s.Clients[clientID]
	if client == nil {
		return fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if !windowHasPane(window, client.FocusedPaneID) {
		return fmt.Errorf("focused pane %d not in window %d", client.FocusedPaneID, window.ID)
	}
	if len(window.Layout.PaneIDs()) >= int(protocol.MaxVisiblePanes) {
		return fmt.Errorf("window %d has reached the %d-pane limit", window.ID, protocol.MaxVisiblePanes)
	}
	return nil
}

func (s *Session) CloseFocusedPane(clientID uint64) (closedPane *Pane, window *Window, client *ClientState, windowClosed bool, closedWindowID uint64, autoCreate bool, err error) {
	c := s.Clients[clientID]
	if c == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("unknown client %d", clientID)
	}
	window = s.Windows[c.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("unknown window %d", c.ActiveWindowID)
	}
	paneIDs := window.Layout.PaneIDs()
	if len(paneIDs) <= 1 {
		closedPane = s.Panes[c.FocusedPaneID]
		s.unwatchPaneProcesses(c.FocusedPaneID)
		delete(s.Panes, c.FocusedPaneID)
		delete(s.Windows, window.ID)
		windowClosed = true
		closedWindowID = window.ID
		if len(s.Windows) == 0 {
			s.markSessionChangedForPersistence()
			return closedPane, nil, cloneClientState(c), true, closedWindowID, true, nil
		}
		nextWindow := s.replacementWindowLocked(c, window.ID)
		s.activateReplacementWindowLocked(c, nextWindow)
		s.markSessionChangedForPersistence()
		return closedPane, cloneWindow(nextWindow), cloneClientState(c), true, closedWindowID, false, nil
	}
	closedPane = s.Panes[c.FocusedPaneID]
	if window.Zoomed && window.ZoomedPaneID == c.FocusedPaneID {
		window.clearZoom()
	}
	window.layoutCycleIndex = layoutPresetCustom
	updated, nextFocusedPaneID, removed := removePaneFromLayout(window.Layout, c.FocusedPaneID)
	if !removed || updated == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("focused pane %d not found in layout", c.FocusedPaneID)
	}
	s.unwatchPaneProcesses(c.FocusedPaneID)
	delete(s.Panes, c.FocusedPaneID)
	window.Layout = updated
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	window.ActivePaneID = nextFocusedPaneID
	c.setFocusedPane(nextFocusedPaneID)
	s.rebuildBindingsLocked(c, window)
	s.markSessionChangedForPersistence()
	return closedPane, cloneWindow(window), cloneClientState(c), false, 0, false, nil
}

// RemovePane applies process exit to authoritative session state. It is a no-op
// when an explicit close already removed the pane before Process.Wait returned.
func (s *Session) RemovePane(paneID, clientID uint64) (window *Window, client *ClientState, finalPane, removed bool, err error) {
	c := s.Clients[clientID]
	if c == nil {
		return nil, nil, false, false, fmt.Errorf("unknown client %d", clientID)
	}
	var owner *Window
	for _, candidate := range s.Windows {
		if windowHasPane(candidate, paneID) {
			owner = candidate
			break
		}
	}
	if owner == nil || s.Panes[paneID] == nil {
		return nil, cloneClientState(c), false, false, nil
	}
	s.unwatchPaneProcesses(paneID)
	delete(s.Panes, paneID)
	if owner.Zoomed && owner.ZoomedPaneID == paneID {
		owner.clearZoom()
	}
	if len(owner.Layout.PaneIDs()) > 1 {
		owner.layoutCycleIndex = layoutPresetCustom
		updated, nextFocusedPaneID, ok := removePaneFromLayout(owner.Layout, paneID)
		if !ok || updated == nil {
			return nil, nil, false, false, fmt.Errorf("pane %d not found in window %d layout", paneID, owner.ID)
		}
		owner.Layout = updated
		if len(updated.PaneIDs()) <= 1 {
			owner.clearZoom()
		}
		owner.LayoutRevision = s.nextLayoutRevisionLocked()
		if owner.ActivePaneID == paneID {
			owner.ActivePaneID = nextFocusedPaneID
		}
		owner.ActivePaneID = windowActivePaneID(owner)
		if c.ActiveWindowID == owner.ID && c.FocusedPaneID == paneID {
			c.setFocusedPane(owner.ActivePaneID)
		}
	} else {
		delete(s.Windows, owner.ID)
		if len(s.Windows) == 0 {
			s.markSessionChangedForPersistence()
			return nil, cloneClientState(c), true, true, nil
		}
		if c.ActiveWindowID == owner.ID {
			nextWindow := s.replacementWindowLocked(c, owner.ID)
			s.activateReplacementWindowLocked(c, nextWindow)
		} else if c.HasLastWindow && c.LastWindowID == owner.ID {
			c.LastWindowID = 0
			c.HasLastWindow = false
		}
	}
	active := s.Windows[c.ActiveWindowID]
	if active == nil {
		return nil, nil, false, false, fmt.Errorf("client %d has no active window after removing pane %d", clientID, paneID)
	}
	s.rebuildBindingsLocked(c, active)
	s.markSessionChangedForPersistence()
	return cloneWindow(active), cloneClientState(c), false, true, nil
}

func (s *Session) CloseWindow(clientID, windowID uint64) (closed uint64, closedPanes []*Pane, replacement *Window, pane *Pane, client *ClientState, autoCreated bool, err error) {
	c := s.Clients[clientID]
	if c == nil {
		return 0, nil, nil, nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	w := s.Windows[windowID]
	if w == nil {
		return 0, nil, nil, nil, nil, false, fmt.Errorf("unknown window %d", windowID)
	}
	paneIDs := w.Layout.PaneIDs()
	if len(paneIDs) == 0 {
		return 0, nil, nil, nil, nil, false, fmt.Errorf("window %d has no panes", windowID)
	}
	wasActive := c.ActiveWindowID == windowID
	closedPanes = make([]*Pane, 0, len(paneIDs))
	for _, paneID := range paneIDs {
		if p := s.Panes[paneID]; p != nil {
			closedPanes = append(closedPanes, p)
		}
		s.unwatchPaneProcesses(paneID)
		delete(s.Panes, paneID)
	}
	delete(s.Windows, windowID)
	closed = windowID

	if len(s.Windows) == 0 {
		s.markSessionChangedForPersistence()
		return closed, closedPanes, nil, nil, cloneClientState(c), true, nil
	}
	if wasActive {
		nextWindow := s.replacementWindowLocked(c, windowID)
		s.activateReplacementWindowLocked(c, nextWindow)
		replacement = cloneWindow(nextWindow)
	} else {
		if c.HasLastWindow && c.LastWindowID == windowID {
			c.LastWindowID = 0
			c.HasLastWindow = false
		}
		activeWindow := s.Windows[c.ActiveWindowID]
		if activeWindow == nil {
			return 0, nil, nil, nil, nil, false, fmt.Errorf("client %d has no active window after closing window %d", clientID, windowID)
		}
		s.rebuildBindingsLocked(c, activeWindow)
		replacement = cloneWindow(activeWindow)
	}
	s.markSessionChangedForPersistence()
	pane = s.Panes[c.FocusedPaneID]
	return closed, closedPanes, replacement, pane, cloneClientState(c), false, nil
}

func (s *Session) replacementWindowLocked(client *ClientState, closedWindowID uint64) *Window {
	if client.HasLastWindow && client.LastWindowID != closedWindowID {
		if window := s.Windows[client.LastWindowID]; window != nil {
			return window
		}
	}
	ids := s.windowIDsLocked()
	if len(ids) == 0 {
		return nil
	}
	return s.Windows[ids[0]]
}

func (s *Session) activateReplacementWindowLocked(client *ClientState, window *Window) {
	client.ActiveWindowID = window.ID
	window.ActivePaneID = windowActivePaneID(window)
	client.setFocusedPane(window.ActivePaneID)
	client.LastWindowID = 0
	client.HasLastWindow = false
	s.rebuildBindingsLocked(client, window)
}

type WindowStatus struct {
	WindowID uint64
	Index    int
	Title    string
	Active   bool
	Zoomed   bool
}

func (s *Session) WindowStatuses(clientID uint64) []WindowStatus {
	client := s.Clients[clientID]
	active := uint64(0)
	if client != nil {
		active = client.ActiveWindowID
	}
	list := make([]WindowStatus, 0, len(s.Windows))
	for _, window := range s.Windows {
		list = append(list, WindowStatus{
			WindowID: window.ID,
			Index:    window.DisplayIndex,
			Title:    window.Name,
			Active:   window.ID == active,
			Zoomed:   window.Zoomed,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	return list
}

func (s *Session) WindowLayout(clientID uint64) (protocol.WindowLayout, error) {
	client := s.Clients[clientID]
	if client == nil {
		return protocol.WindowLayout{}, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return protocol.WindowLayout{}, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	rect := Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)}
	placements := visibleWindowPlacements(window, rect)
	out := make([]protocol.PanePlacement, 0, len(placements))
	for _, placement := range placements {
		slot := uint8(0)
		for _, binding := range client.RenderBindings {
			if binding.PaneID == placement.PaneID {
				slot = uint8(binding.Slot)
				break
			}
		}
		out = append(out, protocol.PanePlacement{
			PaneID: placement.PaneID,
			Slot:   slot,
			Rect: protocol.Rect{
				X:      placement.Rect.X,
				Y:      placement.Rect.Y,
				Width:  placement.Rect.Width,
				Height: placement.Rect.Height,
			},
		})
	}
	return protocol.WindowLayout{
		WindowID:       window.ID,
		FocusedPaneID:  client.FocusedPaneID,
		LayoutRevision: window.LayoutRevision,
		Panes:          out,
	}, nil
}

func (s *Session) VisiblePlacements(clientID uint64) ([]PanePlacement, *Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	placements := clonePlacements(visibleWindowPlacements(window, Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)}))
	return placements, cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) BindingForPane(clientID, paneID uint64) (RenderBinding, bool) {
	client := s.Clients[clientID]
	if client == nil {
		return RenderBinding{}, false
	}
	for _, binding := range client.RenderBindings {
		if binding.PaneID == paneID {
			return binding, true
		}
	}
	return RenderBinding{}, false
}

func (s *Session) RenderBindings(clientID uint64) ([]RenderBinding, *ClientState) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	bindings := append([]RenderBinding(nil), client.RenderBindings...)
	return bindings, cloneClientState(client)
}

func (s *Session) RebuildRenderBindings(clientID uint64) ([]RenderBinding, *Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	s.rebuildBindingsLocked(client, window)
	return append([]RenderBinding(nil), client.RenderBindings...), cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) rebuildBindingsLocked(client *ClientState, window *Window) {
	placements := visibleWindowPlacements(window, Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)})
	sort.Slice(placements, func(i, j int) bool {
		if placements[i].Rect.Y != placements[j].Rect.Y {
			return placements[i].Rect.Y < placements[j].Rect.Y
		}
		if placements[i].Rect.X == placements[j].Rect.X {
			return placements[i].PaneID < placements[j].PaneID
		}
		return placements[i].Rect.X < placements[j].Rect.X
	})
	bindings := make([]RenderBinding, 0, len(placements))
	for slot, placement := range placements {
		bindings = append(bindings, RenderBinding{
			Slot:   slot,
			PaneID: placement.PaneID,
		})
	}
	client.RenderBindings = bindings
}

func (s *Session) UpdatePaneTitle(paneID uint64, title string) *Window {
	for _, window := range s.Windows {
		if window.AutomaticName && windowHasPane(window, paneID) {
			window.Name = title
			return cloneWindow(window)
		}
	}
	return nil
}

func (s *Session) IsFocusedPane(clientID, paneID uint64) bool {
	client := s.Clients[clientID]
	return client != nil && client.FocusedPaneID == paneID
}

func (s *Session) SnapshotClient(clientID uint64) *ClientState {
	return cloneClientState(s.Clients[clientID])
}

func (s *Session) ResizeAll(cols, rows uint16) {
	type resizeTarget struct {
		pane *Pane
		rect Rect
	}
	var targets []resizeTarget
	for _, client := range s.Clients {
		client.TerminalCols = cols
		client.TerminalRows = rows
		if window := s.Windows[client.ActiveWindowID]; window != nil {
			s.rebuildBindingsLocked(client, window)
		}
	}
	for _, window := range s.Windows {
		window.LayoutRevision = s.nextLayoutRevisionLocked()
		placements := window.Layout.Compute(Rect{Width: int(cols), Height: int(rows)})
		byPane := make(map[uint64]Rect, len(placements))
		for _, placement := range placements {
			byPane[placement.PaneID] = placement.Rect
		}
		if window.Zoomed && windowHasPane(window, window.ZoomedPaneID) {
			byPane[window.ZoomedPaneID] = Rect{Width: int(cols), Height: int(rows)}
		}
		for paneID, rect := range byPane {
			if pane := s.Panes[paneID]; pane != nil {
				targets = append(targets, resizeTarget{pane: pane, rect: rect})
			}
		}
	}
	for _, target := range targets {
		_ = target.pane.resize(uint16(target.rect.Width), uint16(target.rect.Height))
	}
}

func (s *Session) windowIDsLocked() []uint64 {
	ids := make([]uint64, 0, len(s.Windows))
	for id := range s.Windows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := s.Windows[ids[i]], s.Windows[ids[j]]
		if left.DisplayIndex != right.DisplayIndex {
			return left.DisplayIndex < right.DisplayIndex
		}
		return ids[i] < ids[j]
	})
	return ids
}

func paneIDsFromLayout(layout LayoutNode) []uint64 {
	if layout == nil {
		return nil
	}
	return layout.PaneIDs()
}

func replacePaneWithSplit(layout LayoutNode, targetPaneID, newPaneID uint64, direction SplitDirection) (LayoutNode, bool) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID != targetPaneID {
			return layout, false
		}
		return &SplitLayout{
			Direction: direction,
			Ratio:     500,
			First:     node,
			Second:    &PaneLayout{PaneID: newPaneID},
		}, true
	case *SplitLayout:
		if updated, ok := replacePaneWithSplit(node.First, targetPaneID, newPaneID, direction); ok {
			node.First = updated
			return node, true
		}
		if updated, ok := replacePaneWithSplit(node.Second, targetPaneID, newPaneID, direction); ok {
			node.Second = updated
			return node, true
		}
	}
	return layout, false
}

func swapLayoutPanes(layout LayoutNode, firstPaneID, secondPaneID uint64) bool {
	first := paneLayoutByID(layout, firstPaneID)
	second := paneLayoutByID(layout, secondPaneID)
	if first == nil || second == nil || first == second {
		return false
	}
	first.PaneID, second.PaneID = second.PaneID, first.PaneID
	return true
}

func paneLayoutByID(layout LayoutNode, paneID uint64) *PaneLayout {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID == paneID {
			return node
		}
	case *SplitLayout:
		if pane := paneLayoutByID(node.First, paneID); pane != nil {
			return pane
		}
		return paneLayoutByID(node.Second, paneID)
	}
	return nil
}

func removePaneFromLayout(layout LayoutNode, targetPaneID uint64) (LayoutNode, uint64, bool) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID == targetPaneID {
			return nil, 0, true
		}
	case *SplitLayout:
		if updated, focusedPaneID, removed := removePaneFromLayout(node.First, targetPaneID); removed {
			if updated == nil {
				return node.Second, firstPaneID(node.Second), true
			}
			node.First = updated
			return node, focusedPaneID, true
		}
		if updated, focusedPaneID, removed := removePaneFromLayout(node.Second, targetPaneID); removed {
			if updated == nil {
				return node.First, firstPaneID(node.First), true
			}
			node.Second = updated
			return node, focusedPaneID, true
		}
	}
	return layout, 0, false
}

func firstPaneID(layout LayoutNode) uint64 {
	if layout == nil {
		return 0
	}
	ids := layout.PaneIDs()
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

func containsPane(ids []uint64, paneID uint64) bool {
	for _, id := range ids {
		if id == paneID {
			return true
		}
	}
	return false
}

func windowHasPane(window *Window, paneID uint64) bool {
	if window == nil {
		return false
	}
	return containsPane(window.Layout.PaneIDs(), paneID)
}

func windowPrimaryPaneID(window *Window) uint64 {
	if window == nil {
		return 0
	}
	ids := window.Layout.PaneIDs()
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

func clonePlacements(in []PanePlacement) []PanePlacement {
	out := make([]PanePlacement, len(in))
	copy(out, in)
	return out
}

func cloneWindow(window *Window) *Window {
	if window == nil {
		return nil
	}
	return &Window{
		ID:               window.ID,
		DisplayIndex:     window.DisplayIndex,
		Name:             window.Name,
		AutomaticName:    window.AutomaticName,
		ActivePaneID:     window.ActivePaneID,
		Zoomed:           window.Zoomed,
		ZoomedPaneID:     window.ZoomedPaneID,
		Layout:           window.Layout,
		LayoutRevision:   window.LayoutRevision,
		layoutCycleIndex: window.layoutCycleIndex,
	}
}

func visibleWindowPlacements(window *Window, rect Rect) []PanePlacement {
	if window == nil || window.Layout == nil {
		return nil
	}
	if window.Zoomed && windowHasPane(window, window.ZoomedPaneID) {
		return []PanePlacement{{PaneID: window.ZoomedPaneID, Rect: rect}}
	}
	return window.Layout.Compute(rect)
}

type processSaveProjection struct {
	Cwd     string
	Command string
}

type processSaveCandidate struct {
	Projection processSaveProjection
	Samples    int
}

const processSaveStableSamples = 2

func (s *Session) watchPaneProcesses(pane *Pane) {
	if pane == nil || s.processMonitor == nil {
		return
	}
	anchor := Anchor{
		Key:         PaneKey{SessionID: s.ID, PaneID: pane.ID},
		Root:        pane.Root,
		PTY:         pane.PTY,
		RootIsShell: len(pane.Launch.RequestedArgv) == 0,
	}
	pane.processMonitor = s.processMonitor
	pane.processKey = anchor.Key
	s.processMonitor.Watch(s, anchor)
}

func (s *Session) unwatchPaneProcesses(paneID uint64) {
	if s.processMonitor != nil {
		s.processMonitor.Unwatch(PaneKey{SessionID: s.ID, PaneID: paneID})
	}
	delete(s.processSaveCandidates, paneID)
}

// applyMonitoredProcessObservations runs on the Session actor. Monitor results
// are advisory and may race pane removal, so every anchor is revalidated
// against authoritative pane state before it can affect names or persistence.
func (s *Session) applyMonitoredProcessObservations(batch monitoredProcessBatch) error {
	if s.processSaveCandidates == nil {
		s.processSaveCandidates = make(map[uint64]processSaveCandidate)
	}
	latest := map[uint64]processSaveProjection{}
	if s.sessionPersistence != nil {
		latest = plannedProcessLeaves(s.sessionPersistence.Plan.Windows)
	}
	nameChanged := false
	for _, update := range batch {
		if update.anchor.Key.SessionID != s.ID {
			continue
		}
		paneID := update.anchor.Key.PaneID
		pane := s.Panes[paneID]
		if pane == nil || pane.Root != update.anchor.Root || pane.PTY != update.anchor.PTY {
			continue
		}
		projection, valid := observedProcessSaveProjection(pane, update.observation, latest[paneID])
		if valid {
			candidate := s.processSaveCandidates[paneID]
			if candidate.Projection == projection {
				candidate.Samples++
			} else {
				candidate = processSaveCandidate{Projection: projection, Samples: 1}
			}
			s.processSaveCandidates[paneID] = candidate
			requiredSamples := processSaveStableSamples
			if update.observation.Status == StatusShellOwned {
				requiredSamples = 1
			}
			if candidate.Samples >= requiredSamples && latest[paneID] != projection {
				s.persistObservedPaneForPersistence(paneID, projection)
				latest[paneID] = projection
			}
		}
		for _, window := range s.Windows {
			if window == nil || !window.AutomaticName || windowActivePaneID(window) != paneID {
				continue
			}
			name := observationWindowName(update.observation)
			if name != "" && name != window.Name {
				window.Name = name
				s.markWindowChangedForPersistence(window.ID)
				nameChanged = true
			}
			break
		}
	}
	if nameChanged {
		return s.publishStatusBar()
	}
	return nil
}

func observedProcessSaveProjection(pane *Pane, observation ProcessObservation, previous processSaveProjection) (processSaveProjection, bool) {
	if pane == nil {
		return processSaveProjection{}, false
	}
	projection := processSaveProjection{Cwd: pane.Launch.Cwd}
	if observation.Root != nil && observation.Root.Cwd != "" {
		projection.Cwd = observation.Root.Cwd
	}
	if len(pane.Launch.RequestedArgv) > 0 {
		projection.Command = shellJoin(pane.Launch.RequestedArgv)
		return projection, true
	}
	switch observation.Status {
	case StatusShellOwned:
		projection.Command = previous.Command
		return projection, true
	case StatusDetected:
		if observation.Candidate == nil {
			return processSaveProjection{}, false
		}
		if isTransientObservedCommand(observation.Candidate) {
			projection.Command = previous.Command
			return projection, true
		}
		projection.Command = observedProcessCommand(observation.Candidate)
		return projection, projection.Command != ""
	default:
		return processSaveProjection{}, false
	}
}

func observedProcessCommand(process *ObservedProcess) string {
	if process == nil {
		return ""
	}
	if len(process.Argv) > 0 {
		return shellJoin(process.Argv)
	}
	return process.Name
}

func isTransientObservedCommand(process *ObservedProcess) bool {
	if process == nil {
		return false
	}
	name := process.Name

	switch name {
	case "ls", "clear", "meja":
		return true
	default:
		return false
	}
}

func windowActivePaneID(window *Window) uint64 {
	if window == nil {
		return 0
	}
	if windowHasPane(window, window.ActivePaneID) {
		return window.ActivePaneID
	}
	return windowPrimaryPaneID(window)
}

func observationWindowName(observation ProcessObservation) string {
	var observed *ObservedProcess
	switch observation.Status {
	case StatusDetected:
		observed = observation.Candidate
		if isTransientObservedCommand(observed) {
			return ""
		}
	case StatusShellOwned:
		observed = observation.Root
	default:
		return ""
	}
	if observed == nil {
		return ""
	}
	name := observed.Name
	if name == "" && observed.Exe != "" {
		name = filepath.Base(strings.TrimSuffix(observed.Exe, " (deleted)"))
	}
	if name == "" && len(observed.Argv) > 0 {
		name = filepath.Base(observed.Argv[0])
	}
	return cleanProcessName(name)
}

func cleanProcessName(name string) string {
	runes := make([]rune, 0, len(name))
	for _, current := range strings.ToValidUTF8(name, "�") {
		if unicode.IsControl(current) {
			continue
		}
		runes = append(runes, current)
		if len(runes) == 64 {
			break
		}
	}
	return strings.TrimSpace(string(runes))
}

func clonePromptState(prompt *PromptState) *PromptState {
	if prompt == nil {
		return nil
	}
	out := *prompt
	out.Text = append([]rune(nil), prompt.Text...)
	out.pendingUTF8 = append([]byte(nil), prompt.pendingUTF8...)
	out.PendingEscape = append([]byte(nil), prompt.PendingEscape...)
	return &out
}

func cloneClientState(c *ClientState) *ClientState {
	if c == nil {
		return nil
	}
	out := *c
	out.RenderBindings = append([]RenderBinding(nil), c.RenderBindings...)
	out.PrefixEscape = append([]byte(nil), c.PrefixEscape...)
	out.Prompt = clonePromptState(c.Prompt)
	return &out
}
