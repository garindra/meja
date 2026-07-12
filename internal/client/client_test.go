package client

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

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

func TestIncomingRenderBurstLog(t *testing.T) {
	var log bytes.Buffer
	ui := &runtimeState{debugRender: true, stderr: &log}
	ui.recordIncomingRenderFrame(protocol.Frame{
		Type:    protocol.MsgPaneUpdate,
		Payload: make([]byte, 7),
	})
	ui.recordIncomingRenderFrame(protocol.Frame{
		Type:    protocol.MsgReplacePane,
		Payload: make([]byte, 3),
	})
	ui.flushIncomingRender()

	got := log.String()
	for _, want := range []string{
		"incoming burst at=",
		"window=50ms",
		"wire_bytes=14",
		"payload_bytes=10",
		"commands=2",
		"types=ReplacePane:1,PaneUpdate:1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("incoming burst log missing %q: %q", want, got)
		}
	}

	ui.closeIncomingRenderLog()
	if strings.Count(log.String(), "incoming burst") != 1 {
		t.Fatalf("closeIncomingRenderLog() duplicated burst log: %q", log.String())
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
		state.ApplyWindowLayout(protocol.WindowLayout{
			WindowID: 1,
			Panes: []protocol.PanePlacement{
				{PaneID: 1, Rect: protocol.Rect{X: 0, Y: 0, Width: 4, Height: 2}},
				{PaneID: 2, Rect: protocol.Rect{X: 5, Y: 0, Width: 4, Height: 2}},
			},
		})
	})

	prefix := prefixIdle
	if inputs, mgmt, detach := processInputByte(&prefix, prefixByte, ui, Config{}); len(inputs) != 0 || len(mgmt) != 0 || detach || prefix != prefixActive {
		t.Fatalf("prefix start failed")
	}
	inputs, mgmt, detach := processInputByte(&prefix, prefixByte, ui, Config{})
	if prefix != prefixIdle || detach || len(mgmt) != 0 || len(inputs) != 1 || inputs[0][0] != prefixByte {
		t.Fatalf("literal prefix forwarding failed: %#v %#v detach=%v", inputs, mgmt, detach)
	}

	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, '1', ui, Config{})
	if detach {
		t.Fatalf("numeric selection unexpectedly detached")
	}
	if len(mgmt) != 1 || mgmt[0].Type != protocol.MsgSelectWindow {
		t.Fatalf("numeric selection failed: %#v", mgmt)
	}
	sel, err := protocol.DecodeSelectWindow(mgmt[0].Payload)
	if err != nil || sel.WindowID != 2 {
		t.Fatalf("SelectWindow = %#v err=%v", sel, err)
	}

	ui.with(func(state *render.ClientState) {
		state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 2, PaneID: 2})
		state.ApplyWindowLayout(protocol.WindowLayout{
			WindowID: 2,
			Panes: []protocol.PanePlacement{
				{PaneID: 2, Rect: protocol.Rect{X: 0, Y: 0, Width: 4, Height: 2}},
				{PaneID: 3, Rect: protocol.Rect{X: 5, Y: 0, Width: 4, Height: 2}},
			},
		})
	})
	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, 'l', ui, Config{})
	if detach {
		t.Fatalf("last-window selection unexpectedly detached")
	}
	if len(mgmt) != 1 || mgmt[0].Type != protocol.MsgSelectWindow {
		t.Fatalf("last-window selection failed: %#v", mgmt)
	}
	sel, err = protocol.DecodeSelectWindow(mgmt[0].Payload)
	if err != nil || sel.WindowID != 1 {
		t.Fatalf("last SelectWindow = %#v err=%v", sel, err)
	}

	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, '%', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgCreateSplit {
		t.Fatalf("split action failed: %#v detach=%v", mgmt, detach)
	}
	if split, err := protocol.DecodeCreateSplit(mgmt[0].Payload); err != nil || split.Direction != protocol.SplitVertical {
		t.Fatalf("vertical split = %#v err=%v", split, err)
	}

	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, '"', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgCreateSplit {
		t.Fatalf("horizontal split action failed: %#v detach=%v", mgmt, detach)
	}
	if split, err := protocol.DecodeCreateSplit(mgmt[0].Payload); err != nil || split.Direction != protocol.SplitHorizontal {
		t.Fatalf("horizontal split = %#v err=%v", split, err)
	}

	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, 0x1b, ui, Config{})
	if detach || len(mgmt) != 0 || prefix != prefixArrowESC {
		t.Fatalf("focus-pane arrow escape failed: mgmt=%#v detach=%v prefix=%v", mgmt, detach, prefix)
	}
	_, mgmt, detach = processInputByte(&prefix, '[', ui, Config{})
	if detach || len(mgmt) != 0 || prefix != prefixArrowCSI {
		t.Fatalf("focus-pane arrow csi failed: mgmt=%#v detach=%v prefix=%v", mgmt, detach, prefix)
	}
	_, mgmt, detach = processInputByte(&prefix, 'C', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgFocusPane {
		t.Fatalf("focus-pane action failed: %#v detach=%v", mgmt, detach)
	}

	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, 'x', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgClosePane {
		t.Fatalf("close-pane action failed: %#v detach=%v", mgmt, detach)
	}

	prefix = prefixActive
	inputs, mgmt, detach = processInputByte(&prefix, 'd', ui, Config{})
	if !detach || prefix != prefixIdle || len(inputs) != 0 || len(mgmt) != 0 {
		t.Fatalf("detach action failed: inputs=%#v mgmt=%#v detach=%v prefix=%v", inputs, mgmt, detach, prefix)
	}

	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, '[', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgEnterHistory {
		t.Fatalf("history entry failed: mgmt=%#v detach=%v", mgmt, detach)
	}
}

func TestProcessInputByteLastWindowToggles(t *testing.T) {
	ui := &runtimeState{ui: render.NewClientState()}
	ui.with(func(state *render.ClientState) {
		state.ApplyWindowList(protocol.WindowList{
			Windows: []protocol.WindowInfo{
				{WindowID: 1, PaneID: 10, Index: 0, Title: "bash"},
				{WindowID: 2, PaneID: 11, Index: 1, Title: "logs"},
			},
		})
		state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
		state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 2, PaneID: 11})
	})

	prefix := prefixActive
	_, mgmt, detach := processInputByte(&prefix, 'l', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgSelectWindow {
		t.Fatalf("first last-window selection failed: %#v detach=%v", mgmt, detach)
	}
	sel, err := protocol.DecodeSelectWindow(mgmt[0].Payload)
	if err != nil || sel.WindowID != 1 {
		t.Fatalf("first last SelectWindow = %#v err=%v", sel, err)
	}

	ui.with(func(state *render.ClientState) {
		state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 10})
	})
	prefix = prefixActive
	_, mgmt, detach = processInputByte(&prefix, 'l', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgSelectWindow {
		t.Fatalf("second last-window selection failed: %#v detach=%v", mgmt, detach)
	}
	sel, err = protocol.DecodeSelectWindow(mgmt[0].Payload)
	if err != nil || sel.WindowID != 2 {
		t.Fatalf("second last SelectWindow = %#v err=%v", sel, err)
	}
}

func TestDrawableRowsExcludesStatusRow(t *testing.T) {
	ui := render.NewClientState()
	ui.SetTerminalSize(80, 24)
	if got := ui.DrawableRows(); got != 23 {
		t.Fatalf("DrawableRows() = %d, want 23", got)
	}
}

func TestCreateWindowPrefixBlocksInputUntilReleased(t *testing.T) {
	ui := &runtimeState{ui: render.NewClientState()}

	prefix := prefixActive
	_, mgmt, detach := processInputByte(&prefix, 'c', ui, Config{})
	if detach || len(mgmt) != 1 || mgmt[0].Type != protocol.MsgCreateWindow {
		t.Fatalf("processInputByte(create) = %#v detach=%v", mgmt, detach)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := ui.waitForInputReady(ctx); err == nil {
		t.Fatal("waitForInputReady() unexpectedly returned while input was blocked")
	}

	ui.setInputBlocked(false)
	if err := ui.waitForInputReady(context.Background()); err != nil {
		t.Fatalf("waitForInputReady() after release = %v", err)
	}
}

func TestForwardInputBatchesContiguousBytes(t *testing.T) {
	ui := &runtimeState{ui: render.NewClientState()}
	ui.with(func(state *render.ClientState) {
		state.ApplyWindowSelected(protocol.WindowSelected{WindowID: 1, PaneID: 7})
	})

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	defer stdinR.Close()

	inputFrames := make(chan protocol.Frame, 8)
	mgmtFrames := make(chan protocol.Frame, 8)
	errs := make(chan error, 1)
	done := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go forwardInput(ctx, stdinR, inputFrames, mgmtFrames, ui, Config{}, errs, done)

	if _, err := stdinW.Write([]byte("abc")); err != nil {
		t.Fatalf("stdin write = %v", err)
	}
	_ = stdinW.Close()

	select {
	case err := <-errs:
		t.Fatalf("forwardInput() error = %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	select {
	case err := <-done:
		t.Fatalf("forwardInput() unexpected done = %v", err)
	default:
	}

	select {
	case frame := <-inputFrames:
		if frame.Type != protocol.MsgInputBytes {
			t.Fatalf("input frame type = %d", frame.Type)
		}
		msg, err := protocol.DecodeInputBytes(frame.Payload)
		if err != nil {
			t.Fatalf("DecodeInputBytes() = %v", err)
		}
		if msg.PaneID != 7 || string(msg.Data) != "abc" {
			t.Fatalf("batched input = pane %d data %q", msg.PaneID, string(msg.Data))
		}
	default:
		t.Fatal("expected one input frame")
	}

	select {
	case frame := <-inputFrames:
		t.Fatalf("unexpected extra input frame: %#v", frame)
	default:
	}

	select {
	case frame := <-mgmtFrames:
		t.Fatalf("unexpected management frame: %#v", frame)
	default:
	}
}
