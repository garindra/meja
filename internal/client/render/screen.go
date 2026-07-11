package render

import "tali/internal/protocol"

type Screen struct {
	Cols  int
	Rows  int
	Cells []protocol.Cell
}

type ClientState struct {
	SessionID uint64

	ActiveWindowID uint64
	LastWindowID   uint64
	FocusedPaneID  uint64
	Windows        []protocol.WindowInfo

	Grid          Screen
	Generation    uint64
	Cursor        protocol.Cursor
	CursorVisible bool

	TerminalCols int
	TerminalRows int

	BindingGeneration uint64

	LastRendered Screen
	Styles       map[uint32]protocol.Style
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
	if s.ActiveWindowID != 0 && s.ActiveWindowID != msg.WindowID {
		s.LastWindowID = s.ActiveWindowID
	}
	s.ActiveWindowID = msg.WindowID
	s.FocusedPaneID = msg.PaneID
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
	if s.LastWindowID == windowID {
		s.LastWindowID = 0
	}
	s.syncWindowSelection()
}

func (s *ClientState) ApplyBind(msg protocol.BindRenderStream) {
	s.SessionID = msg.SessionID
	s.FocusedPaneID = msg.PaneID
	s.BindingGeneration = msg.BindingGeneration
	s.Grid = Screen{}
	s.Generation = 0
	s.Cursor = protocol.Cursor{}
	s.CursorVisible = true
	s.LastRendered = Screen{}
	s.Styles = map[uint32]protocol.Style{
		0: {
			FG: protocol.Color{Mode: "default"},
			BG: protocol.Color{Mode: "default"},
		},
	}
}

func (s *ClientState) ApplyReplace(msg protocol.ReplacePane) bool {
	if msg.BindingGeneration != s.BindingGeneration {
		return false
	}
	s.SessionID = msg.SessionID
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
	return true
}

func (s *ClientState) ApplySetRun(msg protocol.SetRun) bool {
	if msg.BindingGeneration != s.BindingGeneration || s.Generation != msg.BaseGeneration {
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
	if msg.BindingGeneration != s.BindingGeneration || s.Generation != msg.BaseGeneration {
		return false
	}
	s.Cursor = msg.Cursor
	s.Generation = msg.Generation
	return true
}

func (s *ClientState) ApplySetCursorVisible(msg protocol.SetCursorVisible) bool {
	if msg.BindingGeneration != s.BindingGeneration || s.Generation != msg.BaseGeneration {
		return false
	}
	s.CursorVisible = msg.Visible
	s.Generation = msg.Generation
	return true
}

func (s *ClientState) DefineStyle(msg protocol.DefineStyle) bool {
	if msg.BindingGeneration != s.BindingGeneration {
		return false
	}
	if s.Styles == nil {
		s.Styles = map[uint32]protocol.Style{}
	}
	s.Styles[msg.ID] = msg.Style
	return true
}

func (s *ClientState) syncWindowSelection() {
	for i := range s.Windows {
		s.Windows[i].Active = s.Windows[i].WindowID == s.ActiveWindowID
		if s.Windows[i].Active && s.FocusedPaneID != 0 && s.Windows[i].PaneID != s.FocusedPaneID {
			s.Windows[i].PaneID = s.FocusedPaneID
		}
	}
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
	if s.LastWindowID == 0 || s.LastWindowID == s.ActiveWindowID {
		return 0, false
	}
	for _, w := range s.Windows {
		if w.WindowID == s.LastWindowID {
			return w.WindowID, true
		}
	}
	return 0, false
}
