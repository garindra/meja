package server

import (
	"fmt"
	"sort"
	"sync"

	"tali/internal/protocol"
)

type Session struct {
	ID      uint64
	Windows map[uint64]*Window
	Panes   map[uint64]*Pane
	Clients map[uint64]*ClientState

	NextWindowID uint64
	NextPaneID   uint64

	mu sync.RWMutex
}

type Window struct {
	ID             uint64
	Name           string
	Layout         LayoutNode
	LayoutRevision uint64
}

type RenderBinding struct {
	Slot              int
	PaneID            uint64
	BindingGeneration uint64
}

type ClientState struct {
	ID             uint64
	SessionID      uint64
	ActiveWindowID uint64
	FocusedPaneID  uint64
	TerminalCols   uint16
	TerminalRows   uint16

	NextBindingGeneration uint64
	RenderBindings        []RenderBinding
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

func (s *Session) NewClient(id uint64) *ClientState {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := &ClientState{ID: id, SessionID: s.ID}
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
		s.ensureClientFocusLocked(client)
		return client
	}
	client := &ClientState{ID: id, SessionID: s.ID}
	s.ensureClientFocusLocked(client)
	s.Clients[id] = client
	return client
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
	client.TerminalCols = cols
	client.TerminalRows = rows
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
	window := &Window{
		ID:             windowID,
		Name:           pane.Title,
		Layout:         &PaneLayout{PaneID: pane.ID},
		LayoutRevision: 1,
	}
	s.Windows[windowID] = window
	s.Panes[pane.ID] = pane
	client := s.ensureClientLocked(activateFor)
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
		client.ActiveWindowID = window.ID
		client.FocusedPaneID = windowPrimaryPaneID(window)
	}
	if !windowHasPane(window, client.FocusedPaneID) {
		client.FocusedPaneID = windowPrimaryPaneID(window)
	}
	pane := s.Panes[client.FocusedPaneID]
	s.rebuildBindingsLocked(client, window)
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
	client.ActiveWindowID = windowID
	client.FocusedPaneID = windowPrimaryPaneID(window)
	s.rebuildBindingsLocked(client, window)
	return cloneWindow(window), cloneClientState(client), nil
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

func (s *Session) SplitFocusedPane(clientID uint64, pane *Pane) (*Window, *ClientState, error) {
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
	if _, ok := window.Layout.(*SplitLayout); ok {
		return nil, nil, fmt.Errorf("window %d already split", window.ID)
	}
	if !windowHasPane(window, client.FocusedPaneID) {
		return nil, nil, fmt.Errorf("focused pane %d not in window %d", client.FocusedPaneID, window.ID)
	}
	s.Panes[pane.ID] = pane
	window.Layout = &SplitLayout{
		Direction: SplitVertical,
		Ratio:     500,
		First:     window.Layout,
		Second:    &PaneLayout{PaneID: pane.ID},
	}
	window.LayoutRevision++
	client.FocusedPaneID = pane.ID
	s.rebuildBindingsLocked(client, window)
	return cloneWindow(window), cloneClientState(client), nil
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
	split, ok := window.Layout.(*SplitLayout)
	if !ok {
		return nil, nil, nil, false, 0, false, fmt.Errorf("unsupported layout for close")
	}
	closedPane = s.Panes[c.FocusedPaneID]
	delete(s.Panes, c.FocusedPaneID)
	if left := paneIDsFromLayout(split.First); containsPane(left, c.FocusedPaneID) {
		window.Layout = split.Second
	} else {
		window.Layout = split.First
	}
	window.LayoutRevision++
	c.FocusedPaneID = windowPrimaryPaneID(window)
	s.rebuildBindingsLocked(c, window)
	return closedPane, cloneWindow(window), cloneClientState(c), false, 0, false, nil
}

func (s *Session) CloseWindow(clientID, windowID uint64) (closed uint64, closedPane *Pane, replacement *Window, pane *Pane, client *ClientState, autoCreated bool, err error) {
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
	closedPane = s.Panes[paneIDs[0]]
	for _, paneID := range paneIDs {
		if p := s.Panes[paneID]; p != nil && closedPane == nil {
			closedPane = p
		}
		delete(s.Panes, paneID)
	}
	delete(s.Windows, windowID)
	closed = windowID

	if len(s.Windows) == 0 {
		return closed, closedPane, nil, nil, cloneClientState(c), true, nil
	}
	ids := s.windowIDsLocked()
	nextWindow := s.Windows[ids[0]]
	c.ActiveWindowID = nextWindow.ID
	c.FocusedPaneID = windowPrimaryPaneID(nextWindow)
	s.rebuildBindingsLocked(c, nextWindow)
	pane = s.Panes[c.FocusedPaneID]
	return closed, closedPane, cloneWindow(nextWindow), pane, cloneClientState(c), false, nil
}

func (s *Session) WindowList(clientID uint64) protocol.WindowList {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	active := uint64(0)
	if client != nil {
		active = client.ActiveWindowID
	}
	ids := s.windowIDsLocked()
	list := make([]protocol.WindowInfo, 0, len(ids))
	for idx, id := range ids {
		window := s.Windows[id]
		list = append(list, protocol.WindowInfo{
			WindowID: window.ID,
			PaneID:   windowPrimaryPaneID(window),
			Index:    idx,
			Title:    window.Name,
			Active:   window.ID == active,
		})
	}
	return protocol.WindowList{Windows: list, ActiveWindowID: active}
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
		out = append(out, protocol.PanePlacement{
			PaneID: placement.PaneID,
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
		if placements[i].Rect.X == placements[j].Rect.X {
			return placements[i].PaneID < placements[j].PaneID
		}
		return placements[i].Rect.X < placements[j].Rect.X
	})
	bindings := make([]RenderBinding, 0, len(placements))
	for slot, placement := range placements {
		client.NextBindingGeneration++
		bindings = append(bindings, RenderBinding{
			Slot:              slot,
			PaneID:            placement.PaneID,
			BindingGeneration: client.NextBindingGeneration,
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
	defer s.mu.Unlock()
	for _, client := range s.Clients {
		client.TerminalCols = cols
		client.TerminalRows = rows
	}
	for _, window := range s.Windows {
		window.LayoutRevision++
		placements := window.Layout.Compute(Rect{Width: int(cols), Height: int(rows)})
		for _, placement := range placements {
			pane := s.Panes[placement.PaneID]
			if pane == nil {
				continue
			}
			_ = pane.Resize(uint16(placement.Rect.Width), uint16(placement.Rect.Height))
			pane.Terminal.Resize(placement.Rect.Width, placement.Rect.Height)
		}
	}
}

func (s *Session) windowIDsLocked() []uint64 {
	ids := make([]uint64, 0, len(s.Windows))
	for id := range s.Windows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func paneIDsFromLayout(layout LayoutNode) []uint64 {
	if layout == nil {
		return nil
	}
	return layout.PaneIDs()
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
		Name:           window.Name,
		Layout:         window.Layout,
		LayoutRevision: window.LayoutRevision,
	}
}

func cloneClientState(c *ClientState) *ClientState {
	if c == nil {
		return nil
	}
	out := *c
	out.RenderBindings = append([]RenderBinding(nil), c.RenderBindings...)
	return &out
}
