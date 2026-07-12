package server

import (
	"fmt"
	"strconv"

	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

type HistorySnapshot struct {
	Rows          []terminal.Row
	Styles        []protocol.StyleDefinition
	Cols          int
	ViewportRows  int
	InitialTop    int
	InitialCursor protocol.Cursor
	CounterStyle  uint32
}

type HistoryView struct {
	Snapshot  *HistorySnapshot
	ViewTop   int
	CursorRow int
	CursorCol int
}

type historyMove struct {
	Delta      int
	OldCounter string
	NewCounter string
	Cursor     protocol.Cursor
	CursorOnly bool
	Exited     bool
	Changed    bool
}

func captureHistorySnapshot(pane *Pane) *HistorySnapshot {
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	history, primary := pane.Terminal.SnapshotRows()
	cols, rows := pane.Terminal.Cols, pane.Terminal.Rows
	projected := projectHistoryRows(history, cols)
	initialTop := len(projected)
	projected = append(projected, normalizeRows(primary, cols)...)
	styles := pane.Terminal.SnapshotStyles()
	styleDefs := make([]protocol.StyleDefinition, 0, len(styles)+1)
	var maxStyleID uint32
	for _, def := range styles {
		styleDefs = append(styleDefs, protocol.StyleDefinition{ID: def.ID, Style: def.Style})
		if def.ID > maxStyleID {
			maxStyleID = def.ID
		}
	}
	counterStyle := maxStyleID + 1
	styleDefs = append(styleDefs, protocol.StyleDefinition{
		ID: counterStyle,
		Style: protocol.Style{
			Bold: true,
			FG:   protocol.Color{Mode: "indexed", Index: 226},
			BG:   protocol.Color{Mode: "default"},
		},
	})
	return &HistorySnapshot{
		Rows:          projected,
		Styles:        styleDefs,
		Cols:          cols,
		ViewportRows:  rows,
		InitialTop:    initialTop,
		InitialCursor: protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY},
		CounterStyle:  counterStyle,
	}
}

func projectHistoryRows(rows []terminal.Row, cols int) []terminal.Row {
	if cols <= 0 {
		return nil
	}
	var out []terminal.Row
	var chain []protocol.Cell
	flush := func() {
		if len(chain) == 0 {
			out = append(out, blankHistoryRow(cols))
			return
		}
		for start := 0; start < len(chain); start += cols {
			end := min(start+cols, len(chain))
			row := blankHistoryRow(cols)
			copy(row.Cells, chain[start:end])
			row.WrapsNext = end < len(chain)
			out = append(out, row)
		}
		chain = nil
	}
	for _, row := range rows {
		cells := row.Cells
		if !row.WrapsNext {
			cells = trimHistoryBlanks(cells)
		}
		chain = append(chain, cells...)
		if !row.WrapsNext {
			flush()
		}
	}
	if len(chain) > 0 {
		flush()
	}
	return out
}

func normalizeRows(rows []terminal.Row, cols int) []terminal.Row {
	out := make([]terminal.Row, len(rows))
	for i, src := range rows {
		out[i] = blankHistoryRow(cols)
		copy(out[i].Cells, src.Cells[:min(len(src.Cells), cols)])
		out[i].WrapsNext = src.WrapsNext
	}
	return out
}

func blankHistoryRow(cols int) terminal.Row {
	row := terminal.Row{Cells: make([]protocol.Cell, cols)}
	for i := range row.Cells {
		row.Cells[i] = protocol.Cell{Rune: ' ', Width: 1}
	}
	return row
}

func trimHistoryBlanks(cells []protocol.Cell) []protocol.Cell {
	end := len(cells)
	for end > 0 && (cells[end-1].Rune == 0 || cells[end-1].Rune == ' ') {
		end--
	}
	return cells[:end]
}

func (s *Session) InstallHistoryView(clientID, paneID uint64, snapshot *HistorySnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil || client.FocusedPaneID != paneID {
		return fmt.Errorf("pane %d is not focused", paneID)
	}
	if snapshot == nil || snapshot.ViewportRows <= 0 || len(snapshot.Rows) < snapshot.ViewportRows {
		return fmt.Errorf("invalid history snapshot for pane %d", paneID)
	}
	if client.HistoryViews == nil {
		client.HistoryViews = map[uint64]*HistoryView{}
	}
	client.HistoryViews[paneID] = &HistoryView{
		Snapshot:  snapshot,
		ViewTop:   snapshot.InitialTop,
		CursorRow: snapshot.InitialTop + snapshot.InitialCursor.Y,
		CursorCol: snapshot.InitialCursor.X,
	}
	return nil
}

func (s *Session) HistoryView(clientID, paneID uint64) *HistoryView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	if client == nil || client.HistoryViews[paneID] == nil {
		return nil
	}
	copyView := *client.HistoryViews[paneID]
	return &copyView
}

func (s *Session) IsHistoryPane(clientID, paneID uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	return client != nil && client.HistoryViews[paneID] != nil
}

func (s *Session) ClearHistoryViews() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, client := range s.Clients {
		client.HistoryViews = map[uint64]*HistoryView{}
	}
}

func (s *Session) moveHistory(clientID, paneID uint64, direction int) (historyMove, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil || client.FocusedPaneID != paneID {
		return historyMove{}, false
	}
	view := client.HistoryViews[paneID]
	if view == nil {
		return historyMove{}, false
	}
	oldCounter := historyCounter(view)
	viewportBottom := view.ViewTop + view.Snapshot.ViewportRows - 1
	move := historyMove{OldCounter: oldCounter}
	if direction < 0 {
		if view.CursorRow > view.ViewTop {
			view.CursorRow--
			move.CursorOnly = true
			move.Changed = true
		} else if view.ViewTop > 0 {
			view.ViewTop--
			view.CursorRow--
			move.Delta = 1
			move.Changed = true
		}
	} else if direction > 0 {
		if view.CursorRow < viewportBottom {
			view.CursorRow++
			move.CursorOnly = true
			move.Changed = true
		} else if view.ViewTop < view.Snapshot.InitialTop {
			view.ViewTop++
			view.CursorRow++
			move.Delta = -1
			move.Changed = true
		}
	}
	move.NewCounter = historyCounter(view)
	move.Cursor = protocol.Cursor{X: min(view.CursorCol, view.Snapshot.Cols-1), Y: view.CursorRow - view.ViewTop}
	return move, true
}

func (s *Session) jumpHistory(clientID, paneID uint64, oldest bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil || client.FocusedPaneID != paneID {
		return false
	}
	view := client.HistoryViews[paneID]
	if view == nil {
		return false
	}
	if oldest {
		view.ViewTop = 0
		view.CursorRow = 0
		view.CursorCol = 0
	} else {
		view.ViewTop = view.Snapshot.InitialTop
		view.CursorRow = view.ViewTop + view.Snapshot.ViewportRows - 1
		view.CursorCol = 0
	}
	return true
}

func (s *Session) exitHistoryAndRebuild(clientID, paneID uint64) ([]RenderBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil || client.HistoryViews[paneID] == nil {
		return nil, false
	}
	delete(client.HistoryViews, paneID)
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, false
	}
	s.rebuildBindingsLocked(client, window)
	return append([]RenderBinding(nil), client.RenderBindings...), true
}

func historyCounter(view *HistoryView) string {
	uncovered := view.Snapshot.InitialTop - view.ViewTop
	return "[" + strconv.Itoa(uncovered) + "/" + strconv.Itoa(view.Snapshot.InitialTop) + "]"
}

func historyViewport(view *HistoryView) []protocol.Cell {
	snapshot := view.Snapshot
	cells := make([]protocol.Cell, 0, snapshot.Cols*snapshot.ViewportRows)
	for row := 0; row < snapshot.ViewportRows; row++ {
		cells = append(cells, snapshot.Rows[view.ViewTop+row].Cells...)
	}
	overlayHistoryCounter(cells[:snapshot.Cols], snapshot.Cols, historyCounter(view), snapshot.CounterStyle)
	return cells
}

func overlayHistoryCounter(row []protocol.Cell, cols int, label string, styleID uint32) {
	if len(label) > cols {
		label = label[len(label)-cols:]
	}
	start := max(0, cols-len(label))
	for i, r := range label {
		if start+i >= len(row) {
			break
		}
		row[start+i] = protocol.Cell{Rune: r, StyleID: styleID, Width: 1}
	}
}
