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
	ID     uint64
	Name   string
	PaneID uint64
}

type ClientState struct {
	ID                uint64
	SessionID         uint64
	ActiveWindowID    uint64
	FocusedPaneID     uint64
	BindingGeneration uint64
}

func NewSession(id uint64) *Session {
	return &Session{
		ID:      id,
		Windows: map[uint64]*Window{},
		Panes:   map[uint64]*Pane{},
		Clients: map[uint64]*ClientState{},
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
	if client := s.Clients[id]; client != nil {
		if client.ActiveWindowID == 0 && client.FocusedPaneID == 0 && len(s.Windows) > 0 {
			ids := s.windowIDsLocked()
			window := s.Windows[ids[0]]
			client.ActiveWindowID = window.ID
			client.FocusedPaneID = window.PaneID
		}
		return cloneClientState(client)
	}
	client := &ClientState{ID: id, SessionID: s.ID}
	if len(s.Windows) > 0 {
		ids := s.windowIDsLocked()
		window := s.Windows[ids[0]]
		client.ActiveWindowID = window.ID
		client.FocusedPaneID = window.PaneID
	}
	s.Clients[id] = client
	return cloneClientState(client)
}

func (s *Session) CreateWindow(pane *Pane, activateFor uint64) (*Window, *ClientState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	windowID := s.NextWindowID
	s.NextWindowID++
	window := &Window{
		ID:     windowID,
		Name:   pane.Title,
		PaneID: pane.ID,
	}
	s.Windows[windowID] = window
	s.Panes[pane.ID] = pane
	client := s.Clients[activateFor]
	if client != nil {
		client.ActiveWindowID = windowID
		client.FocusedPaneID = pane.ID
		client.BindingGeneration++
	}
	return window, client
}

func (s *Session) HasWindows() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.Windows) > 0
}

func (s *Session) ReattachClient(clientID uint64) (*Window, *Pane, *ClientState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil {
		client = &ClientState{ID: clientID, SessionID: s.ID}
		s.Clients[clientID] = client
	}
	if len(s.Windows) == 0 {
		return nil, nil, nil, fmt.Errorf("session has no windows")
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		ids := s.windowIDsLocked()
		window = s.Windows[ids[0]]
		client.ActiveWindowID = window.ID
		client.FocusedPaneID = window.PaneID
	}
	if client.FocusedPaneID == 0 {
		client.FocusedPaneID = window.PaneID
	}
	pane := s.Panes[client.FocusedPaneID]
	if pane == nil {
		client.FocusedPaneID = window.PaneID
		pane = s.Panes[window.PaneID]
	}
	client.BindingGeneration++
	return window, pane, cloneClientState(client), nil
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

func (s *Session) SelectWindow(clientID, windowID uint64) (*Window, *Pane, *ClientState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[windowID]
	if window == nil {
		return nil, nil, nil, fmt.Errorf("unknown window %d", windowID)
	}
	client.ActiveWindowID = windowID
	client.FocusedPaneID = window.PaneID
	client.BindingGeneration++
	return window, s.Panes[window.PaneID], cloneClientState(client), nil
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
	closedPane = s.Panes[w.PaneID]
	delete(s.Panes, w.PaneID)
	delete(s.Windows, windowID)
	closed = windowID

	if len(s.Windows) == 0 {
		return closed, closedPane, nil, nil, cloneClientState(c), true, nil
	}
	ids := s.windowIDsLocked()
	nextID := ids[0]
	c.ActiveWindowID = nextID
	c.FocusedPaneID = s.Windows[nextID].PaneID
	c.BindingGeneration++
	return closed, closedPane, s.Windows[nextID], s.Panes[s.Windows[nextID].PaneID], cloneClientState(c), false, nil
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
			PaneID:   window.PaneID,
			Index:    idx,
			Title:    window.Name,
			Active:   window.ID == active,
		})
	}
	return protocol.WindowList{Windows: list, ActiveWindowID: active}
}

func (s *Session) UpdatePaneTitle(paneID uint64, title string) *Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, window := range s.Windows {
		if window.PaneID == paneID {
			window.Name = title
			return &Window{ID: window.ID, Name: window.Name, PaneID: window.PaneID}
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, pane := range s.Panes {
		_ = pane.Resize(cols, rows)
		pane.Terminal.Resize(int(cols), int(rows))
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

func cloneClientState(c *ClientState) *ClientState {
	if c == nil {
		return nil
	}
	out := *c
	return &out
}
