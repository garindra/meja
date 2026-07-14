package server

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/creack/pty"

	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

var ptyReadBuffers = sync.Pool{New: func() any { return make([]byte, 32*1024) }}

const (
	renderIdleFlush   = time.Millisecond
	renderMaxBatchAge = 10 * time.Millisecond
)

func (p *Pane) attachOutput(lease *OutputLease, refresh func(*renderOutput) error) error {
	return p.attachOutputMode(lease, true, refresh)
}

func (p *Pane) attachOutputMode(lease *OutputLease, live bool, refresh func(*renderOutput) error) error {
	if p.commands == nil {
		if refresh == nil {
			return nil
		}
		return refresh(newRenderOutput(lease.Stream))
	}
	select {
	case p.commands <- paneCommand{attach: lease, live: live, refresh: refresh}:
		return nil
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
}

func (p *Pane) detachOutput(stream io.Writer) error {
	if p.commands == nil {
		return nil
	}
	return p.sendRenderCommand(paneCommand{detach: stream})
}

func (p *Pane) releaseOutput(done chan<- *OutputLease) {
	if p.commands == nil {
		done <- p.outputLease
		p.outputLease = nil
		return
	}
	release := &paneOutputRelease{done: done, acked: make(chan struct{})}
	select {
	case p.commands <- paneCommand{release: release}:
		go func() {
			select {
			case <-p.mainDone:
				release.acknowledge()
			case <-release.acked:
			}
		}()
	case <-p.mainDone:
		release.acknowledge()
	case <-p.done:
		release.acknowledge()
	}
}

func (p *Pane) applyRender(render func(*renderOutput) error) error {
	if p.commands == nil {
		return nil
	}
	return p.sendRenderCommand(paneCommand{apply: render})
}

func (p *Pane) sendRenderCommand(command paneCommand) error {
	done := make(chan error, 1)
	command.done = done
	select {
	case p.commands <- command:
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
}

func relayPTYOutput(pane *Pane) {
	defer close(pane.ptyOutput)
	for {
		buf := ptyReadBuffers.Get().([]byte)
		n, err := pane.PTY.Read(buf)
		if n > 0 {
			select {
			case pane.ptyOutput <- buf[:n]:
			case <-pane.done:
				ptyReadBuffers.Put(buf[:cap(buf)])
				return
			}
		} else {
			ptyReadBuffers.Put(buf[:cap(buf)])
		}
		if err != nil {
			return
		}
	}
}

func runPTYWriter(pane *Pane, failed func(error)) {
	defer close(pane.writerDone)
	for {
		select {
		case data := <-pane.ptyInput:
			if err := writeAll(pane.PTY, data); err != nil {
				failed(err)
				return
			}
		case <-pane.done:
			return
		}
	}
}

func (pane *Pane) run() {
	defer close(pane.mainDone)
	var output *renderOutput
	liveOutput := false
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
		if output != nil && liveOutput {
			if err := emitTerminalUpdate(output, pane, current); err != nil {
				output = nil
				pane.outputLease = nil
			}
		}
	}
	for {
		select {
		case command := <-pane.commands:
			stop(idle)
			stop(maxAge)
			idle, idleC, maxAge, maxC = nil, nil, nil, nil
			aggregate = terminal.Update{}
			pending = false
			if command.release != nil {
				lease := pane.outputLease
				pane.outputLease = nil
				output = nil
				liveOutput = false
				command.release.returnLease(lease)
				continue
			}
			if command.detach != nil {
				if pane.outputLease != nil && pane.outputLease.Stream == command.detach {
					pane.outputLease = nil
					output = nil
					liveOutput = false
				}
				command.done <- nil
				continue
			}
			if command.attach != nil {
				pane.outputLease = command.attach
				output = newRenderOutput(command.attach.Stream)
				liveOutput = command.live
				if command.refresh != nil {
					if err := command.refresh(output); err != nil {
						pane.outputLease = nil
						output = nil
						liveOutput = false
					}
				}
				continue
			}
			if command.apply != nil && output != nil {
				err := command.apply(output)
				if err != nil {
					pane.outputLease = nil
					output = nil
					liveOutput = false
				}
				command.done <- err
			} else if command.resize != nil {
				err := error(nil)
				if pane.PTY != nil {
					err = pty.Setsize(pane.PTY, &pty.Winsize{Cols: command.resize.cols, Rows: command.resize.rows})
				}
				pane.terminal.Resize(int(command.resize.cols), int(command.resize.rows))
				pane.publishTerminalMetadata()
				command.done <- err
			} else if command.history != nil {
				command.history <- captureTerminalHistorySnapshot(pane.terminal)
				command.done <- nil
			} else {
				command.done <- nil
			}
		case data, ok := <-pane.ptyOutput:
			if !ok {
				flush()
				return
			}
			update := pane.terminal.Apply(data)
			ptyReadBuffers.Put(data[:cap(data)])
			for _, reply := range update.Replies {
				if err := pane.sendOwnedInput(reply); err != nil {
					return
				}
			}
			update.Replies = nil
			pane.publishTerminalMetadata()
			aggregate.Merge(update, pane.terminal.Rows)
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
		case <-pane.done:
			return
		}
	}
}

func (s *Session) emitHistoryMove(pane *Pane, move historyMove) error {
	view := s.HistoryView(clientID0, pane.ID)
	if view == nil {
		return nil
	}
	return pane.applyRender(func(output *renderOutput) error {
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

func emitTerminalUpdate(output *renderOutput, pane *Pane, update terminal.Update) error {
	if update.FullRedraw {
		return sendFullRender(output, pane)
	}
	rows := make([]int, 0, len(update.DirtySpans))
	for row := range update.DirtySpans {
		rows = append(rows, row)
	}
	sort.Ints(rows)
	runs := make([]displayCellRun, 0, len(rows))
	for _, row := range rows {
		span := update.DirtySpans[row]
		start := row*pane.terminal.Cols + span.Start
		end := row*pane.terminal.Cols + span.End
		runs = append(runs, displayCellRun{Row: row, Column: span.Start, Cells: append([]protocol.Cell(nil), pane.terminal.Cells[start:end]...)})
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
	for _, def := range pane.terminal.SnapshotStyles() {
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
		if update.ScrollDelta == 0 {
			return nil
		}
	}
	if update.ScrollDelta != 0 {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeScroll, Delta: update.ScrollDelta}); err != nil {
			return err
		}
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
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.terminal.CursorX, Y: pane.terminal.CursorY}, Visible: pane.terminal.CursorVisible}}); err != nil {
			return err
		}
	}
	return output.present()
}

func sendFullRender(output *renderOutput, pane *Pane) error {
	styleDefs := pane.terminal.SnapshotStyles()
	styleByID := make(map[uint32]protocol.Style, len(styleDefs))
	for _, def := range styleDefs {
		styleByID[def.ID] = def.Style
		if err := installStyle(output, def.ID, def.Style); err != nil {
			return err
		}
	}
	compiler := newDisplayCompiler(output, styleByID)
	for row := 0; row < pane.terminal.Rows; row++ {
		start := row * pane.terminal.Cols
		if err := compiler.writeCells(row, 0, pane.terminal.Cells[start:start+pane.terminal.Cols]); err != nil {
			return err
		}
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.terminal.CursorX, Y: pane.terminal.CursorY}, Visible: pane.terminal.CursorVisible}}); err != nil {
		return err
	}
	return output.present()
}

func sendCurrentViewSnapshot(output *renderOutput, pane *Pane, view *HistoryView) error {
	if view != nil {
		return sendHistorySnapshot(output, pane, view)
	}
	return sendFullRender(output, pane)
}

func sendHistorySnapshot(output *renderOutput, pane *Pane, view *HistoryView) error {
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

func (s *Session) sendHistorySnapshotSerialized(pane *Pane, view *HistoryView) error {
	return pane.applyRender(func(output *renderOutput) error {
		return sendHistorySnapshot(output, pane, view)
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
