package server

import (
	"fmt"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

type RenderEpoch uint64
type RenderVersion uint64

type PublicationKind uint8

const (
	PublicationDelta PublicationKind = iota
	PublicationKeyframe
)

const (
	publicationBufferCount        = 2
	publicationKeyframeDensityPct = 50
	publicationAnchorInterval     = 256
	initialPublicationClusterCap  = 64 << 10
)

type semanticCellKind uint8

const (
	semanticBlank semanticCellKind = iota
	semanticScalar
	semanticCluster
	semanticContinuation
)

// semanticCell is publication-local. Cluster payloads are offsets into the
// owning semantic viewport or publication arena, never pane cluster handles.
type semanticCell struct {
	payload    uint32
	clusterLen uint16
	style      uint16
	kind       semanticCellKind
	width      uint8
}

type publishedRun struct {
	Row       uint16
	Column    uint16
	Columns   uint16
	CellStart uint32
}

type publishedScroll struct {
	Top    uint16
	Bottom uint16
	Delta  int16
}

type publishedCursor struct {
	X       uint16
	Y       uint16
	Visible bool
}

type viewPublication struct {
	Epoch          RenderEpoch
	BaseVersion    RenderVersion
	TargetVersion  RenderVersion
	Kind           PublicationKind
	LayoutRevision protocol.ClientLayoutRevision
	Barrier        bool
	Cols           uint16
	Rows           uint16
	HasScroll      bool
	Scroll         publishedScroll
	Runs           []publishedRun
	Cells          []semanticCell
	Styles         []protocol.Style
	Clusters       []byte
	Cursor         publishedCursor
	CursorChanged  bool

	candidateCells uint64
	changedCells   uint64
	cancelledCells uint64
}

type viewPublicationBuffer struct {
	publication  viewPublication
	returnTo     chan<- *viewPublicationBuffer
	metrics      *paneRenderMetrics
	fromPTYDrain bool
}

func (b *viewPublicationBuffer) reset(cols, rows int) {
	p := &b.publication
	p.Epoch = 0
	p.BaseVersion = 0
	p.TargetVersion = 0
	p.Kind = PublicationDelta
	p.LayoutRevision = 0
	p.Barrier = false
	p.Cols = uint16(cols)
	p.Rows = uint16(rows)
	p.HasScroll = false
	p.Scroll = publishedScroll{}
	p.Runs = p.Runs[:0]
	p.Cells = p.Cells[:0]
	p.Styles = p.Styles[:0]
	p.Clusters = p.Clusters[:0]
	p.Cursor = publishedCursor{}
	p.CursorChanged = false
	p.candidateCells = 0
	p.changedCells = 0
	p.cancelledCells = 0
	b.fromPTYDrain = false
}

func (b *viewPublicationBuffer) release() {
	if b == nil || b.returnTo == nil {
		return
	}
	returnTo := b.returnTo
	b.returnTo = nil
	b.metrics = nil
	returnTo <- b
}

type semanticViewport struct {
	cols     int
	rows     int
	epoch    RenderEpoch
	version  RenderVersion
	valid    bool
	cells    []semanticCell
	styles   []protocol.Style
	clusters []byte
	cursor   publishedCursor
}

func (v *semanticViewport) reset(cols, rows int) {
	total := cols * rows
	v.cols, v.rows = cols, rows
	if cap(v.cells) < total {
		v.cells = make([]semanticCell, total)
	} else {
		v.cells = v.cells[:total]
		clear(v.cells)
	}
	v.styles = v.styles[:0]
	v.clusters = v.clusters[:0]
	v.cursor = publishedCursor{}
}

type paneRenderMetrics struct {
	ptyBytes                   atomic.Uint64
	ptyDrainOpportunities      atomic.Uint64
	ptyDrainsCompleted         atomic.Uint64
	ptyDrainReads              atomic.Uint64
	ptyDrainDurationNanos      atomic.Uint64
	ptyDrainStoppedEmpty       atomic.Uint64
	ptyDrainStoppedByteBudget  atomic.Uint64
	ptyDrainStoppedTimeBudget  atomic.Uint64
	ptyDrainStoppedEOF         atomic.Uint64
	ptyDrainStoppedError       atomic.Uint64
	ptyDrainStoppedCancelled   atomic.Uint64
	ptyDrainPublications       atomic.Uint64
	ptyDrainPresents           atomic.Uint64
	ptyDrainCancelledCells     atomic.Uint64
	publications               atomic.Uint64
	presents                   atomic.Uint64
	candidateCells             atomic.Uint64
	changedCells               atomic.Uint64
	changedRuns                atomic.Uint64
	keyframes                  atomic.Uint64
	deltas                     atomic.Uint64
	uncompressedBytes          atomic.Uint64
	physicalWrites             atomic.Uint64
	publicationBufferStarved   atomic.Uint64
	confirmerWriteBlockedNanos atomic.Uint64
	cancelledCells             atomic.Uint64
}

type paneRenderMetricsSnapshot struct {
	PTYBytes                   uint64
	PTYDrainOpportunities      uint64
	PTYDrainsCompleted         uint64
	PTYDrainReads              uint64
	PTYDrainDurationNanos      uint64
	PTYDrainStoppedEmpty       uint64
	PTYDrainStoppedByteBudget  uint64
	PTYDrainStoppedTimeBudget  uint64
	PTYDrainStoppedEOF         uint64
	PTYDrainStoppedError       uint64
	PTYDrainStoppedCancelled   uint64
	PTYDrainPublications       uint64
	PTYDrainPresents           uint64
	PTYDrainCancelledCells     uint64
	Publications               uint64
	Presents                   uint64
	CandidateCells             uint64
	ChangedCells               uint64
	ChangedRuns                uint64
	Keyframes                  uint64
	Deltas                     uint64
	UncompressedBytes          uint64
	PhysicalWrites             uint64
	PublicationBufferStarved   uint64
	ConfirmerWriteBlockedNanos uint64
	CancelledCells             uint64
}

func (m *paneRenderMetrics) snapshot() paneRenderMetricsSnapshot {
	if m == nil {
		return paneRenderMetricsSnapshot{}
	}
	return paneRenderMetricsSnapshot{
		PTYBytes:                   m.ptyBytes.Load(),
		PTYDrainOpportunities:      m.ptyDrainOpportunities.Load(),
		PTYDrainsCompleted:         m.ptyDrainsCompleted.Load(),
		PTYDrainReads:              m.ptyDrainReads.Load(),
		PTYDrainDurationNanos:      m.ptyDrainDurationNanos.Load(),
		PTYDrainStoppedEmpty:       m.ptyDrainStoppedEmpty.Load(),
		PTYDrainStoppedByteBudget:  m.ptyDrainStoppedByteBudget.Load(),
		PTYDrainStoppedTimeBudget:  m.ptyDrainStoppedTimeBudget.Load(),
		PTYDrainStoppedEOF:         m.ptyDrainStoppedEOF.Load(),
		PTYDrainStoppedError:       m.ptyDrainStoppedError.Load(),
		PTYDrainStoppedCancelled:   m.ptyDrainStoppedCancelled.Load(),
		PTYDrainPublications:       m.ptyDrainPublications.Load(),
		PTYDrainPresents:           m.ptyDrainPresents.Load(),
		PTYDrainCancelledCells:     m.ptyDrainCancelledCells.Load(),
		Publications:               m.publications.Load(),
		Presents:                   m.presents.Load(),
		CandidateCells:             m.candidateCells.Load(),
		ChangedCells:               m.changedCells.Load(),
		ChangedRuns:                m.changedRuns.Load(),
		Keyframes:                  m.keyframes.Load(),
		Deltas:                     m.deltas.Load(),
		UncompressedBytes:          m.uncompressedBytes.Load(),
		PhysicalWrites:             m.physicalWrites.Load(),
		PublicationBufferStarved:   m.publicationBufferStarved.Load(),
		ConfirmerWriteBlockedNanos: m.confirmerWriteBlockedNanos.Load(),
		CancelledCells:             m.cancelledCells.Load(),
	}
}

func (m *paneRenderMetrics) recordPTYDrain(event ptyDrainEvent) {
	m.ptyDrainsCompleted.Add(1)
	m.ptyDrainReads.Add(uint64(event.reads))
	m.ptyDrainDurationNanos.Add(uint64(max(time.Duration(0), event.duration)))
	switch event.reason {
	case ptyDrainStoppedEmpty:
		m.ptyDrainStoppedEmpty.Add(1)
	case ptyDrainStoppedByteBudget:
		m.ptyDrainStoppedByteBudget.Add(1)
	case ptyDrainStoppedTimeBudget:
		m.ptyDrainStoppedTimeBudget.Add(1)
	case ptyDrainStoppedEOF:
		m.ptyDrainStoppedEOF.Add(1)
	case ptyDrainStoppedError:
		m.ptyDrainStoppedError.Add(1)
	case ptyDrainStoppedCancelled:
		m.ptyDrainStoppedCancelled.Add(1)
	}
}

func (m *paneRenderMetrics) recordPublication(publication *viewPublication, fromPTYDrain bool) {
	m.publications.Add(1)
	if fromPTYDrain {
		m.ptyDrainPublications.Add(1)
	}
	m.candidateCells.Add(publication.candidateCells)
	m.changedCells.Add(publication.changedCells)
	m.changedRuns.Add(uint64(len(publication.Runs)))
	m.cancelledCells.Add(publication.cancelledCells)
	if publication.Kind == PublicationKeyframe {
		m.keyframes.Add(1)
	} else {
		m.deltas.Add(1)
	}
}

func (m *paneRenderMetrics) recordCompiledFrame(bytes int, fromPTYDrain bool) {
	m.presents.Add(1)
	if fromPTYDrain {
		m.ptyDrainPresents.Add(1)
	}
	m.uncompressedBytes.Add(uint64(bytes))
}

type panePublicationState struct {
	pane              *Pane
	lease             *OutputLease
	failure           <-chan error
	layoutRevision    protocol.ClientLayoutRevision
	epoch             RenderEpoch
	version           RenderVersion
	snapshot          semanticViewport
	nextSnapshot      semanticViewport
	free              chan *viewPublicationBuffer
	buffers           [publicationBufferCount]viewPublicationBuffer
	pending           *viewPublicationBuffer
	dirty             []DirtySpan
	dirtyRows         int
	scroll            *ScrollRegion
	scrollBuf         ScrollRegion
	cursorDirty       bool
	keyframe          bool
	barrier           bool
	starved           bool
	diff              []bool
	counterScratch    []byte
	syncWaiters       []chan<- *OutputLease
	attributePTYDrain bool
}

func (s *panePublicationState) attributeNextPreparationToDrain() {
	s.attributePTYDrain = true
}

func newPanePublicationState(pane *Pane) *panePublicationState {
	s := &panePublicationState{
		pane:  pane,
		epoch: 1,
		free:  make(chan *viewPublicationBuffer, publicationBufferCount),
	}
	for index := range s.buffers {
		s.free <- &s.buffers[index]
	}
	return s
}

func (s *panePublicationState) rows() int {
	if s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil {
		return s.pane.historyView.Snapshot.ViewportRows
	}
	return s.pane.terminal.Rows
}

func (s *panePublicationState) cols() int {
	if s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil {
		return s.pane.historyView.Snapshot.Cols
	}
	return s.pane.terminal.Cols
}

func (s *panePublicationState) ensureGeometry() {
	rows, cols := s.rows(), s.cols()
	if len(s.dirty) != rows {
		s.dirty = make([]DirtySpan, rows)
		s.dirtyRows = 0
	}
	if cap(s.diff) < cols {
		s.diff = make([]bool, cols)
	} else {
		s.diff = s.diff[:cols]
	}
	total := rows * cols
	if cap(s.snapshot.cells) < total {
		s.snapshot.cells = make([]semanticCell, total)
		s.snapshot.cells = s.snapshot.cells[:0]
	}
	if cap(s.nextSnapshot.cells) < total {
		s.nextSnapshot.cells = make([]semanticCell, total)
		s.nextSnapshot.cells = s.nextSnapshot.cells[:0]
	}
	clusterCap := max(initialPublicationClusterCap, total*8)
	if cap(s.snapshot.clusters) < clusterCap {
		s.snapshot.clusters = make([]byte, 0, clusterCap)
	}
	if cap(s.nextSnapshot.clusters) < clusterCap {
		s.nextSnapshot.clusters = make([]byte, 0, clusterCap)
	}
}

func (s *panePublicationState) attach(lease *OutputLease, layoutRevision protocol.ClientLayoutRevision) {
	s.cancelPending()
	s.lease = lease
	s.failure = lease.failures()
	s.layoutRevision = layoutRevision
	s.invalidateEpoch(true)
	s.ensureGeometry()
}

func (s *panePublicationState) detach() {
	s.cancelPending()
	s.lease = nil
	s.failure = nil
	s.layoutRevision = 0
	s.clearMutation()
	s.snapshot.valid = false
}

func (s *panePublicationState) cancelPending() {
	if s.pending != nil {
		// A cancelled START_RENDER has not established the confirmer's stream
		// state. Preserve its barrier for the replacement publication.
		s.barrier = s.barrier || s.pending.publication.Barrier
		s.pending.release()
		s.pending = nil
	}
}

func (s *panePublicationState) invalidateEpoch(barrier bool) {
	s.cancelPending()
	s.epoch++
	if s.epoch == 0 {
		s.epoch = 1
	}
	s.version = 0
	s.snapshot.valid = false
	s.keyframe = true
	s.barrier = s.barrier || barrier
	s.scroll = nil
	s.cursorDirty = true
	s.ensureGeometry()
	for row := range s.dirty {
		s.dirty[row] = DirtySpan{Start: 0, End: s.cols()}
	}
	s.dirtyRows = len(s.dirty)
}

func (s *panePublicationState) failures() <-chan error {
	return s.failure
}

func (s *panePublicationState) hasMutation() bool {
	return s.lease != nil && (s.keyframe || s.barrier || s.scroll != nil || s.cursorDirty || s.dirtyRows > 0)
}

func (s *panePublicationState) blocksPTY() bool {
	return s.pending != nil || s.hasMutation()
}

func (s *panePublicationState) available() <-chan *viewPublicationBuffer {
	if s.pending != nil || !s.hasMutation() {
		s.starved = false
		return nil
	}
	if len(s.free) == 0 {
		if !s.starved {
			s.pane.renderMetrics.publicationBufferStarved.Add(1)
			s.starved = true
		}
		return s.free
	}
	s.starved = false
	return s.free
}

func (s *panePublicationState) submission() (chan<- confirmerMessage, confirmerMessage) {
	if s.pending == nil || s.lease == nil {
		return nil, confirmerMessage{}
	}
	return s.lease.submissions(), confirmerMessage{publication: s.pending}
}

func (s *panePublicationState) merge(update Update) {
	if s.lease == nil || s.pane.currentViewMode() != paneViewLive {
		return
	}
	s.mergeViewMutation(ViewMutation{
		DirtySpans:    update.DirtySpans,
		ScrollRegion:  update.ScrollRegion,
		FullRedraw:    update.FullRedraw,
		CursorChanged: update.CursorChanged || update.VisibleChange,
	})
}

func (s *panePublicationState) mergeViewMutation(update ViewMutation) {
	if s.lease == nil {
		return
	}
	s.ensureGeometry()
	if update.FullRedraw {
		s.keyframe = true
	}
	if region := update.ScrollRegion; region != nil && !s.keyframe {
		if s.scroll != nil &&
			(s.scroll.Top != region.Top || s.scroll.Bottom != region.Bottom ||
				(s.scroll.Delta < 0) != (region.Delta < 0)) {
			s.keyframe = true
			s.scroll = nil
		} else {
			s.dirtyRows -= shiftDirtyRows(s.dirty, region.Top, region.Bottom, region.Delta)
			if s.scroll == nil {
				s.scrollBuf = *region
				s.scroll = &s.scrollBuf
			} else {
				s.scroll.Delta += region.Delta
			}
			height := region.Bottom - region.Top
			if s.scroll != nil && (s.scroll.Delta <= -height || s.scroll.Delta >= height) {
				s.keyframe = true
				s.scroll = nil
			}
		}
	}
	cols := s.cols()
	for row := 0; row < len(s.dirty) && row < len(update.DirtySpans); row++ {
		if mergeDirtySpan(&s.dirty[row], update.DirtySpans[row], cols) {
			s.dirtyRows++
		}
	}
	if s.keyframe {
		for row := range s.dirty {
			s.dirty[row] = DirtySpan{Start: 0, End: cols}
		}
		s.dirtyRows = len(s.dirty)
	}
	s.cursorDirty = s.cursorDirty || update.CursorChanged || update.ScrollRegion != nil
}

func mergeDirtySpan(dst *DirtySpan, next DirtySpan, cols int) bool {
	next.Start = max(0, next.Start)
	next.End = min(cols, next.End)
	if next.Start >= next.End {
		return false
	}
	if dst.End <= dst.Start {
		*dst = next
		return true
	}
	dst.Start = min(dst.Start, next.Start)
	dst.End = max(dst.End, next.End)
	return false
}

func shiftDirtyRows(spans []DirtySpan, top, bottom, delta int) int {
	if top < 0 || top >= bottom || bottom > len(spans) || delta == 0 {
		return 0
	}
	spans = spans[top:bottom]
	rows := len(spans)
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

type currentSemanticCell struct {
	kind  semanticCellKind
	width uint8
	r     rune
	text  string
	style protocol.Style
}

type displayStyleSource interface {
	LookupStyle(uint32) (protocol.Style, bool)
}

func canonicalStyle(style protocol.Style) protocol.Style {
	if style.FG.Mode == "" {
		style.FG.Mode = "default"
	}
	if style.BG.Mode == "" {
		style.BG.Mode = "default"
	}
	return style
}

func (s *panePublicationState) historyCounterBytes() []byte {
	view := s.pane.historyView
	s.counterScratch = s.counterScratch[:0]
	s.counterScratch = append(s.counterScratch, '[')
	s.counterScratch = strconv.AppendInt(s.counterScratch, int64(view.Snapshot.InitialTop-view.ViewTop), 10)
	s.counterScratch = append(s.counterScratch, '/')
	s.counterScratch = strconv.AppendInt(s.counterScratch, int64(view.Snapshot.InitialTop), 10)
	s.counterScratch = append(s.counterScratch, ']')
	return s.counterScratch
}

func (s *panePublicationState) currentCell(row, column int, counter []byte) (currentSemanticCell, error) {
	var cells []cellWord
	var styles displayStyleSource
	var clusters *clusterStore
	history := s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil
	if history {
		view := s.pane.historyView
		counterStart := view.Snapshot.Cols - len(counter)
		if row == 0 && column >= counterStart {
			return currentSemanticCell{
				kind: semanticScalar, width: 1, r: rune(counter[column-counterStart]),
				style: canonicalStyle(historyCounterStyle),
			}, nil
		}
		cells = view.Snapshot.row(view.ViewTop + row)
		styles = view.Snapshot
		clusters = view.Snapshot.clusters
	} else {
		cells = s.pane.terminal.gridRow(row)
		styles = s.pane.terminal
		clusters = &s.pane.terminal.clusters
	}
	word := cells[column]
	style, ok := styles.LookupStyle(uint32(word.styleID()))
	if !ok {
		return currentSemanticCell{}, fmt.Errorf("pane %d cell style %d is unavailable", s.pane.ID, word.styleID())
	}
	if history && historySelectionContains(s.pane.historyView.Selection, s.pane.historyView.ViewTop+row, column) {
		style = historySelectionStyle
	}
	cell := currentSemanticCell{width: word.width(), style: canonicalStyle(style)}
	switch {
	case word.width() == 0:
		cell.kind = semanticContinuation
	case word.isBlank():
		cell.kind = semanticBlank
	case func() bool { r, ok := word.scalar(); cell.r = r; return ok }():
		cell.kind = semanticScalar
	default:
		cell.kind = semanticCluster
		cell.text = cellTextFromStore(word, clusters)
		if cell.text == "" {
			return currentSemanticCell{}, fmt.Errorf("pane %d cluster cell has no text", s.pane.ID)
		}
	}
	return cell, nil
}

func semanticStyleAt(styles []protocol.Style, index uint16) (protocol.Style, bool) {
	if int(index) >= len(styles) {
		return protocol.Style{}, false
	}
	return styles[index], true
}

func semanticClusterBytes(cell semanticCell, clusters []byte) ([]byte, bool) {
	start := int(cell.payload)
	end := start + int(cell.clusterLen)
	if cell.kind != semanticCluster || start < 0 || end > len(clusters) {
		return nil, false
	}
	return clusters[start:end], true
}

func currentEqualsSemantic(current currentSemanticCell, previous semanticCell, styles []protocol.Style, clusters []byte) bool {
	if current.kind != previous.kind || current.width != previous.width {
		return false
	}
	style, ok := semanticStyleAt(styles, previous.style)
	if !ok || current.style != style {
		return false
	}
	switch current.kind {
	case semanticScalar:
		return uint32(current.r) == previous.payload
	case semanticCluster:
		text, ok := semanticClusterBytes(previous, clusters)
		if !ok || len(current.text) != len(text) {
			return false
		}
		for index := range text {
			if current.text[index] != text[index] {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func internSemanticStyle(styles *[]protocol.Style, style protocol.Style) (uint16, error) {
	style = canonicalStyle(style)
	for index := range *styles {
		if (*styles)[index] == style {
			return uint16(index), nil
		}
	}
	if len(*styles) >= maxTerminalStyles || len(*styles) > math.MaxUint16 {
		return 0, fmt.Errorf("publication style capacity exceeded")
	}
	*styles = append(*styles, style)
	return uint16(len(*styles) - 1), nil
}

func appendCurrentSemantic(dst *[]semanticCell, styles *[]protocol.Style, clusters *[]byte, current currentSemanticCell) error {
	style, err := internSemanticStyle(styles, current.style)
	if err != nil {
		return err
	}
	cell := semanticCell{kind: current.kind, width: current.width, style: style}
	switch current.kind {
	case semanticScalar:
		cell.payload = uint32(current.r)
	case semanticCluster:
		if len(current.text) > maxGraphemeClusterBytes || len(*clusters) > math.MaxUint32-len(current.text) {
			return fmt.Errorf("publication cluster capacity exceeded")
		}
		cell.payload = uint32(len(*clusters))
		cell.clusterLen = uint16(len(current.text))
		*clusters = append(*clusters, current.text...)
	}
	*dst = append(*dst, cell)
	return nil
}

func translateSemanticCell(dstStyles *[]protocol.Style, dstClusters *[]byte, src semanticCell, srcStyles []protocol.Style, srcClusters []byte) (semanticCell, error) {
	style, ok := semanticStyleAt(srcStyles, src.style)
	if !ok {
		return semanticCell{}, fmt.Errorf("semantic cell style %d is unavailable", src.style)
	}
	styleIndex, err := internSemanticStyle(dstStyles, style)
	if err != nil {
		return semanticCell{}, err
	}
	cell := src
	cell.style = styleIndex
	if src.kind == semanticCluster {
		text, ok := semanticClusterBytes(src, srcClusters)
		if !ok || len(*dstClusters) > math.MaxUint32-len(text) {
			return semanticCell{}, fmt.Errorf("semantic cluster range is invalid")
		}
		cell.payload = uint32(len(*dstClusters))
		*dstClusters = append(*dstClusters, text...)
	}
	return cell, nil
}

func (s *panePublicationState) previousCell(row, column int) (semanticCell, []protocol.Style, []byte, bool) {
	if !s.snapshot.valid || s.snapshot.cols != s.cols() || s.snapshot.rows != s.rows() {
		return semanticCell{}, nil, nil, false
	}
	sourceRow := row
	if region := s.scroll; region != nil && row >= region.Top && row < region.Bottom {
		sourceRow = row - region.Delta
		if sourceRow < region.Top || sourceRow >= region.Bottom {
			return semanticCell{}, nil, nil, false
		}
	}
	index := sourceRow*s.snapshot.cols + column
	return s.snapshot.cells[index], s.snapshot.styles, s.snapshot.clusters, true
}

func (s *panePublicationState) candidateCellCount(keyframe bool) int {
	if keyframe {
		return s.rows() * s.cols()
	}
	total := 0
	for _, span := range s.dirty {
		if span.End > span.Start {
			total += span.End - span.Start
		}
	}
	return total
}

func (s *panePublicationState) currentCursor() publishedCursor {
	if s.pane.currentViewMode() == paneViewHistory && s.pane.historyView != nil {
		view := s.pane.historyView
		return publishedCursor{
			X:       uint16(min(max(view.CursorCol, 0), view.Snapshot.Cols-1)),
			Y:       uint16(min(max(view.CursorRow-view.ViewTop, 0), view.Snapshot.ViewportRows-1)),
			Visible: true,
		}
	}
	return publishedCursor{
		X:       uint16(max(0, s.pane.terminal.CursorX)),
		Y:       uint16(max(0, s.pane.terminal.CursorY)),
		Visible: s.pane.terminal.CursorVisible,
	}
}

func (s *panePublicationState) prepare(buffer *viewPublicationBuffer) error {
	if buffer == nil || s.pending != nil || !s.hasMutation() {
		return nil
	}
	s.ensureGeometry()
	rows, cols := s.rows(), s.cols()
	buffer.reset(cols, rows)
	buffer.returnTo = s.free
	publication := &buffer.publication
	total := rows * cols
	if cap(publication.Cells) < total {
		publication.Cells = make([]semanticCell, 0, total)
	}
	if cap(publication.Runs) < total {
		publication.Runs = make([]publishedRun, 0, total)
	}
	clusterCap := max(initialPublicationClusterCap, total*8)
	if cap(publication.Clusters) < clusterCap {
		publication.Clusters = make([]byte, 0, clusterCap)
	}
	counter := []byte(nil)
	counterStart := cols
	if s.pane.currentViewMode() == paneViewHistory {
		counter = s.historyCounterBytes()
		counterStart = max(0, cols-len(counter))
	}
	keyframe := s.keyframe || s.barrier || !s.snapshot.valid ||
		(s.version > 0 && (uint64(s.version)+1)%publicationAnchorInterval == 0)
	candidates := s.candidateCellCount(keyframe)
	if !keyframe && rows*cols > 0 && candidates*100 >= rows*cols*publicationKeyframeDensityPct {
		keyframe = true
		candidates = rows * cols
	} else if !keyframe && len(counter) > 0 && rows > 0 {
		span := s.dirty[0]
		overlap := max(0, min(cols, span.End)-max(counterStart, span.Start))
		candidates += cols - counterStart - overlap
	}
	publication.Epoch = s.epoch
	publication.Kind = PublicationDelta
	publication.BaseVersion = s.version
	if keyframe {
		publication.Kind = PublicationKeyframe
		publication.BaseVersion = 0
	}
	publication.TargetVersion = s.version + 1
	publication.LayoutRevision = s.layoutRevision
	publication.Barrier = s.barrier
	if s.scroll != nil && !keyframe {
		publication.HasScroll = true
		publication.Scroll = publishedScroll{
			Top: uint16(s.scroll.Top), Bottom: uint16(s.scroll.Bottom), Delta: int16(s.scroll.Delta),
		}
	}
	for row := 0; row < rows; row++ {
		clear(s.diff)
		start, end := 0, cols
		if !keyframe {
			span := s.dirty[row]
			start, end = max(0, span.Start), min(cols, span.End)
			if row == 0 && len(counter) > 0 {
				if end <= start {
					start, end = counterStart, cols
				} else {
					start = min(start, counterStart)
					end = max(end, cols)
				}
			}
			if end <= start {
				continue
			}
			for start > 0 {
				current, err := s.currentCell(row, start, counter)
				if err != nil {
					buffer.release()
					return err
				}
				previous, _, _, previousOK := s.previousCell(row, start)
				if current.kind != semanticContinuation &&
					(!previousOK || previous.kind != semanticContinuation) {
					break
				}
				start--
			}
			for end < cols {
				current, err := s.currentCell(row, end-1, counter)
				if err != nil {
					buffer.release()
					return err
				}
				previous, _, _, previousOK := s.previousCell(row, end-1)
				if current.width != 2 && (!previousOK || previous.width != 2) {
					break
				}
				end++
			}
		}
		for column := start; column < end; column++ {
			current, err := s.currentCell(row, column, counter)
			if err != nil {
				buffer.release()
				return err
			}
			if keyframe {
				s.diff[column] = true
				continue
			}
			previous, styles, clusters, ok := s.previousCell(row, column)
			s.diff[column] = !ok || !currentEqualsSemantic(current, previous, styles, clusters)
		}
		for column := start; column < end; column++ {
			if !s.diff[column] {
				continue
			}
			current, err := s.currentCell(row, column, counter)
			if err != nil {
				buffer.release()
				return err
			}
			previous, _, _, previousOK := s.previousCell(row, column)
			if current.kind == semanticContinuation && column > 0 {
				s.diff[column-1] = true
				start = min(start, column-1)
			}
			if current.width == 2 && column+1 < cols {
				s.diff[column+1] = true
				end = max(end, column+2)
			}
			if previousOK && previous.kind == semanticContinuation && column > 0 {
				s.diff[column-1] = true
				start = min(start, column-1)
			}
			if previousOK && previous.width == 2 && column+1 < cols {
				s.diff[column+1] = true
				end = max(end, column+2)
			}
		}
		for column := start; column < end; {
			if !s.diff[column] {
				column++
				continue
			}
			runStart := column
			for column < end && s.diff[column] {
				column++
			}
			runEnd := column
			cellStart := len(publication.Cells)
			for currentColumn := runStart; currentColumn < runEnd; currentColumn++ {
				current, err := s.currentCell(row, currentColumn, counter)
				if err != nil {
					buffer.release()
					return err
				}
				if err := appendCurrentSemantic(&publication.Cells, &publication.Styles, &publication.Clusters, current); err != nil {
					buffer.release()
					return err
				}
			}
			publication.Runs = append(publication.Runs, publishedRun{
				Row: uint16(row), Column: uint16(runStart), Columns: uint16(runEnd - runStart), CellStart: uint32(cellStart),
			})
			publication.changedCells += uint64(runEnd - runStart)
		}
	}
	publication.candidateCells = uint64(candidates)
	if publication.changedCells > publication.candidateCells {
		publication.candidateCells = publication.changedCells
	}
	if publication.changedCells < publication.candidateCells {
		publication.cancelledCells = publication.candidateCells - publication.changedCells
	}
	cursor := s.currentCursor()
	publication.Cursor = cursor
	publication.CursorChanged = keyframe || s.barrier || publication.HasScroll ||
		s.cursorDirty && (!s.snapshot.valid || cursor != s.snapshot.cursor)
	if len(publication.Runs) == 0 && !publication.HasScroll && !publication.CursorChanged && !keyframe {
		s.pane.renderMetrics.candidateCells.Add(publication.candidateCells)
		s.pane.renderMetrics.cancelledCells.Add(publication.cancelledCells)
		if s.attributePTYDrain {
			s.pane.renderMetrics.ptyDrainCancelledCells.Add(publication.cancelledCells)
			s.attributePTYDrain = false
		}
		s.clearMutation()
		buffer.release()
		return nil
	}
	buffer.metrics = &s.pane.renderMetrics
	buffer.fromPTYDrain = s.attributePTYDrain
	s.attributePTYDrain = false
	if err := s.prepareNextSnapshot(publication); err != nil {
		buffer.release()
		return err
	}
	s.pending = buffer
	s.clearMutation()
	return nil
}

func (s *panePublicationState) prepareNextSnapshot(publication *viewPublication) error {
	rows, cols := int(publication.Rows), int(publication.Cols)
	next := &s.nextSnapshot
	next.reset(cols, rows)
	if publication.Kind != PublicationKeyframe {
		defaultStyle, err := internSemanticStyle(&next.styles, protocol.CanonicalDefaultStyle())
		if err != nil {
			return err
		}
		blank := semanticCell{kind: semanticBlank, width: 1, style: defaultStyle}
		for row := 0; row < rows; row++ {
			for column := 0; column < cols; column++ {
				sourceRow := row
				if publication.HasScroll && row >= int(publication.Scroll.Top) && row < int(publication.Scroll.Bottom) {
					sourceRow = row - int(publication.Scroll.Delta)
					if sourceRow < int(publication.Scroll.Top) || sourceRow >= int(publication.Scroll.Bottom) {
						next.cells[row*cols+column] = blank
						continue
					}
				}
				source := s.snapshot.cells[sourceRow*cols+column]
				copied, err := translateSemanticCell(&next.styles, &next.clusters, source, s.snapshot.styles, s.snapshot.clusters)
				if err != nil {
					return err
				}
				next.cells[row*cols+column] = copied
			}
		}
	}
	for _, run := range publication.Runs {
		cellStart := int(run.CellStart)
		for offset := 0; offset < int(run.Columns); offset++ {
			copied, err := translateSemanticCell(&next.styles, &next.clusters,
				publication.Cells[cellStart+offset], publication.Styles, publication.Clusters)
			if err != nil {
				return err
			}
			next.cells[int(run.Row)*cols+int(run.Column)+offset] = copied
		}
	}
	if publication.CursorChanged {
		next.cursor = publication.Cursor
	} else {
		next.cursor = s.snapshot.cursor
	}
	next.epoch = publication.Epoch
	next.version = publication.TargetVersion
	next.valid = true
	return nil
}

func (s *panePublicationState) handedOff() {
	if s.pending == nil {
		return
	}
	publication := &s.pending.publication
	s.version = publication.TargetVersion
	s.snapshot, s.nextSnapshot = s.nextSnapshot, s.snapshot
	s.pane.renderMetrics.recordPublication(publication, s.pending.fromPTYDrain)
	s.pending = nil
}

func (s *panePublicationState) clearMutation() {
	clear(s.dirty)
	s.dirtyRows = 0
	s.scroll = nil
	s.cursorDirty = false
	s.keyframe = false
	s.barrier = false
}

func (s *panePublicationState) requestSync(done chan<- *OutputLease) {
	if done != nil {
		s.syncWaiters = append(s.syncWaiters, done)
	}
}

func (s *panePublicationState) flushSyncWaiters() {
	if s.pending != nil || s.hasMutation() {
		return
	}
	for _, done := range s.syncWaiters {
		done <- s.lease
	}
	s.syncWaiters = s.syncWaiters[:0]
}

func semanticCellText(cell semanticCell, clusters []byte) ([]byte, rune, error) {
	switch cell.kind {
	case semanticBlank:
		return nil, ' ', nil
	case semanticScalar:
		return nil, rune(cell.payload), nil
	case semanticCluster:
		text, ok := semanticClusterBytes(cell, clusters)
		if !ok {
			return nil, 0, fmt.Errorf("publication cluster range is invalid")
		}
		return text, 0, nil
	case semanticContinuation:
		return nil, 0, nil
	default:
		return nil, 0, fmt.Errorf("invalid semantic cell kind %d", cell.kind)
	}
}
