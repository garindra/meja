package render

import "tali/internal/protocol"

type Screen struct {
	Cols  int
	Rows  int
	Cells []protocol.Cell
}

type ClientState struct {
	SessionID     uint64
	WindowID      uint64
	PaneID        uint64
	Grid          Screen
	Generation    uint64
	Cursor        protocol.Cursor
	CursorVisible bool
	LastRendered  Screen
	Styles        map[uint32]protocol.Style
}

func NewClientState() *ClientState {
	return &ClientState{
		Styles: map[uint32]protocol.Style{
			0: {
				FG: protocol.Color{Mode: "default"},
				BG: protocol.Color{Mode: "default"},
			},
		},
	}
}

func (s *ClientState) ApplyReplace(msg protocol.ReplacePane) {
	s.SessionID = msg.SessionID
	s.WindowID = msg.WindowID
	s.PaneID = msg.PaneID
	s.Generation = msg.Generation
	s.Cursor = msg.Cursor
	s.CursorVisible = msg.CursorVisible
	s.Grid = Screen{Cols: msg.Cols, Rows: msg.Rows, Cells: append([]protocol.Cell(nil), msg.Cells...)}
	if s.Styles == nil {
		s.Styles = map[uint32]protocol.Style{}
	}
	for _, def := range msg.Styles {
		s.Styles[def.ID] = def.Style
	}
}

func (s *ClientState) ApplySetRun(msg protocol.SetRun) bool {
	if s.Generation != msg.BaseGeneration {
		return false
	}
	if msg.Row < 0 || msg.Row >= s.Grid.Rows || msg.Column < 0 || msg.Column >= s.Grid.Cols {
		return false
	}
	base := msg.Row*s.Grid.Cols + msg.Column
	for i, cell := range msg.Cells {
		idx := base + i
		if idx >= len(s.Grid.Cells) || idx >= (msg.Row+1)*s.Grid.Cols {
			break
		}
		s.Grid.Cells[idx] = cell
	}
	s.Generation = msg.Generation
	return true
}

func (s *ClientState) ApplySetCursor(msg protocol.SetCursor) bool {
	if s.Generation != msg.BaseGeneration {
		return false
	}
	s.Cursor = msg.Cursor
	s.Generation = msg.Generation
	return true
}

func (s *ClientState) ApplySetCursorVisible(msg protocol.SetCursorVisible) bool {
	if s.Generation != msg.BaseGeneration {
		return false
	}
	s.CursorVisible = msg.Visible
	s.Generation = msg.Generation
	return true
}

func (s *ClientState) DefineStyle(msg protocol.DefineStyle) {
	if s.Styles == nil {
		s.Styles = map[uint32]protocol.Style{}
	}
	s.Styles[msg.ID] = msg.Style
}
