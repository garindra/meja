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
	HasActiveWindow bool
	FocusedPaneID   uint64

	Layout             LayoutDescription
	Panes              map[uint64]*PaneViewState
	RenderSlots        map[uint8]uint64
	pendingReplaces    map[uint8]protocol.ReplacePane
	transitioningSlots map[uint8]bool

	TerminalCols int
	TerminalRows int

	LastRendered  Screen
	composedCells []composedCell
	StatusBar     Screen
	StatusStyles  map[uint32]protocol.Style

	pendingDamageRects []protocol.Rect
	fullContentDirty   bool
	tabBarDirty        bool

	lastCursorX       int
	lastCursorY       int
	lastCursorVisible bool
	hasRenderedCursor bool
}

func (s *ClientState) ApplyStatusBar(msg protocol.StatusBar) {
	s.StatusBar = Screen{Cols: msg.Cols, Rows: 1, Cells: append([]protocol.Cell(nil), msg.Cells...)}
	s.StatusStyles = defaultStyles()
	for _, def := range msg.Styles {
		s.StatusStyles[def.ID] = def.Style
	}
	s.tabBarDirty = true
}

func NewClientState() *ClientState {
	return &ClientState{
		Panes:              map[uint64]*PaneViewState{},
		RenderSlots:        map[uint8]uint64{},
		pendingReplaces:    map[uint8]protocol.ReplacePane{},
		transitioningSlots: map[uint8]bool{},
		fullContentDirty:   true,
		tabBarDirty:        true,
	}
}

func (s *ClientState) SetTerminalSize(cols, rows int) {
	if s.TerminalCols != cols || s.TerminalRows != rows {
		s.fullContentDirty = true
		s.tabBarDirty = true
		s.pendingDamageRects = s.pendingDamageRects[:0]
	}
	s.TerminalCols = cols
	s.TerminalRows = rows
}

func (s *ClientState) DrawableRows() int {
	if s.TerminalRows <= 1 {
		return 1
	}
	return s.TerminalRows - 1
}

func (s *ClientState) ApplyWindowLayout(msg protocol.WindowLayout) bool {
	windowChanged := s.HasActiveWindow && s.ActiveWindowID != msg.WindowID
	focusChanged := s.HasActiveWindow && !windowChanged && s.FocusedPaneID != msg.FocusedPaneID
	if windowChanged {
		s.pendingDamageRects = s.pendingDamageRects[:0]
		for slot := range s.RenderSlots {
			s.transitioningSlots[slot] = true
			delete(s.RenderSlots, slot)
		}
	}
	s.ActiveWindowID = msg.WindowID
	s.HasActiveWindow = true
	s.FocusedPaneID = msg.FocusedPaneID
	layoutChanged := !sameLayout(s.Layout, msg)
	s.Layout = LayoutDescription{
		WindowID:       msg.WindowID,
		LayoutRevision: msg.LayoutRevision,
		Panes:          append([]protocol.PanePlacement(nil), msg.Panes...),
	}
	if layoutChanged {
		s.fullContentDirty = true
		s.pendingDamageRects = s.pendingDamageRects[:0]
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
			s.transitioningSlots[slot] = true
			delete(s.RenderSlots, slot)
		}
	}
	presented := false
	for slot, pending := range s.pendingReplaces {
		if pending.WindowID != msg.WindowID {
			continue
		}
		delete(s.pendingReplaces, slot)
		if s.applyReplace(slot, pending) {
			presented = true
		}
	}
	return presented || focusChanged
}

func (s *ClientState) ApplyBind(msg protocol.BindRenderStream) {
	if s.SessionID != msg.SessionID {
		s.tabBarDirty = true
	}
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
	accepted, _ := s.ApplyReplaceResult(slot, msg)
	return accepted
}

func (s *ClientState) ApplyReplaceResult(slot uint8, msg protocol.ReplacePane) (bool, bool) {
	if !s.layoutContainsPane(msg.PaneID) || s.Layout.WindowID != msg.WindowID {
		msg.Cells = append([]protocol.Cell(nil), msg.Cells...)
		msg.Styles = append([]protocol.StyleDefinition(nil), msg.Styles...)
		s.pendingReplaces[slot] = msg
		return true, false
	}
	delete(s.pendingReplaces, slot)
	if !s.applyReplace(slot, msg) {
		return false, false
	}
	return true, true
}

func (s *ClientState) applyReplace(slot uint8, msg protocol.ReplacePane) bool {
	oldPaneID, bound := s.RenderSlots[slot]
	pane := s.Panes[msg.PaneID]
	if pane != nil && pane.BindingGeneration > msg.BindingGeneration {
		return false
	}
	if bound && oldPaneID != msg.PaneID && !s.layoutContainsPane(oldPaneID) {
		delete(s.Panes, oldPaneID)
	}
	s.RenderSlots[slot] = msg.PaneID
	delete(s.transitioningSlots, slot)
	pane = s.ensurePane(msg.PaneID)
	if s.SessionID != msg.SessionID {
		s.tabBarDirty = true
	}
	s.SessionID = msg.SessionID
	pane.PaneID = msg.PaneID
	pane.BindingGeneration = msg.BindingGeneration
	pane.Slot = slot
	pane.Generation = msg.Generation
	pane.Cursor = msg.Cursor
	pane.CursorVisible = msg.CursorVisible
	pane.Grid = Screen{Cols: msg.Cols, Rows: msg.Rows, Cells: append([]protocol.Cell(nil), msg.Cells...)}
	pane.Styles = defaultStyles()
	for _, def := range msg.Styles {
		pane.Styles[def.ID] = def.Style
	}
	s.markDamageRect(pane.Rect)
	return true
}

func (s *ClientState) ApplyPaneUpdate(slot uint8, msg protocol.PaneUpdate) bool {
	accepted, _ := s.ApplyPaneUpdateResult(slot, msg)
	return accepted
}

func (s *ClientState) ApplyScrollPane(slot uint8, delta int) bool {
	paneID, ok := s.RenderSlots[slot]
	if !ok || delta == 0 {
		return ok
	}
	pane := s.Panes[paneID]
	if pane == nil || pane.Grid.Cols <= 0 || pane.Grid.Rows <= 0 {
		return false
	}
	rows := pane.Grid.Rows
	if delta >= rows || delta <= -rows {
		for i := range pane.Grid.Cells {
			pane.Grid.Cells[i] = protocol.Cell{Rune: ' ', Width: 1}
		}
	} else if delta > 0 {
		shift := delta * pane.Grid.Cols
		copy(pane.Grid.Cells[shift:], pane.Grid.Cells[:len(pane.Grid.Cells)-shift])
		for i := 0; i < shift; i++ {
			pane.Grid.Cells[i] = protocol.Cell{Rune: ' ', Width: 1}
		}
	} else {
		shift := -delta * pane.Grid.Cols
		copy(pane.Grid.Cells, pane.Grid.Cells[shift:])
		for i := len(pane.Grid.Cells) - shift; i < len(pane.Grid.Cells); i++ {
			pane.Grid.Cells[i] = protocol.Cell{Rune: ' ', Width: 1}
		}
	}
	s.markDamageRect(pane.Rect)
	return true
}

func (s *ClientState) ApplyPaneUpdateResult(slot uint8, msg protocol.PaneUpdate) (bool, bool) {
	paneID, ok := s.RenderSlots[slot]
	if !ok {
		if s.transitioningSlots[slot] {
			return true, false
		}
		return false, false
	}
	pane := s.Panes[paneID]
	if pane == nil || msg.BindingGeneration != pane.BindingGeneration || pane.Generation != msg.BaseGeneration {
		return false, false
	}
	for _, run := range msg.Runs {
		if run.Row < 0 || run.Row >= pane.Grid.Rows || run.Column < 0 || run.Column >= pane.Grid.Cols || len(run.Cells) > pane.Grid.Cols-run.Column {
			return false, false
		}
	}
	if msg.CursorChanged && (msg.Cursor.X < 0 || msg.Cursor.X >= pane.Grid.Cols || msg.Cursor.Y < 0 || msg.Cursor.Y >= pane.Grid.Rows) {
		return false, false
	}
	if pane.Styles == nil {
		pane.Styles = defaultStyles()
	}
	for _, def := range msg.Styles {
		pane.Styles[def.ID] = def.Style
	}
	for _, run := range msg.Runs {
		base := run.Row*pane.Grid.Cols + run.Column
		copy(pane.Grid.Cells[base:base+len(run.Cells)], run.Cells)
		s.markDamageRect(protocol.Rect{
			X:      pane.Rect.X + run.Column,
			Y:      pane.Rect.Y + run.Row,
			Width:  len(run.Cells),
			Height: 1,
		})
	}
	if msg.CursorChanged {
		pane.Cursor = msg.Cursor
	}
	if msg.CursorVisibleChanged {
		pane.CursorVisible = msg.CursorVisible
	}
	pane.Generation = msg.Generation
	return true, true
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
	written := 0
	for i, cell := range msg.Cells {
		idx := base + i
		if idx >= len(pane.Grid.Cells) || idx >= (msg.Row+1)*pane.Grid.Cols {
			break
		}
		pane.Grid.Cells[idx] = cell
		written++
	}
	s.markDamageRect(protocol.Rect{
		X:      pane.Rect.X + msg.Column,
		Y:      pane.Rect.Y + msg.Row,
		Width:  written,
		Height: 1,
	})
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

func (s *ClientState) markDamageRect(rect protocol.Rect) {
	if rect.Width <= 0 || rect.Height <= 0 {
		return
	}
	s.pendingDamageRects = append(s.pendingDamageRects, rect)
}

func sameLayout(current LayoutDescription, next protocol.WindowLayout) bool {
	if current.WindowID != next.WindowID || len(current.Panes) != len(next.Panes) {
		return false
	}
	for i := range current.Panes {
		if current.Panes[i] != next.Panes[i] {
			return false
		}
	}
	return true
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

func (s *ClientState) orderedLayoutPanes() []protocol.PanePlacement {
	out := append([]protocol.PanePlacement(nil), s.Layout.Panes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rect.Y != out[j].Rect.Y {
			return out[i].Rect.Y < out[j].Rect.Y
		}
		if out[i].Rect.X == out[j].Rect.X {
			return out[i].PaneID < out[j].PaneID
		}
		return out[i].Rect.X < out[j].Rect.X
	})
	return out
}
