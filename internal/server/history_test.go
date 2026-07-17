package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestHistorySnapshotIsIndependentAndMovesAtViewportBoundary(t *testing.T) {
	pane := &Pane{ID: 0, terminal: newTerminal(4, 3)}
	pane.terminal.History = []Row{
		historyTestRow("old1"),
		historyTestRow("old2"),
	}
	pane.terminal.GridRows = []Row{
		historyTestRow("live"),
		historyTestRow("mid "),
		historyTestRow("end "),
	}
	pane.terminal.CursorY = 2

	snapshot := captureTerminalHistorySnapshot(pane.terminal)
	pane.terminal.History[0].Cells[0].Rune = 'X'
	if got := string(snapshot.Rows[0].Cells[0].Rune); got != "o" {
		t.Fatalf("snapshot aliased canonical history: %q", got)
	}

	if err := pane.installHistoryView(snapshot); err != nil {
		t.Fatalf("installHistoryView() error = %v", err)
	}
	for i := 0; i < 2; i++ {
		move, ok := pane.moveHistory(-1)
		if !ok || !move.CursorOnly || move.Delta != 0 {
			t.Fatalf("cursor-only move %d = %#v ok=%v", i, move, ok)
		}
	}
	move, ok := pane.moveHistory(-1)
	if !ok || move.Delta != 1 || move.NewCounter != "[1/2]" {
		t.Fatalf("boundary move = %#v ok=%v", move, ok)
	}
}

func TestPanesRetainIndependentHistoryViews(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 8, 4
	pane0 := &Pane{ID: s.AddPaneID(), terminal: newTerminal(8, 4)}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), terminal: newTerminal(8, 4)}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if err := pane1.installHistoryView(captureTerminalHistorySnapshot(pane1.terminal)); err != nil {
		t.Fatalf("install pane1 history = %v", err)
	}
	if _, _, err := s.FocusPane(0, pane0.ID); err != nil {
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
	pane.terminal.GridRows = []Row{historyTestRow("live"), historyTestRow("end ")}
	ptyOutput := startTestPaneLoop(pane)
	defer func() {
		close(ptyOutput)
		<-pane.mainDone
		pane.stop()
	}()
	sendPTYOutput := func(data string) {
		buffer := ptyReadBuffers.Get().([]byte)
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
	if wire.Len() <= historyBytes {
		t.Fatal("exiting history did not repaint the pane's existing output stream")
	}
	if !displayCommandsContainText(decodePendingCommands(t, wire.Bytes()[historyBytes:]), "XYve") {
		t.Fatal("exiting history did not render the pane's current terminal on the existing stream")
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

func historyTestRow(text string) Row {
	cells := make([]protocol.Cell, 4)
	for i := range cells {
		cells[i] = protocol.Cell{Rune: ' ', Width: 1}
	}
	for i, r := range text {
		if i >= len(cells) {
			break
		}
		cells[i].Rune = r
	}
	return Row{Cells: cells, WrapsNext: strings.HasSuffix(text, "\\")}
}
