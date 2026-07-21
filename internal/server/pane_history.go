package server

import (
	"bytes"
	"fmt"
	"io"
	"strconv"

	"github.com/garindra/meja/internal/protocol"
)

type paneHistorySnapshot struct {
	grid          rowStore
	clusters      *clusterStore
	styles        []protocol.StyleDefinition
	Cols          int
	ViewportRows  int
	InitialTop    int
	InitialCursor protocol.Cursor
	CounterStyle  uint32
}

type paneHistoryView struct {
	Snapshot  *paneHistorySnapshot
	ViewTop   int
	CursorRow int
	CursorCol int
	Selection *paneHistorySelection
}

type paneHistoryPosition struct {
	Row int
	Col int
}

type paneHistorySelection struct {
	Anchor       paneHistoryPosition
	Head         paneHistoryPosition
	ExitOnFinish bool
	CopySingle   bool
}

const maxClipboardSelectionBytes = 1 << 20

type historyMove struct {
	Delta      int
	OldCounter string
	NewCounter string
	Cursor     protocol.Cursor
	Changed    bool
}

func captureTerminalHistorySnapshot(state *TerminalState) *paneHistorySnapshot {
	cols, rows := state.Cols, state.Rows
	grid := state.grid.clone(cols, &state.clusters)
	initialTop := int(grid.count) - rows
	styleDefs := make([]protocol.StyleDefinition, 0, len(state.styleByID)+1)
	for id, style := range state.styleByID {
		styleID := uint32(id)
		styleDefs = append(styleDefs, protocol.StyleDefinition{ID: styleID, Style: style})
	}
	counterStyle := uint32(len(state.styleByID))
	counterDefinition := protocol.StyleDefinition{
		ID: counterStyle,
		Style: protocol.Style{
			Bold: true,
			FG:   protocol.Color{Mode: "indexed", Index: 226},
			BG:   protocol.Color{Mode: "default"},
		},
	}
	styleDefs = append(styleDefs, counterDefinition)
	return &paneHistorySnapshot{
		grid:          grid,
		clusters:      &state.clusters,
		styles:        styleDefs,
		Cols:          cols,
		ViewportRows:  rows,
		InitialTop:    initialTop,
		InitialCursor: protocol.Cursor{X: state.CursorX, Y: state.CursorY},
		CounterStyle:  counterStyle,
	}
}

func (p *Pane) enterHistoryMode() (bool, error) {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistoryEnter})
	return result.Changed, err
}

func (s *paneHistorySnapshot) row(row int) []cellWord {
	return s.grid.logicalRow(row, s.Cols)
}

func (s *paneHistorySnapshot) LookupStyle(id uint32) (protocol.Style, bool) {
	if id&historySelectionStyleMask != 0 {
		style, ok := s.LookupStyle(id &^ historySelectionStyleMask)
		if !ok {
			return protocol.Style{}, false
		}
		style = protocol.Style{
			FG: protocol.Color{Mode: "indexed", Index: 0},
			BG: protocol.Color{Mode: "indexed", Index: 226},
		}
		return style, true
	}
	if uint64(id) >= uint64(len(s.styles)) || s.styles[id].ID != id {
		return protocol.Style{}, false
	}
	return s.styles[id].Style, true
}

func (s *paneHistorySnapshot) release() {
	if s != nil && s.clusters != nil {
		s.grid.release(s.Cols, s.clusters)
		s.clusters = nil
	}
}

func (p *Pane) installHistoryView(snapshot *paneHistorySnapshot) error {
	if snapshot == nil || snapshot.ViewportRows <= 0 || int(snapshot.grid.count) < snapshot.ViewportRows {
		return fmt.Errorf("invalid history snapshot for pane %d", p.ID)
	}
	p.historyView = &paneHistoryView{
		Snapshot:  snapshot,
		ViewTop:   snapshot.InitialTop,
		CursorRow: snapshot.InitialTop + snapshot.InitialCursor.Y,
		CursorCol: snapshot.InitialCursor.X,
	}
	p.viewMode.Store(uint32(paneViewHistory))
	return nil
}

func (p *Pane) isHistoryMode() bool {
	return p != nil && p.currentViewMode() == paneViewHistory
}

func (p *Pane) currentViewMode() paneViewMode {
	if p == nil {
		return paneViewLive
	}
	return paneViewMode(p.viewMode.Load())
}

func (s *Session) exitAllPaneHistoryModes() error {
	for _, pane := range s.Panes {
		if _, err := pane.exitHistoryMode(); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pane) moveHistory(direction int) (historyMove, bool) {
	view := p.historyView
	if view == nil {
		return historyMove{}, false
	}
	oldCounter := historyCounter(view)
	viewportBottom := view.ViewTop + view.Snapshot.ViewportRows - 1
	move := historyMove{OldCounter: oldCounter}
	if direction < 0 {
		if view.CursorRow > view.ViewTop {
			view.CursorRow--
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

func (p *Pane) jumpHistory(oldest bool) bool {
	view := p.historyView
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

func (p *Pane) exitHistoryModeNow() bool {
	if !p.isHistoryMode() {
		return false
	}
	p.historyView.Snapshot.release()
	p.historyView = nil
	p.viewMode.Store(uint32(paneViewLive))
	return true
}

func (p *Pane) exitHistoryMode() (bool, error) {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistoryExit})
	return result.Changed, err
}

func (p *Pane) handleHistoryInput(data []byte) ([]byte, error) {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistoryInput, Data: append([]byte(nil), data...)})
	return result.Data, err
}

func (p *Pane) sendHistoryRequest(request paneHistoryRequest) (paneHistoryResult, error) {
	if p.commands == nil {
		result := p.handleHistoryRequest(&request)
		return result, result.Err
	}
	result := make(chan paneHistoryResult, 1)
	request.Result = result
	select {
	case p.commands <- paneCommand{history: &request}:
	case <-p.mainDone:
		return paneHistoryResult{}, io.ErrClosedPipe
	case <-p.done:
		return paneHistoryResult{}, io.ErrClosedPipe
	}
	select {
	case outcome := <-result:
		return outcome, outcome.Err
	case <-p.mainDone:
		return paneHistoryResult{}, io.ErrClosedPipe
	case <-p.done:
		return paneHistoryResult{}, io.ErrClosedPipe
	}
}

func (p *Pane) handleHistoryRequest(request *paneHistoryRequest) paneHistoryResult {
	switch request.Action {
	case paneHistoryEnter:
		if p.isHistoryMode() {
			return paneHistoryResult{}
		}
		err := p.installHistoryView(captureTerminalHistorySnapshot(p.terminal))
		return paneHistoryResult{Changed: err == nil, Err: err}
	case paneHistoryExit:
		changed := p.exitHistoryModeNow()
		return paneHistoryResult{Changed: changed}
	case paneHistoryInput:
		return p.handleHistoryInputNow(request.Data)
	case paneHistorySelectionBegin:
		return p.beginHistorySelectionNow(request.Row, request.Column, request.Auto)
	case paneHistorySelectionUpdate:
		return p.updateHistorySelectionNow(request.Row, request.Column)
	case paneHistorySelectionFinish:
		return p.finishHistorySelectionNow()
	case paneHistorySelectionCancel:
		return p.cancelHistorySelectionNow()
	case paneHistorySelectionBeginCursor:
		return p.beginHistorySelectionAtCursorNow(request.Auto)
	case paneHistorySelectionCopy:
		return p.finishHistorySelectionNow(request.Cancel)
	case paneHistorySelectionClear:
		return p.clearHistorySelectionNow()
	default:
		return paneHistoryResult{Err: fmt.Errorf("pane %d received invalid history action %d", p.ID, request.Action)}
	}
}

func (p *Pane) beginHistorySelection(row, column int, auto bool) error {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistorySelectionBegin, Row: row, Column: column, Auto: auto})
	if err != nil {
		return err
	}
	return result.Err
}

func (p *Pane) updateHistorySelection(row, column int) error {
	_, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistorySelectionUpdate, Row: row, Column: column})
	return err
}

func (p *Pane) finishHistorySelection() ([]byte, error) {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistorySelectionFinish})
	if err != nil {
		return nil, err
	}
	return result.Data, result.Err
}

func (p *Pane) cancelHistorySelection() error {
	_, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistorySelectionCancel})
	return err
}

func (p *Pane) beginHistorySelectionAtCursor(auto bool) error {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistorySelectionBeginCursor, Auto: auto})
	if err != nil {
		return err
	}
	return result.Err
}

func (p *Pane) copyHistorySelection(cancel bool) ([]byte, error) {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistorySelectionCopy, Cancel: cancel})
	if err != nil {
		return nil, err
	}
	return result.Data, result.Err
}

func (p *Pane) clearHistorySelection() error {
	result, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistorySelectionClear})
	if err != nil {
		return err
	}
	return result.Err
}

func (p *Pane) beginHistorySelectionNow(row, column int, auto bool) paneHistoryResult {
	if !p.isHistoryMode() {
		if err := p.installHistoryView(captureTerminalHistorySnapshot(p.terminal)); err != nil {
			return paneHistoryResult{Err: err}
		}
	}
	view := p.historyView
	position := view.pointerPosition(row, column)
	view.Selection = &paneHistorySelection{Anchor: position, Head: position, ExitOnFinish: auto}
	return paneHistoryResult{Changed: true}
}

func (p *Pane) beginHistorySelectionAtCursorNow(auto bool) paneHistoryResult {
	view := p.historyView
	if view == nil {
		return paneHistoryResult{}
	}
	position := view.cursorPosition()
	view.Selection = &paneHistorySelection{Anchor: position, Head: position, ExitOnFinish: auto}
	return paneHistoryResult{Changed: true}
}

func (p *Pane) clearHistorySelectionNow() paneHistoryResult {
	view := p.historyView
	if view == nil || view.Selection == nil {
		return paneHistoryResult{}
	}
	view.Selection = nil
	return paneHistoryResult{Changed: true}
}

func (p *Pane) updateHistorySelectionNow(row, column int) paneHistoryResult {
	view := p.historyView
	if view == nil || view.Selection == nil {
		return paneHistoryResult{}
	}
	position := view.pointerPosition(row, column)
	if position == view.Selection.Head {
		return paneHistoryResult{}
	}
	view.Selection.Head = position
	return paneHistoryResult{Changed: true}
}

func (p *Pane) finishHistorySelectionNow(forceCancel ...bool) paneHistoryResult {
	view := p.historyView
	if view == nil || view.Selection == nil {
		return paneHistoryResult{}
	}
	selection := *view.Selection
	if len(forceCancel) > 0 && forceCancel[0] {
		selection.ExitOnFinish = true
	}
	var data []byte
	var err error
	if selection.Anchor != selection.Head || selection.CopySingle {
		data, err = extractHistorySelection(view.Snapshot, selection)
	}
	resultErr := err
	if selection.ExitOnFinish {
		p.exitHistoryModeNow()
	} else {
		view.Selection = nil
	}
	return paneHistoryResult{Changed: true, Data: data, Err: resultErr}
}

func (p *Pane) cancelHistorySelectionNow() paneHistoryResult {
	view := p.historyView
	if view == nil || view.Selection == nil {
		return paneHistoryResult{}
	}
	if view.Selection.ExitOnFinish {
		p.exitHistoryModeNow()
	} else {
		view.Selection = nil
	}
	return paneHistoryResult{Changed: true}
}

func (v *paneHistoryView) pointerPosition(row, column int) paneHistoryPosition {
	row = min(max(row, 0), v.Snapshot.ViewportRows-1)
	column = min(max(column, 0), v.Snapshot.Cols-1)
	logicalRow := v.ViewTop + row
	cells := v.Snapshot.row(logicalRow)
	for column > 0 && cells[column].width() == 0 {
		column--
	}
	return paneHistoryPosition{Row: logicalRow, Col: column}
}

func (v *paneHistoryView) cursorPosition() paneHistoryPosition {
	column := min(max(v.CursorCol, 0), v.Snapshot.Cols-1)
	cells := v.Snapshot.row(v.CursorRow)
	for column > 0 && cells[column].width() == 0 {
		column--
	}
	return paneHistoryPosition{Row: v.CursorRow, Col: column}
}

func normalizedHistorySelection(selection paneHistorySelection) (paneHistoryPosition, paneHistoryPosition) {
	start, end := selection.Anchor, selection.Head
	if start.Row > end.Row || start.Row == end.Row && start.Col > end.Col {
		start, end = end, start
	}
	return start, end
}

func extractHistorySelection(snapshot *paneHistorySnapshot, selection paneHistorySelection) ([]byte, error) {
	start, end := normalizedHistorySelection(selection)
	var out bytes.Buffer
	for row := start.Row; row <= end.Row; row++ {
		first, last := 0, snapshot.Cols-1
		if row == start.Row {
			first = start.Col
		}
		if row == end.Row {
			last = end.Col
		}
		var line bytes.Buffer
		cells := snapshot.row(row)
		for column := first; column <= last; column++ {
			cell := cells[column]
			if cell.width() == 0 {
				continue
			}
			text := cellTextFromStore(cell, snapshot.clusters)
			if text == "" {
				text = " "
			}
			line.WriteString(text)
		}
		lineBytes := line.Bytes()
		if !snapshot.grid.logicalWrapped(row) || row == end.Row {
			lineBytes = bytes.TrimRight(lineBytes, " ")
		}
		if out.Len()+len(lineBytes) > maxClipboardSelectionBytes {
			return nil, fmt.Errorf("selection exceeds %d bytes", maxClipboardSelectionBytes)
		}
		out.Write(lineBytes)
		if row < end.Row && !snapshot.grid.logicalWrapped(row) {
			if out.Len()+1 > maxClipboardSelectionBytes {
				return nil, fmt.Errorf("selection exceeds %d bytes", maxClipboardSelectionBytes)
			}
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), nil
}

func (p *Pane) handleHistoryInputNow(data []byte) paneHistoryResult {
	for len(data) > 0 {
		if data[0] == ' ' {
			view := p.historyView
			if view == nil {
				return paneHistoryResult{}
			}
			position := view.cursorPosition()
			view.Selection = &paneHistorySelection{Anchor: position, Head: position, ExitOnFinish: true, CopySingle: true}
			data = data[1:]
			continue
		}
		if data[0] == '\r' || data[0] == '\n' {
			result := p.finishHistorySelectionNow()
			if result.Changed || result.Err != nil {
				return result
			}
			data = data[1:]
			continue
		}
		if delta, consumed := decodeHistoryHorizontalInput(data); consumed > 0 {
			view := p.historyView
			view.CursorCol = min(max(view.CursorCol+delta, 0), view.Snapshot.Cols-1)
			if view.Selection != nil {
				view.Selection.Head = view.cursorPosition()
			}
			data = data[min(consumed, len(data)):]
			continue
		}
		direction, count, exit, consumed := decodeHistoryInput(data)
		if consumed <= 0 {
			consumed = 1
		}
		data = data[min(consumed, len(data)):]
		if exit {
			exited := p.exitHistoryModeNow()
			return paneHistoryResult{Changed: exited}
		}
		if count < 0 {
			if p.jumpHistory(count == -1) {
				if p.historyView.Selection != nil {
					p.historyView.Selection.Head = p.historyView.cursorPosition()
				}
			}
			continue
		}
		for i := 0; i < count; i++ {
			move, ok := p.moveHistory(direction)
			if !ok {
				return paneHistoryResult{}
			}
			if !move.Changed {
				break
			}
			if p.historyView.Selection != nil {
				p.historyView.Selection.Head = p.historyView.cursorPosition()
			}
		}
	}
	return paneHistoryResult{}
}

func decodeHistoryHorizontalInput(data []byte) (delta, consumed int) {
	if len(data) == 0 {
		return 0, 0
	}
	switch data[0] {
	case 'h':
		return -1, 1
	case 'l':
		return 1, 1
	case 0x1b:
		if len(data) >= 3 && data[1] == '[' {
			switch data[2] {
			case 'D':
				return -1, 3
			case 'C':
				return 1, 3
			}
		}
	}
	return 0, 0
}

func decodeHistoryInput(data []byte) (direction, count int, exit bool, consumed int) {
	if len(data) == 0 {
		return 0, 0, false, 0
	}
	switch data[0] {
	case 'q', 0x03, 0x1b:
		if len(data) >= 3 && data[0] == 0x1b && data[1] == '[' {
			switch data[2] {
			case 'A':
				return -1, 1, false, 3
			case 'B':
				return 1, 1, false, 3
			case '5', '6':
				if len(data) >= 4 && data[3] == '~' {
					direction = -1
					if data[2] == '6' {
						direction = 1
					}
					return direction, 12, false, 4
				}
			}
		}
		return 0, 0, true, 1
	case 0x15:
		return -1, 6, false, 1
	case 0x04:
		return 1, 6, false, 1
	case 'g':
		return 0, -1, false, 1
	case 'G':
		return 0, -2, false, 1
	case 'k':
		return -1, 1, false, 1
	case 'j':
		return 1, 1, false, 1
	default:
		return 0, 0, false, 1
	}
}

func historyCounter(view *paneHistoryView) string {
	uncovered := view.Snapshot.InitialTop - view.ViewTop
	return "[" + strconv.Itoa(uncovered) + "/" + strconv.Itoa(view.Snapshot.InitialTop) + "]"
}
