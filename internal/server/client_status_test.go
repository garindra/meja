package server

import "testing"

func TestStatusTabWindowAtUsesRowBelowContent(t *testing.T) {
	client := &ClientInstance{terminalCols: 80, terminalRows: 23}
	client.currentView.StatusValid = true
	client.currentView.Status = clientStatusState{
		SessionID: 1,
		Windows:   []WindowStatus{{WindowID: 1, Index: 0, Title: "bash", Active: true}},
	}
	// "[1] 0:bash* " puts the first tab at columns 4..10.
	const x = 5

	// terminalRows counts content rows, so the status bar is row 23 and row
	// 22 is the last row of pane content.
	if index, ok := client.statusTabWindowAt(x, 23); !ok || index != 0 {
		t.Fatalf("status row click = %d, %v; want window 0", index, ok)
	}
	if index, ok := client.statusTabWindowAt(x, 22); ok {
		t.Fatalf("last content row click selected window %d", index)
	}
}
