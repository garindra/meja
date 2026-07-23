package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

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
	if !ok || move.Delta != 1 || move.NewCounter != "[1/2]" {
		t.Fatalf("boundary move = %#v ok=%v", move, ok)
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
	client := newStandaloneClient(s)
	client.TerminalCols, client.TerminalRows = 8, 4
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
	ptyOutput := startTestPaneLoop(pane)
	defer func() {
		close(ptyOutput)
		<-pane.mainDone
		pane.stop()
	}()
	sendPTYOutput := func(data string) {
		buffer := takePTYReadBuffer()
		n := copy(buffer, data)
		ptyOutput <- buffer[:n]
	}

	var wire bytes.Buffer
	if err := pane.attachOutputStream(testOutputLease(0, &wire), 7); err != nil {
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
	if !displayCommandsContainText(historyCommands, "end ") {
		t.Fatalf("history mode did not render the pane-owned frozen view: %#v", historyCommands)
	}

	sendPTYOutput("X")
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
	if !displayCommandsContainText(decodePendingCommands(t, wire.Bytes()[historyBytes:]), "XYve") {
		t.Fatal("exiting history did not render the pane's current terminal on the existing stream")
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
