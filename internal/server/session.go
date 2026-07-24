package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func (s *SessionState) attachedClient() *ClientIdentity {
	if s == nil || s.daemon == nil {
		return nil
	}
	return s.daemon.clients[s.ClientID]
}

// SessionState is the externally visible passive session view. The daemon owns
// the shared group/window/pane graph; this object retains session-local view and
// durable model data only.
type SessionState struct {
	ID               uint64
	ClientID         ClientID
	Name             string
	CreatedAt        int64
	GroupID          uint64
	Windows          map[uint64]*Window
	Panes            map[uint64]*Pane
	Links            []WindowLink
	WindowViews      map[uint64]SessionWindowView
	ActiveWindowID   uint64
	PreviousWindowID uint64
	group            *GroupState
	grouped          atomic.Bool

	lastWindowLayoutRevision WindowLayoutRevision

	daemon  *Daemon
	rootDir string
}

// WindowLayoutRevision versions canonical daemon-owned window geometry. It is
// never sent to a frontend or used as a ClientLayoutRevision.
type WindowLayoutRevision uint64

type Window struct {
	ID               uint64
	GroupID          uint64
	DisplayIndex     int
	Name             string
	AutomaticName    bool
	ActivePaneID     uint64
	Zoomed           bool
	ZoomedPaneID     uint64
	Layout           LayoutNode
	LayoutRevision   WindowLayoutRevision
	Cols             uint16
	Rows             uint16
	layoutCycleIndex int
}

func (w *Window) clearZoom() {
	if w == nil {
		return
	}
	w.Zoomed = false
	w.ZoomedPaneID = 0
}

type PromptMode uint8

const (
	PromptModeText PromptMode = iota + 1
	PromptModeConfirm
)

type PromptAction uint8

const (
	PromptActionNone PromptAction = iota
	PromptActionChanged
	PromptActionSubmit
	PromptActionCancel
)

type PromptState struct {
	Mode          PromptMode
	Action        PromptAction
	Label         string
	Text          []rune
	Cursor        int
	pendingUTF8   []byte
	PendingEscape []byte
}

type promptResult struct {
	Submitted bool
	Text      string
}

type promptContinuation func(promptResult) (bool, error)

type PaneSwapDirection int8

const (
	SwapPanePrevious PaneSwapDirection = -1
	SwapPaneNext     PaneSwapDirection = 1
)

func NewSessionState(id uint64) *SessionState {
	group := newGroup(id)
	session := &SessionState{
		ID:          id,
		CreatedAt:   time.Now().Unix(),
		Windows:     map[uint64]*Window{},
		Panes:       map[uint64]*Pane{},
		WindowViews: map[uint64]SessionWindowView{},
		GroupID:     id,
		group:       group,
	}
	group.addSession(session)
	return session
}

func (s *SessionState) contextualPaneRequest(request paneRequest) paneRequest {
	request.MejaSessionTarget = strconv.FormatUint(s.ID, 10)
	if s.group != nil {
		// This internal target remains stable while externally visible session
		// names and IDs change. The command layer resolves it to a surviving
		// session in the same execution group.
		request.MejaSessionTarget = "@" + strconv.FormatUint(s.group.ID, 10)
	}
	if s.daemon != nil && s.daemon.groups != nil {
		request.MejaSocket = s.daemon.controlPath
	}
	return request
}

func (s *SessionState) nextWindowLayoutRevisionNow() WindowLayoutRevision {
	s.lastWindowLayoutRevision++
	return s.lastWindowLayoutRevision
}

func (s *SessionState) markSessionChangedForPersistence() {
	s.persistSessionForPersistence()
}

func (s *SessionState) markWindowChangedForPersistence(windowID uint64) {
	s.persistWindowForPersistence(windowID)
}

func (s *SessionState) setRoot(root string) {
	root = filepath.Clean(root)
	if root == s.rootDir {
		return
	}
	s.rootDir = root
	s.markSessionChangedForPersistence()
}

func (s *SessionState) SessionName() string {
	return s.Name
}

func (s *SessionState) setSessionName(name string) {
	s.Name = name
}

func (s *SessionState) createWindowNow(pane *Pane, cols, rows uint16) (*Window, error) {
	if pane == nil {
		return nil, errors.New("pane is unavailable")
	}
	if s.daemon == nil {
		return nil, errSessionUnavailable
	}
	if s.daemon.nextWindowID == 0 {
		s.daemon.nextWindowID = 1
	}
	windowID := s.daemon.nextWindowID
	s.daemon.nextWindowID++
	displayIndex := s.lowestAvailableWindowDisplayIndex()
	window := &Window{
		ID:               windowID,
		GroupID:          s.GroupID,
		DisplayIndex:     displayIndex,
		Name:             pane.Title,
		AutomaticName:    true,
		ActivePaneID:     pane.ID,
		Layout:           &PaneLayout{PaneID: pane.ID},
		LayoutRevision:   s.nextWindowLayoutRevisionNow(),
		Cols:             cols,
		Rows:             rows,
		layoutCycleIndex: layoutPresetCustom,
	}
	if window.Cols == 0 || window.Rows == 0 {
		paneCols, paneRows := pane.TerminalSize()
		window.Cols, window.Rows = uint16(paneCols), uint16(paneRows)
	}
	if s.group == nil {
		s.daemon.ensureSessionGroupInActor(s)
	}
	if err := s.daemon.addWindowToGroupNow(s, window, pane); err != nil {
		return nil, err
	}
	if s.ActiveWindowID != window.ID {
		s.PreviousWindowID = s.ActiveWindowID
	}
	s.ActiveWindowID = windowID
	s.WindowViews[windowID] = SessionWindowView{FocusedPaneID: pane.ID}
	s.markSessionChangedForPersistence()
	return window, nil
}

func (s *SessionState) HasWindows() bool {
	return len(s.Windows) > 0
}

func (s *SessionState) PanesSnapshot() []*Pane {
	panes := make([]*Pane, 0, len(s.Panes))
	for _, pane := range s.Panes {
		panes = append(panes, pane)
	}
	return panes
}

func (s *SessionState) Pane(id uint64) *Pane {
	return s.Panes[id]
}

func (s *SessionState) activePane() *Pane {
	if s == nil {
		return nil
	}
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return nil
	}
	view := s.groupWindowViewNow(window.ID)
	paneID := view.FocusedPaneID
	if paneID == 0 {
		paneID = window.ActivePaneID
	}
	return s.Panes[paneID]
}

func (s *SessionState) lowestAvailableWindowDisplayIndex() int {
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

func (s *SessionState) RenameWindow(windowID uint64, name string) (*Window, error) {
	window := s.Windows[windowID]
	if window == nil {
		return nil, fmt.Errorf("unknown window %d", windowID)
	}
	// Empty names are valid; normal status projection remains well-formed.
	changed := window.Name != name || window.AutomaticName
	if s.daemon != nil {
		if err := s.daemon.renameWindow(window, name); err != nil {
			return nil, err
		}
	} else {
		window.Name = name
		window.AutomaticName = false
	}
	if changed {
		s.markWindowChangedForPersistence(windowID)
		if s.isGrouped() && s.daemon != nil {
			var members []*SessionState
			s.daemon.call(func() { members = s.groupMembersNow() })
			for _, member := range members {
				if member == s {
					continue
				}
				if member.attachedClient() != nil {
					postClientCommand(member.attachedClient().State.Active, clientInstanceCommand{RefreshStatus: true})
				}
			}
		}
	}
	return cloneWindow(window), nil
}

func (s *SessionState) focusPaneNow(paneID uint64) (*Window, error) {
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return nil, errors.New("client has no active window")
	}
	if !windowHasPane(window, paneID) {
		return nil, fmt.Errorf("pane %d not visible in window %d", paneID, window.ID)
	}
	view := s.groupWindowViewNow(window.ID)
	if (view.ZoomedPaneID != 0 || (!s.isGrouped() && window.Zoomed)) && view.ZoomedPaneID != paneID {
		view.ZoomedPaneID = 0
		if !s.isGrouped() {
			window.clearZoom()
		}
		window.LayoutRevision = s.nextWindowLayoutRevisionNow()
	}
	changed := view.FocusedPaneID != paneID
	view.focusPane(paneID)
	s.setGroupWindowViewNow(window.ID, view)
	if !s.isGrouped() {
		window.ActivePaneID = paneID
	}
	if changed {
		s.markWindowChangedForPersistence(window.ID)
	}
	return cloneWindow(window), nil
}

func (s *SessionState) toggleZoomNow() (*Window, bool, error) {
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return nil, false, errors.New("client has no active window")
	}
	if len(window.Layout.PaneIDs()) <= 1 {
		return cloneWindow(window), false, nil
	}
	view := s.groupWindowViewNow(window.ID)
	focusedPaneID := view.FocusedPaneID
	if focusedPaneID == 0 {
		focusedPaneID = window.ActivePaneID
	}
	if view.ZoomedPaneID != 0 || (!s.isGrouped() && window.Zoomed) {
		view.ZoomedPaneID = 0
		if !s.isGrouped() {
			window.clearZoom()
		}
	} else {
		if !windowHasPane(window, focusedPaneID) {
			return nil, false, fmt.Errorf("focused pane %d not in window %d", focusedPaneID, window.ID)
		}
		view.ZoomedPaneID = focusedPaneID
		if !s.isGrouped() {
			window.Zoomed = true
			window.ZoomedPaneID = focusedPaneID
		}
	}
	s.setGroupWindowViewNow(window.ID, view)
	window.LayoutRevision = s.nextWindowLayoutRevisionNow()
	return cloneWindow(window), true, nil
}

func (s *SessionState) cycleWindowLayoutNow() (*Window, bool, error) {
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return nil, false, errors.New("client has no active window")
	}
	paneIDs := window.Layout.PaneIDs()
	if len(paneIDs) <= 1 {
		return cloneWindow(window), false, nil
	}
	view := s.groupWindowViewNow(window.ID)
	focusedPaneID := view.FocusedPaneID
	if focusedPaneID == 0 {
		focusedPaneID = window.ActivePaneID
	}

	next := 0
	if window.layoutCycleIndex >= 0 {
		next = (window.layoutCycleIndex + 1) % layoutPresetCount
	}
	window.Layout = buildPresetLayout(paneIDs, focusedPaneID, next)
	window.layoutCycleIndex = next
	s.clearGroupWindowZoomNow(window)
	window.LayoutRevision = s.nextWindowLayoutRevisionNow()
	s.markWindowChangedForPersistence(window.ID)
	return cloneWindow(window), true, nil
}

func (s *SessionState) resizeFocusedPaneNow(direction PaneResizeDirection, amount int) (*Window, bool, error) {
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return nil, false, errors.New("client has no active window")
	}
	if direction > ResizePaneRight {
		return nil, false, fmt.Errorf("invalid pane resize direction %d", direction)
	}
	if amount <= 0 {
		return nil, false, fmt.Errorf("pane resize amount must be positive")
	}
	view := s.groupWindowViewNow(window.ID)
	focusedPaneID := view.FocusedPaneID
	if focusedPaneID == 0 {
		focusedPaneID = window.ActivePaneID
	}
	unzoomed := view.ZoomedPaneID != 0 || (!s.isGrouped() && window.Zoomed)
	s.clearGroupWindowZoomNow(window)
	resized := ResizePaneBoundary(window.Layout, focusedPaneID, direction, amount, Rect{
		Width:  int(window.Cols),
		Height: int(window.Rows),
	})
	if !resized && !unzoomed {
		return cloneWindow(window), false, nil
	}
	window.layoutCycleIndex = layoutPresetCustom
	window.LayoutRevision = s.nextWindowLayoutRevisionNow()
	if resized {
		s.markWindowChangedForPersistence(window.ID)
	}
	return cloneWindow(window), true, nil
}

func (s *SessionState) splitFocusedPaneNow(pane *Pane, direction SplitDirection) (*Window, error) {
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return nil, errors.New("client has no active window")
	}
	view := s.groupWindowViewNow(window.ID)
	focusedPaneID := view.FocusedPaneID
	if focusedPaneID == 0 {
		focusedPaneID = window.ActivePaneID
	}
	if len(window.Layout.PaneIDs()) >= int(protocol.MaxVisiblePanes) {
		return nil, fmt.Errorf("window %d has reached the %d-pane limit", window.ID, protocol.MaxVisiblePanes)
	}
	if !windowHasPane(window, focusedPaneID) {
		return nil, fmt.Errorf("focused pane %d not in window %d", focusedPaneID, window.ID)
	}
	if direction != SplitVertical && direction != SplitHorizontal {
		return nil, fmt.Errorf("invalid split direction %d", direction)
	}
	s.clearGroupWindowZoomNow(window)
	view.ZoomedPaneID = 0
	window.layoutCycleIndex = layoutPresetCustom
	updated, replaced := replacePaneWithSplit(window.Layout, focusedPaneID, pane.ID, direction)
	if !replaced {
		return nil, fmt.Errorf("focused pane %d not found in layout", focusedPaneID)
	}
	if err := s.daemon.addPaneToWindowGroupNow(s, window, pane, updated); err != nil {
		return nil, err
	}
	view.focusPane(pane.ID)
	s.setGroupWindowViewNow(window.ID, view)
	s.markWindowChangedForPersistence(window.ID)
	return cloneWindow(window), nil
}

func (s *SessionState) swapFocusedPaneNow(direction PaneSwapDirection) (*Window, bool, error) {
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return nil, false, errors.New("client has no active window")
	}
	if direction != SwapPanePrevious && direction != SwapPaneNext {
		return nil, false, fmt.Errorf("invalid pane swap direction %d", direction)
	}
	s.clearGroupWindowZoomNow(window)
	view := s.groupWindowViewNow(window.ID)
	focusedPaneID := view.FocusedPaneID
	if focusedPaneID == 0 {
		focusedPaneID = window.ActivePaneID
	}
	placements := visibleWindowPlacementsForSession(s, window, Rect{Width: int(window.Cols), Height: int(window.Rows)})
	if len(placements) < 2 {
		return cloneWindow(window), false, nil
	}
	current := -1
	for i, placement := range placements {
		if placement.PaneID == focusedPaneID {
			current = i
			break
		}
	}
	if current < 0 {
		return cloneWindow(window), false, nil
	}
	target := (current + int(direction) + len(placements)) % len(placements)
	targetPaneID := placements[target].PaneID
	if !swapLayoutPanes(window.Layout, focusedPaneID, targetPaneID) {
		return nil, false, fmt.Errorf("swap panes %d and %d in window %d", focusedPaneID, targetPaneID, window.ID)
	}
	window.layoutCycleIndex = layoutPresetCustom
	window.LayoutRevision = s.nextWindowLayoutRevisionNow()
	s.markWindowChangedForPersistence(window.ID)
	return cloneWindow(window), true, nil
}

func (s *SessionState) CanSplitFocusedPane() error {
	window := s.Windows[s.ActiveWindowID]
	if window == nil {
		return errors.New("client has no active window")
	}
	view := s.groupWindowViewNow(window.ID)
	focusedPaneID := view.FocusedPaneID
	if focusedPaneID == 0 {
		focusedPaneID = window.ActivePaneID
	}
	if !windowHasPane(window, focusedPaneID) {
		return fmt.Errorf("focused pane %d not in window %d", focusedPaneID, window.ID)
	}
	if len(window.Layout.PaneIDs()) >= int(protocol.MaxVisiblePanes) {
		return fmt.Errorf("window %d has reached the %d-pane limit", window.ID, protocol.MaxVisiblePanes)
	}
	return nil
}

type WindowStatus struct {
	WindowID uint64
	Index    int
	Title    string
	Active   bool
	Zoomed   bool
}

func (s *SessionState) WindowStatuses() []WindowStatus {
	active := s.ActiveWindowID
	list := make([]WindowStatus, 0, len(s.Windows))
	for _, window := range s.Windows {
		zoomed := window.Zoomed
		if s.isGrouped() {
			zoomed = s.groupWindowViewNow(window.ID).ZoomedPaneID != 0
		}
		list = append(list, WindowStatus{
			WindowID: window.ID,
			Index:    window.DisplayIndex,
			Title:    window.Name,
			Active:   window.ID == active,
			Zoomed:   zoomed,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	return list
}

func (s *SessionState) UpdatePaneTitle(paneID uint64, title string) *Window {
	for _, window := range s.Windows {
		if window.AutomaticName && windowHasPane(window, paneID) {
			window.Name = title
			return cloneWindow(window)
		}
	}
	return nil
}

// resizeSessionWindowModelNow records target geometry without issuing pane
// actor commands. Attached transitions use this form so ClientInstance can
// reclaim the old output leases before applying the daemon-authorized grids.
func resizeSessionWindowModelNow(s *SessionState, windowID uint64, cols, rows uint16) error {
	if s == nil {
		return errSessionUnavailable
	}
	window := s.Windows[windowID]
	if window == nil {
		return fmt.Errorf("unknown window %d", windowID)
	}
	window.Cols = cols
	window.Rows = rows
	window.LayoutRevision = s.nextWindowLayoutRevisionNow()
	return nil
}

// orderedWindowIDs returns canonical window IDs in user-visible display order.
// It performs no locking; callers obtain the required state snapshot or actor
// serialization before reading the session maps.
func (s *SessionState) orderedWindowIDs() []uint64 {
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
