package server

import (
	"errors"
	"fmt"
	"sort"
)

// GroupState is the daemon-owned execution graph shared by one or more
// externally visible sessions. A singleton session has a GroupState too; this
// makes the ownership rules identical before and after grouping.
type GroupState struct {
	ID         uint64
	SessionIDs map[uint64]struct{}
	Windows    map[uint64]*Window
	Panes      map[uint64]*Pane
	memberIDs  []uint64
}

// WindowLink is a session-local display link to a canonical Window. The
// Window object itself is shared by every session in the group.
type WindowLink struct {
	WindowID     uint64
	DisplayIndex int
}

// SessionWindowView contains state that must not leak between mirrors.
type SessionWindowView struct {
	FocusedPaneID uint64
	ZoomedPaneID  uint64
	focusHistory  [8]uint64
	focusDepth    uint8
}

func (v *SessionWindowView) focusPane(paneID uint64) {
	if v == nil || v.FocusedPaneID == paneID {
		return
	}
	depth := 0
	for index := 0; index < int(v.focusDepth); index++ {
		if previous := v.focusHistory[index]; previous != paneID {
			v.focusHistory[depth] = previous
			depth++
		}
	}
	if depth == len(v.focusHistory) {
		copy(v.focusHistory[:], v.focusHistory[1:])
		depth--
	}
	v.focusHistory[depth] = v.FocusedPaneID
	depth++
	for index := depth; index < len(v.focusHistory); index++ {
		v.focusHistory[index] = 0
	}
	v.focusDepth = uint8(depth)
	v.FocusedPaneID = paneID
}

func (v *SessionWindowView) removePane(window *Window, paneID, layoutFallback uint64) uint64 {
	if v == nil {
		return layoutFallback
	}
	depth := 0
	for index := 0; index < int(v.focusDepth); index++ {
		if previous := v.focusHistory[index]; previous != paneID && windowHasPane(window, previous) {
			v.focusHistory[depth] = previous
			depth++
		}
	}
	for index := depth; index < len(v.focusHistory); index++ {
		v.focusHistory[index] = 0
	}
	v.focusDepth = uint8(depth)
	if v.FocusedPaneID != paneID && windowHasPane(window, v.FocusedPaneID) {
		return v.FocusedPaneID
	}
	if depth > 0 {
		depth--
		v.FocusedPaneID = v.focusHistory[depth]
		v.focusHistory[depth] = 0
		v.focusDepth = uint8(depth)
		return v.FocusedPaneID
	}
	v.FocusedPaneID = layoutFallback
	return layoutFallback
}

// WindowViewLease is the single live viewer of a canonical window.
type WindowViewLease struct {
	WindowID   uint64
	SessionID  uint64
	ClientID   ClientID
	Generation uint64
}

func newGroup(id uint64) *GroupState {
	return &GroupState{ID: id, SessionIDs: make(map[uint64]struct{}), Windows: make(map[uint64]*Window), Panes: make(map[uint64]*Pane)}
}

func (g *GroupState) publishMembers() {
	ids := make([]uint64, 0, len(g.SessionIDs))
	for id := range g.SessionIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	g.memberIDs = ids
}

func (g *GroupState) memberIDsSnapshot() []uint64 {
	if g == nil {
		return nil
	}
	return append([]uint64(nil), g.memberIDs...)
}

func (g *GroupState) addSession(s *SessionState) {
	if g == nil || s == nil {
		return
	}
	g.SessionIDs[s.ID] = struct{}{}
	g.publishMembers()
	s.GroupID = g.ID
	s.group = g
	for id, window := range g.Windows {
		s.Windows[id] = window
		s.Links = append(s.Links, WindowLink{WindowID: id, DisplayIndex: window.DisplayIndex})
		if _, ok := s.WindowViews[id]; !ok {
			s.WindowViews[id] = SessionWindowView{FocusedPaneID: window.ActivePaneID}
		}
	}
	for id, pane := range g.Panes {
		s.Panes[id] = pane
	}
}

func (g *GroupState) addWindow(window *Window, pane *Pane) error {
	if g == nil || window == nil {
		return errSessionUnavailable
	}
	if window.GroupID != 0 && window.GroupID != g.ID {
		return fmt.Errorf("window %d already belongs to group %d", window.ID, window.GroupID)
	}
	if existing := g.Windows[window.ID]; existing != nil && existing != window {
		return fmt.Errorf("window %d already belongs to another window state", window.ID)
	}
	if pane != nil {
		if pane.WindowID != 0 && pane.WindowID != window.ID {
			return fmt.Errorf("pane %d already belongs to window %d", pane.ID, pane.WindowID)
		}
		if existing := g.Panes[pane.ID]; existing != nil && existing != pane {
			return fmt.Errorf("pane %d already belongs to another pane state", pane.ID)
		}
		pane.WindowID = window.ID
	}
	window.GroupID = g.ID
	g.Windows[window.ID] = window
	if pane != nil {
		g.Panes[pane.ID] = pane
	}
	return nil
}

func (s *SessionState) groupWindowViewNow(windowID uint64) SessionWindowView {
	view, ok := s.WindowViews[windowID]
	if !ok {
		window := s.Windows[windowID]
		if window != nil {
			view = SessionWindowView{FocusedPaneID: window.ActivePaneID}
		}
		s.WindowViews[windowID] = view
	}
	return view
}

func (s *SessionState) setGroupWindowViewNow(windowID uint64, view SessionWindowView) {
	if s.WindowViews == nil {
		s.WindowViews = make(map[uint64]SessionWindowView)
	}
	s.WindowViews[windowID] = view
}

func (s *SessionState) clearGroupWindowZoomNow(window *Window) {
	if window == nil {
		return
	}
	view := s.groupWindowViewNow(window.ID)
	view.ZoomedPaneID = 0
	s.setGroupWindowViewNow(window.ID, view)
	window.clearZoom()
}

func visibleWindowPlacementsForSession(s *SessionState, window *Window, rect Rect) []PanePlacement {
	if s == nil || window == nil {
		return nil
	}
	view := s.groupWindowViewNow(window.ID)
	zoomedPaneID := view.ZoomedPaneID
	if !s.isGrouped() && window.Zoomed {
		zoomedPaneID = window.ZoomedPaneID
	}
	if (zoomedPaneID != 0 || (!s.isGrouped() && window.Zoomed)) && windowHasPane(window, zoomedPaneID) {
		return []PanePlacement{{PaneID: zoomedPaneID, Rect: rect}}
	}
	return window.Layout.Compute(rect)
}

func (s *SessionState) syncGroupLinksNow() {
	if s.group == nil {
		return
	}
	for memberID := range s.group.SessionIDs {
		member := s.daemon.sessions[memberID]
		if member == nil {
			continue
		}
		member.Windows = make(map[uint64]*Window, len(s.group.Windows))
		member.Links = member.Links[:0]
		member.Panes = make(map[uint64]*Pane, len(s.group.Panes))
		for id, window := range s.group.Windows {
			member.Windows[id] = window
			member.Links = append(member.Links, WindowLink{WindowID: id, DisplayIndex: window.DisplayIndex})
		}
		for id, pane := range s.group.Panes {
			member.Panes[id] = pane
		}
	}
}

func (s *SessionState) groupMembersNow() []*SessionState {
	if s == nil || s.group == nil {
		return []*SessionState{s}
	}
	if s.daemon == nil {
		return []*SessionState{s}
	}
	members := make([]*SessionState, 0, len(s.group.SessionIDs))
	for id := range s.group.SessionIDs {
		if member := s.daemon.sessions[id]; member != nil {
			members = append(members, member)
		}
	}
	return members
}

func (s *SessionState) isGrouped() bool {
	return s != nil && s.grouped
}

// removeGroupWindowNow runs inside the daemon transaction above. It repairs
// every session link and view before returning the panes that need one-time
// process termination.
func (s *SessionState) removeGroupWindowNow(windowID uint64) ([]*Pane, bool) {
	if s == nil || s.group == nil {
		return nil, false
	}
	window := s.group.Windows[windowID]
	if window == nil {
		return nil, false
	}
	delete(s.daemon.windowLeases, windowID)
	var panes []*Pane
	for _, paneID := range window.Layout.PaneIDs() {
		if pane := s.group.Panes[paneID]; pane != nil {
			panes = append(panes, pane)
			delete(s.group.Panes, paneID)
			delete(s.daemon.panes, paneID)
		}
	}
	delete(s.group.Windows, windowID)
	delete(s.daemon.windows, windowID)
	members := s.groupMembersNow()
	for _, member := range members {
		delete(member.Windows, windowID)
		delete(member.WindowViews, windowID)
		filtered := member.Links[:0]
		for _, link := range member.Links {
			if link.WindowID != windowID {
				filtered = append(filtered, link)
			}
		}
		member.Links = filtered
		for _, pane := range panes {
			delete(member.Panes, pane.ID)
		}
		if member.ActiveWindowID == windowID || member.Windows[member.ActiveWindowID] == nil {
			previousWindowID := member.PreviousWindowID
			member.ActiveWindowID = 0
			member.PreviousWindowID = windowID
			ids := member.orderedWindowIDs()
			if len(ids) > 0 {
				replacement := (*Window)(nil)
				attached := member.attachedClient()
				viewable := func(candidateID uint64) bool {
					if attached == nil {
						return true
					}
					lease := member.daemon.windowLeases[candidateID]
					return lease == nil || lease.ClientID == attached.ID
				}
				// PreviousWindowID has priority over display order. It is
				// considered only when it survived the destroyed window.
				if previousWindowID != 0 && previousWindowID != windowID {
					if candidate := member.Windows[previousWindowID]; candidate != nil && viewable(candidate.ID) {
						replacement = candidate
					}
				}
				if replacement == nil {
					for _, candidateID := range ids {
						candidate := member.Windows[candidateID]
						if viewable(candidateID) {
							replacement = candidate
							break
						}
					}
				}
				if replacement == nil {
					continue
				}
				member.ActiveWindowID = replacement.ID
				if attached != nil {
					if member.daemon.windowLeases == nil {
						member.daemon.windowLeases = make(map[uint64]*WindowViewLease)
					}
					generation := uint64(1)
					if previous := member.daemon.windowLeases[replacement.ID]; previous != nil {
						generation = previous.Generation + 1
					}
					member.daemon.windowLeases[replacement.ID] = &WindowViewLease{WindowID: replacement.ID, SessionID: member.ID, ClientID: attached.ID, Generation: generation}
				}
			}
		}
		if member.PreviousWindowID == windowID {
			member.PreviousWindowID = 0
		}
	}
	return panes, true
}

func (s *SessionState) removeGroupPaneNow(paneID uint64) (*Pane, *Window, bool, error) {
	if s == nil || s.group == nil {
		return nil, nil, false, nil
	}
	pane := s.group.Panes[paneID]
	if pane == nil {
		return nil, nil, false, nil
	}
	var owner *Window
	for _, candidate := range s.group.Windows {
		if windowHasPane(candidate, paneID) {
			owner = candidate
			break
		}
	}
	if owner == nil {
		return nil, nil, false, nil
	}
	delete(s.group.Panes, paneID)
	delete(s.daemon.panes, paneID)
	for _, member := range s.groupMembersNow() {
		delete(member.Panes, paneID)
	}
	if len(owner.Layout.PaneIDs()) <= 1 {
		_, ok := s.removeGroupWindowNow(owner.ID)
		return pane, owner, ok, nil
	}
	updated, nextFocused, ok := removePaneFromLayout(owner.Layout, paneID)
	if !ok || updated == nil {
		return nil, nil, false, fmt.Errorf("pane %d not found in window %d layout", paneID, owner.ID)
	}
	owner.Layout = updated
	owner.ActivePaneID = nextFocused
	owner.LayoutRevision++
	if len(updated.PaneIDs()) <= 1 || owner.ZoomedPaneID == paneID {
		owner.clearZoom()
	}
	for _, member := range s.groupMembersNow() {
		view := member.groupWindowViewNow(owner.ID)
		if len(updated.PaneIDs()) <= 1 || view.ZoomedPaneID == paneID {
			view.ZoomedPaneID = 0
		}
		view.removePane(owner, paneID, nextFocused)
		member.setGroupWindowViewNow(owner.ID, view)
	}
	return pane, owner, true, nil
}

// ensureSessionGroupInActor creates or publishes a session's canonical group.
// Production callers already hold the daemon actor turn; test fixtures may
// call it synchronously while constructing otherwise unreachable state.
func (d *Daemon) ensureSessionGroupInActor(s *SessionState) *GroupState {
	if s == nil {
		return nil
	}
	if s.group != nil {
		if d != nil {
			if d.groups == nil {
				d.groups = make(map[uint64]*GroupState)
			}
			d.groups[s.group.ID] = s.group
		}
		return s.group
	}
	id := s.ID
	if d != nil {
		if d.nextGroupID == 0 {
			d.nextGroupID = 1
		}
		id = d.nextGroupID
		d.nextGroupID++
	}
	g := newGroup(id)
	g.addSession(s)
	s.grouped = false
	if d != nil {
		if d.groups == nil {
			d.groups = make(map[uint64]*GroupState)
		}
		d.groups[id] = g
	}
	return g
}

func (d *Daemon) addWindowToGroupNow(session *SessionState, window *Window, pane *Pane) error {
	var err error
	if d.panes == nil {
		d.panes = make(map[uint64]*Pane)
	}
	if d.windows == nil {
		d.windows = make(map[uint64]*Window)
	}
	if session == nil || session.group == nil {
		err = errSessionUnavailable
		return err
	}
	g := session.group
	if err = g.addWindow(window, pane); err != nil {
		return err
	}
	d.panes[pane.ID] = pane
	d.windows[window.ID] = window
	for memberID := range g.SessionIDs {
		member := d.sessions[memberID]
		if member == nil && memberID == session.ID {
			member = session
		}
		if member == nil {
			continue
		}
		member.Windows[window.ID] = window
		member.Panes[pane.ID] = pane
		member.Links = append(member.Links, WindowLink{WindowID: window.ID, DisplayIndex: window.DisplayIndex})
		member.WindowViews[window.ID] = SessionWindowView{FocusedPaneID: pane.ID}
	}
	return err
}

func (d *Daemon) addPaneToWindowGroupNow(session *SessionState, window *Window, pane *Pane, layout LayoutNode) error {
	var err error
	if d.panes == nil {
		d.panes = make(map[uint64]*Pane)
	}
	if d.windows == nil {
		d.windows = make(map[uint64]*Window)
	}
	if session == nil || session.group == nil || window == nil || pane == nil {
		err = errSessionUnavailable
		return err
	}
	if window.GroupID != session.group.ID {
		err = fmt.Errorf("window %d does not belong to session group", window.ID)
		return err
	}
	if pane.WindowID != 0 && pane.WindowID != window.ID {
		err = fmt.Errorf("pane %d already belongs to window %d", pane.ID, pane.WindowID)
		return err
	}
	if existing := session.group.Panes[pane.ID]; existing != nil && existing != pane {
		err = fmt.Errorf("pane %d already belongs to another pane state", pane.ID)
		return err
	}
	pane.WindowID = window.ID
	d.panes[pane.ID] = pane
	d.windows[window.ID] = window
	window.Layout = layout
	window.LayoutRevision = session.nextWindowLayoutRevisionNow()
	window.ActivePaneID = pane.ID
	session.group.Panes[pane.ID] = pane
	for memberID := range session.group.SessionIDs {
		member := d.sessions[memberID]
		if member == nil && memberID == session.ID {
			member = session
		}
		if member != nil {
			member.Panes[pane.ID] = pane
		}
	}
	return err
}

func (d *Daemon) renameSessionWindow(sessionID, windowID uint64, name string) (*Window, error) {
	var err error
	var snapshot *Window
	var refresh []clientStatusDelivery
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			err = errSessionUnavailable
			return
		}
		window := state.Windows[windowID]
		if window == nil {
			err = fmt.Errorf("unknown window %d", windowID)
			return
		}
		changed := window.Name != name || window.AutomaticName
		window.Name = name
		window.AutomaticName = false
		if changed {
			state.markWindowChangedForPersistence(windowID)
			if state.isGrouped() {
				for _, member := range state.groupMembersNow() {
					if member != state {
						if client := member.attachedClient(); client != nil {
							refresh = append(refresh, clientStatusDelivery{
								Connection: client.State.Active,
								Status:     d.clientStatusSnapshotNow(client, member),
							})
						}
					}
				}
			}
		}
		snapshot = cloneWindow(window)
	})
	for _, delivery := range refresh {
		postClientCommand(delivery.Connection, clientInstanceCommand{
			RefreshStatus: true, Status: delivery.Status, HasStatus: true,
		})
	}
	return snapshot, err
}

func (d *Daemon) groupSessionInActor(base, mirror *SessionState) error {
	if d.sessions[base.ID] != base || d.sessions[mirror.ID] != mirror {
		return errSessionUnavailable
	}
	d.ensureSessionGroupInActor(base)
	return groupSessionsNow(base, mirror)
}

func groupSessionsNow(base, mirror *SessionState) error {
	if base.group == nil {
		if base.daemon != nil {
			base.daemon.ensureSessionGroupInActor(base)
		} else {
			g := newGroup(base.ID)
			g.addSession(base)
		}
	}
	g := base.group
	if mirror.group != nil && mirror.group != g {
		if len(mirror.group.SessionIDs) > 1 {
			return errors.New("a session cannot join more than one group")
		}
		oldGroup := mirror.group
		delete(mirror.group.SessionIDs, mirror.ID)
		oldGroup.publishMembers()
		if base.daemon != nil && len(oldGroup.SessionIDs) == 0 {
			delete(base.daemon.groups, oldGroup.ID)
		}
		mirror.group = nil
		mirror.GroupID = 0
		mirror.Windows = make(map[uint64]*Window)
		mirror.Panes = make(map[uint64]*Pane)
		mirror.Links = nil
		mirror.WindowViews = make(map[uint64]SessionWindowView)
	}
	g.addSession(mirror)
	if base.daemon != nil {
		for memberID := range g.SessionIDs {
			if member := base.daemon.sessions[memberID]; member != nil {
				member.grouped = true
			}
		}
	}
	if mirror.rootDir == "" {
		mirror.rootDir = base.rootDir
	}
	if base.ActiveWindowID != 0 {
		mirror.ActiveWindowID = base.ActiveWindowID
		mirror.PreviousWindowID = base.PreviousWindowID
	}
	for id, view := range base.WindowViews {
		if _, ok := mirror.WindowViews[id]; !ok {
			mirror.WindowViews[id] = view
		}
	}
	if d := base.daemon; d != nil {
		if d.groups == nil {
			d.groups = make(map[uint64]*GroupState)
		}
		d.groups[g.ID] = g
		for memberID := range g.SessionIDs {
			if member := d.sessions[memberID]; member != nil {
				member.syncGroupLinksNow()
			}
		}
	}
	return nil
}

// activateCreatedWindowNow completes new-window activation in the same daemon
// transaction as graph insertion. It is called only while the request actor
// is already running; transport/output work remains with the client actor.
func (d *Daemon) activateCreatedWindowNow(state *SessionState, windowID uint64) error {
	if d == nil || state == nil {
		return errSessionUnavailable
	}
	client := d.clients[state.ClientID]
	if client == nil {
		return nil
	}
	window := state.Windows[windowID]
	if window == nil {
		return fmt.Errorf("unknown new window %d", windowID)
	}
	if current := d.windowLeases[windowID]; current != nil && current.ClientID != client.ID {
		return fmt.Errorf("window %d is currently viewed by another client", window.DisplayIndex)
	}
	oldWindowID := d.windowForClientNow(client.ID)
	if oldWindowID != 0 && oldWindowID != windowID {
		old := d.windowLeases[oldWindowID]
		if old == nil || old.ClientID != client.ID {
			return errors.New("stale client window lease")
		}
	}
	generation := uint64(1)
	if current := d.windowLeases[windowID]; current != nil {
		generation = current.Generation + 1
	}
	// Acquire before release. If validation fails, the old lease is untouched.
	d.windowLeases[windowID] = &WindowViewLease{WindowID: windowID, SessionID: state.ID, ClientID: client.ID, Generation: generation}
	if oldWindowID != 0 && oldWindowID != windowID {
		delete(d.windowLeases, oldWindowID)
	}
	return nil
}

// selectWindow is the atomic view transition. Target acquisition happens
// before releasing the old lease, so every rejected selection leaves the
// logical view and its current lease untouched.
func (d *Daemon) selectWindow(clientID ClientID, sessionID, windowID uint64) (ViewTransition, error) {
	var transition ViewTransition
	var err error
	d.call(func() {
		state := d.sessions[sessionID]
		client := d.clients[state.ClientID]
		if state == nil || client == nil {
			err = errSessionUnavailable
			return
		}
		target := state.Windows[windowID]
		if target == nil {
			err = fmt.Errorf("unknown window %d", windowID)
			return
		}
		if client.ID != clientID {
			err = errors.New("stale client")
			return
		}
		current := d.windowLeases[windowID]
		if clientID != 0 && current != nil && current.ClientID != clientID {
			owner := d.sessions[current.SessionID]
			name := "unknown"
			if owner != nil {
				name = owner.Name
			}
			err = fmt.Errorf("window %d is currently viewed by session %q", target.DisplayIndex, name)
			return
		}
		oldWindowID := state.ActiveWindowID
		if oldWindowID == 0 {
			ids := state.orderedWindowIDs()
			if len(ids) > 0 {
				oldWindowID = ids[0]
			}
		}
		oldLeaseWindowID := uint64(0)
		if clientID != 0 {
			oldLeaseWindowID = d.windowForClientNow(clientID)
			if oldLeaseWindowID == 0 {
				oldLeaseWindowID = oldWindowID
			}
			if oldLeaseWindowID != 0 {
				old := d.windowLeases[oldLeaseWindowID]
				if old == nil || old.ClientID != clientID {
					err = errors.New("stale client window lease")
					return
				}
			}
		}
		if _, prepareErr := prepareClientWindowGeometryNow(client, state, windowID); prepareErr != nil {
			err = prepareErr
			return
		}
		generation := uint64(1)
		if current != nil {
			generation = current.Generation + 1
		}
		if oldWindowID == windowID {
			if current != nil {
				generation = current.Generation
			}
		}
		// Acquire the target before releasing the source. No client fields or
		// source state are changed before every validation above succeeds.
		if clientID != 0 {
			d.windowLeases[windowID] = &WindowViewLease{WindowID: windowID, SessionID: sessionID, ClientID: clientID, Generation: generation}
			if oldLeaseWindowID != 0 && oldLeaseWindowID != windowID {
				delete(d.windowLeases, oldLeaseWindowID)
			}
		}
		if oldWindowID != windowID {
			state.PreviousWindowID = oldWindowID
		}
		state.ActiveWindowID = windowID
		view := state.groupWindowViewNow(windowID)
		if view.FocusedPaneID == 0 || !windowHasPane(target, view.FocusedPaneID) {
			view.FocusedPaneID = target.ActivePaneID
		}
		state.setGroupWindowViewNow(windowID, view)
		state.markSessionChangedForPersistence()
		transition = d.prepareViewTransitionNow(viewTransitionSelectWindow, client, state)
	})
	return transition, err
}

func clientViewportSize(client *ClientIdentity, fallback *Window) (uint16, uint16) {
	if client == nil {
		if fallback == nil {
			return 0, 0
		}
		return fallback.Cols, fallback.Rows
	}
	cols, rows := client.terminalCols, client.terminalRows
	if (cols == 0 || rows == 0) && fallback != nil {
		cols, rows = fallback.Cols, fallback.Rows
	}
	return cols, rows
}

// windowSelectionTarget resolves window-navigation commands from the same
// daemon-owned session view that selectWindow mutates. currentView.Layout is only
// the installed projection of that view and can legitimately lag the
// transaction.
func (d *Daemon) windowSelectionTarget(sessionID uint64, delta int, last bool) (uint64, bool) {
	var target uint64
	var ok bool
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil || len(state.Windows) == 0 {
			return
		}
		if last {
			target = state.PreviousWindowID
			ok = target != 0 && target != state.ActiveWindowID && state.Windows[target] != nil
			return
		}
		ids := state.orderedWindowIDs()
		for index, id := range ids {
			if id == state.ActiveWindowID {
				target = ids[(index+delta+len(ids))%len(ids)]
				ok = true
				return
			}
		}
		target, ok = ids[0], true
	})
	return target, ok
}

func (d *Daemon) releaseWindowView(clientID ClientID, windowID, generation uint64) bool {
	released := false
	d.call(func() {
		lease := d.windowLeases[windowID]
		if lease != nil && lease.ClientID == clientID && lease.Generation == generation {
			delete(d.windowLeases, windowID)
			released = true
		}
	})
	return released
}

// mutateClientView serializes a session-view graph mutation and captures the
// immutable projection which the ClientInstance actor installs afterward.
// ClientInstance.currentView.Layout never crosses into the daemon transaction.
func (d *Daemon) mutateClientView(reason ViewTransitionReason, clientID ClientID, mutate func(*SessionState) (*Window, bool, error)) (*Window, ViewTransition, bool, error) {
	var window *Window
	var transition ViewTransition
	var changed bool
	var err error
	if d == nil || clientID == 0 {
		return nil, transition, false, errSessionUnavailable
	}
	d.call(func() {
		client := d.clients[clientID]
		if client == nil {
			err = errSessionUnavailable
			return
		}
		state := d.sessions[client.SessionID]
		if state == nil || d.clients[state.ClientID] != client {
			err = errSessionUnavailable
			return
		}
		window, changed, err = mutate(state)
		if err == nil {
			transition = d.prepareViewTransitionNow(reason, client, state)
		}
	})
	return window, transition, changed, err
}

func (d *Daemon) focusClientPane(clientID ClientID, paneID uint64) (*Window, ViewTransition, error) {
	var window *Window
	var transition ViewTransition
	var err error
	d.call(func() {
		client := d.clients[clientID]
		if client == nil {
			err = errSessionUnavailable
			return
		}
		state := d.sessions[client.SessionID]
		if state == nil || d.clients[state.ClientID] != client {
			err = errSessionUnavailable
			return
		}
		activeWindow := state.Windows[state.ActiveWindowID]
		var previousWindowRevision WindowLayoutRevision
		if activeWindow != nil {
			previousWindowRevision = activeWindow.LayoutRevision
		}
		window, err = state.focusPaneNow(paneID)
		if err == nil {
			if activeWindow != nil && activeWindow.LayoutRevision != previousWindowRevision {
				transition = d.prepareViewTransitionNow(viewTransitionLayout, client, state)
			} else {
				transition = d.prepareFocusTransitionNow(client, state)
			}
		}
	})
	return window, transition, err
}

func (d *Daemon) toggleClientZoom(clientID ClientID) (*Window, ViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, clientID, func(state *SessionState) (*Window, bool, error) {
		window, changed, err := state.toggleZoomNow()
		return window, changed, err
	})
}

func (d *Daemon) splitClientPane(clientID ClientID, pane *Pane, direction SplitDirection) (*Window, ViewTransition, error) {
	if pane == nil {
		return nil, ViewTransition{}, errors.New("split pane is unavailable")
	}
	window, transition, _, err := d.mutateClientView(viewTransitionSplitPane, clientID, func(state *SessionState) (*Window, bool, error) {
		window, err := state.splitFocusedPaneNow(pane, direction)
		return window, err == nil, err
	})
	return window, transition, err
}

func (d *Daemon) cycleWindowLayout(clientID ClientID) (*Window, ViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, clientID, func(state *SessionState) (*Window, bool, error) {
		return state.cycleWindowLayoutNow()
	})
}

func (d *Daemon) resizeClientPane(clientID ClientID, direction PaneResizeDirection, amount int) (*Window, ViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, clientID, func(state *SessionState) (*Window, bool, error) {
		return state.resizeFocusedPaneNow(direction, amount)
	})
}

func (d *Daemon) swapClientPane(clientID ClientID, direction PaneSwapDirection) (*Window, ViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, clientID, func(state *SessionState) (*Window, bool, error) {
		return state.swapFocusedPaneNow(direction)
	})
}

func (d *Daemon) createClientWindow(clientID ClientID, pane *Pane, cols, rows uint16) (*Window, ViewTransition, error) {
	var window *Window
	var transition ViewTransition
	var err error
	if d == nil || clientID == 0 {
		return nil, transition, errSessionUnavailable
	}
	d.call(func() {
		client := d.clients[clientID]
		if client == nil {
			err = errSessionUnavailable
			return
		}
		state := d.sessions[client.SessionID]
		if state == nil || (d.clients[state.ClientID] != nil && d.clients[state.ClientID] != client) {
			err = errSessionUnavailable
			return
		}
		var canonical *Window
		canonical, err = state.createWindowNow(pane, cols, rows)
		if err != nil {
			return
		}
		if err = d.activateCreatedWindowNow(state, canonical.ID); err != nil {
			return
		}
		if _, prepareErr := prepareClientWindowGeometryNow(client, state, state.ActiveWindowID); prepareErr != nil {
			err = prepareErr
			return
		}
		transition = d.prepareViewTransitionNow(viewTransitionCreateWindow, client, state)
		window = cloneWindow(canonical)
	})
	return window, transition, err
}

func (d *Daemon) startClientWindow(clientID ClientID, cwd string, argv []string, cols, rows uint16, shell string) (*Window, ViewTransition, error) {
	if d == nil || clientID == 0 {
		return nil, ViewTransition{}, errSessionUnavailable
	}
	var sessionID uint64
	var request paneRequest
	d.call(func() {
		if client := d.clients[clientID]; client != nil {
			if state := d.sessions[client.SessionID]; state != nil {
				sessionID = state.ID
				request = state.contextualPaneRequest(paneRequest{Cwd: cwd, Command: argv, Cols: cols, Rows: rows, Shell: shell})
			}
		}
	})
	if sessionID == 0 {
		return nil, ViewTransition{}, errSessionUnavailable
	}
	paneID, err := d.allocatePaneID()
	if err != nil {
		return nil, ViewTransition{}, err
	}
	pane, err := startPaneProcess(paneID, request)
	if err != nil {
		return nil, ViewTransition{}, fmt.Errorf("start pane: %w", err)
	}
	window, transition, err := d.createClientWindow(clientID, pane, cols, rows)
	if err != nil {
		_ = terminatePane(pane)
		return nil, ViewTransition{}, err
	}
	d.startPane(sessionID, pane)
	return window, transition, nil
}

func (d *Daemon) startSessionWindowID(sessionID uint64, cwd string, argv []string, cols, rows uint16, shell string) (*Pane, *Window, error) {
	if d == nil || sessionID == 0 {
		return nil, nil, errSessionUnavailable
	}
	var request paneRequest
	d.call(func() {
		if state := d.sessions[sessionID]; state != nil {
			request = state.contextualPaneRequest(paneRequest{Cwd: cwd, Command: argv, Cols: cols, Rows: rows, Shell: shell})
		}
	})
	if request.Cwd == "" {
		return nil, nil, errSessionUnavailable
	}
	paneID, err := d.allocatePaneID()
	if err != nil {
		return nil, nil, err
	}
	pane, err := startPaneProcess(paneID, request)
	if err != nil {
		return nil, nil, fmt.Errorf("start pane: %w", err)
	}
	var window *Window
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			err = errSessionUnavailable
			return
		}
		var created *Window
		created, err = state.createWindowNow(pane, cols, rows)
		if err == nil {
			err = d.activateCreatedWindowNow(state, created.ID)
			window = cloneWindow(created)
		}
	})
	if err != nil {
		_ = terminatePane(pane)
		return nil, nil, err
	}
	d.startPane(sessionID, pane)
	return pane, window, nil
}

func (d *Daemon) startClientSplit(clientID ClientID, cwd string, cols, rows uint16, shell string, direction SplitDirection) (ViewTransition, error) {
	if d == nil || clientID == 0 {
		return ViewTransition{}, errSessionUnavailable
	}
	var sessionID uint64
	var request paneRequest
	d.call(func() {
		if client := d.clients[clientID]; client != nil {
			if state := d.sessions[client.SessionID]; state != nil {
				sessionID = state.ID
				request = state.contextualPaneRequest(paneRequest{Cwd: cwd, Cols: cols, Rows: rows, Shell: shell})
			}
		}
	})
	if sessionID == 0 {
		return ViewTransition{}, errSessionUnavailable
	}
	paneID, err := d.allocatePaneID()
	if err != nil {
		return ViewTransition{}, err
	}
	pane, err := startPaneProcess(paneID, request)
	if err != nil {
		return ViewTransition{}, fmt.Errorf("start split pane: %w", err)
	}
	_, transition, err := d.splitClientPane(clientID, pane, direction)
	if err != nil {
		_ = terminatePane(pane)
		return ViewTransition{}, err
	}
	d.startPane(sessionID, pane)
	return transition, nil
}

func (d *Daemon) startSessionSplitID(sessionID uint64, cwd string, shell string, direction SplitDirection) error {
	if d == nil || sessionID == 0 {
		return errSessionUnavailable
	}
	var request paneRequest
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			return
		}
		active := state.activePane()
		if active == nil {
			return
		}
		cols, rows := active.TerminalSize()
		request = state.contextualPaneRequest(paneRequest{Cwd: cwd, Cols: uint16(cols), Rows: uint16(rows), Shell: shell})
	})
	if request.Cwd == "" {
		return errors.New("split-window requires an active pane")
	}
	paneID, err := d.allocatePaneID()
	if err != nil {
		return err
	}
	pane, err := startPaneProcess(paneID, request)
	if err != nil {
		return fmt.Errorf("start split pane: %w", err)
	}
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			err = errSessionUnavailable
			return
		}
		_, err = state.splitFocusedPaneNow(pane, direction)
	})
	if err != nil {
		_ = terminatePane(pane)
		return err
	}
	d.startPane(sessionID, pane)
	return nil
}

type commandSessionClientSnapshot struct {
	SessionID  uint64
	RootDir    string
	WindowCols uint16
	WindowRows uint16
	ActivePane *Pane
	CanSplit   error
	Client     *commandClientSnapshot
}

func (d *Daemon) commandSessionAndClient(clientID ClientID, sessionID uint64) *commandSessionClientSnapshot {
	var snapshot *commandSessionClientSnapshot
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			return
		}
		snapshot = &commandSessionClientSnapshot{SessionID: state.ID, RootDir: state.rootDir}
		if window := state.Windows[state.ActiveWindowID]; window != nil {
			snapshot.WindowCols, snapshot.WindowRows = window.Cols, window.Rows
			paneID := state.groupWindowViewNow(window.ID).FocusedPaneID
			if paneID == 0 {
				paneID = window.ActivePaneID
			}
			snapshot.ActivePane = state.Panes[paneID]
		}
		snapshot.CanSplit = state.CanSplitFocusedPane()
		candidate := d.clients[state.ClientID]
		if candidate != nil && (clientID == 0 || candidate.ID == clientID) {
			snapshot.Client = commandClientSnapshotNow(candidate)
		}
	})
	return snapshot
}

func (d *Daemon) createCommandWindow(clientID ClientID, sessionID uint64, cols, rows uint16) (*ViewTransition, error) {
	state := d.commandSessionAndClient(clientID, sessionID)
	if state == nil {
		return nil, errSessionUnavailable
	}
	client := state.Client
	if client != nil {
		clientCols, clientRows := client.TerminalCols, client.TerminalRows
		if clientCols == 0 || clientRows == 0 {
			clientCols, clientRows = state.WindowCols, state.WindowRows
		}
		window, transition, err := d.startClientWindow(client.ID, state.RootDir, nil, clientCols, clientRows, client.Shell)
		if err == nil && window == nil {
			err = errors.New("create window: daemon rejected graph insertion")
		}
		return &transition, err
	}
	if cols == 0 || rows == 0 {
		cols, rows = state.WindowCols, state.WindowRows
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 23
	}
	_, _, err := d.startSessionWindowID(state.SessionID, state.RootDir, nil, cols, rows, defaultShell())
	return nil, err
}

func (d *Daemon) splitCommandWindow(clientID ClientID, sessionID uint64, direction SplitDirection) (*ViewTransition, error) {
	state := d.commandSessionAndClient(clientID, sessionID)
	if state == nil {
		return nil, errSessionUnavailable
	}
	client := state.Client
	if client != nil {
		if state.ActivePane == nil {
			return nil, nil
		}
		if state.CanSplit != nil {
			return nil, state.CanSplit
		}
		cols, rows := state.ActivePane.TerminalSize()
		transition, err := d.startClientSplit(client.ID, state.RootDir, uint16(cols), uint16(rows), client.Shell, direction)
		return &transition, err
	}
	return nil, d.startSessionSplitID(state.SessionID, state.RootDir, defaultShell(), direction)
}

type clientPaneRemoval struct {
	Pane           *Pane
	Panes          []*Pane
	Window         *Window
	Transition     ViewTransition
	WindowClosed   bool
	ClosedWindowID uint64
	FinalPane      bool
	Removed        bool
}

func (d *Daemon) removeClientPane(clientID ClientID, paneID uint64) (clientPaneRemoval, error) {
	var result clientPaneRemoval
	var err error
	if d == nil || clientID == 0 {
		return result, errSessionUnavailable
	}
	d.call(func() {
		client := d.clients[clientID]
		if client == nil {
			err = errSessionUnavailable
			return
		}
		state := d.sessions[client.SessionID]
		if state == nil || d.clients[state.ClientID] != client {
			err = errSessionUnavailable
			return
		}
		pane, owner, removed, removeErr := state.removeGroupPaneNow(paneID)
		if removeErr != nil {
			err = removeErr
			return
		}
		result.Pane, result.Removed = pane, removed
		if !removed {
			return
		}
		result.WindowClosed = owner != nil && state.Windows[owner.ID] == nil
		if result.WindowClosed {
			result.ClosedWindowID = owner.ID
		}
		result.FinalPane = state.ActiveWindowID == 0 || len(state.Windows) == 0
		if result.FinalPane {
			result.Transition = ViewTransition{Reason: viewTransitionClosePane, Projection: ClientProjectionPlan{ClientID: client.ID, SessionID: state.ID, Close: true, CloseReason: "no viewable fallback window"}}
			d.reserveViewTransitionDeliveryNow(client, &result.Transition)
			return
		}
		result.Window = cloneWindow(state.Windows[state.ActiveWindowID])
		if _, err = prepareClientWindowGeometryNow(client, state, state.ActiveWindowID); err != nil {
			return
		}
		result.Transition = d.prepareViewTransitionNow(viewTransitionClosePane, client, state, pane)
		state.markSessionChangedForPersistence()
	})
	return result, err
}

// closeCommandPane resolves the live client from stable command identities.
// The command layer never receives the ClientInstance pointer itself.
func (d *Daemon) closeCommandPane(clientID ClientID, sessionID, paneID uint64) (*ViewTransition, error) {
	if d == nil || sessionID == 0 || paneID == 0 {
		return nil, errSessionUnavailable
	}
	var resolvedClientID ClientID
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			return
		}
		candidate := d.clients[state.ClientID]
		if candidate == nil || (clientID != 0 && candidate.ID != clientID) {
			return
		}
		resolvedClientID = candidate.ID
	})
	if resolvedClientID == 0 {
		return nil, errors.New("kill-pane client is no longer attached")
	}
	result, err := d.removeClientPane(resolvedClientID, paneID)
	if err != nil {
		return nil, err
	}
	_ = terminatePane(result.Pane)
	if result.FinalPane {
		_ = d.shutdownSessionID(sessionID, defaultPaneTerminationTimeouts)
	}
	if !result.FinalPane && result.Window == nil {
		return nil, errors.New("pane removal produced no fallback window")
	}
	return &result.Transition, nil
}

func (d *Daemon) killCommandPaneNow(sessionID, paneID uint64) (*ViewTransition, error) {
	if d == nil || sessionID == 0 || paneID == 0 {
		return nil, errSessionUnavailable
	}
	var exists bool
	var attachedClientID ClientID
	d.call(func() {
		state := d.sessions[sessionID]
		if state != nil {
			exists = true
			if client := d.clients[state.ClientID]; client != nil {
				attachedClientID = client.ID
			}
		}
	})
	if !exists {
		return nil, errSessionUnavailable
	}
	if attachedClientID != 0 {
		return d.closeCommandPane(attachedClientID, sessionID, paneID)
	}
	var pane *Pane
	var final bool
	var err error
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			err = errSessionUnavailable
			return
		}
		var removed bool
		pane, _, removed, err = state.removeGroupPaneNow(paneID)
		if err == nil && !removed {
			err = errors.New("pane was not removed")
		}
		final = err == nil && (state.ActiveWindowID == 0 || len(state.Windows) == 0)
		if err == nil {
			state.markSessionChangedForPersistence()
		}
	})
	if err != nil {
		return nil, err
	}
	_ = terminatePane(pane)
	if final {
		return nil, d.shutdownSessionID(sessionID, defaultPaneTerminationTimeouts)
	}
	return nil, nil
}
