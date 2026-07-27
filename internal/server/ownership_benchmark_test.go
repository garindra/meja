package server

import (
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

var benchmarkPaneSink *Pane
var benchmarkWindowSink *Window

func BenchmarkPaneMetadataRead(b *testing.B) {
	pane := &Pane{}
	metadata := paneTerminalMetadata{
		applicationCursorKeys: true,
		bracketedPaste:        true,
		mouseTracking:         MouseTrackingMotion,
		mouseEncoding:         MouseEncodingSGR,
	}
	pane.metadata.Store(&metadata)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := pane.InputMode(); !got.bracketedPaste {
			b.Fatal("metadata read lost published value")
		}
	}
}

func BenchmarkClientViewPaneResolution(b *testing.B) {
	panes := make([]Pane, 8)
	view := ClientView{
		Panes:    make([]ClientPanePlacement, len(panes)),
		paneByID: make(map[uint64]*Pane, len(panes)),
	}
	for index := range panes {
		panes[index].ID = uint64(index + 1)
		view.Panes[index] = ClientPanePlacement{Pane: &panes[index]}
		view.paneByID[panes[index].ID] = &panes[index]
	}
	view.Layout.FocusedPaneID = panes[len(panes)-1].ID
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkPaneSink = view.Pane(6)
		benchmarkPaneSink = view.FocusedPane()
	}
}

func BenchmarkOrdinaryKeyRouting(b *testing.B) {
	pane := &Pane{ID: 1, ptyInput: make(chan []byte, 1)}
	metadata := paneTerminalMetadata{}
	pane.metadata.Store(&metadata)
	client := &ClientInstance{
		currentView: ClientView{
			Layout: protocol.ClientLayout{FocusedPaneID: pane.ID},
			Panes:  []ClientPanePlacement{{Pane: pane}},
			paneByID: map[uint64]*Pane{
				pane.ID: pane,
			},
		},
		heldKeys: make(map[frontendHeldKey]uint64),
	}
	key := frontendKeyEvent{Code: frontendKeyRune, Rune: 'x', Action: frontendKeyPress}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := client.handleFrontendKey(key); err != nil {
			b.Fatal(err)
		}
		<-pane.ptyInput
	}
}

func BenchmarkPointerHitTestAndRouting(b *testing.B) {
	pane := &Pane{ID: 1, ptyInput: make(chan []byte, 1)}
	metadata := paneTerminalMetadata{mouseTracking: MouseTrackingMotion, mouseEncoding: MouseEncodingSGR}
	pane.metadata.Store(&metadata)
	layout := protocol.ClientLayout{
		LayoutRevision: 1,
		FocusedPaneID:  pane.ID,
		Panes: []protocol.PanePlacement{{
			PaneID: pane.ID,
			Rect:   protocol.Rect{Width: 80, Height: 24},
		}},
	}
	client := &ClientInstance{
		currentView: ClientView{
			Layout: layout,
			Panes:  []ClientPanePlacement{{Pane: pane, Placement: layout.Panes[0]}},
			paneByID: map[uint64]*Pane{
				pane.ID: pane,
			},
		},
	}
	event := frontendPointerEvent{Action: frontendPointerWheelDown, X: 40, Y: 12}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := client.handleFrontendPointer(layout.LayoutRevision, event); err != nil {
			b.Fatal(err)
		}
		<-pane.ptyInput
	}
}

func BenchmarkRequiredMailboxEnqueueDequeue(b *testing.B) {
	connection := newClientConnection()
	// Measure the bounded queue itself without scheduling the persistent
	// delivery worker. Worker lifetime is covered by deterministic tests.
	connection.workerOnce.Do(func() {})
	command := clientInstanceCommand{ClearStatusMessage: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !connection.enqueueRequired(command) {
			b.Fatal("required mailbox saturated")
		}
		<-connection.required
	}
}

func BenchmarkCoalescedStatusNotification(b *testing.B) {
	connection := newClientConnection()
	connection.workerOnce.Do(func() {})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for range 8 {
			connection.enqueueStatusRefresh(clientStatusState{SessionID: 1}, true)
		}
		<-connection.refresh
	}
}

func BenchmarkTransitionSnapshotDeepClone(b *testing.B) {
	window := &Window{
		ID: 1,
		Layout: &SplitLayout{
			Direction: SplitVertical,
			Ratio:     500,
			First: &SplitLayout{
				Direction: SplitHorizontal,
				Ratio:     400,
				First:     &PaneLayout{PaneID: 1},
				Second:    &PaneLayout{PaneID: 2},
			},
			Second: &SplitLayout{
				Direction: SplitHorizontal,
				Ratio:     600,
				First:     &PaneLayout{PaneID: 3},
				Second:    &PaneLayout{PaneID: 4},
			},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkWindowSink = cloneWindow(window)
	}
}
