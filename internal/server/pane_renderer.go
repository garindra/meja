package server

import (
	"fmt"
	"io"
	"sort"
	"time"

	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

func relayPTYToTerminal(pane *Pane, updates chan<- terminal.Update) {
	defer close(updates)
	buf := make([]byte, 32*1024)
	for {
		n, err := pane.PTY.Read(buf)
		if n > 0 {
			pane.terminalMu.Lock()
			update := pane.Terminal.Apply(buf[:n])
			pane.terminalMu.Unlock()
			for _, reply := range update.Replies {
				if _, err := pane.WriteInput(reply); err != nil {
					return
				}
			}
			update.Replies = nil
			updates <- update
		}
		if err != nil {
			return
		}
	}
}

func (s *sessionState) runPaneRenderer(pane *Pane, updates <-chan terminal.Update) {
	defer close(pane.rendererDone)
	var output *renderOutput
	var aggregate terminal.Update
	var idle, maxAge *time.Timer
	var idleC, maxC <-chan time.Time
	pending := false
	stop := func(timer *time.Timer) {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	flush := func() {
		if !pending {
			return
		}
		stop(idle)
		stop(maxAge)
		idle, idleC, maxAge, maxC = nil, nil, nil, nil
		current := aggregate
		aggregate = terminal.Update{}
		pending = false
		if output != nil {
			if err := s.emitTerminalUpdate(output, pane, current); err != nil {
				output = nil
				pane.outputStream = nil
			}
		}
	}
	for {
		select {
		case command := <-pane.renderCommands:
			stop(idle)
			stop(maxAge)
			idle, idleC, maxAge, maxC = nil, nil, nil, nil
			aggregate = terminal.Update{}
			pending = false
			if command.release != nil {
				pane.outputStream = nil
				output = nil
				command.release.acknowledge()
				continue
			}
			if command.detach != nil {
				if pane.outputStream == command.detach {
					pane.outputStream = nil
					output = nil
				}
				command.done <- nil
				continue
			}
			if command.attach != nil {
				pane.outputStream = command.attach
				output = newRenderOutput(command.attach)
				if command.refresh != nil {
					if err := command.refresh(output); err != nil {
						pane.outputStream = nil
						output = nil
					}
				}
				continue
			}
			if command.apply != nil && output != nil {
				err := command.apply(output)
				if err != nil {
					pane.outputStream = nil
					output = nil
				}
				command.done <- err
			} else {
				command.done <- nil
			}
		case update, ok := <-updates:
			if !ok {
				flush()
				return
			}
			mergeTerminalUpdate(&aggregate, update)
			if !pending {
				pending = true
				maxAge = time.NewTimer(renderMaxBatchAge)
				maxC = maxAge.C
			}
			stop(idle)
			idle = time.NewTimer(renderIdleFlush)
			idleC = idle.C
		case <-idleC:
			flush()
		case <-maxC:
			flush()
		}
	}
}

func mergeTerminalUpdate(dst *terminal.Update, src terminal.Update) {
	if dst.DirtyRows == nil {
		dst.DirtyRows = make(map[int]struct{})
	}
	if dst.DirtySpans == nil {
		dst.DirtySpans = make(map[int]terminal.DirtySpan)
	}
	if dst.DefinedStyles == nil {
		dst.DefinedStyles = make(map[uint32]terminal.Style)
	}
	for row := range src.DirtyRows {
		dst.DirtyRows[row] = struct{}{}
	}
	for row, span := range src.DirtySpans {
		current, ok := dst.DirtySpans[row]
		if !ok {
			dst.DirtySpans[row] = span
			continue
		}
		if span.Start < current.Start {
			current.Start = span.Start
		}
		if span.End > current.End {
			current.End = span.End
		}
		dst.DirtySpans[row] = current
	}
	for id, style := range src.DefinedStyles {
		dst.DefinedStyles[id] = style
	}
	dst.FullRedraw = dst.FullRedraw || src.FullRedraw
	dst.CursorChanged = dst.CursorChanged || src.CursorChanged
	dst.VisibleChange = dst.VisibleChange || src.VisibleChange
}

func (s *sessionState) emitHistoryMove(pane *Pane, move historyMove) error {
	view := s.session.HistoryView(clientID0, pane.ID)
	if view == nil {
		return nil
	}
	return pane.applyRender(func(output *renderOutput) error {
		pane.terminalMu.Lock()
		defer pane.terminalMu.Unlock()
		if move.Delta != 0 {
			if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeScroll, Delta: move.Delta}); err != nil {
				return err
			}
		}
		compiler := newDisplayCompiler(output, styleDefinitionsMap(view.Snapshot.Styles))
		for _, run := range historyMoveRuns(view, move) {
			if err := compiler.writeCells(run.Row, run.Column, run.Cells); err != nil {
				return err
			}
		}
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: move.Cursor, Visible: true}}); err != nil {
			return err
		}
		return output.present()
	})
}

type displayCellRun struct {
	Row, Column int
	Cells       []protocol.Cell
}

func historyMoveRuns(view *HistoryView, move historyMove) []displayCellRun {
	if move.Delta == 0 {
		return nil
	}
	snapshot := view.Snapshot
	runs := make([]displayCellRun, 0, 2)
	if move.Delta > 0 {
		cells := append([]protocol.Cell(nil), snapshot.Rows[view.ViewTop].Cells...)
		overlayHistoryCounter(cells, snapshot.Cols, move.NewCounter, snapshot.CounterStyle)
		runs = append(runs, displayCellRun{Row: 0, Cells: cells})
		if snapshot.ViewportRows > 1 {
			runs = append(runs, historyCounterRun(view, 1, move.OldCounter, ""))
		}
		return runs
	}
	bottom := snapshot.ViewportRows - 1
	rowIndex := view.ViewTop + bottom
	runs = append(runs, displayCellRun{Row: bottom, Cells: append([]protocol.Cell(nil), snapshot.Rows[rowIndex].Cells...)})
	runs = append(runs, historyCounterRun(view, 0, move.OldCounter, move.NewCounter))
	return runs
}

func historyCounterRun(view *HistoryView, viewportRow int, oldLabel, newLabel string) displayCellRun {
	snapshot := view.Snapshot
	width := max(len(oldLabel), len(newLabel))
	start := max(0, snapshot.Cols-width)
	rowIndex := view.ViewTop + viewportRow
	cells := append([]protocol.Cell(nil), snapshot.Rows[rowIndex].Cells[start:]...)
	if newLabel != "" {
		overlayHistoryCounter(cells, len(cells), newLabel, snapshot.CounterStyle)
	}
	return displayCellRun{Row: viewportRow, Column: start, Cells: cells}
}

func styleDefinitionsMap(defs []protocol.StyleDefinition) map[uint32]protocol.Style {
	styles := make(map[uint32]protocol.Style, len(defs))
	for _, def := range defs {
		styles[def.ID] = def.Style
	}
	return styles
}

func (s *sessionState) emitTerminalUpdate(output *renderOutput, pane *Pane, update terminal.Update) error {
	if s.session.IsHistoryPane(clientID0, pane.ID) || s.windowForPane(pane.ID) == nil {
		return nil
	}
	if update.FullRedraw {
		return s.sendFullRender(output, pane)
	}
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()

	rows := make([]int, 0, len(update.DirtySpans))
	for row := range update.DirtySpans {
		rows = append(rows, row)
	}
	sort.Ints(rows)
	runs := make([]displayCellRun, 0, len(rows))
	for _, row := range rows {
		span := update.DirtySpans[row]
		start := row*pane.Terminal.Cols + span.Start
		end := row*pane.Terminal.Cols + span.End
		runs = append(runs, displayCellRun{Row: row, Column: span.Start, Cells: append([]protocol.Cell(nil), pane.Terminal.Cells[start:end]...)})
	}
	neededStyles := make(map[uint32]struct{})
	for id := range update.DefinedStyles {
		neededStyles[id] = struct{}{}
	}
	for _, run := range runs {
		for _, cell := range run.Cells {
			neededStyles[cell.StyleID] = struct{}{}
		}
	}
	styleByID := make(map[uint32]protocol.Style)
	for _, def := range pane.Terminal.SnapshotStyles() {
		styleByID[def.ID] = def.Style
	}
	styleIDs := make([]int, 0, len(neededStyles))
	for id := range neededStyles {
		styleIDs = append(styleIDs, int(id))
	}
	sort.Ints(styleIDs)
	styles := make([]protocol.StyleDefinition, 0, len(styleIDs))
	for _, rawID := range styleIDs {
		id := uint32(rawID)
		style, ok := styleByID[id]
		if !ok {
			return fmt.Errorf("terminal style %d is undefined", id)
		}
		styles = append(styles, protocol.StyleDefinition{ID: id, Style: style})
	}
	if len(styles) == 0 && len(runs) == 0 && !update.CursorChanged && !update.VisibleChange {
		return nil
	}
	for _, def := range styles {
		if err := installStyle(output, def.ID, def.Style); err != nil {
			return err
		}
	}
	compiler := newDisplayCompiler(output, styleByID)
	for _, run := range runs {
		if err := compiler.writeCells(run.Row, run.Column, run.Cells); err != nil {
			return err
		}
	}
	if update.CursorChanged || update.VisibleChange {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY}, Visible: pane.Terminal.CursorVisible}}); err != nil {
			return err
		}
	}
	return output.present()
}

func (s *sessionState) sendFullRender(output *renderOutput, pane *Pane) error {
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	styleDefs := pane.Terminal.SnapshotStyles()
	styleByID := make(map[uint32]protocol.Style, len(styleDefs))
	for _, def := range styleDefs {
		styleByID[def.ID] = def.Style
		if err := installStyle(output, def.ID, def.Style); err != nil {
			return err
		}
	}
	compiler := newDisplayCompiler(output, styleByID)
	for row := 0; row < pane.Terminal.Rows; row++ {
		start := row * pane.Terminal.Cols
		if err := compiler.writeCells(row, 0, pane.Terminal.Cells[start:start+pane.Terminal.Cols]); err != nil {
			return err
		}
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY}, Visible: pane.Terminal.CursorVisible}}); err != nil {
		return err
	}
	return output.present()
}

func (s *sessionState) sendCurrentViewSnapshot(output *renderOutput, pane *Pane) error {
	if view := s.session.HistoryView(clientID0, pane.ID); view != nil {
		return s.sendHistorySnapshot(output, pane, view)
	}
	return s.sendFullRender(output, pane)
}

func (s *sessionState) sendHistorySnapshot(output *renderOutput, pane *Pane, view *HistoryView) error {
	pane.terminalMu.Lock()
	defer pane.terminalMu.Unlock()
	snapshot := view.Snapshot
	for _, def := range snapshot.Styles {
		if err := installStyle(output, def.ID, def.Style); err != nil {
			return err
		}
	}
	cells := historyViewport(view)
	compiler := newDisplayCompiler(output, styleDefinitionsMap(snapshot.Styles))
	for row := 0; row < snapshot.ViewportRows; row++ {
		start := row * snapshot.Cols
		if err := compiler.writeCells(row, 0, cells[start:start+snapshot.Cols]); err != nil {
			return err
		}
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: min(view.CursorCol, snapshot.Cols-1), Y: view.CursorRow - view.ViewTop}, Visible: true}}); err != nil {
		return err
	}
	return output.present()
}

func (s *sessionState) sendHistorySnapshotSerialized(pane *Pane, view *HistoryView) error {
	return pane.applyRender(func(output *renderOutput) error {
		return s.sendHistorySnapshot(output, pane, view)
	})
}

func installStyle(output *renderOutput, id uint32, style protocol.Style) error {
	if id == protocol.CanonicalDefaultStyleID && !protocol.IsCanonicalDefaultStyle(style) {
		return fmt.Errorf("style %d must be canonical default", id)
	}
	if installed, ok := output.installedStyles[id]; ok && installed == style {
		return nil
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeStyleInstall, StyleID: id, Style: style}); err != nil {
		return err
	}
	output.installedStyles[id] = style
	return nil
}

func (s *sessionState) windowForPane(paneID uint64) *Window {
	s.session.mu.RLock()
	defer s.session.mu.RUnlock()
	for _, window := range s.session.Windows {
		if windowHasPane(window, paneID) {
			cp := *window
			return &cp
		}
	}
	return nil
}

type renderOutput struct {
	stream          io.Writer
	pending         []byte
	installedStyles map[uint32]protocol.Style
}

func newRenderOutput(stream ...io.Writer) *renderOutput {
	output := &renderOutput{stream: io.Discard, installedStyles: make(map[uint32]protocol.Style)}
	if len(stream) > 0 {
		output.stream = stream[0]
	}
	return output
}

func (o *renderOutput) append(command protocol.DisplayCommand) error {
	if o.pending == nil {
		o.pending = make([]byte, 0, 4096)
	}
	encoder := protocol.NewDisplayEncoder(o.pending)
	if err := encoder.AppendCommand(command); err != nil {
		return err
	}
	o.pending = encoder.Bytes()
	return nil
}

func (o *renderOutput) commit() error {
	if len(o.pending) == 0 {
		return nil
	}
	data := o.pending
	o.pending = nil
	return writeAll(o.stream, data)
}

func (o *renderOutput) present() error {
	if err := o.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
		return err
	}
	return o.commit()
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
