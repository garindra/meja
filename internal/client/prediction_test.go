package client

import (
	"bytes"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestPredictionInputDecoderAcceptsLegacyAndKittyText(t *testing.T) {
	var decoder predictionInputDecoder
	var got []byte
	for _, part := range [][]byte{
		[]byte("ab"),
		[]byte("\x1b[9"),
		[]byte("9;1:1u"),
		[]byte("\x1b[100;1:2u"),
		[]byte("\x1b[100;1:3u"),
		[]byte("\x1b[127u"),
	} {
		got = append(got, decoder.Feed(part)...)
	}
	if want := []byte("abcd\x7f"); !bytes.Equal(got, want) {
		t.Fatalf("decoded prediction input = %q, want %q", got, want)
	}
}

func TestPredictionInputDecoderRejectsPaneDependentInput(t *testing.T) {
	var decoder predictionInputDecoder
	got := decoder.Feed([]byte("\x1b[<64;20;10M\x1b[<0;2;2M\x1b[<32;3;3M\x1b[I\x1b[200~not predicted\x1b[201~\x1b[97;1:3u\x1b[98;5u"))
	if want := []byte{0, 0, 0, 0}; !bytes.Equal(got, want) {
		t.Fatalf("prediction boundaries = %v, want %v", got, want)
	}
	if decoder.state != predictionDecodeGround {
		t.Fatalf("decoder state = %v", decoder.state)
	}
}

func testPredictionContext() predictionContext {
	return predictionContext{
		target: predictionTarget{paneID: 1, slot: 0, layoutRevision: 1},
		cursor: protocol.Cursor{}, cursorVisible: true,
		width: 8, height: 1,
	}
}

func applyPredictorFrame(t *testing.T, predictor *inputPredictor, cache *paneScanoutCache, frame renderFrame) predictionResult {
	t.Helper()
	evidence := frameEvidence{touched: make(map[cellPosition]authoritativeCellChange), cursorUpdated: frame.cursorUpdated, scrolled: frame.scrollDelta != 0}
	cache.scroll(frame.scrollDelta)
	for _, span := range frame.spans {
		if err := applySpanToCache(cache, span, &evidence); err != nil {
			t.Fatal(err)
		}
	}
	return predictor.applyAuthoritativeFrame(testPredictionContext().target, frame, evidence, cache)
}

func TestInputPredictorRevealsSuffixAfterPrefixConfirmation(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	if result, changed := predictor.applyLocalInput([]byte("abc"), testPredictionContext(), cache); changed || len(result.frame.spans) != 0 {
		t.Fatalf("untrusted local result = %#v changed=%v", result, changed)
	}

	frame := renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	}
	result := applyPredictorFrame(t, &predictor, cache, frame)
	if len(predictor.scopes) != 1 || !predictor.scopes[0].trusted || len(predictor.scopes[0].pending) != 2 {
		t.Fatalf("predictor after confirmation = %#v", predictor)
	}
	if len(result.frame.spans) != 2 || string(result.frame.spans[1].text) != "bc" || result.frame.spans[1].column != 1 {
		t.Fatalf("decorated frame = %#v", result.frame)
	}
	if !result.hasCursorOverride || result.cursorOverride.Cursor != (protocol.Cursor{X: 3}) {
		t.Fatalf("cursor override = %#v present=%v", result.cursorOverride, result.hasCursorOverride)
	}
}

func TestInputPredictorRepairsVisibleSuffixOnConflict(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	predictor.applyLocalInput([]byte("abc"), testPredictionContext(), cache)
	applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	})

	result := applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 1, styleID: 0, cellWidth: 1, text: []byte("x")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	})
	if predictor.active() {
		t.Fatalf("predictor remained active after conflict: %#v", predictor)
	}
	if result.hasCursorOverride {
		t.Fatalf("conflict retained cursor override %#v", result.cursorOverride)
	}
	if len(result.frame.spans) < 2 || string(result.frame.spans[len(result.frame.spans)-1].text) != " " {
		t.Fatalf("conflict did not repair remaining predicted cell: %#v", result.frame.spans)
	}
}

func TestInputPredictorRejectsMatchingContentWithWrongCursor(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	predictor.applyLocalInput([]byte("abc"), testPredictionContext(), cache)
	result := applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 7}, cursorVisible: true, cursorUpdated: true,
	})
	if predictor.active() || result.hasCursorOverride {
		t.Fatalf("cursor mismatch was accepted: predictor=%#v result=%#v", predictor, result)
	}
}

func TestInputPredictorBoundaryIgnoresRestOfInputChunk(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	predictor.applyLocalInput([]byte("a"), testPredictionContext(), cache)
	applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	})

	result, changed := predictor.applyLocalInput([]byte("b\rsecret"), testPredictionContext(), cache)
	if !changed || len(predictor.scopes) != 1 || !predictor.scopes[0].closed || len(predictor.scopes[0].pending) != 1 {
		t.Fatalf("boundary state = %#v changed=%v", predictor, changed)
	}
	if got := predictor.scopes[0].pending[0].char; got != 'b' {
		t.Fatalf("pending character = %q", got)
	}
	if len(result.frame.spans) != 1 || string(result.frame.spans[0].text) != "b" {
		t.Fatalf("boundary display = %#v", result.frame.spans)
	}
}

func TestInputPredictorShowsBackspaceInTrustedScope(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	predictor.applyLocalInput([]byte("abc"), testPredictionContext(), cache)
	applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("abc")}},
		cursor:         protocol.Cursor{X: 3}, cursorVisible: true, cursorUpdated: true,
	})

	result, changed := predictor.applyLocalInput([]byte{0x7f}, predictionContext{
		target: testPredictionContext().target, cursor: protocol.Cursor{X: 3}, cursorVisible: true, width: 8, height: 1,
	}, cache)
	if !changed || len(result.frame.spans) != 1 || string(result.frame.spans[0].text) != " " || result.frame.spans[0].column != 2 {
		t.Fatalf("backspace display = %#v changed=%v", result.frame.spans, changed)
	}
	if !result.hasCursorOverride || result.cursorOverride.Cursor != (protocol.Cursor{X: 2}) {
		t.Fatalf("backspace cursor = %#v present=%v", result.cursorOverride, result.hasCursorOverride)
	}
	if pending := predictor.pendingOperations(); len(pending) != 1 || pending[0].op.kind != predictionBackspace {
		t.Fatalf("backspace operations = %#v", pending)
	}
}

func TestInputPredictorAcceptsCoalescedInsertAndBackspace(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	predictor.applyLocalInput([]byte("abc\x7f"), testPredictionContext(), cache)
	result := applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("ab ")}},
		cursor:         protocol.Cursor{X: 2}, cursorVisible: true, cursorUpdated: true,
	})
	if len(predictor.pendingOperations()) != 0 || len(predictor.scopes) != 1 || !predictor.scopes[0].trusted {
		t.Fatalf("coalesced state = %#v", predictor)
	}
	if result.hasCursorOverride {
		t.Fatalf("coalesced result retained cursor override %#v", result.cursorOverride)
	}
}

func TestInputPredictorKeepsBackspaceOverIntermediateInsertFrame(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	predictor.applyLocalInput([]byte("a"), testPredictionContext(), cache)
	applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	})
	predictor.applyLocalInput([]byte("c\x7f"), predictionContext{
		target: testPredictionContext().target, cursor: protocol.Cursor{X: 1}, cursorVisible: true, width: 8, height: 1,
	}, cache)

	result := applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 1, styleID: 0, cellWidth: 1, text: []byte("c")}},
		cursor:         protocol.Cursor{X: 2}, cursorVisible: true, cursorUpdated: true,
	})
	if pending := predictor.pendingOperations(); len(pending) != 1 || pending[0].op.kind != predictionBackspace {
		t.Fatalf("intermediate state = %#v", predictor)
	}
	if len(result.frame.spans) != 2 || string(result.frame.spans[1].text) != " " || result.frame.spans[1].column != 1 {
		t.Fatalf("intermediate decoration = %#v", result.frame.spans)
	}
	if !result.hasCursorOverride || result.cursorOverride.Cursor != (protocol.Cursor{X: 1}) {
		t.Fatalf("intermediate cursor = %#v present=%v", result.cursorOverride, result.hasCursorOverride)
	}
}

func TestInputPredictorSupportsTypingAfterBackspace(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	var predictor inputPredictor
	predictor.applyLocalInput([]byte("abc"), testPredictionContext(), cache)
	applyPredictorFrame(t, &predictor, cache, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("abc")}},
		cursor:         protocol.Cursor{X: 3}, cursorVisible: true, cursorUpdated: true,
	})
	context := predictionContext{
		target: testPredictionContext().target, cursor: protocol.Cursor{X: 3}, cursorVisible: true, width: 8, height: 1,
	}
	result, changed := predictor.applyLocalInput([]byte("\x7fd"), context, cache)
	if !changed || len(result.frame.spans) != 2 || string(result.frame.spans[0].text) != " " || string(result.frame.spans[1].text) != "d" {
		t.Fatalf("replacement display = %#v changed=%v", result.frame.spans, changed)
	}
	if !result.hasCursorOverride || result.cursorOverride.Cursor != (protocol.Cursor{X: 3}) {
		t.Fatalf("replacement cursor = %#v present=%v", result.cursorOverride, result.hasCursorOverride)
	}
}

func TestInputPredictorRefusesNonblankTarget(t *testing.T) {
	cache := newPaneScanoutCache(8, 1)
	cache.row(0)[0] = scanoutCell{Cluster: "x", Width: 1}
	var predictor inputPredictor
	if result, changed := predictor.applyLocalInput([]byte("a"), testPredictionContext(), cache); changed || len(result.frame.spans) != 0 || len(predictor.pendingOperations()) != 0 {
		t.Fatalf("nonblank prediction result=%#v changed=%v predictor=%#v", result, changed, predictor)
	}
}

func TestScanoutPredictionDecoratesAuthoritativeFrame(t *testing.T) {
	s := newScanoutState(true)
	s.cols, s.rows = 8, 2
	s.layout = testSnapshotLayout(1)
	s.layout.Panes[0].Rect.Height = 1
	s.caches[0] = newPaneScanoutCache(8, 1)
	s.styles[0] = defaultStyles()
	s.cursors[0] = protocol.CursorUpdate{Visible: true}
	s.selectAuthoritativeCursor()

	if changed, err := s.acceptLocalInput([]byte("ab")); err != nil || changed {
		t.Fatalf("untrusted local input changed=%v err=%v", changed, err)
	}
	if _, err := s.acceptFrame(0, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	}); err != nil {
		t.Fatal(err)
	}
	out := string(s.takeANSI())
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") || !strings.HasSuffix(out, "\x1b[1;3H\x1b[?25h") {
		t.Fatalf("prediction output = %q", out)
	}
	if got := s.caches[0].row(0)[1].Cluster; got != "" {
		t.Fatalf("prediction contaminated authoritative cache with %q", got)
	}
}

func TestScanoutPredictionEmitsBackspaceWithoutChangingAuthoritativeCache(t *testing.T) {
	s := newScanoutState(true)
	s.cols, s.rows = 8, 2
	s.layout = testSnapshotLayout(1)
	s.layout.Panes[0].Rect.Height = 1
	s.caches[0] = newPaneScanoutCache(8, 1)
	s.styles[0] = defaultStyles()
	s.cursors[0] = protocol.CursorUpdate{Visible: true}
	s.selectAuthoritativeCursor()

	s.acceptLocalInput([]byte("a"))
	if _, err := s.acceptFrame(0, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.takeANSI()

	changed, err := s.acceptLocalInput([]byte{0x7f})
	if err != nil || !changed {
		t.Fatalf("backspace changed=%v err=%v", changed, err)
	}
	out := string(s.takeANSI())
	if !strings.Contains(out, " ") || !strings.HasSuffix(out, "\x1b[1;1H\x1b[?25h") {
		t.Fatalf("backspace output = %q", out)
	}
	if got := s.caches[0].row(0)[0].Cluster; got != "a" {
		t.Fatalf("backspace changed authoritative cache to %q", got)
	}
}

func TestUnfocusedPaneFrameDoesNotConfirmPrediction(t *testing.T) {
	s := newScanoutState(true)
	s.cols, s.rows = 10, 2
	s.layout = protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, FocusedPaneID: 1, Panes: []protocol.PanePlacement{
		{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 4, Height: 1}},
		{PaneID: 2, Slot: 1, Rect: protocol.Rect{X: 5, Width: 4, Height: 1}},
	}}
	s.caches[0] = newPaneScanoutCache(4, 1)
	s.caches[1] = newPaneScanoutCache(4, 1)
	s.styles[0], s.styles[1] = defaultStyles(), defaultStyles()
	s.cursors[0] = protocol.CursorUpdate{Visible: true}
	s.cursors[1] = protocol.CursorUpdate{Visible: true}
	s.selectAuthoritativeCursor()
	s.acceptLocalInput([]byte("ab"))

	if _, err := s.acceptFrame(1, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(s.predictor.scopes) != 1 || s.predictor.scopes[0].trusted || len(s.predictor.scopes[0].pending) != 2 {
		t.Fatalf("unfocused frame changed predictor: %#v", s.predictor)
	}
}

func TestFocusChangeRepairsPredictionAndUsesNewPaneCursor(t *testing.T) {
	s := newScanoutState(true)
	s.cols, s.rows = 10, 2
	s.layout = protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, FocusedPaneID: 1, Panes: []protocol.PanePlacement{
		{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 4, Height: 1}},
		{PaneID: 2, Slot: 1, Rect: protocol.Rect{X: 5, Width: 4, Height: 1}},
	}}
	s.caches[0] = newPaneScanoutCache(4, 1)
	s.caches[1] = newPaneScanoutCache(4, 1)
	s.styles[0], s.styles[1] = defaultStyles(), defaultStyles()
	s.cursors[0] = protocol.CursorUpdate{Visible: true}
	s.cursors[1] = protocol.CursorUpdate{Cursor: protocol.Cursor{X: 2}, Visible: true}
	s.selectAuthoritativeCursor()

	s.acceptLocalInput([]byte("ab"))
	if _, err := s.acceptFrame(0, renderFrame{
		layoutRevision: 1,
		spans:          []paintSpan{{kind: paintText, row: 0, column: 0, styleID: 0, cellWidth: 1, text: []byte("a")}},
		cursor:         protocol.Cursor{X: 1}, cursorVisible: true, cursorUpdated: true,
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.takeANSI()

	next := s.layout
	next.FocusedPaneID = 2
	if _, err := s.acceptLayout(next); err != nil {
		t.Fatal(err)
	}
	out := string(s.takeANSI())
	if s.predictor.active() {
		t.Fatalf("predictor survived focus change: %#v", s.predictor)
	}
	if !strings.Contains(out, "\x1b[1;2H") || !strings.HasSuffix(out, "\x1b[1;8H\x1b[?25h") {
		t.Fatalf("focus repair output = %q", out)
	}
}
