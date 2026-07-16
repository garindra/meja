package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/control"
	"github.com/garindra/meja/internal/protocol"
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
	start, err := controllerCommand("/opt/meja", selector, "", "")
	if err != nil || start != "'/opt/meja' '-L' 'dev' __control-v1 start-session" {
		t.Fatalf("start command = %q, %v", start, err)
	}
	connect, err := controllerCommand("/opt/meja", selector, "42", "")
	if err != nil || connect != "'/opt/meja' '-L' 'dev' __control-v1 connect-session '42'" {
		t.Fatalf("connect command = %q, %v", connect, err)
	}
}

func TestControllerCommandQuotesExactSocketPath(t *testing.T) {
	command, err := controllerCommand("/opt/meja", control.SocketSelector{Path: "/tmp/meja user's/dev.sock"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "'/opt/meja' '-S' '/tmp/meja user'\\''s/dev.sock' __control-v1 start-session"
	if command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}
}

func TestControllerCommandQuotesSessionNames(t *testing.T) {
	selector := control.SocketSelector{Profile: "default"}
	start, err := controllerCommand("meja", selector, "", "my work")
	if err != nil || !strings.HasSuffix(start, "start-session 'my work'") {
		t.Fatalf("named start command = %q, %v", start, err)
	}
	attach, err := controllerCommand("meja", selector, "my work", "")
	if err != nil || !strings.HasSuffix(attach, "connect-session 'my work'") {
		t.Fatalf("named attach command = %q, %v", attach, err)
	}
}

func TestRestoreControllerCommandIncludesProfileNameAndMode(t *testing.T) {
	command, err := restoreControllerCommand("/opt/meja", control.SocketSelector{Profile: "dev"}, "my work", "prepare")
	if err != nil {
		t.Fatal(err)
	}
	want := "'/opt/meja' '-L' 'dev' __control-v1 restore-session 'my work' 'prepare'"
	if command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}
}

func TestSSHCommandErrorIncludesRemoteStderr(t *testing.T) {
	err := sshCommandError("SSH bootstrap failed", io.EOF, "bash: meja: command not found\n")
	want := "SSH bootstrap failed: EOF: bash: meja: command not found"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestInitialManagementFrameActivatesTerminalAfterRead(t *testing.T) {
	var wire bytes.Buffer
	want := protocol.Frame{Type: protocol.MsgSessionAttachOK, Payload: []byte("attached")}
	if err := protocol.NewEncoder(&wire).WriteFrame(want); err != nil {
		t.Fatal(err)
	}
	activated := false
	got, err := readInitialManagementFrame(protocol.NewDecoder(&wire, protocol.DefaultMaxFrameSize), func() error {
		activated = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !activated {
		t.Fatal("terminal was not activated after receiving the initial management frame")
	}
	if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("frame = %#v, want %#v", got, want)
	}
}

func TestInitialManagementReadFailureDoesNotActivateTerminal(t *testing.T) {
	activated := false
	_, err := readInitialManagementFrame(protocol.NewDecoder(bytes.NewReader(nil), protocol.DefaultMaxFrameSize), func() error {
		activated = true
		return nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want EOF", err)
	}
	if activated {
		t.Fatal("terminal activated before an initial management frame was received")
	}
}

func TestQUICDialIdleTimeoutReportsUnreachableUDPAddress(t *testing.T) {
	err := quicDialError("example.com:60000", &quic.IdleTimeoutError{})
	want := "UDP example.com:60000 is unreachable: timeout: no recent network activity"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	var timeout *quic.IdleTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatal("formatted error does not preserve the QUIC idle timeout")
	}
}

func TestQUICDialOtherErrorsKeepDialContext(t *testing.T) {
	cause := errors.New("resolve failed")
	err := quicDialError("example.com:60000", cause)
	want := "dial example.com:60000: resolve failed"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if !errors.Is(err, cause) {
		t.Fatal("formatted error does not preserve the dial failure")
	}
}

func TestIncomingRenderBurstLog(t *testing.T) {
	var log bytes.Buffer
	diagnostics := newRenderDiagnostics(&log)
	diagnostics.reportCommand(0, protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteText, Text: []byte("x")}, 3)
	diagnostics.reportCommand(0, protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}, 1)
	diagnostics.reportRedraw("test", 7)
	diagnostics.close()

	got := log.String()
	for _, want := range []string{
		"incoming burst at=",
		"window=50ms",
		"wire_bytes=4",
		"text_bytes=1",
		"commands=2",
		"types=WriteText:1,Present:1",
		"redraw request #1: test",
		"redraw write #1 bytes=7",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("incoming burst log missing %q: %q", want, got)
		}
	}

	diagnostics.close()
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

func TestForwardInputBatchesContiguousBytes(t *testing.T) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	defer stdinR.Close()

	inputFrames := make(chan protocol.Frame, 8)
	errs := make(chan error, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var input atomic.Pointer[inputDestination]
	input.Store(&inputDestination{frames: inputFrames, done: ctx.Done()})
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	go forwardInput(ctx, stdinR, &input, ui, errs, cancel)

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
	case event := <-ui.events:
		local, ok := event.(localInputEvent)
		if !ok || string(local.data) != "abc" {
			t.Fatalf("local input event = %#v", event)
		}
	default:
		t.Fatal("expected local input event")
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

func TestCtrlCExitsWhileDisconnected(t *testing.T) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var input atomic.Pointer[inputDestination]
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	errs := make(chan error, 1)
	go forwardInput(ctx, stdinR, &input, ui, errs, cancel)

	if _, err := stdinW.Write([]byte{0x03}); err != nil {
		t.Fatalf("stdin write = %v", err)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errDisconnectedInterrupt) {
			t.Fatalf("cancellation cause = %v", context.Cause(ctx))
		}
		if err := clientExitError(ctx); err != nil {
			t.Fatalf("clientExitError() = %v, want nil", err)
		}
	case err := <-errs:
		t.Fatalf("forwardInput() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("Ctrl+C did not stop the disconnected client")
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

func TestDroppedPredictedInputQueuesResetAfterLocalEvent(t *testing.T) {
	done := make(chan struct{})
	close(done)
	frames := make(chan protocol.Frame)
	var input atomic.Pointer[inputDestination]
	input.Store(&inputDestination{frames: frames, done: done})
	ui := &runtimeState{events: make(chan renderEvent, 2)}
	if _, err := sendCurrentPredictedInput(&input, ui, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if event := <-ui.events; event.(localInputEvent).data[0] != 'a' {
		t.Fatalf("first event = %#v", event)
	}
	if event := <-ui.events; event != (inputPredictionResetEvent{}) {
		t.Fatalf("second event = %#v", event)
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
	ui.emit(paneFrameEvent{slot: 0, frame: renderFrame{layoutRevision: 9, spans: []paintSpan{{kind: paintText, styleID: 0, cellWidth: 1, text: []byte("buffered")}}}})
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
	waitForBufferText(t, &stdout, " Reconnecting")
	waitForBufferText(t, &stdout, "Press Ctrl+C to exit")
	ui.stopConnection()
	before := stdout.Len()
	ui.emit(layoutEvent{layout: testSnapshotLayout(11)})
	ui.emit(paneFrameEvent{slot: 0, frame: renderFrame{layoutRevision: 11}})
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

func TestStatusFullRefreshClearsReconnectIndicator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{stdout: &stdout, events: make(chan renderEvent, 32)}
	errs := make(chan error, 1)
	go ui.renderLoop(ctx, errs)
	ui.emit(sizeEvent{cols: 80, rows: 24})
	ui.beginConnection(true, time.Now())
	waitForBufferText(t, &stdout, " Reconnecting")
	ui.emit(paneFrameEvent{slot: protocol.StatusRenderSlot, frame: renderFrame{
		styleInstalls: []protocol.StyleDefinition{{ID: 1, Style: protocol.Style{BG: protocol.Color{Mode: "rgb", R: 42, G: 88, B: 170}}}},
		spans: []paintSpan{
			{kind: paintFill, styleID: 1, cellWidth: 1, fillRune: ' ', fillColumns: 80},
			{kind: paintText, styleID: 1, cellWidth: 1, text: []byte("ready")},
		},
	}})
	ui.beginConnection(false, time.Time{})
	waitForBufferText(t, &stdout, "ready")
	stdout.mu.Lock()
	output := stdout.String()
	stdout.mu.Unlock()
	lastReconnect := strings.LastIndex(output, " Reconnecting")
	if lastReconnect < 0 || !strings.Contains(output[lastReconnect:], "ready") {
		t.Fatalf("reconnect indicator was not replaced after connection became usable: %q", output)
	}
}

func TestLayoutActivationPreservesStatusRow(t *testing.T) {
	s := newScanoutState(true)
	s.cols, s.rows = 12, 4
	status := renderFrame{
		styleInstalls: []protocol.StyleDefinition{{ID: 1, Style: protocol.Style{BG: protocol.Color{Mode: "rgb", R: 42, G: 88, B: 170}}}},
		spans: []paintSpan{
			{kind: paintFill, styleID: 1, cellWidth: 1, fillRune: ' ', fillColumns: 12},
			{kind: paintText, styleID: 1, cellWidth: 1, text: []byte("1:shell*")},
		},
	}
	if _, err := s.acceptFrame(protocol.StatusRenderSlot, status); err != nil {
		t.Fatal(err)
	}
	_ = s.takeANSI()
	layout := protocol.WindowLayout{WindowID: 1, LayoutRevision: 2, FocusedPaneID: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 12, Height: 3}}}}
	_, _ = s.acceptLayout(layout)
	if _, err := s.acceptFrame(0, renderFrame{layoutRevision: 2}); err != nil {
		t.Fatal(err)
	}
	out := string(s.takeANSI())
	if strings.Contains(out, "\x1b[2J") {
		t.Fatalf("layout activation cleared status row: %q", out)
	}
	if !strings.Contains(out, "\x1b[1;1H\x1b[2K") || strings.Contains(out, "\x1b[4;1H\x1b[2K") {
		t.Fatalf("layout activation did not clear only content rows: %q", out)
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

func TestSessionReplacementIsTerminal(t *testing.T) {
	err := &quic.ApplicationError{
		ErrorCode:    protocol.SessionReplacedErrorCode,
		ErrorMessage: "session attached elsewhere",
	}
	done := make(chan connectionResult, 1)
	managementLoop(protocol.NewDecoder(failingReader{err: err}, protocol.DefaultMaxFrameSize), &runtimeState{}, done, nil)
	if result := <-done; !result.graceful || result.err != nil {
		t.Fatalf("replacement result = %#v, want graceful terminal result", result)
	}
	if isTerminalQUICClose(&quic.ApplicationError{ErrorCode: 1, ErrorMessage: "transport failure"}) {
		t.Fatal("ordinary application error was treated as terminal")
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
	waitForBufferText(t, &first, " Reconnecting")
	time.Sleep(20 * time.Millisecond)
	second.mu.Lock()
	leaked := strings.Contains(second.String(), " Reconnecting")
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
func TestSGRForStyleRendersAdvertisedAttributes(t *testing.T) {
	got := sgrForStyle(protocol.Style{
		Bold: true, Dim: true, Blink: true, Italic: true, Underline: true, Reverse: true, Invisible: true,
		FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"},
	})
	if got != "\x1b[0;1;2;5;3;4;7;8;39;49m" {
		t.Fatalf("attribute SGR=%q", got)
	}
}

func TestReconnectIndicatorUsesBlackTextOnOrangeBackground(t *testing.T) {
	state := newScanoutState(true)
	state.cols, state.rows = 80, 24
	state.setReconnecting(true, time.Now(), time.Now())

	out := string(state.takeANSI())
	if !strings.Contains(out, "\x1b[0;30;48;2;255;165;0m") {
		t.Fatalf("reconnect style = %q, want black text on orange background", out)
	}
}

func TestPinnedTLSRequiresExactSPKI(t *testing.T) {
	spki := []byte("test spki")
	hash := sha256.Sum256(spki)
	config, err := loadTLSConfig(hexHash(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if !config.InsecureSkipVerify || config.VerifyConnection == nil {
		t.Fatal("pinning configuration is not mandatory")
	}
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{RawSubjectPublicKeyInfo: spki}}}); err != nil {
		t.Fatal(err)
	}
	wrong := []byte("wrong")
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{RawSubjectPublicKeyInfo: wrong}}}); err == nil {
		t.Fatal("accepted wrong SPKI")
	}
}

func TestStatusOutputAcceptsBarrierlessDisplayBatch(t *testing.T) {
	encoder := protocol.NewDisplayEncoder(nil)
	commands := []protocol.DisplayCommand{
		{Opcode: protocol.DisplayOpcodeStyleInstall, StyleID: 1, Style: protocol.Style{Bold: true}},
		{Opcode: protocol.DisplayOpcodeSetWritePosition},
		{Opcode: protocol.DisplayOpcodeSetWriteStyle, StyleID: 1},
		{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte("status")},
		{Opcode: protocol.DisplayOpcodePresent},
	}
	for _, command := range commands {
		if err := encoder.AppendCommand(command); err != nil {
			t.Fatal(err)
		}
	}
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	errs := make(chan error, 1)
	readOutputStream(protocol.StatusRenderSlot, protocol.NewDisplayDecoder(bytes.NewReader(encoder.Bytes())), ui, errs, nil, nil)
	select {
	case event := <-ui.events:
		frame, ok := event.(paneFrameEvent)
		if !ok || frame.slot != protocol.StatusRenderSlot || frame.frame.layoutRevision != 0 || len(frame.frame.spans) != 1 {
			t.Fatalf("status event = %#v", event)
		}
	default:
		t.Fatal("barrierless status batch was not emitted")
	}
}

func TestPaneOutputStillRequiresRelayoutBarrier(t *testing.T) {
	encoder := protocol.NewDisplayEncoder(nil)
	if err := encoder.AppendCommand(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte("pane")}); err != nil {
		t.Fatal(err)
	}
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	errs := make(chan error, 1)
	readOutputStream(0, protocol.NewDisplayDecoder(bytes.NewReader(encoder.Bytes())), ui, errs, nil, nil)
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "before RELAYOUT_BARRIER") {
			t.Fatalf("unexpected error: %v", err)
		}
	default:
		t.Fatal("pane command without barrier was accepted")
	}
}

func TestDisplayFrameCompilerExpandsWireLatches(t *testing.T) {
	c := displayFrameCompiler{slot: 0, styles: defaultStyles(), cursorVisible: true}
	commands := []protocol.DisplayCommand{
		{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 4},
		{Opcode: protocol.DisplayOpcodeStyleInstall, StyleID: 2, Style: protocol.Style{Bold: true}},
		{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 3, Column: 5},
		{Opcode: protocol.DisplayOpcodeSetWriteStyle, StyleID: 2},
		{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte("ab")},
		{Opcode: protocol.DisplayOpcodeWriteTextUTF8Default, Text: []byte("c")},
		{Opcode: protocol.DisplayOpcodePresent},
	}
	var frame renderFrame
	for _, command := range commands {
		ready, err := c.apply(command)
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			frame = c.frame
		}
	}
	if len(frame.spans) != 2 || frame.spans[0].column != 5 || frame.spans[0].styleID != 2 || frame.spans[1].column != 7 || frame.spans[1].styleID != 0 {
		t.Fatalf("compiled frame = %#v", frame)
	}
}

func TestNativeScrollUsesRectangularMargins(t *testing.T) {
	s := newScanoutState(true)
	s.cols, s.rows = 12, 5
	layout := protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, FocusedPaneID: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{X: 6, Width: 6, Height: 4}}}}
	if _, err := s.acceptLayout(layout); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acceptFrame(0, renderFrame{layoutRevision: 1}); err != nil {
		t.Fatal(err)
	}
	_ = s.takeANSI()
	if _, err := s.acceptFrame(0, renderFrame{layoutRevision: 1, scrollDeltas: []int{-1}}); err != nil {
		t.Fatal(err)
	}
	out := string(s.takeANSI())
	if !strings.Contains(out, "\x1b[1;4r") || !strings.Contains(out, "\x1b[7;12s") || !strings.Contains(out, "\x1b[1S") {
		t.Fatalf("native scroll output = %q", out)
	}
}

func TestFallbackScrollRetainsVisibleRows(t *testing.T) {
	s := newScanoutState(false)
	s.cols, s.rows = 4, 4
	layout := protocol.WindowLayout{WindowID: 1, LayoutRevision: 1, FocusedPaneID: 1, Panes: []protocol.PanePlacement{{PaneID: 1, Slot: 0, Rect: protocol.Rect{Width: 4, Height: 3}}}}
	_, _ = s.acceptLayout(layout)
	full := renderFrame{layoutRevision: 1, spans: []paintSpan{
		{kind: paintText, row: 0, styleID: 0, cellWidth: 1, text: []byte("aaaa")},
		{kind: paintText, row: 1, styleID: 0, cellWidth: 1, text: []byte("bbbb")},
		{kind: paintText, row: 2, styleID: 0, cellWidth: 1, text: []byte("cccc")},
	}}
	if _, err := s.acceptFrame(0, full); err != nil {
		t.Fatal(err)
	}
	_ = s.takeANSI()
	if _, err := s.acceptFrame(0, renderFrame{layoutRevision: 1, scrollDeltas: []int{-1}, spans: []paintSpan{{kind: paintText, row: 2, styleID: 0, cellWidth: 1, text: []byte("dddd")}}}); err != nil {
		t.Fatal(err)
	}
	out := string(s.takeANSI())
	if !strings.Contains(out, "bbbb") || !strings.Contains(out, "cccc") || !strings.Contains(out, "dddd") || strings.Contains(out, "\x1b[1S") {
		t.Fatalf("fallback scroll output = %q", out)
	}
}

func hexHash(data []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(data)*2)
	for i, b := range data {
		out[2*i], out[2*i+1] = digits[b>>4], digits[b&15]
	}
	return string(out)
}
