package server

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestPaneHistoryModePublishesImmutableMetadataConcurrently(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	shutdown := startTestPaneLoop(pane)
	defer func() {
		close(shutdown)
		<-pane.mainDone
		pane.stop()
	}()

	stopReaders := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					_ = pane.InputMode().historyMode
				}
			}
		}()
	}
	for range 100 {
		changed, err := pane.enterHistoryMode()
		if err != nil || !changed || !pane.InputMode().historyMode {
			t.Fatalf("enter history changed=%t metadata=%t err=%v", changed, pane.InputMode().historyMode, err)
		}
		changed, err = pane.exitHistoryMode()
		if err != nil || !changed || pane.InputMode().historyMode {
			t.Fatalf("exit history changed=%t metadata=%t err=%v", changed, pane.InputMode().historyMode, err)
		}
	}
	close(stopReaders)
	readers.Wait()
}

func TestHistorySnapshotIsIndependentAndMovesAtViewportBoundary(t *testing.T) {
	pane := &Pane{ID: 0, terminal: newTerminal(4, 3)}
	history := []decodedTestRow{
		historyTestRow("old1"),
		historyTestRow("old2"),
	}
	visible := []decodedTestRow{
		historyTestRow("live"),
		historyTestRow("mid "),
		historyTestRow("end "),
	}
	setTestRows(pane.terminal, history, visible)
	pane.terminal.CursorY = 2

	snapshot := captureTerminalHistorySnapshot(pane.terminal)
	pane.terminal.replaceTextCell(pane.terminal.grid.logicalRow(0, 4), 0, "X", 1, 0)
	if got := cellTextFromStore(snapshot.row(0)[0], snapshot.clusters); got != "o" {
		t.Fatalf("snapshot aliased canonical history: %q", got)
	}

	if err := pane.installHistoryView(snapshot); err != nil {
		t.Fatalf("installHistoryView() error = %v", err)
	}
	for i := 0; i < 2; i++ {
		move, ok := pane.moveHistory(-1)
		if !ok || move.Delta != 0 {
			t.Fatalf("cursor-only move %d = %#v ok=%v", i, move, ok)
		}
	}
	move, ok := pane.moveHistory(-1)
	if !ok || move.Delta != 1 || historyCounter(pane.historyView) != "[1/2]" {
		t.Fatalf("boundary move = %#v ok=%v", move, ok)
	}
}

func TestHistoryInputBuildsIncrementalScrollDamageInBothDirections(t *testing.T) {
	tests := []struct {
		name        string
		viewTop     int
		cursorRow   int
		input       string
		wantDelta   int
		wantDirty   []int
		wantCounter string
	}{
		{
			name: "content upward", viewTop: 0, cursorRow: 2, input: "jj",
			wantDelta: -2, wantDirty: []int{1, 2}, wantCounter: "[2/4]",
		},
		{
			name: "content downward", viewTop: 2, cursorRow: 2, input: "kk",
			wantDelta: 2, wantDirty: []int{0, 1, 2}, wantCounter: "[4/4]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pane := newHistoryRenderTestPane(t)
			pane.historyView.ViewTop = tt.viewTop
			pane.historyView.CursorRow = tt.cursorRow

			result := pane.handleHistoryInputNow([]byte(tt.input))
			if !result.Changed || result.Render.FullRedraw || !result.Render.CursorChanged {
				t.Fatalf("history result = %#v", result)
			}
			wantRegion := ScrollRegion{Top: 0, Bottom: 3, Delta: tt.wantDelta}
			if result.Render.ScrollRegion == nil || *result.Render.ScrollRegion != wantRegion {
				t.Fatalf("scroll region = %#v, want %#v", result.Render.ScrollRegion, wantRegion)
			}
			for row, span := range result.Render.DirtySpans {
				want := false
				for _, dirtyRow := range tt.wantDirty {
					want = want || row == dirtyRow
				}
				if want != (span.End > span.Start) {
					t.Fatalf("row %d damage = %#v, want dirty=%t", row, span, want)
				}
			}
			if got := historyCounter(pane.historyView); got != tt.wantCounter {
				t.Fatalf("counter = %q, want %q", got, tt.wantCounter)
			}
			if got := result.Render.DirtySpans[0]; tt.wantDelta < 0 && got != (DirtySpan{}) {
				t.Fatalf("upward scroll retained discarded counter damage: %#v", got)
			}
			if got := result.Render.DirtySpans[2]; tt.wantDelta > 0 &&
				got != (DirtySpan{Start: 3, End: 8}) {
				t.Fatalf("downward scroll counter cleanup = %#v, want [3,8)", got)
			}
		})
	}
}

func TestHistoryRenderStateCoalescesSameDirectionAndFallsBackSafely(t *testing.T) {
	pane := newHistoryRenderTestPane(t)
	pane.historyView.ViewTop = 0
	pane.historyView.CursorRow = 2
	first := pane.handleHistoryInputNow([]byte("j"))
	second := pane.handleHistoryInputNow([]byte("j"))

	state := newPanePublicationState(pane)
	state.lease = &OutputLease{}
	state.ensureGeometry()
	state.mergeViewMutation(first.Render)
	state.mergeViewMutation(second.Render)
	if state.scroll == nil || *state.scroll != (ScrollRegion{Top: 0, Bottom: 3, Delta: -2}) {
		t.Fatalf("coalesced history scroll = %#v", state.scroll)
	}
	if state.dirty[0] != (DirtySpan{}) ||
		state.dirty[1] != (DirtySpan{Start: 0, End: 8}) ||
		state.dirty[2] != (DirtySpan{Start: 0, End: 8}) {
		t.Fatalf("coalesced exposed-row damage = %#v", state.dirty)
	}

	opposing := ViewMutation{}
	opposing.reset(3)
	opposing.ScrollRegion = &ScrollRegion{Top: 0, Bottom: 3, Delta: 1}
	state.mergeViewMutation(opposing)
	if state.scroll != nil || state.dirtyRows != 3 {
		t.Fatalf("opposing movement did not force full redraw: %#v", state)
	}
}

func TestHistoryCursorAndSimpleSelectionDamageStayIncremental(t *testing.T) {
	pane := newHistoryRenderTestPane(t)
	pane.historyView.ViewTop = 1
	pane.historyView.CursorRow = 2
	cursorOnly := pane.handleHistoryInputNow([]byte("k"))
	if !cursorOnly.Changed || !cursorOnly.Render.CursorChanged ||
		cursorOnly.Render.FullRedraw || cursorOnly.Render.ScrollRegion != nil ||
		cursorOnly.Render.HasDamage() {
		t.Fatalf("cursor-only movement render = %#v", cursorOnly.Render)
	}
	if got := pane.historyView.cursorPosition(); got != (paneHistoryPosition{Row: 1, Col: 0}) {
		t.Fatalf("cursor-only movement position = %#v", got)
	}

	pane = newHistoryRenderTestPane(t)
	pane.historyView.ViewTop = 1
	pane.historyView.CursorRow = 2
	position := pane.historyView.cursorPosition()
	pane.historyView.Selection = &paneHistorySelection{Anchor: position, Head: position}
	selection := pane.handleHistoryInputNow([]byte("l"))
	if !selection.Changed || !selection.Render.CursorChanged ||
		selection.Render.FullRedraw || selection.Render.ScrollRegion != nil {
		t.Fatalf("simple selection movement render = %#v", selection.Render)
	}
	if got, want := selection.Render.DirtySpans[1], (DirtySpan{Start: 0, End: 2}); got != want {
		t.Fatalf("simple selection damage = %#v, want %#v", got, want)
	}
}

func TestHistoryInputFallsBackForJumpsAndSelectionMovement(t *testing.T) {
	pane := newHistoryRenderTestPane(t)
	jump := pane.handleHistoryInputNow([]byte("g"))
	if !jump.Changed || !jump.Render.FullRedraw || jump.Render.ScrollRegion != nil {
		t.Fatalf("jump %q render = %#v", "g", jump.Render)
	}
	jump = pane.handleHistoryInputNow([]byte("G"))
	if !jump.Changed || !jump.Render.FullRedraw || jump.Render.ScrollRegion != nil {
		t.Fatalf("jump %q render = %#v", "G", jump.Render)
	}

	pane = newHistoryRenderTestPane(t)
	pane.historyView.ViewTop = 0
	pane.historyView.CursorRow = 2
	position := pane.historyView.cursorPosition()
	pane.historyView.Selection = &paneHistorySelection{Anchor: position, Head: position}
	selectionMove := pane.handleHistoryInputNow([]byte("j"))
	if !selectionMove.Changed || !selectionMove.Render.FullRedraw || selectionMove.Render.ScrollRegion != nil {
		t.Fatalf("selection movement render = %#v", selectionMove.Render)
	}

	pane = newHistoryRenderTestPane(t)
	pane.historyView.ViewTop = 0
	pane.historyView.CursorRow = 2
	fullViewport := pane.handleHistoryInputNow([]byte("jjj"))
	if !fullViewport.Changed || !fullViewport.Render.FullRedraw || fullViewport.Render.ScrollRegion != nil {
		t.Fatalf("full-viewport movement render = %#v", fullViewport.Render)
	}
}

func TestHistoryJumpsAtTheirDestinationAreNoOps(t *testing.T) {
	pane := newHistoryRenderTestPane(t)
	if result := pane.handleHistoryInputNow([]byte("G")); !result.Changed || !result.Render.FullRedraw {
		t.Fatalf("initial bottom jump = %#v", result)
	}
	if result := pane.handleHistoryInputNow([]byte("G")); result.Changed || result.Render.HasRenderChange() {
		t.Fatalf("repeated bottom jump = %#v", result)
	}
	if result := pane.handleHistoryInputNow([]byte("g")); !result.Changed || !result.Render.FullRedraw {
		t.Fatalf("initial top jump = %#v", result)
	}
	if result := pane.handleHistoryInputNow([]byte("g")); result.Changed || result.Render.HasRenderChange() {
		t.Fatalf("top jump at top = %#v", result)
	}
	if result := pane.handleHistoryInputNow([]byte("G")); !result.Changed || !result.Render.FullRedraw {
		t.Fatalf("bottom jump after top = %#v", result)
	}
	if result := pane.handleHistoryInputNow([]byte("G")); result.Changed || result.Render.HasRenderChange() {
		t.Fatalf("repeated bottom jump = %#v", result)
	}
}

func TestHistoryStructuralChangesReturnExplicitFullRedraws(t *testing.T) {
	pane := &Pane{ID: 0, terminal: newTerminal(8, 3)}
	enter := pane.handleHistoryRequest(&paneHistoryRequest{Action: paneHistoryEnter})
	if !enter.Changed || enter.Err != nil || !enter.Render.FullRedraw {
		t.Fatalf("history enter = %#v", enter)
	}

	begin := pane.beginHistorySelectionAtCursorNow(false)
	if !begin.Changed || !begin.Render.FullRedraw {
		t.Fatalf("selection begin = %#v", begin)
	}
	update := pane.updateHistorySelectionNow(1, 1)
	if !update.Changed || !update.Render.FullRedraw {
		t.Fatalf("selection update = %#v", update)
	}
	clear := pane.clearHistorySelectionNow()
	if !clear.Changed || !clear.Render.FullRedraw {
		t.Fatalf("selection clear = %#v", clear)
	}

	exit := pane.handleHistoryRequest(&paneHistoryRequest{Action: paneHistoryExit})
	if !exit.Changed || !exit.Render.FullRedraw {
		t.Fatalf("history exit = %#v", exit)
	}
}

func TestHistorySnapshotNeverSplitsClusterAcrossRows(t *testing.T) {
	term := newTerminal(5, 1)
	rows := []decodedTestRow{{Cells: []decodedTestCell{
		{Cluster: "a", Width: 1},
		{Cluster: "b", Width: 1},
		{Cluster: "c", Width: 1},
		{Cluster: "👩‍💻", Width: 2},
		{Width: 0},
	}}}
	setTestRows(term, nil, rows)
	snapshot := captureTerminalHistorySnapshot(term)
	defer snapshot.release()
	if anchor, continuation := snapshot.row(0)[3], snapshot.row(0)[4]; cellTextFromStore(anchor, snapshot.clusters) != "👩‍💻" || anchor.width() != 2 || continuation.width() != 0 {
		t.Fatalf("snapshot cluster = %#v %#v", anchor, continuation)
	}
}

func TestHistoryCounterStyleCannotCollideWithLiveTerminalStyles(t *testing.T) {
	term := newTerminal(5, 1)
	term.Apply([]byte("\x1b[31mred"))
	snapshot := captureTerminalHistorySnapshot(term)
	defer snapshot.release()

	term.Apply([]byte("\x1b[7mreverse"))
	reverseID := term.currentStyleID()
	if reverseID < firstLiveTerminalStyleID || reverseID == snapshot.CounterStyle || reverseID == historySelectionStyleID {
		t.Fatalf("live style %d collided with reserved history styles", reverseID)
	}
	counter, ok := snapshot.LookupStyle(snapshot.CounterStyle)
	if !ok || counter != historyCounterStyle {
		t.Fatalf("history counter style = %#v, ok=%t", counter, ok)
	}
}

func TestHistorySelectionExtractsUTF8AndJoinsSoftWrappedRows(t *testing.T) {
	term := newTerminal(5, 3)
	setTestRows(term, nil, []decodedTestRow{
		{Cells: []decodedTestCell{{Cluster: "h", Width: 1}, {Cluster: "e", Width: 1}, {Cluster: "l", Width: 1}, {Cluster: "l", Width: 1}, {Cluster: "o", Width: 1}}, WrapsNext: true},
		{Cells: []decodedTestCell{{Cluster: "w", Width: 1}, {Cluster: "o", Width: 1}, {Cluster: "r", Width: 1}, {Cluster: "l", Width: 1}, {Cluster: "d", Width: 1}}},
		{Cells: []decodedTestCell{{Cluster: "👩‍💻", Width: 2}, {Width: 0}, {Cluster: "!", Width: 1}, {Width: 1}, {Width: 1}}},
	})
	snapshot := captureTerminalHistorySnapshot(term)
	defer snapshot.release()

	data, err := extractHistorySelection(snapshot, paneHistorySelection{
		Anchor: paneHistoryPosition{Row: 0, Col: 0},
		Head:   paneHistoryPosition{Row: 2, Col: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "helloworld\n👩‍💻!"; got != want {
		t.Fatalf("selection = %q, want %q", got, want)
	}
}

func TestHistorySelectionPositionSnapsWideContinuationToAnchor(t *testing.T) {
	term := newTerminal(4, 1)
	setTestRows(term, nil, []decodedTestRow{{Cells: []decodedTestCell{
		{Cluster: "a", Width: 1}, {Cluster: "界", Width: 2}, {Width: 0}, {Cluster: "z", Width: 1},
	}}})
	view := &paneHistoryView{Snapshot: captureTerminalHistorySnapshot(term)}
	defer view.Snapshot.release()
	if got := view.pointerPosition(0, 2); got != (paneHistoryPosition{Row: 0, Col: 1}) {
		t.Fatalf("continuation position = %#v", got)
	}
}

func TestPanesRetainIndependentHistoryViews(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(8, 4)
	pane0 := &Pane{ID: testAddPaneID(s), terminal: newTerminal(8, 4)}
	createTestWindow(s, pane0)
	pane1 := &Pane{ID: testAddPaneID(s), terminal: newTerminal(8, 4)}
	if _, _, err := splitTestFocusedPane(s, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if err := pane1.installHistoryView(captureTerminalHistorySnapshot(pane1.terminal)); err != nil {
		t.Fatalf("install pane1 history = %v", err)
	}
	if _, _, err := focusTestSessionPane(s, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	}
	if err := pane0.installHistoryView(captureTerminalHistorySnapshot(pane0.terminal)); err != nil {
		t.Fatalf("install pane0 history = %v", err)
	}
	if !pane0.isHistoryMode() || !pane1.isHistoryMode() {
		t.Fatal("multiple pane history views were not retained")
	}
}

func TestPaneOutputStreamRendersItsOwnedFrozenHistoryMode(t *testing.T) {
	pane := &Pane{ID: 0, terminal: newTerminal(4, 2)}
	setTestRows(pane.terminal, nil, []decodedTestRow{historyTestRow("live"), historyTestRow("end ")})
	shutdown := startTestPaneLoop(pane)
	defer func() {
		close(shutdown)
		<-pane.mainDone
		pane.stop()
	}()
	sendPTYOutput := func(data string) {
		sendTestPTYOutput(t, pane, data)
	}

	var wire bytes.Buffer
	if err := pane.installOutputLease(testOutputLease(0, &wire), 7, uint16(pane.terminal.Cols), uint16(pane.terminal.Rows)); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	liveBytes := wire.Len()

	if _, err := pane.enterHistoryMode(); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if wire.Len() <= liveBytes {
		t.Fatal("entering history did not repaint the pane's existing output stream")
	}
	historyCommands := decodePendingCommands(t, wire.Bytes()[liveBytes:])
	if containsOpcode(commandOpcodes(historyCommands), protocol.DisplayOpcodeScrollRegion) {
		t.Fatalf("entering history used incremental scroll: %#v", historyCommands)
	}
	if !displayCommandsContainText(historyCommands, "end ") {
		t.Fatalf("history mode did not render the pane-owned frozen view: %#v", historyCommands)
	}

	// A new live style allocated while history is visible must not reuse the
	// history counter's connection-local style ID.
	sendPTYOutput("\x1b[7mX\x1b[0m")
	syncPaneRenderer(t, pane)
	historyBytes := wire.Len()
	sendPTYOutput("Y")
	syncPaneRenderer(t, pane)
	if wire.Len() != historyBytes {
		t.Fatal("live terminal damage was emitted while pane was in history mode")
	}

	exited, err := pane.exitHistoryMode()
	if err != nil {
		t.Fatal(err)
	}
	if !exited {
		t.Fatal("pane did not exit history mode")
	}
	syncPaneRenderer(t, pane)
	if wire.Len() <= historyBytes {
		t.Fatal("exiting history did not repaint the pane's existing output stream")
	}
	exitCommands := decodePendingCommands(t, wire.Bytes()[historyBytes:])
	if containsOpcode(commandOpcodes(exitCommands), protocol.DisplayOpcodeScrollRegion) {
		t.Fatalf("exiting history used incremental scroll: %#v", exitCommands)
	}
	if !displayCommandsContainText(exitCommands, "X") || !displayCommandsContainText(exitCommands, "Yve") {
		t.Fatal("exiting history did not render the pane's current terminal on the existing stream")
	}
	installed := make(map[uint32]protocol.Style)
	for _, command := range decodePendingCommands(t, wire.Bytes()) {
		if command.Opcode != protocol.DisplayOpcodeStyleInstall {
			continue
		}
		if previous, ok := installed[command.StyleID]; ok && previous != command.Style {
			t.Fatalf("style %d was redefined across history/live output: %#v then %#v", command.StyleID, previous, command.Style)
		}
		installed[command.StyleID] = command.Style
	}
}

func TestOneLineHistoryMovementEmitsScrollAndOnlyExposedContent(t *testing.T) {
	pane := &Pane{ID: 0, terminal: newTerminal(8, 3)}
	setTestRows(pane.terminal,
		[]decodedTestRow{
			historyTestRow("h000"),
			historyTestRow("h111"),
			historyTestRow("h222"),
			historyTestRow("h333"),
		},
		[]decodedTestRow{
			historyTestRow("v111"),
			historyTestRow("v222"),
			historyTestRow("v333"),
		},
	)
	pane.terminal.CursorX = 0
	pane.terminal.CursorY = 0
	shutdown := startTestPaneLoop(pane)
	defer func() {
		close(shutdown)
		<-pane.mainDone
		pane.stop()
	}()

	var wire bytes.Buffer
	if err := pane.installOutputLease(testOutputLease(0, &wire), 7, 8, 3); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if _, err := pane.enterHistoryMode(); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if _, err := pane.handleHistoryInput([]byte("kk")); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if _, err := pane.handleHistoryInput([]byte("jj")); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)

	offset := wire.Len()
	if _, err := pane.handleHistoryInput([]byte("j")); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	commands := decodePendingCommands(t, wire.Bytes()[offset:])

	scrolls, exposedPositions := 0, 0
	var cursor protocol.CursorUpdate
	counterFound := false
	for _, command := range commands {
		switch command.Opcode {
		case protocol.DisplayOpcodeScrollRegion:
			scrolls++
			if command.ScrollRegion != (protocol.ScrollRegion{Top: 0, Bottom: 3, Delta: -1}) {
				t.Fatalf("scroll = %#v, want [0,3) delta -1", command.ScrollRegion)
			}
		case protocol.DisplayOpcodeSetWritePosition:
			if command.Row == 2 {
				exposedPositions++
			} else if command.Row != 0 {
				t.Fatalf("incremental history repaint targeted row %d: %#v", command.Row, commands)
			}
		case protocol.DisplayOpcodeWriteTextUTF8:
			counterFound = counterFound || string(command.Text) == "[1/4]"
		case protocol.DisplayOpcodeCursorUpdate:
			cursor = command.Cursor
		}
	}
	if scrolls != 1 || exposedPositions != 1 {
		t.Fatalf("scrolls=%d exposed positions=%d commands=%#v", scrolls, exposedPositions, commands)
	}
	if !displayCommandsContainText(commands, "v222") {
		t.Fatalf("exposed bottom row was not repainted: %#v", commands)
	}
	for _, hidden := range []string{"h333", "v111", "v333"} {
		if displayCommandsContainText(commands, hidden) {
			t.Fatalf("incremental movement repainted hidden row %q: %#v", hidden, commands)
		}
	}
	if !counterFound {
		t.Fatalf("history counter was not updated: %#v", commands)
	}
	if cursor.Cursor != (protocol.Cursor{X: 0, Y: 2}) || !cursor.Visible {
		t.Fatalf("history cursor = %#v, want visible (0,2)", cursor)
	}
}

func TestHistoryKeyboardSelectionCopiesAndExits(t *testing.T) {
	pane := &Pane{ID: 0, terminal: newTerminal(5, 2)}
	row := func(text string) decodedTestRow {
		cells := make([]decodedTestCell, 0, len(text))
		for _, r := range text {
			cells = append(cells, decodedTestCell{Cluster: string(r), Width: 1})
		}
		return decodedTestRow{Cells: cells}
	}
	setTestRows(pane.terminal, nil, []decodedTestRow{row("hello"), row("world")})
	pane.terminal.CursorX = 0
	pane.terminal.CursorY = 0
	if result := pane.handleHistoryRequest(&paneHistoryRequest{Action: paneHistoryEnter}); result.Err != nil {
		t.Fatal(result.Err)
	}

	data, err := pane.handleHistoryInput([]byte(" \x1b[C\x1b[C\x1b[C\x1b[C\r"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "hello"; got != want {
		t.Fatalf("keyboard selection = %q, want %q", got, want)
	}
	if pane.isHistoryMode() {
		t.Fatal("keyboard copy did not exit history mode")
	}
}

func displayCommandsContainText(commands []protocol.DisplayCommand, text string) bool {
	for _, command := range commands {
		if bytes.Contains(command.Text, []byte(text)) {
			return true
		}
	}
	return false
}

func TestControlCExitsHistoryInputMode(t *testing.T) {
	direction, count, exit, consumed := decodeHistoryInput([]byte{0x03})
	if !exit || consumed != 1 || direction != 0 || count != 0 {
		t.Fatalf("decodeHistoryInput(Ctrl+C) = direction=%d count=%d exit=%v consumed=%d", direction, count, exit, consumed)
	}
}

func historyTestRow(text string) decodedTestRow {
	cells := make([]decodedTestCell, 4)
	for i := range cells {
		cells[i] = decodedTestCell{Width: 1}
	}
	for i, r := range text {
		if i >= len(cells) {
			break
		}
		cells[i].Cluster = string(r)
	}
	return decodedTestRow{Cells: cells, WrapsNext: strings.HasSuffix(text, "\\")}
}

func newHistoryRenderTestPane(t *testing.T) *Pane {
	t.Helper()
	pane := &Pane{ID: 0, terminal: newTerminal(8, 3)}
	setTestRows(pane.terminal,
		[]decodedTestRow{
			historyTestRow("h000"),
			historyTestRow("h111"),
			historyTestRow("h222"),
			historyTestRow("h333"),
		},
		[]decodedTestRow{
			historyTestRow("v111"),
			historyTestRow("v222"),
			historyTestRow("v333"),
		},
	)
	if err := pane.installHistoryView(captureTerminalHistorySnapshot(pane.terminal)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pane.historyView != nil {
			pane.exitHistoryModeNow()
		}
	})
	return pane
}
