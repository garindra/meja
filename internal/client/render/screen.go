package render

import (
	"sort"

	"tali/internal/protocol"
)

type Screen struct {
	Cols, Rows int
	Cells      []protocol.Cell
}
type LayoutDescription struct {
	WindowID, LayoutRevision uint64
	Panes                    []protocol.PanePlacement
}
type PaneViewState struct {
	PaneID        uint64
	Rect          protocol.Rect
	Grid          Screen
	Cursor        protocol.Cursor
	CursorVisible bool
	Slot          uint8
	Styles        map[uint32]protocol.Style
}
type StreamState struct {
	Row, Column int
	StyleID     uint32
}

type ClientState struct {
	ActiveWindowID                       uint64
	HasActiveWindow                      bool
	FocusedPaneID                        uint64
	Layout                               LayoutDescription
	Panes                                map[uint64]*PaneViewState
	RenderSlots                          map[uint8]uint64
	Streams                              map[uint8]*StreamState
	TerminalCols, TerminalRows           int
	LastRendered                         Screen
	composedCells                        []composedCell
	StatusBar                            Screen
	StatusStyles                         map[uint32]protocol.Style
	pendingDamageRects                   []protocol.Rect
	fullContentDirty, tabBarDirty        bool
	lastCursorX, lastCursorY             int
	lastCursorVisible, hasRenderedCursor bool
}

func NewClientState() *ClientState {
	return &ClientState{Panes: map[uint64]*PaneViewState{}, RenderSlots: map[uint8]uint64{}, Streams: map[uint8]*StreamState{}, fullContentDirty: true, tabBarDirty: true}
}
func (s *ClientState) ApplyStatusBar(msg protocol.StatusBar) {
	s.StatusBar = Screen{Cols: msg.Cols, Rows: 1, Cells: append([]protocol.Cell(nil), msg.Cells...)}
	s.StatusStyles = defaultStyles()
	for _, d := range msg.Styles {
		s.StatusStyles[d.ID] = d.Style
	}
	s.tabBarDirty = true
}
func (s *ClientState) SetTerminalSize(cols, rows int) {
	if s.TerminalCols != cols || s.TerminalRows != rows {
		s.fullContentDirty = true
		s.tabBarDirty = true
		s.pendingDamageRects = nil
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
	s.ActiveWindowID = msg.WindowID
	s.HasActiveWindow = true
	s.FocusedPaneID = msg.FocusedPaneID
	layoutChanged := !sameLayout(s.Layout, msg)
	s.Layout = LayoutDescription{WindowID: msg.WindowID, LayoutRevision: msg.LayoutRevision, Panes: append([]protocol.PanePlacement(nil), msg.Panes...)}
	visible := map[uint64]struct{}{}
	nextSlots := map[uint8]uint64{}
	for _, p := range msg.Panes {
		visible[p.PaneID] = struct{}{}
		nextSlots[p.Slot] = p.PaneID
		pane := s.ensurePane(p.PaneID)
		if pane.Grid.Cols != p.Rect.Width || pane.Grid.Rows != p.Rect.Height {
			pane.Grid = blankScreen(p.Rect.Width, p.Rect.Height)
		}
		pane.Rect = p.Rect
		pane.Slot = p.Slot
		if s.Streams[p.Slot] == nil {
			s.Streams[p.Slot] = &StreamState{}
		}
	}
	for id := range s.Panes {
		if _, ok := visible[id]; !ok {
			delete(s.Panes, id)
		}
	}
	s.RenderSlots = nextSlots
	if layoutChanged || windowChanged {
		s.fullContentDirty = true
		s.pendingDamageRects = nil
	}
	return focusChanged && !layoutChanged
}

func (s *ClientState) ResetStream(slot uint8) bool {
	if s.slotPane(slot) == nil {
		return false
	}
	s.Streams[slot] = &StreamState{}
	return true
}
func (s *ClientState) InstallStyle(slot uint8, msg protocol.StyleInstall) bool {
	p := s.slotPane(slot)
	if p == nil {
		return false
	}
	if msg.ID == protocol.CanonicalDefaultStyleID && !protocol.IsCanonicalDefaultStyle(msg.Style) {
		return false
	}
	p.Styles[msg.ID] = msg.Style
	return true
}
func (s *ClientState) SetWritePosition(slot uint8, msg protocol.SetWritePosition) bool {
	st, p := s.streamPane(slot)
	if p == nil || msg.Row < 0 || msg.Row >= p.Grid.Rows || msg.Column < 0 || msg.Column >= p.Grid.Cols {
		return false
	}
	st.Row, st.Column = msg.Row, msg.Column
	return true
}
func (s *ClientState) SetWriteStyle(slot uint8, msg protocol.SetWriteStyle) bool {
	st, p := s.streamPane(slot)
	if p == nil {
		return false
	}
	if _, ok := p.Styles[msg.StyleID]; !ok {
		return false
	}
	st.StyleID = msg.StyleID
	return true
}
func (s *ClientState) WriteText(slot uint8, msg protocol.WriteText) bool {
	st, p := s.streamPane(slot)
	if st == nil || p == nil {
		return false
	}
	return s.writeText(st, p, msg, st.StyleID)
}

// WriteTextDefault renders with style 0 without changing the stream latch.
func (s *ClientState) WriteTextDefault(slot uint8, text []byte) bool {
	st, p := s.streamPane(slot)
	if st == nil || p == nil {
		return false
	}
	style, ok := p.Styles[protocol.CanonicalDefaultStyleID]
	if !ok || !protocol.IsCanonicalDefaultStyle(style) {
		return false
	}
	return s.writeText(st, p, protocol.WriteText{CellWidth: 1, Text: text}, 0)
}

func (s *ClientState) writeText(st *StreamState, p *PaneViewState, msg protocol.WriteText, styleID uint32) bool {
	if p == nil {
		return false
	}
	start := st.Column
	for _, r := range string(msg.Text) {
		w := int(msg.CellWidth)
		if st.Column+w > p.Grid.Cols {
			return false
		}
		idx := st.Row*p.Grid.Cols + st.Column
		p.Grid.Cells[idx] = protocol.Cell{Rune: r, StyleID: styleID, Width: msg.CellWidth}
		for n := 1; n < w; n++ {
			p.Grid.Cells[idx+n] = protocol.Cell{StyleID: styleID, Width: 0}
		}
		st.Column += w
	}
	s.markDamageRect(protocol.Rect{X: p.Rect.X + start, Y: p.Rect.Y + st.Row, Width: st.Column - start, Height: 1})
	return true
}
func (s *ClientState) Fill(slot uint8, msg protocol.Fill) bool {
	st, p := s.streamPane(slot)
	if p == nil || st.Column+msg.Columns > p.Grid.Cols {
		return false
	}
	start := st.Column
	end := start + msg.Columns
	for st.Column < end {
		w := int(msg.Width)
		if st.Column+w > end {
			return false
		}
		idx := st.Row*p.Grid.Cols + st.Column
		p.Grid.Cells[idx] = protocol.Cell{Rune: msg.Rune, StyleID: st.StyleID, Width: msg.Width}
		for n := 1; n < w; n++ {
			p.Grid.Cells[idx+n] = protocol.Cell{StyleID: st.StyleID, Width: 0}
		}
		st.Column += w
	}
	s.markDamageRect(protocol.Rect{X: p.Rect.X + start, Y: p.Rect.Y + st.Row, Width: msg.Columns, Height: 1})
	return true
}
func (s *ClientState) UpdateCursor(slot uint8, msg protocol.CursorUpdate) bool {
	p := s.slotPane(slot)
	if p == nil || msg.Cursor.X < 0 || msg.Cursor.X >= p.Grid.Cols || msg.Cursor.Y < 0 || msg.Cursor.Y >= p.Grid.Rows {
		return false
	}
	p.Cursor = msg.Cursor
	p.CursorVisible = msg.Visible
	return true
}
func (s *ClientState) ApplyScroll(slot uint8, delta int) bool {
	p := s.slotPane(slot)
	if p == nil || delta == 0 {
		return p != nil
	}
	rows := p.Grid.Rows
	if delta >= rows || delta <= -rows {
		p.Grid = blankScreen(p.Grid.Cols, p.Grid.Rows)
	} else if delta > 0 {
		shift := delta * p.Grid.Cols
		copy(p.Grid.Cells[shift:], p.Grid.Cells[:len(p.Grid.Cells)-shift])
		fillBlank(p.Grid.Cells[:shift])
	} else {
		shift := -delta * p.Grid.Cols
		copy(p.Grid.Cells, p.Grid.Cells[shift:])
		fillBlank(p.Grid.Cells[len(p.Grid.Cells)-shift:])
	}
	s.markDamageRect(p.Rect)
	return true
}

func (s *ClientState) slotPane(slot uint8) *PaneViewState {
	id, ok := s.RenderSlots[slot]
	if !ok {
		return nil
	}
	return s.Panes[id]
}
func (s *ClientState) streamPane(slot uint8) (*StreamState, *PaneViewState) {
	p := s.slotPane(slot)
	if p == nil {
		return nil, nil
	}
	st := s.Streams[slot]
	if st == nil {
		st = &StreamState{}
		s.Streams[slot] = st
	}
	return st, p
}
func (s *ClientState) markDamageRect(r protocol.Rect) {
	if r.Width > 0 && r.Height > 0 {
		s.pendingDamageRects = append(s.pendingDamageRects, r)
	}
}
func (s *ClientState) ensurePane(id uint64) *PaneViewState {
	if p := s.Panes[id]; p != nil {
		return p
	}
	p := &PaneViewState{PaneID: id, CursorVisible: true, Styles: defaultStyles()}
	s.Panes[id] = p
	return p
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
func sameLayout(a LayoutDescription, b protocol.WindowLayout) bool {
	if a.WindowID != b.WindowID || a.LayoutRevision != b.LayoutRevision || len(a.Panes) != len(b.Panes) {
		return false
	}
	for i := range a.Panes {
		if a.Panes[i] != b.Panes[i] {
			return false
		}
	}
	return true
}
func defaultStyles() map[uint32]protocol.Style {
	return map[uint32]protocol.Style{protocol.CanonicalDefaultStyleID: protocol.CanonicalDefaultStyle()}
}
func blankScreen(cols, rows int) Screen {
	cells := make([]protocol.Cell, max(0, cols*rows))
	fillBlank(cells)
	return Screen{Cols: cols, Rows: rows, Cells: cells}
}
func fillBlank(cells []protocol.Cell) {
	for i := range cells {
		cells[i] = protocol.Cell{Rune: ' ', Width: 1}
	}
}
