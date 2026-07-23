package server

import (
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
)

// GroupState is the daemon-owned execution graph shared by one or more
// externally visible sessions. A singleton session has a GroupState too; this
// makes the ownership rules identical before and after grouping.
type GroupState struct {
	ID         uint64
	SessionIDs map[uint64]struct{}
	Windows    map[uint64]*Window
	Panes      map[uint64]*Pane
	members    atomic.Value // []uint64; immutable membership snapshots for callbacks
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
	WindowID     uint64
	SessionID    uint64
	AttachmentID uint64
	Generation   uint64
}

// ClientProjectionPlan identifies the view a client is allowed to apply.
// It is intentionally immutable once returned by the daemon transaction.
type ClientProjectionPlan struct {
	AttachmentID        uint64
	SessionID           uint64
	WindowID            uint64
	ViewLeaseGeneration uint64
	ProjectionRevision  uint64
	LayoutRevision      uint64
	Cols                uint16
	Rows                uint16
	Panes               []RenderPane
	Bindings            []RenderBinding
	FocusedPaneID       uint64
	Status              StatusUpdate
	FullSnapshot        bool
	Close               bool
	CloseReason         string
}

type RenderPane struct {
	PaneID uint64
	Rect   Rect
}

type StatusUpdate struct {
	Width    int
	Text     string
	Location string
	StyleID  uint32
}

func newGroup(id uint64) *GroupState {
	g := &GroupState{ID: id, SessionIDs: make(map[uint64]struct{}), Windows: make(map[uint64]*Window), Panes: make(map[uint64]*Pane)}
	g.members.Store([]uint64(nil))
	return g
}

func (g *GroupState) publishMembers() {
	ids := make([]uint64, 0, len(g.SessionIDs))
	for id := range g.SessionIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	g.members.Store(ids)
}

func (g *GroupState) memberIDsSnapshot() []uint64 {
	if g == nil {
		return nil
	}
	ids, _ := g.members.Load().([]uint64)
	return ids
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

func (s *SessionState) groupWindowViewLocked(windowID uint64) SessionWindowView {
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

func (s *SessionState) setGroupWindowViewLocked(windowID uint64, view SessionWindowView) {
	if s.WindowViews == nil {
		s.WindowViews = make(map[uint64]SessionWindowView)
	}
	s.WindowViews[windowID] = view
}

func (s *SessionState) clearGroupWindowZoomLocked(window *Window) {
	if window == nil {
		return
	}
	view := s.groupWindowViewLocked(window.ID)
	view.ZoomedPaneID = 0
	s.setGroupWindowViewLocked(window.ID, view)
	window.clearZoom()
}

func visibleWindowPlacementsForSession(s *SessionState, window *Window, rect Rect) []PanePlacement {
	if s == nil || window == nil {
		return nil
	}
	view := s.groupWindowViewLocked(window.ID)
	zoomedPaneID := view.ZoomedPaneID
	if !s.isGrouped() && window.Zoomed {
		zoomedPaneID = window.ZoomedPaneID
	}
	if (zoomedPaneID != 0 || (!s.isGrouped() && window.Zoomed)) && windowHasPane(window, zoomedPaneID) {
		return []PanePlacement{{PaneID: zoomedPaneID, Rect: rect}}
	}
	return window.Layout.Compute(rect)
}

func (s *SessionState) syncGroupLinksLocked() {
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

func (s *SessionState) groupMembersLocked() []*SessionState {
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
	return s != nil && s.grouped.Load()
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
			s.daemon.paneIndex.Delete(paneID)
		}
	}
	delete(s.group.Windows, windowID)
	s.daemon.windowIndex.Delete(windowID)
	members := s.groupMembersLocked()
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
					return lease == nil || lease.AttachmentID == attached.AttachmentID
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
					member.daemon.windowLeases[replacement.ID] = &WindowViewLease{WindowID: replacement.ID, SessionID: member.ID, AttachmentID: attached.AttachmentID, Generation: generation}
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
	s.daemon.paneIndex.Delete(paneID)
	for _, member := range s.groupMembersLocked() {
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
	for _, member := range s.groupMembersLocked() {
		view := member.groupWindowViewLocked(owner.ID)
		if len(updated.PaneIDs()) <= 1 || view.ZoomedPaneID == paneID {
			view.ZoomedPaneID = 0
		}
		view.removePane(owner, paneID, nextFocused)
		member.setGroupWindowViewLocked(owner.ID, view)
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
			d.groupIndex.Store(s.group.ID, s.group)
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
	s.grouped.Store(false)
	if d != nil {
		if d.groups == nil {
			d.groups = make(map[uint64]*GroupState)
		}
		d.groups[id] = g
		d.groupIndex.Store(id, g)
	}
	return g
}

func (d *Daemon) addWindowToGroupNow(session *SessionState, window *Window, pane *Pane) error {
	var err error
	if d.panes == nil {
		d.panes = make(map[uint64]*Pane)
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
	d.paneIndex.Store(pane.ID, pane)
	d.windowIndex.Store(window.ID, window)
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
	d.paneIndex.Store(pane.ID, pane)
	d.windowIndex.Store(window.ID, window)
	window.Layout = layout
	window.LayoutRevision = session.nextLayoutRevisionLocked()
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

func (d *Daemon) renameWindow(window *Window, name string) error {
	var err error
	d.call(func() {
		if window == nil {
			err = errSessionUnavailable
			return
		}
		window.Name = name
		window.AutomaticName = false
	})
	return err
}

// groupSession promotes base's singleton graph (or joins its existing graph)
// and adds mirror without creating any runtime resources.
func (d *Daemon) groupSession(base *SessionState, mirror *SessionState) error {
	if base == nil || mirror == nil {
		return errors.New("grouping requires two sessions")
	}
	if d != nil {
		var err error
		d.call(func() { err = d.groupSessionLocked(base, mirror) })
		return err
	}
	return groupSessionsLocked(base, mirror)
}

func (d *Daemon) groupSessionLocked(base, mirror *SessionState) error {
	if d.sessions[base.ID] != base || d.sessions[mirror.ID] != mirror {
		return errSessionUnavailable
	}
	d.ensureSessionGroupInActor(base)
	return groupSessionsLocked(base, mirror)
}

func groupSessionsLocked(base, mirror *SessionState) error {
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
				member.grouped.Store(true)
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
		d.groupIndex.Store(g.ID, g)
		for memberID := range g.SessionIDs {
			if member := d.sessions[memberID]; member != nil {
				member.syncGroupLinksLocked()
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
	client := d.clients[state.ID]
	if client == nil || client.AttachmentID == 0 {
		return nil
	}
	window := state.Windows[windowID]
	if window == nil {
		return fmt.Errorf("unknown new window %d", windowID)
	}
	if current := d.windowLeases[windowID]; current != nil && current.AttachmentID != client.AttachmentID {
		return fmt.Errorf("window %d is currently viewed by another client", window.DisplayIndex)
	}
	oldWindowID := d.windowForAttachmentLocked(client.AttachmentID)
	if oldWindowID != 0 && oldWindowID != windowID {
		old := d.windowLeases[oldWindowID]
		if old == nil || old.AttachmentID != client.AttachmentID {
			return errors.New("stale client window lease")
		}
	}
	generation := uint64(1)
	if current := d.windowLeases[windowID]; current != nil {
		generation = current.Generation + 1
	}
	// Acquire before release. If validation fails, the old lease is untouched.
	d.windowLeases[windowID] = &WindowViewLease{WindowID: windowID, SessionID: state.ID, AttachmentID: client.AttachmentID, Generation: generation}
	if oldWindowID != 0 && oldWindowID != windowID {
		delete(d.windowLeases, oldWindowID)
	}
	return nil
}

// selectWindow is the atomic view transition. Target acquisition happens
// before releasing the old lease, so every rejected selection leaves the
// logical view and its current lease untouched.
func (d *Daemon) selectWindow(attachmentID, sessionID, windowID uint64) (PreparedViewTransition, error) {
	var transition PreparedViewTransition
	var err error
	d.call(func() {
		state := d.sessions[sessionID]
		client := d.clients[sessionID]
		if state == nil || client == nil {
			err = errSessionUnavailable
			return
		}
		target := state.Windows[windowID]
		if target == nil {
			err = fmt.Errorf("unknown window %d", windowID)
			return
		}
		if client.AttachmentID != attachmentID {
			err = errors.New("stale client attachment")
			return
		}
		current := d.windowLeases[windowID]
		if attachmentID != 0 && current != nil && current.AttachmentID != attachmentID {
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
		if attachmentID != 0 {
			oldLeaseWindowID = d.windowForAttachmentLocked(attachmentID)
			if oldLeaseWindowID == 0 {
				oldLeaseWindowID = oldWindowID
			}
			if oldLeaseWindowID != 0 {
				old := d.windowLeases[oldLeaseWindowID]
				if old == nil || old.AttachmentID != attachmentID {
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
		if attachmentID != 0 {
			d.windowLeases[windowID] = &WindowViewLease{WindowID: windowID, SessionID: sessionID, AttachmentID: attachmentID, Generation: generation}
			if oldLeaseWindowID != 0 && oldLeaseWindowID != windowID {
				delete(d.windowLeases, oldLeaseWindowID)
			}
		}
		if oldWindowID != windowID {
			state.PreviousWindowID = oldWindowID
		}
		state.ActiveWindowID = windowID
		view := state.groupWindowViewLocked(windowID)
		if view.FocusedPaneID == 0 || !windowHasPane(target, view.FocusedPaneID) {
			view.FocusedPaneID = target.ActivePaneID
		}
		state.setGroupWindowViewLocked(windowID, view)
		transition = d.prepareViewTransitionNow(viewTransitionSelectWindow, client, state, true)
	})
	return transition, err
}

func clientViewportSize(client *ClientInstance, fallback *Window) (uint16, uint16) {
	if client == nil {
		if fallback == nil {
			return 0, 0
		}
		return fallback.Cols, fallback.Rows
	}
	cols, rows := uint16(client.terminalCols.Load()), uint16(client.terminalRows.Load())
	if (cols == 0 || rows == 0) && client.clientState != nil {
		cols, rows = client.clientState.TerminalCols, client.clientState.TerminalRows
	}
	if (cols == 0 || rows == 0) && fallback != nil {
		cols, rows = fallback.Cols, fallback.Rows
	}
	return cols, rows
}

// windowSelectionTarget resolves window-navigation commands from the same
// daemon-owned session view that selectWindow mutates. ClientState is only the
// installed projection of that view and can legitimately lag the transaction.
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

func (d *Daemon) releaseWindowView(attachmentID, windowID, generation uint64) bool {
	released := false
	d.call(func() {
		lease := d.windowLeases[windowID]
		if lease != nil && lease.AttachmentID == attachmentID && lease.Generation == generation {
			delete(d.windowLeases, windowID)
			released = true
		}
	})
	return released
}

func (d *Daemon) validateWindowView(attachmentID, windowID, generation uint64) error {
	var err error
	d.call(func() {
		lease := d.windowLeases[windowID]
		if lease == nil || lease.AttachmentID != attachmentID || lease.Generation != generation {
			err = errors.New("stale or invalid window view lease")
		}
	})
	return err
}

// mutateClientView serializes a session-view graph mutation and captures the
// immutable projection which the ClientInstance actor installs afterward.
// ClientState never crosses into the daemon transaction.
func (d *Daemon) mutateClientView(reason ViewTransitionReason, client *ClientInstance, mutate func(*SessionState) (*Window, bool, error)) (*Window, PreparedViewTransition, bool, error) {
	var window *Window
	var transition PreparedViewTransition
	var changed bool
	var err error
	if d == nil || client == nil {
		return nil, transition, false, errSessionUnavailable
	}
	d.call(func() {
		state := d.sessions[client.sessionID]
		if state == nil || d.clients[state.ID] != client {
			err = errSessionUnavailable
			return
		}
		window, changed, err = mutate(state)
		if err == nil {
			transition = d.prepareViewTransitionNow(reason, client, state, true)
		}
	})
	return window, transition, changed, err
}

func (d *Daemon) focusClientPane(client *ClientInstance, paneID uint64) (*Window, ClientProjectionPlan, error) {
	window, transition, _, err := d.mutateClientView(viewTransitionLayout, client, func(state *SessionState) (*Window, bool, error) {
		before := state.groupWindowViewLocked(state.ActiveWindowID).FocusedPaneID
		window, err := state.focusPaneNow(paneID)
		return window, before != paneID, err
	})
	// Focus changes only the cursor owner. Existing pane bindings and grids
	// remain valid, so the frontend can apply the focused-pane field to its
	// current layout immediately without waiting for snapshots that will never
	// be emitted.
	transition.Projection.FullSnapshot = false
	return window, transition.Projection, err
}

func (d *Daemon) toggleClientZoom(client *ClientInstance) (*Window, PreparedViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, client, func(state *SessionState) (*Window, bool, error) {
		window, changed, err := state.toggleZoomNow()
		return window, changed, err
	})
}

func (d *Daemon) splitClientPane(client *ClientInstance, pane *Pane, direction SplitDirection) (*Window, PreparedViewTransition, error) {
	if pane == nil {
		return nil, PreparedViewTransition{}, errors.New("split pane is unavailable")
	}
	window, transition, _, err := d.mutateClientView(viewTransitionSplitPane, client, func(state *SessionState) (*Window, bool, error) {
		window, err := state.splitFocusedPaneNow(pane, direction)
		return window, err == nil, err
	})
	return window, transition, err
}

func (d *Daemon) cycleClientLayout(client *ClientInstance) (*Window, PreparedViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, client, func(state *SessionState) (*Window, bool, error) {
		return state.cycleWindowLayoutNow()
	})
}

func (d *Daemon) resizeClientPane(client *ClientInstance, direction PaneResizeDirection, amount int) (*Window, PreparedViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, client, func(state *SessionState) (*Window, bool, error) {
		return state.resizeFocusedPaneNow(direction, amount)
	})
}

func (d *Daemon) swapClientPane(client *ClientInstance, direction PaneSwapDirection) (*Window, PreparedViewTransition, bool, error) {
	return d.mutateClientView(viewTransitionLayout, client, func(state *SessionState) (*Window, bool, error) {
		return state.swapFocusedPaneNow(direction)
	})
}

func (d *Daemon) createClientWindow(client *ClientInstance, pane *Pane, cols, rows uint16) (*Window, PreparedViewTransition, error) {
	var window *Window
	var transition PreparedViewTransition
	var err error
	if d == nil || client == nil {
		return nil, transition, errSessionUnavailable
	}
	d.call(func() {
		state := d.sessions[client.sessionID]
		if state == nil || (d.clients[state.ID] != nil && d.clients[state.ID] != client) {
			err = errSessionUnavailable
			return
		}
		window, err = state.createWindowNow(pane, cols, rows)
		if err != nil {
			return
		}
		if err = d.activateCreatedWindowNow(state, window.ID); err != nil {
			return
		}
		if _, prepareErr := prepareClientWindowGeometryNow(client, state, state.ActiveWindowID); prepareErr != nil {
			err = prepareErr
			return
		}
		transition = d.prepareViewTransitionNow(viewTransitionCreateWindow, client, state, true)
	})
	return window, transition, err
}

func (d *Daemon) startClientWindow(client *ClientInstance, cwd string, argv []string, cols, rows uint16, shell string) (*Window, PreparedViewTransition, error) {
	if d == nil || client == nil {
		return nil, PreparedViewTransition{}, errSessionUnavailable
	}
	state := client.sessionState()
	if state == nil {
		return nil, PreparedViewTransition{}, errSessionUnavailable
	}
	paneID, err := d.allocatePaneID()
	if err != nil {
		return nil, PreparedViewTransition{}, err
	}
	pane, err := startPaneProcess(paneID, state.contextualPaneRequest(paneRequest{Cwd: cwd, Command: argv, Cols: cols, Rows: rows, Shell: shell}))
	if err != nil {
		return nil, PreparedViewTransition{}, fmt.Errorf("start pane: %w", err)
	}
	window, transition, err := d.createClientWindow(client, pane, cols, rows)
	if err != nil {
		_ = terminatePane(pane)
		return nil, PreparedViewTransition{}, err
	}
	d.startPane(state, pane)
	return window, transition, nil
}

func (d *Daemon) startSessionWindow(state *SessionState, cwd string, argv []string, cols, rows uint16, shell string) (*Pane, *Window, error) {
	if d == nil || state == nil {
		return nil, nil, errSessionUnavailable
	}
	paneID, err := d.allocatePaneID()
	if err != nil {
		return nil, nil, err
	}
	pane, err := startPaneProcess(paneID, state.contextualPaneRequest(paneRequest{Cwd: cwd, Command: argv, Cols: cols, Rows: rows, Shell: shell}))
	if err != nil {
		return nil, nil, fmt.Errorf("start pane: %w", err)
	}
	var window *Window
	d.call(func() {
		if d.sessions[state.ID] != state {
			err = errSessionUnavailable
			return
		}
		window, err = state.createWindowNow(pane, cols, rows)
		if err == nil {
			err = d.activateCreatedWindowNow(state, window.ID)
		}
	})
	if err != nil {
		_ = terminatePane(pane)
		return nil, nil, err
	}
	d.startPane(state, pane)
	return pane, window, nil
}

func (d *Daemon) startClientSplit(client *ClientInstance, cwd string, cols, rows uint16, shell string, direction SplitDirection) (PreparedViewTransition, error) {
	if d == nil || client == nil {
		return PreparedViewTransition{}, errSessionUnavailable
	}
	state := client.sessionState()
	if state == nil {
		return PreparedViewTransition{}, errSessionUnavailable
	}
	paneID, err := d.allocatePaneID()
	if err != nil {
		return PreparedViewTransition{}, err
	}
	pane, err := startPaneProcess(paneID, state.contextualPaneRequest(paneRequest{Cwd: cwd, Cols: cols, Rows: rows, Shell: shell}))
	if err != nil {
		return PreparedViewTransition{}, fmt.Errorf("start split pane: %w", err)
	}
	_, transition, err := d.splitClientPane(client, pane, direction)
	if err != nil {
		_ = terminatePane(pane)
		return PreparedViewTransition{}, err
	}
	d.startPane(state, pane)
	return transition, nil
}

func (d *Daemon) startSessionSplit(state *SessionState, cwd string, shell string, direction SplitDirection) error {
	if d == nil || state == nil {
		return errSessionUnavailable
	}
	active := state.activePane()
	if active == nil {
		return errors.New("split-window requires an active pane")
	}
	cols, rows := active.TerminalSize()
	paneID, err := d.allocatePaneID()
	if err != nil {
		return err
	}
	pane, err := startPaneProcess(paneID, state.contextualPaneRequest(paneRequest{Cwd: cwd, Cols: uint16(cols), Rows: uint16(rows), Shell: shell}))
	if err != nil {
		return fmt.Errorf("start split pane: %w", err)
	}
	d.call(func() {
		if d.sessions[state.ID] != state {
			err = errSessionUnavailable
			return
		}
		_, err = state.splitFocusedPaneNow(pane, direction)
	})
	if err != nil {
		_ = terminatePane(pane)
		return err
	}
	d.startPane(state, pane)
	return nil
}

func (d *Daemon) commandSessionAndClient(clientInstanceID, sessionID uint64) (*SessionState, *ClientInstance) {
	var state *SessionState
	var client *ClientInstance
	d.call(func() {
		state = d.sessions[sessionID]
		candidate := d.clients[sessionID]
		if candidate != nil && (clientInstanceID == 0 || candidate.AttachmentID == clientInstanceID) {
			client = candidate
		}
	})
	return state, client
}

func (d *Daemon) createCommandWindow(clientInstanceID, sessionID uint64, cols, rows uint16) (*PreparedViewTransition, error) {
	state, client := d.commandSessionAndClient(clientInstanceID, sessionID)
	if state == nil {
		return nil, errSessionUnavailable
	}
	if client != nil {
		clientCols, clientRows, err := client.createWindowSize()
		if err != nil {
			return nil, err
		}
		window, transition, err := d.startClientWindow(client, state.rootDir, nil, clientCols, clientRows, client.shell)
		if err == nil && window == nil {
			err = errors.New("create window: daemon rejected graph insertion")
		}
		return &transition, err
	}
	if cols == 0 || rows == 0 {
		if window := state.Windows[state.ActiveWindowID]; window != nil {
			cols, rows = window.Cols, window.Rows
		}
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 23
	}
	_, _, err := d.startSessionWindow(state, state.rootDir, nil, cols, rows, defaultShell())
	return nil, err
}

func (d *Daemon) splitCommandWindow(clientInstanceID, sessionID uint64, direction SplitDirection) (*PreparedViewTransition, error) {
	state, client := d.commandSessionAndClient(clientInstanceID, sessionID)
	if state == nil {
		return nil, errSessionUnavailable
	}
	if client != nil {
		activePane, clientState := client.activePane(), client.snapshotClient()
		if activePane == nil || clientState == nil {
			return nil, nil
		}
		if err := state.CanSplitFocusedPane(); err != nil {
			return nil, err
		}
		cols, rows := activePane.TerminalSize()
		transition, err := d.startClientSplit(client, state.rootDir, uint16(cols), uint16(rows), client.shell, direction)
		return &transition, err
	}
	return nil, d.startSessionSplit(state, state.rootDir, defaultShell(), direction)
}

type clientPaneRemoval struct {
	Pane           *Pane
	Panes          []*Pane
	Window         *Window
	Transition     PreparedViewTransition
	WindowClosed   bool
	ClosedWindowID uint64
	FinalPane      bool
	Removed        bool
}

func (d *Daemon) removeClientPane(client *ClientInstance, paneID uint64) (clientPaneRemoval, error) {
	var result clientPaneRemoval
	var err error
	if d == nil || client == nil {
		return result, errSessionUnavailable
	}
	d.call(func() {
		state := d.sessions[client.sessionID]
		if state == nil || d.clients[state.ID] != client {
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
			result.Transition = PreparedViewTransition{Reason: viewTransitionClosePane, Projection: ClientProjectionPlan{AttachmentID: client.AttachmentID, SessionID: state.ID, Close: true, CloseReason: "no viewable fallback window"}, RemovedPanes: []*Pane{pane}}
			return
		}
		result.Window = cloneWindow(state.Windows[state.ActiveWindowID])
		if _, err = prepareClientWindowGeometryNow(client, state, state.ActiveWindowID); err != nil {
			return
		}
		result.Transition = d.prepareViewTransitionNow(viewTransitionClosePane, client, state, true, pane)
		state.markSessionChangedForPersistence()
	})
	return result, err
}

// closeCommandPane resolves the live client from stable command identities.
// The command layer never receives the ClientInstance pointer itself.
func (d *Daemon) closeCommandPane(clientInstanceID, sessionID, paneID uint64) (*PreparedViewTransition, error) {
	if d == nil || sessionID == 0 || paneID == 0 {
		return nil, errSessionUnavailable
	}
	var client *ClientInstance
	d.call(func() {
		candidate := d.clients[sessionID]
		if candidate == nil || (clientInstanceID != 0 && candidate.AttachmentID != clientInstanceID) {
			return
		}
		client = candidate
	})
	if client == nil {
		return nil, errors.New("kill-pane client is no longer attached")
	}
	result, err := d.removeClientPane(client, paneID)
	if err != nil {
		return nil, err
	}
	_ = terminatePane(result.Pane)
	if result.FinalPane {
		_ = d.shutdownSession(client.sessionState())
	}
	if !result.FinalPane && result.Window == nil {
		return nil, errors.New("pane removal produced no fallback window")
	}
	return &result.Transition, nil
}

func (d *Daemon) killCommandPaneNow(sessionID, paneID uint64) (*PreparedViewTransition, error) {
	if d == nil || sessionID == 0 || paneID == 0 {
		return nil, errSessionUnavailable
	}
	var state *SessionState
	var client *ClientInstance
	d.call(func() {
		state = d.sessions[sessionID]
		client = d.clients[sessionID]
	})
	if state == nil {
		return nil, errSessionUnavailable
	}
	if client != nil {
		return d.closeCommandPane(client.AttachmentID, sessionID, paneID)
	}
	var pane *Pane
	var final bool
	var err error
	d.call(func() {
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
		return nil, d.shutdownSession(state)
	}
	return nil, nil
}
