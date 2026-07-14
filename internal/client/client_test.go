package client

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"tali/internal/client/render"
	"tali/internal/control"
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

func TestControllerCommandSelectsStartOrConnect(t *testing.T) {
	selector := control.SocketSelector{Profile: "dev"}
	start, err := controllerCommand("/opt/tali", selector, "", "")
	if err != nil || start != "'/opt/tali' '-L' 'dev' __control-v1 start-session" {
		t.Fatalf("start command = %q, %v", start, err)
	}
	connect, err := controllerCommand("/opt/tali", selector, "42", "")
	if err != nil || connect != "'/opt/tali' '-L' 'dev' __control-v1 connect-session '42'" {
		t.Fatalf("connect command = %q, %v", connect, err)
	}
}

func TestControllerCommandQuotesExactSocketPath(t *testing.T) {
	command, err := controllerCommand("/opt/tali", control.SocketSelector{Path: "/tmp/tali user's/dev.sock"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "'/opt/tali' '-S' '/tmp/tali user'\\''s/dev.sock' __control-v1 start-session"
	if command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}
}

func TestControllerCommandQuotesSessionNames(t *testing.T) {
	selector := control.SocketSelector{Profile: "default"}
	start, err := controllerCommand("tali", selector, "", "my work")
	if err != nil || !strings.HasSuffix(start, "start-session 'my work'") {
		t.Fatalf("named start command = %q, %v", start, err)
	}
	attach, err := controllerCommand("tali", selector, "my work", "")
	if err != nil || !strings.HasSuffix(attach, "connect-session 'my work'") {
		t.Fatalf("named attach command = %q, %v", attach, err)
	}
}

func TestSSHCommandErrorIncludesRemoteStderr(t *testing.T) {
	err := sshCommandError("SSH bootstrap failed", io.EOF, "bash: tali: command not found\n")
	want := "SSH bootstrap failed: EOF: bash: tali: command not found"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestIncomingRenderBurstLog(t *testing.T) {
	var log bytes.Buffer
	ui := &runtimeState{debugRender: true, stderr: &log}
	ui.recordIncomingRenderCommand(0, protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteText, Text: []byte("x")}, 3)
	ui.recordIncomingRenderCommand(0, protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}, 1)
	ui.flushIncomingRender()

	got := log.String()
	for _, want := range []string{
		"incoming burst at=",
		"window=50ms",
		"wire_bytes=4",
		"text_bytes=1",
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
	go readOutputStream(0, protocol.NewDisplayDecoder(reader), ui, done, nil, nil)
	write := func(data []byte) {
		t.Helper()
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	encoder := protocol.NewDisplayEncoder(nil)
	encoder.AppendRelayoutBarrier(protocol.RelayoutBarrier{LayoutRevision: 1})
	write(encoder.Bytes())
	encoder.Reset(nil)
	encoder.AppendSetWritePosition(protocol.SetWritePosition{})
	encoder.AppendSetWriteStyle(protocol.SetWriteStyle{})
	encoder.AppendWriteTextUTF8([]byte("x"))
	write(encoder.Bytes())
	time.Sleep(10 * time.Millisecond)
	if stdout.Len() != 0 {
		t.Fatalf("rendered %d bytes before PRESENT", stdout.Len())
	}
	encoder.Reset(nil)
	encoder.AppendPresent()
	write(encoder.Bytes())
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
	var input atomic.Pointer[inputDestination]
	input.Store(&inputDestination{frames: inputFrames, done: ctx.Done()})
	go forwardInput(ctx, stdinR, &input, errs, done)

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

func TestInputRouterDropsInputWhileDisconnected(t *testing.T) {
	var input atomic.Pointer[inputDestination]
	if err := sendCurrentInputEncoded(&input, protocol.MsgInputBytes, protocol.InputBytes{Data: []byte("dropped")}, protocol.EncodeInputBytes); err != nil {
		t.Fatal(err)
	}
}

func TestInputRoutesAfterConnectionDestinationIsInstalled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var input atomic.Pointer[inputDestination]
	frames := make(chan protocol.Frame, 1)
	if err := sendCurrentInputEncoded(&input, protocol.MsgInputBytes, protocol.InputBytes{Data: []byte("before")}, protocol.EncodeInputBytes); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
		t.Fatal("input routed without a connection destination")
	default:
	}
	input.Store(&inputDestination{frames: frames, done: ctx.Done()})
	if err := sendCurrentInputEncoded(&input, protocol.MsgInputBytes, protocol.InputBytes{Data: []byte("after")}, protocol.EncodeInputBytes); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatal("input was not routed after installing a connection destination")
	}
}

func TestPaneBatchBeforeLayoutIsBuffered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{stdout: &stdout, events: make(chan renderEvent, 32)}
	errs := make(chan error, 1)
	go ui.renderLoop(ctx, errs)
	ui.emit(sizeEvent{cols: 80, rows: 24})
	ui.beginConnection(false, time.Now())
	ui.emit(paneBatchEvent{slot: 0, layoutRevision: 9, commands: []protocol.DisplayCommand{{Opcode: protocol.DisplayOpcodeWriteTextUTF8Default, Text: []byte("buffered")}}})
	time.Sleep(20 * time.Millisecond)
	stdout.mu.Lock()
	early := strings.Contains(stdout.String(), "buffered")
	stdout.mu.Unlock()
	if early {
		t.Fatal("pane batch rendered before its layout")
	}
	ui.emit(layoutEvent{layout: testSnapshotLayout(9)})
	waitForBufferText(t, &stdout, "buffered")
}

func TestStoppedConnectionEventsAreDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{stdout: &stdout, events: make(chan renderEvent, 64)}
	errs := make(chan error, 1)
	go ui.renderLoop(ctx, errs)
	ui.emit(sizeEvent{cols: 80, rows: 24})
	ui.beginConnection(true, time.Now())
	waitForBufferText(t, &stdout, "tali is reconnecting")
	ui.stopConnection()
	before := stdout.Len()
	ui.emit(statusEvent{status: protocol.StatusBar{}})
	ui.emit(layoutEvent{layout: testSnapshotLayout(11)})
	ui.emit(paneBatchEvent{slot: 0, layoutRevision: 11})
	ui.sync(ctx)
	if stdout.Len() != before {
		t.Fatal("stopped connection event changed the UI")
	}
}

func TestDestroyWaitsForConnectionWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	live := &liveConnection{ctx: ctx, cancel: cancel}
	started := make(chan struct{})
	stopped := make(chan struct{})
	live.start(func() {
		close(started)
		<-ctx.Done()
		close(stopped)
	})
	<-started

	live.destroy()
	select {
	case <-stopped:
	default:
		t.Fatal("destroy returned before connection worker stopped")
	}
}

func TestConnectedEventClearsReconnectIndicator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{stdout: &stdout, events: make(chan renderEvent, 32)}
	errs := make(chan error, 1)
	go ui.renderLoop(ctx, errs)
	ui.emit(sizeEvent{cols: 80, rows: 24})
	ui.beginConnection(true, time.Now())
	waitForBufferText(t, &stdout, "tali is reconnecting")
	ui.beginConnection(false, time.Time{})
	time.Sleep(20 * time.Millisecond)
	stdout.mu.Lock()
	output := stdout.String()
	stdout.mu.Unlock()
	lastReconnect := strings.LastIndex(output, "tali is reconnecting")
	if lastReconnect < 0 || !strings.Contains(output[lastReconnect:], "\x1b[24;1H") {
		t.Fatalf("reconnect indicator was not replaced after connection became usable: %q", output)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestCleanQUICCloseWinsWhenOutputEOFArrivesFirst(t *testing.T) {
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	outputErrors := make(chan error, 1)
	readOutputStream(0, protocol.NewDisplayDecoder(bytes.NewReader(nil)), ui, outputErrors, nil, nil)

	done := make(chan connectionResult, 1)
	err := &quic.ApplicationError{ErrorCode: 0, ErrorMessage: "server stopped"}
	managementLoop(protocol.NewDecoder(failingReader{err: err}, protocol.DefaultMaxFrameSize), ui, done, nil)
	if result := <-done; !result.graceful || result.err != nil {
		t.Fatalf("terminal result = %#v, want graceful", result)
	}
}

func TestReconnectEventsPreserveLastContact(t *testing.T) {
	ui := &runtimeState{events: make(chan renderEvent, 2)}
	lastContact := time.Now().Add(-time.Minute)
	ui.beginConnection(true, lastContact)
	ui.beginConnection(true, lastContact)
	for i := 0; i < 2; i++ {
		reconnect := (<-ui.events).(reconnectEvent)
		if !reconnect.lastContact.Equal(lastContact) {
			t.Fatalf("reconnect event = %#v", reconnect)
		}
	}
}

func TestHeartbeatExpiresAfterMissedPongs(t *testing.T) {
	now := time.Now()
	if heartbeatExpired(now, now.Add(-clientPingTimeout+time.Millisecond).UnixNano()) {
		t.Fatal("heartbeat expired before timeout")
	}
	if !heartbeatExpired(now, now.Add(-clientPingTimeout).UnixNano()) {
		t.Fatal("heartbeat remained live at timeout")
	}
}

func TestReconnectStateIsLocalToRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var first, second lockedBuffer
	ui1 := &runtimeState{stdout: &first, events: make(chan renderEvent, 8)}
	ui2 := &runtimeState{stdout: &second, events: make(chan renderEvent, 8)}
	go ui1.renderLoop(ctx, make(chan error, 1))
	go ui2.renderLoop(ctx, make(chan error, 1))
	ui1.emit(sizeEvent{cols: 80, rows: 24})
	ui2.emit(sizeEvent{cols: 80, rows: 24})
	ui1.beginConnection(true, time.Now())
	waitForBufferText(t, &first, "tali is reconnecting")
	time.Sleep(20 * time.Millisecond)
	second.mu.Lock()
	leaked := strings.Contains(second.String(), "tali is reconnecting")
	second.mu.Unlock()
	if leaked {
		t.Fatal("reconnect state leaked to an independent runtime")
	}
}

func testSnapshotLayout(revision uint64) protocol.WindowLayout {
	return protocol.WindowLayout{WindowID: 1, FocusedPaneID: 1, LayoutRevision: revision, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 80, Height: 23}}}}
}

func waitForBufferText(t *testing.T, buffer *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		buffer.mu.Lock()
		found := strings.Contains(buffer.String(), want)
		buffer.mu.Unlock()
		if found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("buffer did not contain %q", want)
}
