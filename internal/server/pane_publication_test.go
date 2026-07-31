package server

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func publicationTestPane(cols, rows int, lines ...string) *Pane {
	pane := &Pane{ID: 1, terminal: newTerminal(cols, rows)}
	for row, line := range lines {
		if row >= rows {
			break
		}
		cells := pane.terminal.gridRow(row)
		column := 0
		for _, r := range line {
			if column >= cols {
				break
			}
			pane.terminal.replaceTextCell(cells, column, string(r), 1, 0)
			column++
		}
	}
	return pane
}

func startPublicationTestState(t *testing.T, pane *Pane) (*panePublicationState, *viewPublicationBuffer) {
	t.Helper()
	state := newPanePublicationState(pane)
	state.lease = &OutputLease{}
	state.invalidateEpoch(true)
	publication := takePublicationTestBuffer(t, state)
	if publication == nil || publication.publication.Kind != PublicationKeyframe {
		t.Fatalf("initial publication = %#v, want keyframe", publication)
	}
	return state, publication
}

func takePublicationTestBuffer(t *testing.T, state *panePublicationState) *viewPublicationBuffer {
	t.Helper()
	var buffer *viewPublicationBuffer
	select {
	case buffer = <-state.free:
	case <-time.After(time.Second):
		t.Fatal("publication buffer was unavailable")
	}
	if err := state.prepare(buffer); err != nil {
		t.Fatal(err)
	}
	if state.pending == nil {
		return nil
	}
	publication := state.pending
	state.handedOff()
	return publication
}

func publicationTestUpdate(rows, row, start, end int) Update {
	spans := make([]DirtySpan, rows)
	spans[row] = DirtySpan{Start: start, End: end}
	return Update{DirtySpans: spans}
}

func publicationCellString(t *testing.T, publication *viewPublication, cell semanticCell) string {
	t.Helper()
	text, r, err := semanticCellText(cell, publication.Clusters)
	if err != nil {
		t.Fatal(err)
	}
	if text != nil {
		return string(text)
	}
	return string(r)
}

func snapshotCellString(t *testing.T, snapshot *semanticViewport, row, column int) string {
	t.Helper()
	cell := snapshot.cells[row*snapshot.cols+column]
	text, r, err := semanticCellText(cell, snapshot.clusters)
	if err != nil {
		t.Fatal(err)
	}
	if text != nil {
		return string(text)
	}
	return string(r)
}

func TestPanePublicationUsesFinalStateAndCancelsReversion(t *testing.T) {
	pane := publicationTestPane(8, 1, "blue")
	state, initial := startPublicationTestState(t, pane)
	initial.release()

	update := publicationTestUpdate(1, 0, 0, 1)
	for _, value := range []string{"r", "g"} {
		pane.terminal.replaceTextCell(pane.terminal.gridRow(0), 0, value, 1, 0)
		state.merge(update)
	}
	green := takePublicationTestBuffer(t, state)
	if green == nil {
		t.Fatal("final green state was cancelled")
	}
	if got := len(green.publication.Runs); got != 1 {
		t.Fatalf("changed runs = %d, want 1", got)
	}
	run := green.publication.Runs[0]
	if run.Column != 0 || run.Columns != 1 {
		t.Fatalf("green run = %#v, want one cell at column 0", run)
	}
	if got := publicationCellString(t, &green.publication, green.publication.Cells[run.CellStart]); got != "g" {
		t.Fatalf("published final state = %q, want green marker", got)
	}
	green.release()

	pane.terminal.replaceTextCell(pane.terminal.gridRow(0), 0, "b", 1, 0)
	state.merge(update)
	blue := takePublicationTestBuffer(t, state)
	if blue == nil {
		t.Fatal("blue baseline publication was cancelled")
	}
	blue.release()
	version := state.version
	for _, value := range []string{"r", "g", "b"} {
		pane.terminal.replaceTextCell(pane.terminal.gridRow(0), 0, value, 1, 0)
		state.merge(update)
	}
	if publication := takePublicationTestBuffer(t, state); publication != nil {
		publication.release()
		t.Fatal("blue -> red -> green -> blue emitted a cell update")
	}
	if state.version != version {
		t.Fatalf("cancelled no-op advanced version from %d to %d", version, state.version)
	}
	metrics := pane.renderMetrics.snapshot()
	if metrics.CancelledCells == 0 {
		t.Fatal("final-state cancellation was not recorded")
	}
}

func TestPanePublicationEmitsSeparatedMinimalRuns(t *testing.T) {
	pane := publicationTestPane(10, 3, "abcdefghij")
	state, initial := startPublicationTestState(t, pane)
	initial.release()

	row := pane.terminal.gridRow(0)
	pane.terminal.replaceTextCell(row, 1, "X", 1, 0)
	pane.terminal.replaceTextCell(row, 8, "Y", 1, 0)
	state.merge(publicationTestUpdate(3, 0, 0, 10))
	publication := takePublicationTestBuffer(t, state)
	if publication == nil {
		t.Fatal("sparse changes were cancelled")
	}
	defer publication.release()
	runs := publication.publication.Runs
	if len(runs) != 2 ||
		runs[0].Column != 1 || runs[0].Columns != 1 ||
		runs[1].Column != 8 || runs[1].Columns != 1 {
		t.Fatalf("sparse runs = %#v, want [1,2) and [8,9)", runs)
	}
}

func TestPanePublicationScrollTransformsAndAdvancesSnapshot(t *testing.T) {
	pane := publicationTestPane(4, 4, "aaaa", "bbbb", "cccc", "dddd")
	state, initial := startPublicationTestState(t, pane)
	initial.release()

	for row, line := range []string{"bbbb", "cccc", "dddd", "eeee"} {
		cells := pane.terminal.gridRow(row)
		for column, r := range line {
			pane.terminal.replaceTextCell(cells, column, string(r), 1, 0)
		}
	}
	spans := make([]DirtySpan, 4)
	spans[3] = DirtySpan{Start: 0, End: 4}
	scroll := ScrollRegion{Top: 0, Bottom: 4, Delta: -1}
	state.merge(Update{DirtySpans: spans, ScrollRegion: &scroll, CursorChanged: true})
	publication := takePublicationTestBuffer(t, state)
	if publication == nil {
		t.Fatal("scroll publication was cancelled")
	}
	if !publication.publication.HasScroll || publication.publication.Kind != PublicationDelta {
		t.Fatalf("scroll publication = %#v, want delta with scroll", publication.publication)
	}
	if len(publication.publication.Runs) != 1 {
		t.Fatalf("scroll repairs = %#v, want one exposed-row run", publication.publication.Runs)
	}
	run := publication.publication.Runs[0]
	if run.Row != 3 || run.Column != 0 || run.Columns != 4 {
		t.Fatalf("exposed repair = %#v, want row 3", run)
	}
	publication.release()

	for row, want := range []string{"bbbb", "cccc", "dddd", "eeee"} {
		var got string
		for column := 0; column < 4; column++ {
			got += snapshotCellString(t, &state.snapshot, row, column)
		}
		if got != want {
			t.Fatalf("retained snapshot row %d = %q, want %q", row, got, want)
		}
	}
	state.merge(Update{DirtySpans: []DirtySpan{{Start: 0, End: 4}, {}, {}, {}}})
	if next := takePublicationTestBuffer(t, state); next != nil {
		next.release()
		t.Fatal("advanced scroll snapshot did not cancel an unchanged comparison")
	}
}

func TestPanePublicationVersionsAndEpochsAdvanceOnlyOnHandoff(t *testing.T) {
	pane := publicationTestPane(4, 1, "aaaa")
	state, initial := startPublicationTestState(t, pane)
	if state.version != 1 {
		t.Fatalf("initial version = %d, want 1", state.version)
	}
	initial.release()

	heldA := <-state.free
	heldB := <-state.free
	pane.terminal.replaceTextCell(pane.terminal.gridRow(0), 0, "b", 1, 0)
	state.merge(publicationTestUpdate(1, 0, 0, 1))
	if available := state.available(); available == nil {
		t.Fatal("starved state did not remain subscribed to a returned buffer")
	}
	if state.version != 1 {
		t.Fatalf("buffer starvation advanced version to %d", state.version)
	}
	state.free <- heldA
	publication := takePublicationTestBuffer(t, state)
	if publication == nil || publication.publication.BaseVersion != 1 || publication.publication.TargetVersion != 2 {
		t.Fatalf("delta version = %#v, want 1 -> 2", publication)
	}
	publication.release()
	state.free <- heldB

	epoch := state.epoch
	state.invalidateEpoch(false)
	keyframe := takePublicationTestBuffer(t, state)
	if keyframe == nil || keyframe.publication.Kind != PublicationKeyframe ||
		keyframe.publication.Epoch == epoch || keyframe.publication.BaseVersion != 0 ||
		keyframe.publication.TargetVersion != 1 {
		t.Fatalf("epoch keyframe = %#v", keyframe)
	}
	keyframe.release()
}

func TestPanePublicationOwnsImmutableClustersStylesAndCursor(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(4, 1)}
	blue := protocol.Style{Bold: true, FG: protocol.Color{Mode: "indexed", Index: 4}, BG: protocol.Color{Mode: "default"}}
	blueID, _ := pane.terminal.styleID(blue)
	pane.terminal.replaceTextCell(pane.terminal.gridRow(0), 0, "e\u0301", 1, blueID)
	pane.terminal.CursorX = 1
	state, publication := startPublicationTestState(t, pane)
	beforeClusters := append([]byte(nil), publication.publication.Clusters...)
	beforeStyles := append([]protocol.Style(nil), publication.publication.Styles...)
	beforeCursor := publication.publication.Cursor

	red := protocol.Style{Italic: true, FG: protocol.Color{Mode: "indexed", Index: 1}, BG: protocol.Color{Mode: "default"}}
	redID, _ := pane.terminal.styleID(red)
	pane.terminal.replaceTextCell(pane.terminal.gridRow(0), 0, "o\u0302", 1, redID)
	pane.terminal.CursorX = 3
	state.merge(publicationTestUpdate(1, 0, 0, 1))

	if !bytes.Equal(publication.publication.Clusters, beforeClusters) {
		t.Fatalf("handed-off cluster bytes changed from %q to %q", beforeClusters, publication.publication.Clusters)
	}
	if len(publication.publication.Styles) != len(beforeStyles) {
		t.Fatalf("handed-off styles length changed from %d to %d", len(beforeStyles), len(publication.publication.Styles))
	}
	for index := range beforeStyles {
		if publication.publication.Styles[index] != beforeStyles[index] {
			t.Fatalf("handed-off style %d changed", index)
		}
	}
	if publication.publication.Cursor != beforeCursor {
		t.Fatalf("handed-off cursor changed from %#v to %#v", beforeCursor, publication.publication.Cursor)
	}
	publication.release()
}

func TestPanePublicationKeepsWideCellsAndGraphemesAtomic(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 2)}
	blankRow := decodedTestRow{Cells: make([]decodedTestCell, 8)}
	setTestRows(pane.terminal, nil, []decodedTestRow{
		{Cells: []decodedTestCell{
			{Cluster: "界", Width: 2}, {Width: 0}, {Width: 1},
			{Cluster: "e\u0301", Width: 1}, {Width: 1}, {Width: 1}, {Width: 1}, {Width: 1},
		}},
		blankRow,
	})
	state, initial := startPublicationTestState(t, pane)
	initial.release()

	setTestRows(pane.terminal, nil, []decodedTestRow{
		{Cells: []decodedTestCell{
			{Cluster: "語", Width: 2}, {Width: 0}, {Width: 1},
			{Cluster: "o\u0302", Width: 1}, {Width: 1}, {Width: 1}, {Width: 1}, {Width: 1},
		}},
		blankRow,
	})
	spans := make([]DirtySpan, 2)
	spans[0] = DirtySpan{Start: 1, End: 4}
	state.merge(Update{DirtySpans: spans})
	publication := takePublicationTestBuffer(t, state)
	if publication == nil {
		t.Fatal("wide/grapheme changes were cancelled")
	}
	defer publication.release()
	runs := publication.publication.Runs
	if len(runs) != 2 || runs[0].Column != 0 || runs[0].Columns != 2 ||
		runs[1].Column != 3 || runs[1].Columns != 1 {
		t.Fatalf("wide/grapheme runs = %#v", runs)
	}
	if publication.publication.Cells[runs[0].CellStart].width != 2 ||
		publication.publication.Cells[runs[0].CellStart+1].kind != semanticContinuation {
		t.Fatal("wide head and continuation were split")
	}
	clusterCell := publication.publication.Cells[runs[1].CellStart]
	if got := publicationCellString(t, &publication.publication, clusterCell); got != "o\u0302" {
		t.Fatalf("grapheme bytes = %q, want o + combining circumflex", got)
	}

	current := currentSemanticCell{
		kind: semanticCluster, width: 1, text: "o\u0302",
		style: canonicalStyle(protocol.CanonicalDefaultStyle()),
	}
	if !currentEqualsSemantic(current, clusterCell, publication.publication.Styles, publication.publication.Clusters) {
		t.Fatal("semantic cluster equality depended on a terminal cluster handle")
	}
}

func TestConfirmerReturnsPublicationBeforeBlockedWriteAndPresentsOnce(t *testing.T) {
	stream := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	lease := testOutputLease(0, stream)
	returned := make(chan *viewPublicationBuffer, 1)
	buffer := &viewPublicationBuffer{returnTo: returned}
	buffer.publication = viewPublication{
		Epoch: 1, TargetVersion: 1, Kind: PublicationKeyframe, Barrier: true,
		Cols: 1, Rows: 1,
		Styles: []protocol.Style{protocol.CanonicalDefaultStyle()},
		Cells:  []semanticCell{{kind: semanticScalar, width: 1, payload: 'x'}},
		Runs:   []publishedRun{{Columns: 1}},
	}
	lease.submissions() <- confirmerMessage{publication: buffer}
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("confirmer did not reach blocked writer")
	}
	select {
	case got := <-returned:
		if got != buffer {
			t.Fatalf("returned buffer = %p, want %p", got, buffer)
		}
	case <-time.After(time.Second):
		t.Fatal("publication buffer was retained behind blocked network write")
	}
	select {
	case duplicate := <-returned:
		t.Fatalf("publication buffer returned twice: %p", duplicate)
	default:
	}
	if cap(lease.ready) != 1 {
		t.Fatalf("confirmer FIFO capacity = %d, want 1", cap(lease.ready))
	}
	close(stream.release)
	if err := lease.sync(); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmerFailureReturnsQueuedPublicationBuffer(t *testing.T) {
	lease := testOutputLease(0, errorWriter{err: io.ErrClosedPipe})
	returned := make(chan *viewPublicationBuffer, 2)
	publication := func(version RenderVersion, barrier bool) *viewPublicationBuffer {
		buffer := &viewPublicationBuffer{returnTo: returned}
		buffer.publication = viewPublication{
			Epoch: 1, BaseVersion: version - 1, TargetVersion: version,
			Kind: PublicationDelta, Barrier: barrier, Cols: 1, Rows: 1,
			Styles: []protocol.Style{protocol.CanonicalDefaultStyle()},
			Cells:  []semanticCell{{kind: semanticScalar, width: 1, payload: 'x'}},
			Runs:   []publishedRun{{Columns: 1}},
		}
		if barrier {
			buffer.publication.Kind = PublicationKeyframe
			buffer.publication.BaseVersion = 0
		}
		return buffer
	}
	first := publication(1, true)
	second := publication(2, false)
	lease.submissions() <- confirmerMessage{publication: first}
	lease.submissions() <- confirmerMessage{publication: second}
	seen := make(map[*viewPublicationBuffer]bool)
	for range 2 {
		select {
		case buffer := <-returned:
			seen[buffer] = true
		case <-time.After(time.Second):
			t.Fatal("confirmer failure retained a publication buffer")
		}
	}
	if !seen[first] || !seen[second] || len(seen) != 2 {
		t.Fatalf("returned buffers = %#v, want each buffer exactly once", seen)
	}
	if err := lease.sync(); err == nil {
		t.Fatal("failed confirmer accepted a later synchronization marker")
	}
}

func TestConfirmerCompilationAndTransferSteadyStateDoNotAllocate(t *testing.T) {
	confirmer := newPaneConfirmer()
	publication := &viewPublication{
		Epoch: 1, TargetVersion: 1, Kind: PublicationKeyframe, Barrier: true,
		Cols: 4, Rows: 1,
		Styles: []protocol.Style{protocol.CanonicalDefaultStyle()},
		Cells: []semanticCell{
			{kind: semanticScalar, width: 1, payload: 'a'},
			{kind: semanticScalar, width: 1, payload: 'b'},
			{kind: semanticScalar, width: 1, payload: 'c'},
			{kind: semanticScalar, width: 1, payload: 'd'},
		},
		Runs: []publishedRun{{Columns: 4}},
	}
	if _, err := confirmer.compile(publication); err != nil {
		t.Fatal(err)
	}
	publication.Barrier = false
	allocations := testing.AllocsPerRun(1000, func() {
		publication.TargetVersion++
		if _, err := confirmer.compile(publication); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("retained confirmer compilation allocated %.2f objects, want 0", allocations)
	}

	channel := make(chan *viewPublicationBuffer, 1)
	buffer := &viewPublicationBuffer{}
	allocations = testing.AllocsPerRun(1000, func() {
		channel <- buffer
		_ = <-channel
	})
	if allocations != 0 {
		t.Fatalf("publication-buffer transfer allocated %.2f objects, want 0", allocations)
	}
}

func TestPaneDeltaKeyframeAndCancellationSteadyStateDoNotAllocate(t *testing.T) {
	pane := publicationTestPane(80, 24)
	state, initial := startPublicationTestState(t, pane)
	initial.release()
	dirty := make([]DirtySpan, pane.terminal.Rows)
	dirty[0] = DirtySpan{Start: 0, End: 1}
	update := Update{DirtySpans: dirty}
	value := rune('a')
	runDelta := func() {
		if value == 'a' {
			value = 'b'
		} else {
			value = 'a'
		}
		word, ok := makeScalarCellWord(value, 1, 0)
		if !ok {
			panic("scalar cell construction failed")
		}
		pane.terminal.replaceCell(pane.terminal.gridRow(0), 0, word)
		state.merge(update)
		buffer := <-state.free
		if err := state.prepare(buffer); err != nil {
			panic(err)
		}
		if state.pending == nil {
			panic("delta publication was cancelled")
		}
		publication := state.pending
		state.handedOff()
		publication.release()
	}
	for range 4 {
		runDelta()
	}
	if allocations := testing.AllocsPerRun(1000, runDelta); allocations != 0 {
		t.Fatalf("retained DELTA publication allocated %.2f objects, want 0", allocations)
	}

	runKeyframe := func() {
		state.keyframe = true
		for row := range state.dirty {
			state.dirty[row] = DirtySpan{Start: 0, End: pane.terminal.Cols}
		}
		state.dirtyRows = len(state.dirty)
		buffer := <-state.free
		if err := state.prepare(buffer); err != nil {
			panic(err)
		}
		if state.pending == nil || state.pending.publication.Kind != PublicationKeyframe {
			panic("keyframe publication was not prepared")
		}
		publication := state.pending
		state.handedOff()
		publication.release()
	}
	for range 2 {
		runKeyframe()
	}
	if allocations := testing.AllocsPerRun(100, runKeyframe); allocations != 0 {
		t.Fatalf("retained KEYFRAME publication allocated %.2f objects, want 0", allocations)
	}

	runCancellation := func() {
		state.merge(update)
		buffer := <-state.free
		if err := state.prepare(buffer); err != nil {
			panic(err)
		}
		if state.pending != nil {
			panic("unchanged final state produced a publication")
		}
	}
	if allocations := testing.AllocsPerRun(1000, runCancellation); allocations != 0 {
		t.Fatalf("final-state comparison allocated %.2f objects, want 0", allocations)
	}
}

type chunkCountingDiscard struct {
	writes int
	max    int
}

func (w *chunkCountingDiscard) Write(data []byte) (int, error) {
	w.writes++
	w.max = max(w.max, len(data))
	return len(data), nil
}

func TestMultiChunkReliableWriteSteadyStateDoesNotAllocate(t *testing.T) {
	writer := &chunkCountingDiscard{}
	lease := testOutputLease(0, writer)
	frame := make([]byte, 4*renderStreamChunkSize+17)
	if err := lease.writeFrame(frame, nil); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 5 || writer.max > renderStreamChunkSize {
		t.Fatalf("physical writes=%d max=%d, want 5 writes bounded by %d", writer.writes, writer.max, renderStreamChunkSize)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		writer.writes, writer.max = 0, 0
		if err := lease.writeFrame(frame, nil); err != nil {
			panic(err)
		}
	}); allocations != 0 {
		t.Fatalf("multi-chunk reliable writing allocated %.2f objects, want 0", allocations)
	}
}

func TestPaneRenderMetricsKeepOnePresentPerPublication(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 2)}
	shutdown := startTestPaneLoop(pane)
	defer close(shutdown)
	if err := pane.installOutputLease(testOutputLease(0, io.Discard), 1, 8, 2); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	sendTestPTYOutput(t, pane, "x")
	syncPaneRenderer(t, pane)
	metrics := pane.renderMetricsSnapshot()
	if metrics.Publications == 0 || metrics.Presents != metrics.Publications {
		t.Fatalf("publications=%d presents=%d, want exactly one PRESENT/publication", metrics.Publications, metrics.Presents)
	}
	if metrics.ChangedCells > metrics.CandidateCells {
		t.Fatalf("changed cells=%d candidates=%d", metrics.ChangedCells, metrics.CandidateCells)
	}
	if metrics.PTYDrainsCompleted == 0 || metrics.PTYBytes == 0 {
		t.Fatalf("PTY diagnostics were not recorded: %#v", metrics)
	}
}

func TestConfirmerRejectsBrokenVersionChain(t *testing.T) {
	confirmer := newPaneConfirmer()
	keyframe := &viewPublication{
		Epoch: 4, TargetVersion: 1, Kind: PublicationKeyframe, Barrier: true,
		Cols: 1, Rows: 1,
		Styles: []protocol.Style{protocol.CanonicalDefaultStyle()},
		Cells:  []semanticCell{{kind: semanticBlank, width: 1}},
		Runs:   []publishedRun{{Columns: 1}},
	}
	if _, err := confirmer.compile(keyframe); err != nil {
		t.Fatal(err)
	}
	anchor := *keyframe
	anchor.Barrier = false
	anchor.TargetVersion = 2
	if _, err := confirmer.compile(&anchor); err != nil {
		t.Fatalf("same-epoch keyframe anchor: %v", err)
	}
	replacementBinding := *keyframe
	if _, err := confirmer.compile(&replacementBinding); err != nil {
		t.Fatalf("barrier did not reset a colliding pane-local epoch: %v", err)
	}
	broken := *keyframe
	broken.Kind = PublicationDelta
	broken.Barrier = false
	broken.BaseVersion = 0
	broken.TargetVersion = 2
	if _, err := confirmer.compile(&broken); err == nil {
		t.Fatal("confirmer accepted a delta with a stale base version")
	}
}

func TestPaneRenderDiagnosticWorkload(t *testing.T) {
	if os.Getenv("MEJA_RUN_RENDER_DIAGNOSTIC") != "1" {
		t.Skip("set MEJA_RUN_RENDER_DIAGNOSTIC=1 to run the paced render diagnostic")
	}
	pane := &Pane{ID: 1, terminal: newTerminal(80, 24)}
	pane.initializeRuntime()
	go pane.run()
	if err := pane.installOutputLease(testOutputLease(0, io.Discard), 1, 80, 24); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	before := pane.renderMetricsSnapshot()
	first := bytes.Repeat([]byte{'A'}, ptyReadBufferSize)
	second := bytes.Repeat([]byte{'B'}, ptyReadBufferSize)
	chunks := make([][]byte, 128)
	for index := range chunks {
		if index%2 == 0 {
			chunks[index] = first
		} else {
			chunks[index] = second
		}
	}
	reader := &scriptedPTYReader{chunks: chunks}
	relayDone := make(chan struct{})
	go func() {
		relayPTYOutputFrom(pane, reader)
		close(relayDone)
	}()
	started := time.Now()
	for time.Since(started) < time.Second {
		time.Sleep(time.Millisecond)
	}
	syncPaneRenderer(t, pane)
	elapsed := time.Since(started).Seconds()
	after := pane.renderMetricsSnapshot()
	publications := after.Publications - before.Publications
	presents := after.Presents - before.Presents
	drains := after.PTYDrainsCompleted - before.PTYDrainsCompleted
	ptyBytes := after.PTYBytes - before.PTYBytes
	drainReads := after.PTYDrainReads - before.PTYDrainReads
	drainsAtEmpty := after.PTYDrainStoppedEmpty - before.PTYDrainStoppedEmpty
	wireBytes := after.UncompressedBytes - before.UncompressedBytes
	candidates := after.CandidateCells - before.CandidateCells
	changed := after.ChangedCells - before.ChangedCells
	t.Logf("elapsed=%.3fs pty_drains/s=%.2f reads/drain=%.2f bytes/drain=%.1f eagain_pct=%.1f pty_bytes/s=%.0f publications/s=%.2f presents/s=%.2f presents/publication=%.2f wire_bytes/s=%.0f avg_bytes/publication=%.1f candidates=%d changed=%d cancelled=%d",
		elapsed,
		float64(drains)/elapsed,
		float64(drainReads)/float64(drains),
		float64(ptyBytes)/float64(drains),
		float64(drainsAtEmpty)*100/float64(drains),
		float64(ptyBytes)/elapsed,
		float64(publications)/elapsed,
		float64(presents)/elapsed,
		float64(presents)/float64(publications),
		float64(wireBytes)/elapsed,
		float64(wireBytes)/float64(publications),
		candidates,
		changed,
		after.CancelledCells-before.CancelledCells,
	)
	pane.stop()
	<-pane.mainDone
	<-relayDone
}

func BenchmarkPaneDeltaPublication(b *testing.B) {
	pane := publicationTestPane(80, 24)
	state := newPanePublicationState(pane)
	state.lease = &OutputLease{}
	state.invalidateEpoch(true)
	initial := takePublicationBenchmarkBuffer(b, state)
	initial.release()
	dirty := make([]DirtySpan, pane.terminal.Rows)
	dirty[0] = DirtySpan{Start: 0, End: 1}
	update := Update{DirtySpans: dirty}
	value := rune('a')
	run := func() {
		if value == 'a' {
			value = 'b'
		} else {
			value = 'a'
		}
		word, _ := makeScalarCellWord(value, 1, 0)
		pane.terminal.replaceCell(pane.terminal.gridRow(0), 0, word)
		state.merge(update)
		publication := takePublicationBenchmarkBuffer(b, state)
		publication.release()
	}
	for range 2 {
		run()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		run()
	}
}

func BenchmarkPaneKeyframePublication(b *testing.B) {
	pane := publicationTestPane(80, 24)
	state := newPanePublicationState(pane)
	state.lease = &OutputLease{}
	state.invalidateEpoch(true)
	initial := takePublicationBenchmarkBuffer(b, state)
	initial.release()
	run := func() {
		state.keyframe = true
		for row := range state.dirty {
			state.dirty[row] = DirtySpan{Start: 0, End: pane.terminal.Cols}
		}
		state.dirtyRows = len(state.dirty)
		publication := takePublicationBenchmarkBuffer(b, state)
		publication.release()
	}
	for range 2 {
		run()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		run()
	}
}

func BenchmarkPaneConfirmerCompilation(b *testing.B) {
	confirmer := newPaneConfirmer()
	publication := &viewPublication{
		Epoch: 1, TargetVersion: 1, Kind: PublicationKeyframe, Barrier: true,
		Cols: 80, Rows: 1,
		Styles: []protocol.Style{protocol.CanonicalDefaultStyle()},
		Cells:  make([]semanticCell, 80),
		Runs:   []publishedRun{{Columns: 80}},
	}
	for index := range publication.Cells {
		publication.Cells[index] = semanticCell{kind: semanticScalar, width: 1, payload: uint32('a' + index%26)}
	}
	if _, err := confirmer.compile(publication); err != nil {
		b.Fatal(err)
	}
	publication.Barrier = false
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		publication.TargetVersion++
		if _, err := confirmer.compile(publication); err != nil {
			b.Fatal(err)
		}
	}
}

func takePublicationBenchmarkBuffer(b *testing.B, state *panePublicationState) *viewPublicationBuffer {
	b.Helper()
	buffer := <-state.free
	if err := state.prepare(buffer); err != nil {
		b.Fatal(err)
	}
	if state.pending == nil {
		b.Fatal("benchmark publication was cancelled")
	}
	publication := state.pending
	state.handedOff()
	return publication
}
