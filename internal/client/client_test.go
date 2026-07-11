package client

import (
	"testing"

	"tali/internal/client/render"
	"tali/internal/protocol"
)

func TestParseTarget(t *testing.T) {
	target, err := ParseTarget("alice@example.com")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	if target.Username != "alice" || target.Hostname != "example.com" || !target.HasExplicitUser {
		t.Fatalf("ParseTarget() = %#v", target)
	}
}

func TestParseTargetHostOnly(t *testing.T) {
	target, err := ParseTarget("myserver")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	if target.Username != "" || target.Hostname != "myserver" || target.HasExplicitUser {
		t.Fatalf("ParseTarget() = %#v", target)
	}
}

func TestParseTargetInvalid(t *testing.T) {
	cases := []string{"", "@example.com", "alice@"}
	for _, tc := range cases {
		if _, err := ParseTarget(tc); err == nil {
			t.Fatalf("ParseTarget(%q) error = nil, want error", tc)
		}
	}
}

func TestProcessInputBytePrefixActions(t *testing.T) {
	ui := &runtimeState{ui: render.NewClientState()}
	ui.with(func(state *render.ClientState) {
		state.ApplyWindowList(protocol.WindowList{
			Windows: []protocol.WindowInfo{
				{WindowID: 1, PaneID: 1, Index: 0, Title: "bash", Active: true},
				{WindowID: 2, PaneID: 2, Index: 1, Title: "logs"},
			},
			ActiveWindowID: 1,
		})
		state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 1})
	})

	prefix := false
	if inputs, mgmt, detach := processInputByte(&prefix, prefixByte, ui, Config{}); len(inputs) != 0 || len(mgmt) != 0 || detach || !prefix {
		t.Fatalf("prefix start failed")
	}
	inputs, mgmt, detach := processInputByte(&prefix, prefixByte, ui, Config{})
	if prefix || detach || len(mgmt) != 0 || len(inputs) != 1 || inputs[0][0] != prefixByte {
		t.Fatalf("literal prefix forwarding failed: %#v %#v detach=%v", inputs, mgmt, detach)
	}

	prefix = true
	_, mgmt, detach = processInputByte(&prefix, '1', ui, Config{})
	if detach {
		t.Fatalf("numeric selection unexpectedly detached")
	}
	if len(mgmt) != 1 || mgmt[0].Type != protocol.MsgSelectWindow {
		t.Fatalf("numeric selection failed: %#v", mgmt)
	}
	var sel protocol.SelectWindow
	if err := protocol.DecodeMessage(mgmt[0], &sel); err != nil || sel.WindowID != 2 {
		t.Fatalf("SelectWindow = %#v err=%v", sel, err)
	}

	prefix = true
	inputs, mgmt, detach = processInputByte(&prefix, 'd', ui, Config{})
	if !detach || prefix || len(inputs) != 0 || len(mgmt) != 0 {
		t.Fatalf("detach action failed: inputs=%#v mgmt=%#v detach=%v prefix=%v", inputs, mgmt, detach, prefix)
	}
}

func TestDrawableRowsExcludesStatusRow(t *testing.T) {
	ui := render.NewClientState()
	ui.SetTerminalSize(80, 24)
	if got := ui.DrawableRows(); got != 23 {
		t.Fatalf("DrawableRows() = %d, want 23", got)
	}
}
