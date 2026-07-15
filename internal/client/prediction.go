package client

import "github.com/garindra/meja/internal/protocol"

type predictionTarget struct {
	paneID         uint64
	slot           uint8
	layoutRevision uint64
}

type predictionContext struct {
	target        predictionTarget
	cursor        protocol.Cursor
	cursorVisible bool
	width, height int
}

type predictionOperation struct {
	sequence    uint64
	kind        predictionOperationKind
	char        byte
	position    protocol.Cursor
	cursorAfter protocol.Cursor
	styleID     uint32
}

type predictionOperationKind uint8

const (
	predictionInsert predictionOperationKind = iota + 1
	predictionBackspace
)

type predictionScope struct {
	id      uint64
	target  predictionTarget
	trusted bool
	closed  bool
	pending []predictionOperation
}

type predictedOperationRef struct {
	scopeIndex int
	op         predictionOperation
}

type cellPosition struct {
	row, column int
}

type authoritativeCellChange struct {
	before, after protocol.Cell
}

type frameEvidence struct {
	touched       map[cellPosition]authoritativeCellChange
	cursorUpdated bool
	scrolled      bool
}

type predictionResult struct {
	frame             renderFrame
	cursorOverride    protocol.CursorUpdate
	hasCursorOverride bool
	repaintPane       bool
}

// inputPredictor owns speculative input operations and produces display-only
// render-frame additions. It never mutates the authoritative pane cache.
type inputPredictor struct {
	nextScopeID  uint64
	nextSequence uint64
	target       predictionTarget
	scopes       []predictionScope

	currentCursor  protocol.Cursor
	hasCursor      bool
	awaitingCursor bool
}

func (p *inputPredictor) applyLocalInput(data []byte, context predictionContext, view *paneScanoutCache) (predictionResult, bool) {
	before := p.visibleOperations()
	if p.active() && p.target != context.target {
		p.clear()
	}
	if !p.active() {
		p.target = context.target
		p.currentCursor = context.cursor
		p.hasCursor = context.cursorVisible
		p.awaitingCursor = false
	}

	for _, b := range data {
		if b == 0x7f {
			if p.awaitingCursor || !p.hasCursor || !context.cursorVisible || !p.acceptBackspace(context, view) {
				p.boundary()
			}
			continue
		}
		if b < 0x20 || b > 0x7e {
			p.boundary()
			continue
		}
		if p.awaitingCursor || !p.hasCursor || !context.cursorVisible {
			continue
		}
		if !p.acceptPrintable(b, context, view) {
			p.boundary()
		}
	}

	after := p.visibleOperations()
	spans := repairRemovedPredictions(before, after, view)
	spans = append(spans, predictionSpans(after)...)
	result := predictionResult{frame: renderFrame{layoutRevision: context.target.layoutRevision, spans: spans}}
	if cursor, ok := lastPredictedCursor(after); ok {
		result.cursorOverride = protocol.CursorUpdate{Cursor: cursor, Visible: true}
		result.hasCursorOverride = true
	}
	changed := len(spans) > 0 || len(before) != len(after)
	return result, changed
}

func (p *inputPredictor) acceptPrintable(b byte, context predictionContext, view *paneScanoutCache) bool {
	if p.currentCursor.Y < 0 || p.currentCursor.Y >= context.height || p.currentCursor.X < 0 || p.currentCursor.X >= context.width-1 {
		return false
	}
	if view == nil || p.currentCursor.Y >= view.rows || p.currentCursor.X >= view.cols {
		return false
	}
	targetCell, ok := p.composedCell(p.currentCursor, view)
	if !ok {
		return false
	}
	if !predictionBlankCell(targetCell) {
		return false
	}

	scope := p.currentScope()
	if scope == nil {
		p.nextScopeID++
		p.scopes = append(p.scopes, predictionScope{id: p.nextScopeID, target: context.target})
		scope = &p.scopes[len(p.scopes)-1]
	}

	styleID := targetCell.StyleID
	if p.currentCursor.X > 0 {
		left, leftOK := p.composedCell(protocol.Cursor{X: p.currentCursor.X - 1, Y: p.currentCursor.Y}, view)
		if leftOK && left.Width == 1 {
			styleID = left.StyleID
		}
	}
	p.nextSequence++
	op := predictionOperation{
		sequence:    p.nextSequence,
		kind:        predictionInsert,
		char:        b,
		position:    p.currentCursor,
		cursorAfter: protocol.Cursor{X: p.currentCursor.X + 1, Y: p.currentCursor.Y},
		styleID:     styleID,
	}
	scope.pending = append(scope.pending, op)
	p.currentCursor = op.cursorAfter
	return true
}

func (p *inputPredictor) acceptBackspace(context predictionContext, view *paneScanoutCache) bool {
	if p.currentCursor.Y < 0 || p.currentCursor.Y >= context.height || p.currentCursor.X <= 0 || p.currentCursor.X >= context.width {
		return false
	}
	if view == nil || p.currentCursor.Y >= view.rows || p.currentCursor.X >= view.cols {
		return false
	}
	current, currentOK := p.composedCell(p.currentCursor, view)
	if !currentOK || !predictionBlankCell(current) {
		return false
	}
	position := protocol.Cursor{X: p.currentCursor.X - 1, Y: p.currentCursor.Y}
	deleted, deletedOK := p.composedCell(position, view)
	if !deletedOK || deleted.Width != 1 || predictionBlankCell(deleted) {
		return false
	}

	scope := p.currentScope()
	if scope == nil {
		p.nextScopeID++
		p.scopes = append(p.scopes, predictionScope{id: p.nextScopeID, target: context.target})
		scope = &p.scopes[len(p.scopes)-1]
	}
	p.nextSequence++
	op := predictionOperation{
		sequence: p.nextSequence, kind: predictionBackspace,
		position: position, cursorAfter: position, styleID: deleted.StyleID,
	}
	scope.pending = append(scope.pending, op)
	p.currentCursor = position
	return true
}

func (p *inputPredictor) applyAuthoritativeFrame(target predictionTarget, incoming renderFrame, evidence frameEvidence, view *paneScanoutCache) predictionResult {
	result := predictionResult{frame: incoming}
	if !p.active() || target != p.target {
		return result
	}
	visibleBefore := p.visibleOperations()
	if evidence.scrolled || !incoming.cursorVisible {
		p.clear()
		if evidence.scrolled {
			result.repaintPane = len(visibleBefore) > 0
		} else {
			result.frame = appendPredictionDecoration(incoming, repairSpans(visibleBefore, view))
		}
		return result
	}

	pending := p.pendingOperations()
	if len(pending) == 0 {
		if p.awaitingCursor {
			p.clear()
			return result
		}
		if evidence.cursorUpdated && p.hasCursor && incoming.cursor != p.currentCursor {
			p.clear()
			return result
		}
		if evidence.cursorUpdated {
			p.currentCursor = incoming.cursor
			p.hasCursor = true
		}
		return p.withVisibleOverlay(result, view)
	}

	matched, credit := matchingPredictionPrefix(pending, evidence, incoming.cursor)

	if matched > 0 {
		if !incoming.cursorVisible {
			p.clear()
			result.frame = appendPredictionDecoration(incoming, repairSpans(visibleBefore, view))
			return result
		}
		for scopeIndex := range credit {
			p.scopes[scopeIndex].trusted = true
		}
		p.removeConfirmedPrefix(matched)
		p.collectClosedScopes()
		if len(p.pendingOperations()) == 0 {
			p.currentCursor = incoming.cursor
			p.hasCursor = true
		}
		return p.withVisibleOverlay(result, view)
	}

	_, conflict := evidence.touched[cellPosition{row: pending[0].op.position.Y, column: pending[0].op.position.X}]
	if conflict || evidence.cursorUpdated {
		p.clear()
		result.frame = appendPredictionDecoration(incoming, repairSpans(visibleBefore, view))
		return result
	}
	return p.withVisibleOverlay(result, view)
}

type predictedCellEffect struct {
	char       byte
	scopeIndex int
}

func matchingPredictionPrefix(pending []predictedOperationRef, evidence frameEvidence, cursor protocol.Cursor) (int, map[int]bool) {
	effects := make(map[cellPosition]predictedCellEffect)
	best := 0
	var bestCredit map[int]bool
	for index, ref := range pending {
		position := cellPosition{row: ref.op.position.Y, column: ref.op.position.X}
		effects[position] = predictedCellEffect{char: ref.op.displayChar(), scopeIndex: ref.scopeIndex}
		if ref.op.cursorAfter != cursor {
			continue
		}
		credit := make(map[int]bool)
		matches := true
		for effectPosition, effect := range effects {
			change, touched := evidence.touched[effectPosition]
			if !touched || !predictionCellMatches(change.after, effect.char) {
				matches = false
				break
			}
			if effect.char != ' ' && !predictionCellMatches(change.before, effect.char) {
				credit[effect.scopeIndex] = true
			}
		}
		if matches {
			best = index + 1
			bestCredit = credit
		}
	}
	return best, bestCredit
}

func (p *inputPredictor) withVisibleOverlay(result predictionResult, _ *paneScanoutCache) predictionResult {
	visible := p.visibleOperations()
	result.frame = appendPredictionDecoration(result.frame, predictionSpans(visible))
	if cursor, ok := lastPredictedCursor(visible); ok {
		result.cursorOverride = protocol.CursorUpdate{Cursor: cursor, Visible: true}
		result.hasCursorOverride = true
	}
	return result
}

func (p *inputPredictor) reset(layoutRevision uint64, view *paneScanoutCache) (renderFrame, bool) {
	visible := p.visibleOperations()
	p.clear()
	spans := repairSpans(visible, view)
	return renderFrame{layoutRevision: layoutRevision, spans: spans}, len(spans) > 0
}

func (p *inputPredictor) boundary() {
	if scope := p.currentScope(); scope != nil {
		scope.closed = true
		if !scope.trusted {
			p.scopes = p.scopes[:len(p.scopes)-1]
		}
	}
	p.hasCursor = false
	p.awaitingCursor = true
	if len(p.scopes) == 0 {
		p.target = predictionTarget{}
	}
}

func (p *inputPredictor) active() bool {
	return p.target != (predictionTarget{}) || len(p.scopes) > 0
}

func (p *inputPredictor) clear() {
	p.target = predictionTarget{}
	p.scopes = nil
	p.hasCursor = false
	p.awaitingCursor = false
}

func (p *inputPredictor) currentScope() *predictionScope {
	if len(p.scopes) == 0 || p.scopes[len(p.scopes)-1].closed {
		return nil
	}
	return &p.scopes[len(p.scopes)-1]
}

func (p *inputPredictor) composedCell(cursor protocol.Cursor, view *paneScanoutCache) (protocol.Cell, bool) {
	if view == nil || cursor.Y < 0 || cursor.Y >= view.rows || cursor.X < 0 || cursor.X >= view.cols {
		return protocol.Cell{}, false
	}
	cell := view.row(cursor.Y)[cursor.X]
	for _, scope := range p.scopes {
		for _, op := range scope.pending {
			if op.position == cursor {
				cell = protocol.Cell{Rune: rune(op.displayChar()), StyleID: op.styleID, Width: 1}
			}
		}
	}
	return cell, true
}

func (p *inputPredictor) pendingOperations() []predictedOperationRef {
	var refs []predictedOperationRef
	for scopeIndex, scope := range p.scopes {
		for _, op := range scope.pending {
			refs = append(refs, predictedOperationRef{scopeIndex: scopeIndex, op: op})
		}
	}
	return refs
}

func (p *inputPredictor) visibleOperations() []predictionOperation {
	var ops []predictionOperation
	for _, scope := range p.scopes {
		if scope.trusted {
			ops = append(ops, scope.pending...)
		}
	}
	return ops
}

func (p *inputPredictor) removeConfirmedPrefix(count int) {
	for i := range p.scopes {
		if count == 0 {
			return
		}
		if count >= len(p.scopes[i].pending) {
			count -= len(p.scopes[i].pending)
			p.scopes[i].pending = nil
			continue
		}
		p.scopes[i].pending = p.scopes[i].pending[count:]
		return
	}
}

func (p *inputPredictor) collectClosedScopes() {
	out := p.scopes[:0]
	for _, scope := range p.scopes {
		if scope.closed && len(scope.pending) == 0 {
			continue
		}
		out = append(out, scope)
	}
	p.scopes = out
	if len(p.scopes) == 0 {
		p.target = predictionTarget{}
	}
}

func predictionBlankCell(cell protocol.Cell) bool {
	return cell.Width == 1 && (cell.Rune == 0 || cell.Rune == ' ')
}

func predictionCellMatches(cell protocol.Cell, b byte) bool {
	r := cell.Rune
	if r == 0 {
		r = ' '
	}
	return cell.Width == 1 && r == rune(b)
}

func (op predictionOperation) displayChar() byte {
	if op.kind == predictionBackspace {
		return ' '
	}
	return op.char
}

func lastPredictedCursor(ops []predictionOperation) (protocol.Cursor, bool) {
	if len(ops) == 0 {
		return protocol.Cursor{}, false
	}
	return ops[len(ops)-1].cursorAfter, true
}

func appendPredictionDecoration(frame renderFrame, decoration []paintSpan) renderFrame {
	if len(decoration) == 0 {
		return frame
	}
	out := frame
	out.spans = append(frame.spans[:len(frame.spans):len(frame.spans)], decoration...)
	return out
}

func repairRemovedPredictions(before, after []predictionOperation, view *paneScanoutCache) []paintSpan {
	remaining := make(map[cellPosition]byte, len(after))
	for _, op := range after {
		remaining[cellPosition{row: op.position.Y, column: op.position.X}] = op.displayChar()
	}
	removed := make([]predictionOperation, 0, len(before))
	for _, op := range before {
		if ch, ok := remaining[cellPosition{row: op.position.Y, column: op.position.X}]; !ok || ch != op.displayChar() {
			removed = append(removed, op)
		}
	}
	return repairSpans(removed, view)
}

func repairSpans(ops []predictionOperation, view *paneScanoutCache) []paintSpan {
	if view == nil || len(ops) == 0 {
		return nil
	}
	seen := make(map[cellPosition]struct{}, len(ops))
	spans := make([]paintSpan, 0, len(ops))
	for _, op := range ops {
		position := cellPosition{row: op.position.Y, column: op.position.X}
		if _, ok := seen[position]; ok || position.row < 0 || position.row >= view.rows || position.column < 0 || position.column >= view.cols {
			continue
		}
		seen[position] = struct{}{}
		cell := view.row(position.row)[position.column]
		r := cell.Rune
		if r == 0 {
			r = ' '
		}
		spans = append(spans, paintSpan{kind: paintText, row: position.row, column: position.column, styleID: cell.StyleID, cellWidth: 1, text: []byte(string(r))})
	}
	return spans
}

func predictionSpans(ops []predictionOperation) []paintSpan {
	if len(ops) == 0 {
		return nil
	}
	spans := make([]paintSpan, 0, len(ops))
	for _, op := range ops {
		if len(spans) > 0 {
			last := &spans[len(spans)-1]
			if last.row == op.position.Y && last.column+len(last.text) == op.position.X && last.styleID == op.styleID {
				last.text = append(last.text, op.displayChar())
				continue
			}
		}
		spans = append(spans, paintSpan{kind: paintText, row: op.position.Y, column: op.position.X, styleID: op.styleID, cellWidth: 1, text: []byte{op.displayChar()}})
	}
	return spans
}
