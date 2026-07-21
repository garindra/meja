package server

import (
	"errors"
	"fmt"

	"github.com/garindra/meja/internal/protocol"
)

const paneRenderTrailerReserve = 512

type queuedPaneRender struct {
	render func(*renderOutput) error
	done   chan error
}

// paneRenderState is owned exclusively by the pane actor. The terminal grid is
// authoritative; this state only records which parts have not yet been offered
// to the current output worker.
type paneRenderState struct {
	pane            *Pane
	lease           *OutputLease
	availableBuffer <-chan *paneRenderBuffer
	failure         <-chan error
	layoutRevision  uint64
	barrierPending  bool
	installedStyles map[uint32]protocol.Style
	dirty           []DirtySpan
	dirtyRows       int
	scrollDelta     int
	cursorDirty     bool
	nextRow         int
	progressive     bool
	due             bool
	refresh         func(*renderOutput) error
	queued          []queuedPaneRender
}

func newPaneRenderState(pane *Pane) *paneRenderState {
	return &paneRenderState{pane: pane}
}

func (s *paneRenderState) attach(lease *OutputLease, layoutRevision uint64, refresh func(*renderOutput) error) {
	s.lease = lease
	s.availableBuffer = lease.availableBuffers()
	s.failure = lease.failures()
	s.layoutRevision = layoutRevision
	s.barrierPending = refresh == nil
	s.installedStyles = make(map[uint32]protocol.Style, 32)
	s.scrollDelta = 0
	s.cursorDirty = true
	s.nextRow = 0
	s.progressive = false
	s.refresh = refresh
	s.queued = nil
	s.ensureRows()
	clear(s.dirty)
	s.dirtyRows = 0
	if refresh == nil {
		s.markFull()
	} else {
		s.cursorDirty = false
	}
	s.due = true
}

func (s *paneRenderState) detach() {
	s.lease = nil
	s.availableBuffer = nil
	s.failure = nil
	s.layoutRevision = 0
	s.barrierPending = false
	s.installedStyles = nil
	s.dirty = nil
	s.dirtyRows = 0
	s.scrollDelta = 0
	s.cursorDirty = false
	s.progressive = false
	s.due = false
	s.refresh = nil
	for _, queued := range s.queued {
		if queued.done != nil {
			queued.done <- nil
		}
	}
	s.queued = nil
}

func (s *paneRenderState) rows() int {
	if s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil {
		return s.pane.historyView.Snapshot.ViewportRows
	}
	return s.pane.terminal.Rows
}

func (s *paneRenderState) cols() int {
	if s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil {
		return s.pane.historyView.Snapshot.Cols
	}
	return s.pane.terminal.Cols
}

func (s *paneRenderState) ensureRows() {
	rows := s.rows()
	if len(s.dirty) == rows {
		return
	}
	oldRows := len(s.dirty)
	if cap(s.dirty) < rows {
		s.dirty = make([]DirtySpan, rows)
	} else {
		s.dirty = s.dirty[:rows]
		if rows > oldRows {
			clear(s.dirty[oldRows:])
		}
	}
	if rows == 0 {
		s.nextRow = 0
	} else if s.nextRow >= rows {
		s.nextRow %= rows
	}
	s.dirtyRows = 0
	for _, span := range s.dirty {
		if span.End > span.Start {
			s.dirtyRows++
		}
	}
}

func (s *paneRenderState) markFull() {
	s.ensureRows()
	cols := s.cols()
	for row := range s.dirty {
		s.dirty[row] = DirtySpan{Start: 0, End: cols}
	}
	s.dirtyRows = len(s.dirty)
	s.scrollDelta = 0
	s.cursorDirty = true
	s.progressive = false
}

func (s *paneRenderState) hasDirty() bool {
	return s.dirtyRows > 0
}

func (s *paneRenderState) hasWork() bool {
	return s.lease != nil && (s.refresh != nil || len(s.queued) > 0 || s.barrierPending || s.scrollDelta != 0 || s.cursorDirty || s.hasDirty())
}

func (s *paneRenderState) hasVisualWork() bool {
	return s.barrierPending || s.scrollDelta != 0 || s.cursorDirty || s.hasDirty()
}

func (s *paneRenderState) available() <-chan *paneRenderBuffer {
	if !s.due || !s.hasWork() || s.lease == nil {
		return nil
	}
	return s.availableBuffer
}

func (s *paneRenderState) failures() <-chan error {
	if s.lease == nil {
		return nil
	}
	return s.failure
}

func mergeDirtySpan(dst *DirtySpan, next DirtySpan, cols int) bool {
	if next.Start < 0 {
		next.Start = 0
	}
	if next.End > cols {
		next.End = cols
	}
	if next.Start >= next.End {
		return false
	}
	if dst.End <= dst.Start {
		*dst = next
		return true
	}
	if next.Start < dst.Start {
		dst.Start = next.Start
	}
	if next.End > dst.End {
		dst.End = next.End
	}
	return false
}

func (s *paneRenderState) merge(update Update) {
	if s.lease == nil || s.pane.currentViewMode() != paneViewLive {
		return
	}
	s.ensureRows()
	if update.FullRedraw || update.ScrollDelta != 0 && s.progressive {
		s.markFull()
	} else {
		if update.ScrollDelta != 0 {
			if s.scrollDelta != 0 && (s.scrollDelta < 0) != (update.ScrollDelta < 0) {
				s.markFull()
			} else {
				s.dirtyRows -= shiftDirtyRows(s.dirty, update.ScrollDelta)
				s.scrollDelta += update.ScrollDelta
				rows := len(s.dirty)
				if s.scrollDelta < -rows {
					s.scrollDelta = -rows
				} else if s.scrollDelta > rows {
					s.scrollDelta = rows
				}
			}
		}
		cols := s.cols()
		for row := 0; row < len(s.dirty) && row < len(update.DirtySpans); row++ {
			if mergeDirtySpan(&s.dirty[row], update.DirtySpans[row], cols) {
				s.dirtyRows++
			}
		}
	}
	s.cursorDirty = s.cursorDirty || update.CursorChanged || update.VisibleChange
}

func shiftDirtyRows(spans []DirtySpan, delta int) int {
	rows := len(spans)
	if rows == 0 || delta == 0 {
		return 0
	}
	dropped := 0
	if delta < 0 {
		shift := min(-delta, rows)
		for _, span := range spans[:shift] {
			if span.End > span.Start {
				dropped++
			}
		}
		copy(spans[:rows-shift], spans[shift:])
		clear(spans[rows-shift:])
		return dropped
	}
	shift := min(delta, rows)
	for _, span := range spans[rows-shift:] {
		if span.End > span.Start {
			dropped++
		}
	}
	copy(spans[shift:], spans[:rows-shift])
	clear(spans[:shift])
	return dropped
}

func cloneRenderStyles(styles map[uint32]protocol.Style) map[uint32]protocol.Style {
	copyStyles := make(map[uint32]protocol.Style, len(styles))
	for id, style := range styles {
		copyStyles[id] = style
	}
	return copyStyles
}

func cloneDirty(spans []DirtySpan) []DirtySpan {
	return append([]DirtySpan(nil), spans...)
}

func (s *paneRenderState) nextDirtyRow() int {
	rows := len(s.dirty)
	if rows == 0 {
		return -1
	}
	for offset := 0; offset < rows; offset++ {
		row := (s.nextRow + offset) % rows
		if span := s.dirty[row]; span.End > span.Start {
			return row
		}
	}
	return -1
}

func (s *paneRenderState) cells(row int) ([]cellWord, displayStyleSource, *clusterStore) {
	if s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil {
		view := s.pane.historyView
		return view.Snapshot.row(view.ViewTop + row), view.Snapshot, view.Snapshot.clusters
	}
	return s.pane.terminal.gridRow(row), s.pane.terminal, &s.pane.terminal.clusters
}

func (s *paneRenderState) selectionBoundary(row, start, end int) (int, bool) {
	if s.pane.currentViewMode() != paneViewHistory || s.pane.historyView == nil {
		return end, false
	}
	view := s.pane.historyView
	logicalRow := view.ViewTop + row
	selected := historySelectionContains(view.Selection, logicalRow, start)
	for column := start + 1; column < end; column++ {
		if historySelectionContains(view.Selection, logicalRow, column) != selected {
			return column, selected
		}
	}
	return end, selected
}

func normalizeCellEnd(cells []cellWord, start, end int) int {
	end = min(end, len(cells))
	for end > start && end < len(cells) && cells[end].width() == 0 {
		end--
	}
	if end == start && start < len(cells) {
		end = min(len(cells), start+int(max(uint8(1), cells[start].width())))
	}
	return end
}

func (s *paneRenderState) tryCells(output *renderOutput, row, start, end int) (int, error) {
	cells, styles, clusters := s.cells(row)
	end, selected := s.selectionBoundary(row, start, end)
	for {
		end = normalizeCellEnd(cells, start, end)
		beforeBytes := output.pending
		beforeStyles := cloneRenderStyles(output.installedStyles)
		compiler := newDisplayCompiler(output, styles, clusters, s.cols())
		if s.pane.currentViewMode() == paneViewLive {
			compiler.installStyles = true
		} else if selected {
			compiler.installStyles = true
			compiler.styleMapper = func(id uint32) uint32 { return historySelectionStyleMask | id }
		} else {
			compiler.installStyles = true
		}
		err := compiler.writeCells(row, start, cells[start:end])
		if err == nil {
			err = compiler.finish()
		}
		if !errors.Is(err, errRenderBufferFull) {
			return end, err
		}
		output.pending = beforeBytes
		output.installedStyles = beforeStyles
		if end-start <= 1 || end-start <= int(cells[start].width()) {
			return start, errRenderBufferFull
		}
		end = start + (end-start)/2
	}
}

func (s *paneRenderState) appendTrailer(output *renderOutput) error {
	output.limit = paneRenderBufferCapacity
	if s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil {
		view := s.pane.historyView
		compiler := newDisplayCompiler(output, view.Snapshot, view.Snapshot.clusters, view.Snapshot.Cols)
		compiler.installStyles = true
		if err := writeHistoryCounter(compiler, view, historyCounter(view)); err != nil {
			return err
		}
		if err := compiler.finish(); err != nil {
			return err
		}
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: min(view.CursorCol, view.Snapshot.Cols-1), Y: view.CursorRow - view.ViewTop}, Visible: true}}); err != nil {
			return err
		}
	} else if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: s.pane.terminal.CursorX, Y: s.pane.terminal.CursorY}, Visible: s.pane.terminal.CursorVisible}}); err != nil {
		return err
	}
	return output.present()
}

func (s *paneRenderState) renderCustom(buffer *paneRenderBuffer, queued queuedPaneRender) error {
	output := newBoundedRenderOutput(buffer, s.installedStyles, paneRenderBufferCapacity)
	if queued.render != nil {
		if err := queued.render(output); err != nil {
			return err
		}
	}
	buffer.data = output.pending
	s.installedStyles = output.installedStyles
	var completed chan error
	if queued.done != nil {
		completed = make(chan error, 1)
	}
	if !s.lease.submitBatch(buffer, completed) {
		return fmt.Errorf("pane %d output lease is unavailable", s.pane.ID)
	}
	if queued.done != nil {
		go func() { queued.done <- <-completed }()
	}
	s.due = s.hasWork()
	return nil
}

func (s *paneRenderState) render(buffer *paneRenderBuffer) error {
	if s.lease == nil {
		return nil
	}
	if s.refresh != nil {
		refresh := s.refresh
		s.refresh = nil
		return s.renderCustom(buffer, queuedPaneRender{render: refresh})
	}
	if len(s.queued) > 0 && !s.hasVisualWork() {
		queued := s.queued[0]
		s.queued = s.queued[1:]
		err := s.renderCustom(buffer, queued)
		if err != nil {
			queued.done <- err
		}
		return err
	}

	dirtyBefore := cloneDirty(s.dirty)
	dirtyRowsBefore := s.dirtyRows
	stylesBefore := cloneRenderStyles(s.installedStyles)
	barrierBefore, scrollBefore, cursorBefore := s.barrierPending, s.scrollDelta, s.cursorDirty
	nextBefore, progressiveBefore := s.nextRow, s.progressive
	rollback := func() {
		s.dirty = dirtyBefore
		s.dirtyRows = dirtyRowsBefore
		s.installedStyles = stylesBefore
		s.barrierPending, s.scrollDelta, s.cursorDirty = barrierBefore, scrollBefore, cursorBefore
		s.nextRow, s.progressive = nextBefore, progressiveBefore
	}

	output := newBoundedRenderOutput(buffer, s.installedStyles, paneRenderBufferCapacity-paneRenderTrailerReserve)
	if s.barrierPending {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeStartRender, LayoutRevision: s.layoutRevision, GridCols: s.cols(), GridRows: s.rows()}); err != nil {
			rollback()
			return err
		}
		if err := installStyle(output, protocol.CanonicalDefaultStyleID, protocol.CanonicalDefaultStyle()); err != nil {
			rollback()
			return err
		}
		s.barrierPending = false
	}
	if s.scrollDelta != 0 {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeScroll, Delta: s.scrollDelta}); err != nil {
			rollback()
			return err
		}
		s.scrollDelta = 0
	}

	for len(output.pending) < output.limit {
		row := s.nextDirtyRow()
		if row < 0 {
			break
		}
		span := s.dirty[row]
		end, err := s.tryCells(output, row, span.Start, span.End)
		if errors.Is(err, errRenderBufferFull) && len(output.pending) > 0 {
			break
		}
		if err != nil {
			rollback()
			return err
		}
		if end <= span.Start {
			break
		}
		if end >= span.End {
			s.dirty[row] = DirtySpan{}
			s.dirtyRows--
		} else {
			s.dirty[row].Start = end
		}
		if len(s.dirty) > 0 {
			s.nextRow = (row + 1) % len(s.dirty)
		}
	}
	if err := s.appendTrailer(output); err != nil {
		rollback()
		return err
	}
	buffer.data = output.pending
	if len(buffer.data) > paneRenderBufferCapacity {
		rollback()
		return fmt.Errorf("pane %d render batch is %d bytes, capacity %d", s.pane.ID, len(buffer.data), paneRenderBufferCapacity)
	}
	if !s.lease.submit(buffer) {
		rollback()
		return fmt.Errorf("pane %d output lease is unavailable", s.pane.ID)
	}
	s.installedStyles = output.installedStyles
	s.cursorDirty = false
	s.progressive = s.hasDirty()
	s.due = s.hasWork()
	return nil
}
