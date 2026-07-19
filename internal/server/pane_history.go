package server

import (
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
}

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

func (p *Pane) handleHistoryInput(data []byte) error {
	_, err := p.sendHistoryRequest(paneHistoryRequest{Action: paneHistoryInput, Data: append([]byte(nil), data...)})
	return err
}

func (p *Pane) sendHistoryRequest(request paneHistoryRequest) (paneHistoryResult, error) {
	if p.commands == nil {
		result := p.handleHistoryRequest(nil, &request)
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

func (p *Pane) handleHistoryRequest(output *renderOutput, request *paneHistoryRequest) paneHistoryResult {
	switch request.Action {
	case paneHistoryEnter:
		if p.isHistoryMode() {
			return paneHistoryResult{}
		}
		err := p.installHistoryView(captureTerminalHistorySnapshot(p.terminal))
		if err == nil && output != nil {
			err = p.renderHistorySnapshot(output)
		}
		return paneHistoryResult{Changed: err == nil, Err: err}
	case paneHistoryExit:
		changed := p.exitHistoryModeNow()
		var err error
		if changed && output != nil {
			err = sendFullRender(output, p)
		}
		return paneHistoryResult{Changed: changed, Err: err}
	case paneHistoryInput:
		return p.handleHistoryInputNow(output, request.Data)
	default:
		return paneHistoryResult{Err: fmt.Errorf("pane %d received invalid history action %d", p.ID, request.Action)}
	}
}

func (p *Pane) handleHistoryInputNow(output *renderOutput, data []byte) paneHistoryResult {
	for len(data) > 0 {
		direction, count, exit, consumed := decodeHistoryInput(data)
		if consumed <= 0 {
			consumed = 1
		}
		data = data[min(consumed, len(data)):]
		if exit {
			exited := p.exitHistoryModeNow()
			var err error
			if exited && output != nil {
				err = sendFullRender(output, p)
			}
			return paneHistoryResult{Changed: exited, Err: err}
		}
		if count < 0 {
			if p.jumpHistory(count == -1) {
				if output != nil {
					if err := p.renderHistorySnapshot(output); err != nil {
						return paneHistoryResult{Err: err}
					}
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
			if output != nil {
				if err := renderHistoryMove(output, p.historyView, move); err != nil {
					return paneHistoryResult{Err: err}
				}
			}
		}
	}
	return paneHistoryResult{}
}

func (p *Pane) renderHistorySnapshot(output *renderOutput) error {
	if !p.isHistoryMode() || p.historyView == nil {
		return fmt.Errorf("pane %d has no history view", p.ID)
	}
	return sendHistorySnapshot(output, p, p.historyView)
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
	default:
		return 0, 0, false, 1
	}
}

func historyCounter(view *paneHistoryView) string {
	uncovered := view.Snapshot.InitialTop - view.ViewTop
	return "[" + strconv.Itoa(uncovered) + "/" + strconv.Itoa(view.Snapshot.InitialTop) + "]"
}
