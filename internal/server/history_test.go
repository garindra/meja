package server

import (
	"strings"
	"testing"

	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

func TestHistorySnapshotIsIndependentAndMovesAtViewportBoundary(t *testing.T) {
	pane := &Pane{ID: 0, terminal: terminal.New(4, 3)}
	pane.terminal.History = []terminal.Row{
		historyTestRow("old1"),
		historyTestRow("old2"),
	}
	pane.terminal.GridRows = []terminal.Row{
		historyTestRow("live"),
		historyTestRow("mid "),
		historyTestRow("end "),
	}
	pane.terminal.CursorY = 2
	pane.terminal.Cells = append(append(append([]protocol.Cell{}, pane.terminal.GridRows[0].Cells...), pane.terminal.GridRows[1].Cells...), pane.terminal.GridRows[2].Cells...)

	snapshot := captureTerminalHistorySnapshot(pane.terminal)
	pane.terminal.History[0].Cells[0].Rune = 'X'
	if got := string(snapshot.Rows[0].Cells[0].Rune); got != "o" {
		t.Fatalf("snapshot aliased canonical history: %q", got)
	}

	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 4, 3
	s.CreateWindow(pane, 0)
	if err := s.InstallHistoryView(0, pane.ID, snapshot); err != nil {
		t.Fatalf("InstallHistoryView() error = %v", err)
	}
	for i := 0; i < 2; i++ {
		move, ok := s.moveHistory(0, pane.ID, -1)
		if !ok || !move.CursorOnly || move.Delta != 0 {
			t.Fatalf("cursor-only move %d = %#v ok=%v", i, move, ok)
		}
	}
	move, ok := s.moveHistory(0, pane.ID, -1)
	if !ok || move.Delta != 1 || move.NewCounter != "[1/2]" {
		t.Fatalf("boundary move = %#v ok=%v", move, ok)
	}
}

func TestClientRetainsHistoryViewsForMultiplePanes(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 8, 4
	pane0 := &Pane{ID: s.AddPaneID(), terminal: terminal.New(8, 4)}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), terminal: terminal.New(8, 4)}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if err := s.InstallHistoryView(0, pane1.ID, captureTerminalHistorySnapshot(pane1.terminal)); err != nil {
		t.Fatalf("install pane1 history = %v", err)
	}
	if _, _, err := s.FocusPane(0, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	}
	if err := s.InstallHistoryView(0, pane0.ID, captureTerminalHistorySnapshot(pane0.terminal)); err != nil {
		t.Fatalf("install pane0 history = %v", err)
	}
	if s.HistoryView(0, pane0.ID) == nil || s.HistoryView(0, pane1.ID) == nil {
		t.Fatal("multiple pane history views were not retained")
	}
}

func TestControlCExitsHistoryInputMode(t *testing.T) {
	direction, count, exit, consumed := decodeHistoryInput([]byte{0x03})
	if !exit || consumed != 1 || direction != 0 || count != 0 {
		t.Fatalf("decodeHistoryInput(Ctrl+C) = direction=%d count=%d exit=%v consumed=%d", direction, count, exit, consumed)
	}
}

func historyTestRow(text string) terminal.Row {
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
	return terminal.Row{Cells: cells, WrapsNext: strings.HasSuffix(text, "\\")}
}
