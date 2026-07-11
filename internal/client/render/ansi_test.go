package render

import (
	"strings"
	"testing"

	"tali/internal/protocol"
)

func TestTabBarShowsActiveMarkerAndSessionID(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(20, 5)
	state.SessionID = 7
	state.Windows = []protocol.WindowInfo{
		{WindowID: 1, PaneID: 1, Index: 0, Title: "bash", Active: true},
		{WindowID: 2, PaneID: 2, Index: 1, Title: "logs"},
	}
	state.ActiveWindowID = 1
	got := string(RenderANSI(state))
	if !strings.Contains(got, "[7]") {
		t.Fatalf("RenderANSI() missing active session id prefix: %q", got)
	}
	if !strings.Contains(got, "0:bash*") {
		t.Fatalf("RenderANSI() missing active tab marker: %q", got)
	}
	if !strings.Contains(got, "\x1b[0;39;48;2;42;99;158m") && !strings.Contains(got, "\x1b[0;48;2;42;99;158;39m") {
		t.Fatalf("RenderANSI() missing rgb tab bar color: %q", got)
	}
}

func TestTabBarTruncatesWithoutWrapping(t *testing.T) {
	state := NewClientState()
	state.SetTerminalSize(10, 3)
	state.SessionID = 7
	state.Windows = []protocol.WindowInfo{
		{WindowID: 1, PaneID: 1, Index: 0, Title: "verylongtitle", Active: true},
	}
	state.ActiveWindowID = 1
	bar := renderTabBar(state)
	if len(bar) < 10 {
		t.Fatalf("tab bar too short: %d", len(bar))
	}
	if strings.Contains(bar, "\n") {
		t.Fatalf("tab bar wrapped: %q", bar)
	}
	if !strings.Contains(bar, "[7]") {
		t.Fatalf("tab bar missing active session prefix: %q", bar)
	}
}
