package render

import (
	"sort"

	"tali/internal/protocol"
)

type Screen struct {
	Cols  int
	Rows  int
	Cells []protocol.Cell
}

type LayoutDescription struct {
	WindowID       uint64
	LayoutRevision uint64
	Panes          []protocol.PanePlacement
}

type PaneViewState struct {
	PaneID            uint64
	Rect              protocol.Rect
	Grid              Screen
	Generation        uint64
	Cursor            protocol.Cursor
	CursorVisible     bool
	BindingGeneration uint64
	Slot              uint8
	Styles            map[uint32]protocol.Style
}

type ClientState struct {
	SessionID uint64

	ActiveWindowID  uint64
	LastWindowID    uint64
	HasActiveWindow bool
	HasLastWindow   bool
	FocusedPaneID   uint64
	Windows         []protocol.WindowInfo

	Layout      LayoutDescription
	Panes       map[uint64]*PaneViewState
	RenderSlots map[uint8]uint64

	TerminalCols int
	TerminalRows int

	LastRendered Screen
}

func NewClientState() *ClientState {
	return &ClientState{
		Panes:       map[uint64]*PaneViewState{},
		RenderSlots: map[uint8]uint64{},
	}
}

func (s *ClientState) SetTerminalSize(cols, rows int) {
	s.TerminalCols = cols
	s.TerminalRows = rows
}

func (s *ClientState) DrawableRows() int {
	if s.TerminalRows <= 1 {
		return 1
	}
	return s.TerminalRows - 1
}

func (s *ClientState) ApplyWindowList(msg protocol.WindowList) {
	s.Windows = append([]protocol.WindowInfo(nil), msg.Windows...)
	s.syncWindowSelection()
}

func (s *ClientState) ApplyWindowSelected(msg protocol.WindowSelected) {
	windowChanged := s.HasActiveWindow && s.ActiveWindowID != msg.WindowID
	if windowChanged {
		s.LastWindowID = s.ActiveWindowID
		s.HasLastWindow = true
	}
	s.ActiveWindowID = msg.WindowID
	s.HasActiveWindow = true
	s.FocusedPaneID = msg.PaneID
	if windowChanged {
		s.resetActiveLayout()
	}
	s.syncWindowSelection()
}

func (s *ClientState) ApplyWindowClosed(windowID uint64) {
	out := s.Windows[:0]
	for _, w := range s.Windows {
		if w.WindowID != windowID {
			out = append(out, w)
		}
	}
	s.Windows = out
	if s.HasLastWindow && s.LastWindowID == windowID {
		s.LastWindowID = 0
		s.HasLastWindow = false
	}
	if s.HasActiveWindow && s.ActiveWindowID == windowID {
		s.ActiveWindowID = 0
		s.HasActiveWindow = false
	}
	s.syncWindowSelection()
}

func (s *ClientState) ApplyWindowLayout(msg protocol.WindowLayout) {
	s.Layout = LayoutDescription{
		WindowID:       msg.WindowID,
		LayoutRevision: msg.LayoutRevision,
		Panes:          append([]protocol.PanePlacement(nil), msg.Panes...),
	}
	visible := make(map[uint64]protocol.Rect, len(msg.Panes))
	for _, pane := range msg.Panes {
		visible[pane.PaneID] = pane.Rect
		view := s.ensurePane(pane.PaneID)
		view.Rect = pane.Rect
	}
	for paneID := range s.Panes {
		if _, ok := visible[paneID]; !ok {
			delete(s.Panes, paneID)
		}
	}
	for slot, paneID := range s.RenderSlots {
		if _, ok := visible[paneID]; !ok {
			delete(s.RenderSlots, slot)
		}
	}
}

func (s *ClientState) ApplyBind(msg protocol.BindRenderStream) {
	s.SessionID = msg.SessionID
	if oldPaneID, ok := s.RenderSlots[msg.Slot]; ok && oldPaneID != msg.PaneID {
		if !s.layoutContainsPane(oldPaneID) {
			delete(s.Panes, oldPaneID)
		}
	}
	s.RenderSlots[msg.Slot] = msg.PaneID
	pane := s.ensurePane(msg.PaneID)
	pane.PaneID = msg.PaneID
	pane.BindingGeneration = msg.BindingGeneration
	pane.Generation = 0
	pane.Cursor = protocol.Cursor{}
	pane.CursorVisible = true
	pane.Grid = Screen{}
	pane.Slot = msg.Slot
	pane.Styles = defaultStyles()
}

func (s *ClientState) ApplyReplace(slot uint8, msg protocol.ReplacePane) bool {
	paneID, ok := s.RenderSlots[slot]
	if !ok || paneID != msg.PaneID {
		return false
	}
	pane := s.ensurePane(paneID)
	if msg.BindingGeneration != pane.BindingGeneration {
		return false
	}
	s.SessionID = msg.SessionID
	pane.Generation = msg.Generation
	pane.Cursor = msg.Cursor
	pane.CursorVisible = msg.CursorVisible
	pane.Grid = Screen{Cols: msg.Cols, Rows: msg.Rows, Cells: append([]protocol.Cell(nil), msg.Cells...)}
	pane.Styles = defaultStyles()
	for _, def := range msg.Styles {
		pane.Styles[def.ID] = def.Style
	}
	return true
}

func (s *ClientState) ApplySetRun(slot uint8, msg protocol.SetRun) bool {
	paneID, ok := s.RenderSlots[slot]
	if !ok {
		return false
	}
	pane := s.Panes[paneID]
	if pane == nil || msg.BindingGeneration != pane.BindingGeneration || pane.Generation != msg.BaseGeneration {
		return false
	}
	if msg.Row < 0 || msg.Row >= pane.Grid.Rows || msg.Column < 0 || msg.Column >= pane.Grid.Cols {
		return false
	}
	base := msg.Row*pane.Grid.Cols + msg.Column
	for i, cell := range msg.Cells {
		idx := base + i
		if idx >= len(pane.Grid.Cells) || idx >= (msg.Row+1)*pane.Grid.Cols {
			break
		}
		pane.Grid.Cells[idx] = cell
	}
	pane.Generation = msg.Generation
	return true
}

func (s *ClientState) ApplySetCursor(slot uint8, msg protocol.SetCursor) bool {
	paneID, ok := s.RenderSlots[slot]
	if !ok {
		return false
	}
	pane := s.Panes[paneID]
	if pane == nil || msg.BindingGeneration != pane.BindingGeneration || pane.Generation != msg.BaseGeneration {
		return false
	}
	pane.Cursor = msg.Cursor
	pane.Generation = msg.Generation
	return true
}

func (s *ClientState) ApplySetCursorVisible(slot uint8, msg protocol.SetCursorVisible) bool {
	paneID, ok := s.RenderSlots[slot]
	if !ok {
		return false
	}
	pane := s.Panes[paneID]
	if pane == nil || msg.BindingGeneration != pane.BindingGeneration || pane.Generation != msg.BaseGeneration {
		return false
	}
	pane.CursorVisible = msg.Visible
	pane.Generation = msg.Generation
	return true
}

func (s *ClientState) DefineStyle(slot uint8, msg protocol.DefineStyle) bool {
	paneID, ok := s.RenderSlots[slot]
	if !ok {
		return false
	}
	pane := s.Panes[paneID]
	if pane == nil || msg.BindingGeneration != pane.BindingGeneration {
		return false
	}
	if pane.Styles == nil {
		pane.Styles = defaultStyles()
	}
	pane.Styles[msg.ID] = msg.Style
	return true
}

func (s *ClientState) syncWindowSelection() {
	for i := range s.Windows {
		s.Windows[i].Active = s.Windows[i].WindowID == s.ActiveWindowID
		if s.Windows[i].Active {
			s.Windows[i].PaneID = s.FocusedPaneID
		}
	}
}

func (s *ClientState) ensurePane(paneID uint64) *PaneViewState {
	if pane := s.Panes[paneID]; pane != nil {
		return pane
	}
	pane := &PaneViewState{
		PaneID:        paneID,
		CursorVisible: true,
		Styles:        defaultStyles(),
	}
	for _, placement := range s.Layout.Panes {
		if placement.PaneID == paneID {
			pane.Rect = placement.Rect
			break
		}
	}
	s.Panes[paneID] = pane
	return pane
}

func defaultStyles() map[uint32]protocol.Style {
	return map[uint32]protocol.Style{
		0: {
			FG: protocol.Color{Mode: "default"},
			BG: protocol.Color{Mode: "default"},
		},
	}
}

func (s *ClientState) layoutContainsPane(paneID uint64) bool {
	for _, pane := range s.Layout.Panes {
		if pane.PaneID == paneID {
			return true
		}
	}
	return false
}

func (s *ClientState) NextWindowID() (uint64, bool) {
	if len(s.Windows) == 0 {
		return 0, false
	}
	for i, w := range s.Windows {
		if w.WindowID == s.ActiveWindowID {
			return s.Windows[(i+1)%len(s.Windows)].WindowID, true
		}
	}
	return s.Windows[0].WindowID, true
}

func (s *ClientState) PreviousWindowID() (uint64, bool) {
	if len(s.Windows) == 0 {
		return 0, false
	}
	for i, w := range s.Windows {
		if w.WindowID == s.ActiveWindowID {
			return s.Windows[(i-1+len(s.Windows))%len(s.Windows)].WindowID, true
		}
	}
	return s.Windows[0].WindowID, true
}

func (s *ClientState) WindowIDByIndex(index int) (uint64, bool) {
	for _, w := range s.Windows {
		if w.Index == index {
			return w.WindowID, true
		}
	}
	return 0, false
}

func (s *ClientState) LastActiveWindowID() (uint64, bool) {
	if !s.HasLastWindow || (s.HasActiveWindow && s.LastWindowID == s.ActiveWindowID) {
		return 0, false
	}
	for _, w := range s.Windows {
		if w.WindowID == s.LastWindowID {
			return w.WindowID, true
		}
	}
	return 0, false
}

func (s *ClientState) NextFocusablePaneID() (uint64, bool) {
	ordered := s.orderedLayoutPanes()
	if len(ordered) <= 1 {
		return 0, false
	}
	for i, pane := range ordered {
		if pane.PaneID == s.FocusedPaneID {
			return ordered[(i+1)%len(ordered)].PaneID, true
		}
	}
	return ordered[0].PaneID, true
}

func (s *ClientState) orderedLayoutPanes() []protocol.PanePlacement {
	out := append([]protocol.PanePlacement(nil), s.Layout.Panes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rect.X == out[j].Rect.X {
			return out[i].PaneID < out[j].PaneID
		}
		return out[i].Rect.X < out[j].Rect.X
	})
	return out
}

func (s *ClientState) resetActiveLayout() {
	rect := protocol.Rect{
		X:      0,
		Y:      0,
		Width:  s.TerminalCols,
		Height: s.DrawableRows(),
	}
	if rect.Width < 0 {
		rect.Width = 0
	}
	if rect.Height < 0 {
		rect.Height = 0
	}
	s.Layout = LayoutDescription{
		WindowID: s.ActiveWindowID,
		Panes: []protocol.PanePlacement{
			{PaneID: s.FocusedPaneID, Rect: rect},
		},
	}
	view := s.ensurePane(s.FocusedPaneID)
	view.Rect = rect
}
