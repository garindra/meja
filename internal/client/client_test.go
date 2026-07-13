package client

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tali/internal/client/render"
	"tali/internal/protocol"
)

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedBuffer) Len() int { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.Len() }

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
	ui.recordIncomingRenderFrame(0, protocol.Frame{
		Type:    protocol.MsgWriteText,
		Payload: make([]byte, 7),
	})
	ui.recordIncomingRenderFrame(0, protocol.Frame{
		Type:    protocol.MsgPresent,
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
		"types=WriteText:1,Present:1",
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

func TestFormatIncomingWriteStyles(t *testing.T) {
	plain := renderStyleKey{slot: 0, id: 1}
	bold := renderStyleKey{slot: 0, id: 2}
	got := formatIncomingWriteStyles(map[renderStyleKey]uint64{plain: 20, bold: 15}, map[renderStyleKey]protocol.Style{plain: {FG: protocol.Color{Mode: "indexed", Index: 7}}, bold: {Bold: true, FG: protocol.Color{Mode: "indexed", Index: 7}}})
	for _, want := range []string{"slot0/id1:20{plain,fg=idx7,bg=default}", "slot0/id2:15{bold,fg=idx7,bg=default}"} {
		if !strings.Contains(got, want) {
			t.Fatalf("styles %q missing %q", got, want)
		}
	}
}

func TestOutputCommandsAreNotRenderedBeforePresent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{stdout: &stdout, events: make(chan renderEvent, 16)}
	errs := make(chan error, 2)
	go ui.renderLoop(ctx, errs)
	ui.emit(sizeEvent{cols: 8, rows: 4})
	ui.emit(layoutEvent{layout: protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, FocusedPaneID: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 8, Height: 3}}}}})
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go readOutputStream(0, protocol.NewDecoder(reader, protocol.DefaultMaxFrameSize), ui, done)
	enc := protocol.NewEncoder(writer)
	write := func(kind uint64, payload []byte) {
		t.Helper()
		if err := enc.WriteFrame(protocol.Frame{Type: kind, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := protocol.EncodeRelayoutBarrier(nil, protocol.RelayoutBarrier{LayoutRevision: 1})
	write(protocol.MsgRelayoutBarrier, b)
	b, _ = protocol.EncodeSetWritePosition(nil, protocol.SetWritePosition{})
	write(protocol.MsgSetWritePosition, b)
	b, _ = protocol.EncodeSetWriteStyle(nil, protocol.SetWriteStyle{})
	write(protocol.MsgSetWriteStyle, b)
	b, _ = protocol.EncodeWriteText(nil, protocol.WriteText{CellWidth: 1, Text: []byte("x")})
	write(protocol.MsgWriteText, b)
	time.Sleep(10 * time.Millisecond)
	if stdout.Len() != 0 {
		t.Fatalf("rendered %d bytes before PRESENT", stdout.Len())
	}
	b, _ = protocol.EncodePresent(nil, protocol.Present{})
	write(protocol.MsgPresent, b)
	deadline := time.Now().Add(time.Second)
	for stdout.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stdout.Len() == 0 {
		t.Fatal("PRESENT did not render")
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
