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

func TestDrawableRowsExcludesStatusRow(t *testing.T) {
	ui := render.NewClientState()
	ui.SetTerminalSize(80, 24)
	if got := ui.DrawableRows(); got != 23 {
		t.Fatalf("DrawableRows() = %d, want 23", got)
	}
}

func TestForwardInputBatchesContiguousBytes(t *testing.T) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	defer stdinR.Close()

	inputFrames := make(chan protocol.Frame, 8)
	errs := make(chan error, 1)
	done := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go forwardInput(ctx, stdinR, inputFrames, errs, done)

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
		if string(msg.Data) != "abc" {
			t.Fatalf("batched input data = %q", string(msg.Data))
		}
	default:
		t.Fatal("expected one input frame")
	}

	select {
	case frame := <-inputFrames:
		t.Fatalf("unexpected extra input frame: %#v", frame)
	default:
	}

}
