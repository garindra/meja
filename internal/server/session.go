package server

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"tali/internal/protocol"
)

type sessionState struct {
	session        *Session
	sessionID      uint64
	attachMu       sync.Mutex
	attachToken    []byte
	attachExpires  time.Time
	attachConsumed bool
	resumeTokens   map[string]uint64
	generation     uint64
	activeConn     quic.Connection
	mgmtFrames     chan protocol.Frame
	outputStreams  map[int]io.Writer
	operations     chan sessionOperation
}

type sessionOperation struct {
	run  func() error
	done chan error
}

func (s *sessionState) coordinate(run func() error) error {
	if s.operations == nil {
		return run()
	}
	done := make(chan error, 1)
	s.operations <- sessionOperation{run: run, done: done}
	return <-done
}

func (s *sessionState) runOperations() {
	for operation := range s.operations {
		operation.done <- operation.run()
	}
}

func (s *sessionState) attachConnection(mgmtFrames chan protocol.Frame, outputStreams map[int]io.Writer) {
	s.attachMu.Lock()
	s.mgmtFrames = mgmtFrames
	s.outputStreams = outputStreams
	s.attachMu.Unlock()
}

func (s *sessionState) detachConnection(mgmtFrames chan protocol.Frame) {
	s.attachMu.Lock()
	if s.mgmtFrames == mgmtFrames {
		s.mgmtFrames = nil
		s.outputStreams = nil
	}
	s.attachMu.Unlock()
}

func (s *sessionState) currentManagementFrames() chan protocol.Frame {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	return s.mgmtFrames
}

func (s *sessionState) currentOutputStream(slot int) io.Writer {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	return s.outputStreams[slot]
}

type Session struct {
	ID      uint64
	Windows map[uint64]*Window
	Panes   map[uint64]*Pane
	Clients map[uint64]*ClientState

	NextWindowID       uint64
	NextWindowIndex    int
	NextPaneID         uint64
	NextLayoutRevision uint64

	mu sync.RWMutex
}

type Window struct {
	ID             uint64
	DisplayIndex   int
	Name           string
	Layout         LayoutNode
	LayoutRevision uint64
}

type PromptKind uint8

const (
	PromptKindRenameWindow PromptKind = iota + 1
)

type PromptAction uint8

const (
	PromptActionNone PromptAction = iota
	PromptActionChanged
	PromptActionSubmit
	PromptActionCancel
)

type PromptState struct {
	Kind           PromptKind
	Action         PromptAction
	TargetWindowID uint64
	Label          string
	Text           []rune
	Cursor         int
	pendingUTF8    []byte
	PendingEscape  []byte
}

type RenderBinding struct {
	Slot   int
	PaneID uint64
}

type ClientState struct {
	ID             uint64
	SessionID      uint64
	ActiveWindowID uint64
	FocusedPaneID  uint64
	TerminalCols   uint16
	TerminalRows   uint16

	RenderBindings []RenderBinding
	HistoryViews   map[uint64]*HistoryView
	InputState     serverInputState
	Prompt         *PromptState
	LastWindowID   uint64
	HasLastWindow  bool
}

func NewSession(id uint64) *Session {
	return &Session{
		ID:           id,
		Windows:      map[uint64]*Window{},
		Panes:        map[uint64]*Pane{},
		Clients:      map[uint64]*ClientState{},
		NextWindowID: 1,
	}
}

func (s *Session) nextLayoutRevisionLocked() uint64 {
	s.NextLayoutRevision++
	return s.NextLayoutRevision
}

func (s *Session) NewClient(id uint64) *ClientState {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := &ClientState{ID: id, SessionID: s.ID, HistoryViews: map[uint64]*HistoryView{}}
	s.Clients[id] = client
	return client
}

func (s *Session) EnsureClient(id uint64) *ClientState {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.ensureClientLocked(id)
	return cloneClientState(client)
}

func (s *Session) ensureClientLocked(id uint64) *ClientState {
	if client := s.Clients[id]; client != nil {
		if client.HistoryViews == nil {
			client.HistoryViews = map[uint64]*HistoryView{}
		}
		s.ensureClientFocusLocked(client)
		return client
	}
	client := &ClientState{ID: id, SessionID: s.ID, HistoryViews: map[uint64]*HistoryView{}}
	s.ensureClientFocusLocked(client)
	s.Clients[id] = client
	return client
}

func (s *Session) BeginPrompt(clientID uint64, kind PromptKind, label, initial string) (*PromptState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	client.Prompt = &PromptState{
		Kind:           kind,
		TargetWindowID: window.ID,
		Label:          label,
		Text:           text,
		Cursor:         len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) BeginRenameWindowPrompt(clientID uint64) (*PromptState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	client.Prompt = &PromptState{
		Kind:           PromptKindRenameWindow,
		TargetWindowID: window.ID,
		Label:          "(rename-window) ",
		Text:           text,
		Cursor:         len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) ActivePrompt(clientID uint64) *PromptState {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		ids := s.windowIDsLocked()
		window = s.Windows[ids[0]]
		client.ActiveWindowID = window.ID
	}
	if !windowHasPane(window, client.FocusedPaneID) {
		client.FocusedPaneID = windowPrimaryPaneID(window)
	}
}

func (s *Session) SetClientSize(clientID uint64, cols, rows uint16) *ClientState {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.NextWindowID == 0 {
		s.NextWindowID = 1
	}
	windowID := s.NextWindowID
	s.NextWindowID++
	displayIndex := s.NextWindowIndex
	s.NextWindowIndex++
	window := &Window{
		ID:             windowID,
		DisplayIndex:   displayIndex,
		Name:           pane.Title,
		Layout:         &PaneLayout{PaneID: pane.ID},
		LayoutRevision: s.nextLayoutRevisionLocked(),
	}
	s.Windows[windowID] = window
	s.Panes[pane.ID] = pane
	client := s.ensureClientLocked(activateFor)
	if client.ActiveWindowID != window.ID {
		client.LastWindowID = client.ActiveWindowID
		client.HasLastWindow = client.LastWindowID != 0
	}
	client.ActiveWindowID = windowID
	client.FocusedPaneID = pane.ID
	s.rebuildBindingsLocked(client, window)
	return window, cloneClientState(client)
}

func (s *Session) HasWindows() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Windows) > 0
}

func (s *Session) PanesSnapshot() []*Pane {
	s.mu.RLock()
	defer s.mu.RUnlock()
	panes := make([]*Pane, 0, len(s.Panes))
	for _, pane := range s.Panes {
		panes = append(panes, pane)
	}
	return panes
}

func (s *Session) Pane(id uint64) *Pane {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Panes[id]
}

func (s *Session) ReattachClient(clientID uint64) (*Window, *Pane, *ClientState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.NextPaneID
	s.NextPaneID++
	return id
}

func (s *Session) ActivePane(clientID uint64) (*Pane, *ClientState) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	return s.Panes[client.FocusedPaneID], cloneClientState(client)
}

func (s *Session) ActiveWindow(clientID uint64) (*Window, *ClientState) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	window := s.Windows[client.ActiveWindowID]
	return cloneWindow(window), cloneClientState(client)
}

func (s *Session) ResolveInputTarget(clientID, requestedPaneID uint64) (*Pane, *ClientState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if client.ActiveWindowID != window.ID {
		client.LastWindowID = client.ActiveWindowID
		client.HasLastWindow = client.LastWindowID != 0
	}
	client.ActiveWindowID = window.ID
	client.FocusedPaneID = windowPrimaryPaneID(window)
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	s.rebuildBindingsLocked(client, window)
}

func (s *Session) RenameWindow(windowID uint64, name string) (*Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	window := s.Windows[windowID]
	if window == nil {
		return nil, fmt.Errorf("unknown window %d", windowID)
	}
	// Empty names are valid; normal status projection remains well-formed.
	window.Name = name
	return cloneWindow(window), nil
}

func (s *Session) FocusPane(clientID, paneID uint64) (*Window, *ClientState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	client.FocusedPaneID = paneID
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) SplitFocusedPane(clientID uint64, pane *Pane, direction SplitDirection) (*Window, *ClientState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	updated, replaced := replacePaneWithSplit(window.Layout, client.FocusedPaneID, pane.ID, direction)
	if !replaced {
		return nil, nil, fmt.Errorf("focused pane %d not found in layout", client.FocusedPaneID)
	}
	s.Panes[pane.ID] = pane
	window.Layout = updated
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	client.FocusedPaneID = pane.ID
	s.rebuildBindingsLocked(client, window)
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) CanSplitFocusedPane(clientID uint64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Clients[clientID]
	if c == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("unknown client %d", clientID)
	}
	window = s.Windows[c.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("unknown window %d", c.ActiveWindowID)
	}
	paneIDs := window.Layout.PaneIDs()
	for _, client := range s.Clients {
		delete(client.HistoryViews, c.FocusedPaneID)
	}
	if len(paneIDs) <= 1 {
		closedPane = s.Panes[c.FocusedPaneID]
		delete(s.Panes, c.FocusedPaneID)
		delete(s.Windows, window.ID)
		windowClosed = true
		closedWindowID = window.ID
		if len(s.Windows) == 0 {
			return closedPane, nil, cloneClientState(c), true, closedWindowID, true, nil
		}
		ids := s.windowIDsLocked()
		nextWindow := s.Windows[ids[0]]
		c.ActiveWindowID = nextWindow.ID
		c.FocusedPaneID = windowPrimaryPaneID(nextWindow)
		s.rebuildBindingsLocked(c, nextWindow)
		return closedPane, cloneWindow(nextWindow), cloneClientState(c), true, closedWindowID, false, nil
	}
	closedPane = s.Panes[c.FocusedPaneID]
	updated, nextFocusedPaneID, removed := removePaneFromLayout(window.Layout, c.FocusedPaneID)
	if !removed || updated == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("focused pane %d not found in layout", c.FocusedPaneID)
	}
	delete(s.Panes, c.FocusedPaneID)
	window.Layout = updated
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	c.FocusedPaneID = nextFocusedPaneID
	s.rebuildBindingsLocked(c, window)
	return closedPane, cloneWindow(window), cloneClientState(c), false, 0, false, nil
}

// RemovePane applies process exit to authoritative session state. It is a no-op
// when an explicit close already removed the pane before Process.Wait returned.
func (s *Session) RemovePane(paneID, clientID uint64) (window *Window, client *ClientState, finalPane, removed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	for _, state := range s.Clients {
		delete(state.HistoryViews, paneID)
	}
	delete(s.Panes, paneID)
	if len(owner.Layout.PaneIDs()) > 1 {
		updated, nextFocusedPaneID, ok := removePaneFromLayout(owner.Layout, paneID)
		if !ok || updated == nil {
			return nil, nil, false, false, fmt.Errorf("pane %d not found in window %d layout", paneID, owner.ID)
		}
		owner.Layout = updated
		owner.LayoutRevision = s.nextLayoutRevisionLocked()
		if c.ActiveWindowID == owner.ID && c.FocusedPaneID == paneID {
			c.FocusedPaneID = nextFocusedPaneID
		}
	} else {
		delete(s.Windows, owner.ID)
		if len(s.Windows) == 0 {
			return nil, cloneClientState(c), true, true, nil
		}
		if c.ActiveWindowID == owner.ID {
			ids := s.windowIDsLocked()
			c.ActiveWindowID = ids[0]
			c.FocusedPaneID = windowPrimaryPaneID(s.Windows[ids[0]])
		}
	}
	active := s.Windows[c.ActiveWindowID]
	if active == nil {
		return nil, nil, false, false, fmt.Errorf("client %d has no active window after removing pane %d", clientID, paneID)
	}
	s.rebuildBindingsLocked(c, active)
	return cloneWindow(active), cloneClientState(c), false, true, nil
}

func (s *Session) CloseWindow(clientID, windowID uint64) (closed uint64, closedPanes []*Pane, replacement *Window, pane *Pane, client *ClientState, autoCreated bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	closedPanes = make([]*Pane, 0, len(paneIDs))
	for _, paneID := range paneIDs {
		for _, client := range s.Clients {
			delete(client.HistoryViews, paneID)
		}
		if p := s.Panes[paneID]; p != nil {
			closedPanes = append(closedPanes, p)
		}
		delete(s.Panes, paneID)
	}
	delete(s.Windows, windowID)
	closed = windowID

	if len(s.Windows) == 0 {
		return closed, closedPanes, nil, nil, cloneClientState(c), true, nil
	}
	ids := s.windowIDsLocked()
	nextWindow := s.Windows[ids[0]]
	c.ActiveWindowID = nextWindow.ID
	c.FocusedPaneID = windowPrimaryPaneID(nextWindow)
	s.rebuildBindingsLocked(c, nextWindow)
	pane = s.Panes[c.FocusedPaneID]
	return closed, closedPanes, cloneWindow(nextWindow), pane, cloneClientState(c), false, nil
}

type WindowStatus struct {
	WindowID uint64
	Index    int
	Title    string
	Active   bool
}

func (s *Session) WindowStatuses(clientID uint64) []WindowStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	return list
}

func (s *Session) WindowLayout(clientID uint64) (protocol.WindowLayout, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	if client == nil {
		return protocol.WindowLayout{}, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return protocol.WindowLayout{}, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	rect := Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)}
	placements := window.Layout.Compute(rect)
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	placements := clonePlacements(window.Layout.Compute(Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)}))
	return placements, cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) BindingForPane(clientID, paneID uint64) (RenderBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	bindings := append([]RenderBinding(nil), client.RenderBindings...)
	return bindings, cloneClientState(client)
}

func (s *Session) RebuildRenderBindings(clientID uint64) ([]RenderBinding, *Window, *ClientState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	placements := window.Layout.Compute(Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)})
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
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, window := range s.Windows {
		if windowHasPane(window, paneID) {
			window.Name = title
			return cloneWindow(window)
		}
	}
	return nil
}

func (s *Session) IsFocusedPane(clientID, paneID uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	return client != nil && client.FocusedPaneID == paneID
}

func (s *Session) SnapshotClient(clientID uint64) *ClientState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneClientState(s.Clients[clientID])
}

func (s *Session) ResizeAll(cols, rows uint16) {
	s.mu.Lock()
	type resizeTarget struct {
		pane *Pane
		rect Rect
	}
	var targets []resizeTarget
	for _, client := range s.Clients {
		client.TerminalCols = cols
		client.TerminalRows = rows
	}
	for _, window := range s.Windows {
		window.LayoutRevision = s.nextLayoutRevisionLocked()
		placements := window.Layout.Compute(Rect{Width: int(cols), Height: int(rows)})
		for _, placement := range placements {
			pane := s.Panes[placement.PaneID]
			if pane == nil {
				continue
			}
			targets = append(targets, resizeTarget{pane: pane, rect: placement.Rect})
		}
	}
	s.mu.Unlock()
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
		ID:             window.ID,
		DisplayIndex:   window.DisplayIndex,
		Name:           window.Name,
		Layout:         window.Layout,
		LayoutRevision: window.LayoutRevision,
	}
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
	out.Prompt = clonePromptState(c.Prompt)
	return &out
}
