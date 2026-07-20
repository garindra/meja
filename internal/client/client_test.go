package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

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

func shortUnixSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "meja-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "meja.sock")
}

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

func TestSSHForwardCommandCarriesOnlySocketSelector(t *testing.T) {
	command, err := sshForwardCommand("/opt/meja", SocketSelector{Path: "/tmp/meja user's/dev.sock"})
	if err != nil {
		t.Fatal(err)
	}
	want := "'/opt/meja' '-S' '/tmp/meja user'\\''s/dev.sock' __ssh-forward-v1"
	if command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}
}

func TestSSHForwardCommandIncludesProfile(t *testing.T) {
	command, err := sshForwardCommand("/opt/meja", SocketSelector{Profile: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	want := "'/opt/meja' '-L' 'dev' __ssh-forward-v1"
	if command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}
}

func TestSocketSelectorArgsPreserveProfileOrExactPath(t *testing.T) {
	args, err := (SocketSelector{Profile: "dev"}).Args()
	if err != nil || len(args) != 2 || args[0] != "-L" || args[1] != "dev" {
		t.Fatalf("profile args = %v, %v", args, err)
	}
	args, err = (SocketSelector{Path: "/private/meja.sock"}).Args()
	if err != nil || len(args) != 2 || args[0] != "-S" || args[1] != "/private/meja.sock" {
		t.Fatalf("socket args = %v, %v", args, err)
	}
}

func TestSocketSelectorRejectsUnsafeProfilesAndRelativePaths(t *testing.T) {
	for _, selector := range []SocketSelector{
		{Profile: "../dev"},
		{Profile: "dev/user"},
		{Path: "relative.sock"},
		{Profile: "dev", Path: "/tmp/meja.sock"},
	} {
		if _, err := selector.Normalize(); err == nil {
			t.Fatalf("selector %#v was accepted", selector)
		}
	}
}

func TestForwardCommandAddsRemoteWorkingDirectoryAndProxiesFrames(t *testing.T) {
	socket := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		request, err := protocol.ReadCommandRequest(conn)
		if err == nil && request.WorkingDirectory == "" {
			err = errors.New("forwarder omitted remote working directory")
		}
		if err == nil && !reflect.DeepEqual(request.Args, []string{"ls"}) {
			err = fmt.Errorf("forwarded args = %v", request.Args)
		}
		if err == nil {
			err = protocol.WriteCommandFrame(conn, protocol.CommandFrame{Type: protocol.CommandFrameExit})
		}
		done <- err
	}()
	var input, output bytes.Buffer
	if err := protocol.WriteCommandRequest(&input, protocol.CommandRequest{Args: []string{"ls"}}); err != nil {
		t.Fatal(err)
	}
	if err := ForwardCommand(context.Background(), SocketSelector{Path: socket}, &input, &output); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.ReadCommandFrame(&output)
	if err != nil || frame.Type != protocol.CommandFrameExit {
		t.Fatalf("forwarded response = %#v, %v", frame, err)
	}
}

func TestRunForwardsArbitraryNonAttachCommandWithoutTerminal(t *testing.T) {
	socket := shortUnixSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	wantArgs := []string{"future-command", "--opaque", "value"}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		request, err := protocol.ReadCommandRequest(conn)
		if err == nil && !reflect.DeepEqual(request.Args, wantArgs) {
			err = fmt.Errorf("forwarded args = %v, want %v", request.Args, wantArgs)
		}
		if err == nil && request.CallerSessionTarget != "17" {
			err = fmt.Errorf("caller session target = %q, want 17", request.CallerSessionTarget)
		}
		if err == nil {
			err = protocol.WriteCommandOutput(conn, protocol.CommandFrameStdout, []byte("future output\n"))
		}
		if err == nil {
			err = protocol.WriteCommandFrame(conn, protocol.CommandFrame{Type: protocol.CommandFrameExit})
		}
		done <- err
	}()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout bytes.Buffer
	err = Run(context.Background(), Config{
		Local:               true,
		SocketSelector:      SocketSelector{Path: socket},
		CallerSessionTarget: "17",
		CommandArgs:         wantArgs,
		Stdin:               stdin,
		Stdout:              &stdout,
		Stderr:              io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "future output\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHelpCommandsMayStartServer(t *testing.T) {
	for _, args := range [][]string{
		{"help"},
		{"--help"},
		{"list-sessions", "--help"},
		{"new-session", "--help"},
	} {
		if !commandMayStartServer(args) {
			t.Fatalf("help request %v may not start server", args)
		}
	}
	for _, args := range [][]string{
		{"list-sessions"},
		{"new-session", "--", "--help"},
	} {
		if got := commandMayStartServer(args); got != (args[0] == "new-session") {
			t.Fatalf("commandMayStartServer(%v) = %v", args, got)
		}
	}
}

func TestSSHCommandErrorIncludesRemoteStderr(t *testing.T) {
	err := sshCommandError("SSH bootstrap failed", io.EOF, "bash: meja: command not found\n")
	want := "SSH bootstrap failed: EOF: bash: meja: command not found"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
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
	encoder.AppendRelayoutBarrier(protocol.RelayoutBarrier{LayoutRevision: 1, Cols: 8, Rows: 3})
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

	controlFrames := make(chan protocol.Frame, 8)
	errs := make(chan error, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var control atomic.Pointer[controlDestination]
	control.Store(&controlDestination{frames: controlFrames, done: ctx.Done()})
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	ui.appliedLayoutRevision.Store(42)
	go forwardInput(ctx, stdinR, &control, ui, errs, cancel)

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
	case frame := <-controlFrames:
		if frame.Type != protocol.MsgFrontendInputBytes {
			t.Fatalf("input frame type = %d", frame.Type)
		}
		msg, err := protocol.DecodeFrontendInputBytes(frame.Payload)
		if err != nil {
			t.Fatalf("DecodeFrontendInputBytes() = %v", err)
		}
		if string(msg.Data) != "abc" {
			t.Fatalf("batched input data = %q", string(msg.Data))
		}
		if msg.LayoutRevision != 42 {
			t.Fatalf("layout revision = %d", msg.LayoutRevision)
		}
	default:
		t.Fatal("expected one input frame")
	}
	select {
	case frame := <-controlFrames:
		t.Fatalf("unexpected extra input frame: %#v", frame)
	default:
	}

}

func TestForwardInputKeepsFragmentedTerminalSequencesTogetherAcrossTransportDelay(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence string
	}{
		{name: "Kitty key release", sequence: "\x1b[115;1:3u"},
		{name: "SGR mouse motion", sequence: "\x1b[<35;69;42M"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			frames := make(chan protocol.Frame, 4)
			reads := make(chan terminalInputRead, 2)
			var control atomic.Pointer[controlDestination]
			control.Store(&controlDestination{frames: frames, done: ctx.Done()})
			ui := &runtimeState{events: make(chan renderEvent, 4)}
			errs := make(chan error, 1)
			go forwardInputReads(ctx, reads, &control, ui, errs, cancel, time.Hour)

			reads <- terminalInputRead{data: []byte{0x1b}}
			time.Sleep(10 * time.Millisecond)
			select {
			case frame := <-frames:
				t.Fatalf("trailing Escape was sent before its local ambiguity window: %#v", frame)
			default:
			}

			reads <- terminalInputRead{data: []byte(test.sequence[1:])}
			close(reads)
			select {
			case err := <-errs:
				t.Fatalf("forwardInputReads() error = %v", err)
			case frame := <-frames:
				msg, err := protocol.DecodeFrontendInputBytes(frame.Payload)
				if err != nil {
					t.Fatal(err)
				}
				if string(msg.Data) != test.sequence || msg.SourceIdle {
					t.Fatalf("forwarded sequence = data %q sourceIdle=%v, want %q without idle", msg.Data, msg.SourceIdle, test.sequence)
				}
			case <-time.After(time.Second):
				t.Fatal("fragmented terminal sequence was not forwarded")
			}
			select {
			case frame := <-frames:
				t.Fatalf("fragmented sequence was split into another frame: %#v", frame)
			default:
			}
		})
	}
}

func TestForwardInputFlushesStandaloneEscapeAtLocalTTYBoundary(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	frames := make(chan protocol.Frame, 2)
	reads := make(chan terminalInputRead, 1)
	var control atomic.Pointer[controlDestination]
	control.Store(&controlDestination{frames: frames, done: ctx.Done()})
	ui := &runtimeState{events: make(chan renderEvent, 2)}
	errs := make(chan error, 1)
	go forwardInputReads(ctx, reads, &control, ui, errs, cancel, 5*time.Millisecond)

	reads <- terminalInputRead{data: []byte{0x1b}}
	select {
	case err := <-errs:
		t.Fatalf("forwardInputReads() error = %v", err)
	case frame := <-frames:
		if frame.Type != protocol.MsgFrontendInputBytes {
			t.Fatalf("first frame type = %d", frame.Type)
		}
		msg, err := protocol.DecodeFrontendInputBytes(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(msg.Data, []byte{0x1b}) {
			t.Fatalf("standalone Escape = %q", msg.Data)
		}
		if !msg.SourceIdle {
			t.Fatal("standalone Escape did not carry its source-idle boundary")
		}
	case <-time.After(time.Second):
		t.Fatal("standalone Escape was not forwarded after its local ambiguity window")
	}
	select {
	case event := <-ui.events:
		input, ok := event.(localInputEvent)
		if !ok || !bytes.Equal(input.data, []byte{0}) {
			t.Fatalf("standalone Escape prediction boundary = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("standalone Escape did not reset prediction")
	}
}

func TestForwardInputDoesNotReplayPendingEscapeAcrossConnectionChange(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	oldFrames := make(chan protocol.Frame, 2)
	newFrames := make(chan protocol.Frame, 2)
	reads := make(chan terminalInputRead, 2)
	oldDestination := &controlDestination{frames: oldFrames, done: ctx.Done()}
	newDestination := &controlDestination{frames: newFrames, done: ctx.Done()}
	var control atomic.Pointer[controlDestination]
	control.Store(oldDestination)
	ui := &runtimeState{events: make(chan renderEvent, 2)}
	errs := make(chan error, 1)
	go forwardInputReads(ctx, reads, &control, ui, errs, cancel, time.Hour)

	reads <- terminalInputRead{data: []byte{0x1b}}
	time.Sleep(10 * time.Millisecond)
	control.Store(newDestination)
	reads <- terminalInputRead{data: []byte("x")}
	close(reads)

	select {
	case frame := <-newFrames:
		msg, err := protocol.DecodeFrontendInputBytes(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if string(msg.Data) != "x" || msg.SourceIdle {
			t.Fatalf("new connection input = data %q sourceIdle=%v", msg.Data, msg.SourceIdle)
		}
	case err := <-errs:
		t.Fatalf("forwardInputReads() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("new connection input was not forwarded")
	}
	select {
	case frame := <-oldFrames:
		t.Fatalf("pending Escape was replayed to old connection: %#v", frame)
	default:
	}
}

func TestInputRouterDropsInputWhileDisconnected(t *testing.T) {
	var control atomic.Pointer[controlDestination]
	if err := sendCurrentControlEncoded(&control, protocol.MsgFrontendInputBytes, protocol.FrontendInputBytes{Data: []byte("dropped")}, protocol.EncodeFrontendInputBytes); err != nil {
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
	var control atomic.Pointer[controlDestination]
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	errs := make(chan error, 1)
	go forwardInput(ctx, stdinR, &control, ui, errs, cancel)

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
	var control atomic.Pointer[controlDestination]
	frames := make(chan protocol.Frame, 1)
	if err := sendCurrentControlEncoded(&control, protocol.MsgFrontendInputBytes, protocol.FrontendInputBytes{Data: []byte("before")}, protocol.EncodeFrontendInputBytes); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
		t.Fatal("input routed without a connection destination")
	default:
	}
	control.Store(&controlDestination{frames: frames, done: ctx.Done()})
	if err := sendCurrentControlEncoded(&control, protocol.MsgFrontendInputBytes, protocol.FrontendInputBytes{Data: []byte("after")}, protocol.EncodeFrontendInputBytes); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatal("input was not routed after installing a connection destination")
	}
}

func TestDroppedFrontendInputDoesNotQueuePredictionEvents(t *testing.T) {
	done := make(chan struct{})
	close(done)
	frames := make(chan protocol.Frame)
	var control atomic.Pointer[controlDestination]
	control.Store(&controlDestination{frames: frames, done: done})
	ui := &runtimeState{events: make(chan renderEvent, 2)}
	if _, err := sendCurrentFrontendInput(&control, ui, []byte("a"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-ui.events:
		t.Fatalf("unexpected prediction event = %#v", event)
	default:
	}
}

func TestQueuedFrontendInputQueuesDecodedPrediction(t *testing.T) {
	done := make(chan struct{})
	frames := make(chan protocol.Frame, 1)
	var control atomic.Pointer[controlDestination]
	control.Store(&controlDestination{frames: frames, done: done})
	ui := &runtimeState{events: make(chan renderEvent, 1)}
	if _, err := sendCurrentFrontendInput(&control, ui, []byte("\x1b[97u"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-ui.events:
		input, ok := event.(localInputEvent)
		if !ok || string(input.data) != "a" {
			t.Fatalf("prediction event = %#v", event)
		}
	default:
		t.Fatal("queued input did not queue its prediction")
	}
	select {
	case frame := <-frames:
		if frame.Type != protocol.MsgFrontendInputBytes {
			t.Fatalf("frame type = %d", frame.Type)
		}
	default:
		t.Fatal("frontend input frame was not queued")
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

func TestFrontendTerminalControlUsesRenderWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{stdout: &stdout, events: make(chan renderEvent, 8), renderDone: make(chan struct{})}
	errs := make(chan error, 1)
	go ui.renderLoop(ctx, errs)
	want := []byte("\x1b[?1003h\x1b[?1006h")
	if err := ui.writeTerminal(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got := stdout.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("terminal output = %q", got)
	}
}

func TestControlLoopAppliesRegisteredExitCommandAndAcknowledges(t *testing.T) {
	var wire bytes.Buffer
	encoder := protocol.NewEncoder(&wire)
	exitCommand := []byte("\x1b[<u")
	payload, err := protocol.EncodeFrontendRegisterTerminalExitCommand(nil, protocol.FrontendRegisterTerminalExitCommand{Data: exitCommand})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(protocol.Frame{Type: protocol.MsgFrontendRegisterTerminalExitCommand, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	wantWrite := []byte("\x1b[>3u")
	payload, err = protocol.EncodeFrontendTerminalWrite(nil, protocol.FrontendTerminalWrite{Data: wantWrite})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(protocol.Frame{Type: protocol.MsgFrontendTerminalWrite, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(protocol.Frame{Type: protocol.MsgFrontendExecuteTerminalExitCommand}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{stdout: &stdout, events: make(chan renderEvent, 8), renderDone: make(chan struct{})}
	go ui.renderLoop(ctx, make(chan error, 1))
	done := make(chan connectionResult, 1)
	controlFrames := make(chan protocol.Frame, 1)
	controlLoop(protocol.NewDecoder(&wire, protocol.DefaultMaxFrameSize), ui, controlFrames, done, nil)
	if result := <-done; result.err != nil {
		t.Fatal(result.err)
	}
	ack := <-controlFrames
	if ack.Type != protocol.MsgFrontendTerminalExitComplete || len(ack.Payload) != 0 {
		t.Fatalf("terminal exit acknowledgment = %#v", ack)
	}
	if got := stdout.Bytes(); !bytes.Equal(got, append(wantWrite, exitCommand...)) {
		t.Fatalf("terminal output = %q", got)
	}
}

func TestFrontendTerminalConfigurationOwnsExitCommandAcrossDecoderReuse(t *testing.T) {
	var wire bytes.Buffer
	encoder := protocol.NewEncoder(&wire)
	exitCommand := []byte("\x1b[?1003;1006;1004;2004l\x1b[<u")
	setup := []byte("\x1b[>3u\x1b[?1003;1006;1004;2004h")
	exitPayload, err := protocol.EncodeFrontendRegisterTerminalExitCommand(nil, protocol.FrontendRegisterTerminalExitCommand{Data: exitCommand})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(protocol.Frame{Type: protocol.MsgFrontendRegisterTerminalExitCommand, Payload: exitPayload}); err != nil {
		t.Fatal(err)
	}
	setupPayload, err := protocol.EncodeFrontendTerminalWrite(nil, protocol.FrontendTerminalWrite{Data: setup})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteFrame(protocol.Frame{Type: protocol.MsgFrontendTerminalWrite, Payload: setupPayload}); err != nil {
		t.Fatal(err)
	}

	gotExit, gotSetup, err := readFrontendTerminalConfiguration(protocol.NewDecoder(&wire, protocol.DefaultMaxFrameSize))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotExit, exitCommand) {
		t.Fatalf("exit command was overwritten by decoder reuse: got %q, want %q", gotExit, exitCommand)
	}
	if !bytes.Equal(gotSetup, setup) {
		t.Fatalf("setup = %q, want %q", gotSetup, setup)
	}
}

func TestTerminalShutdownEndsSingleWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{
		stdout:            &stdout,
		events:            make(chan renderEvent, 8),
		renderDone:        make(chan struct{}),
		renderExitCommand: make(chan []byte, 1),
	}
	go ui.renderLoop(ctx, make(chan error, 1))
	if err := ui.registerTerminalExitCommand(ctx, []byte("registered-exit")); err != nil {
		t.Fatal(err)
	}
	if err := ui.shutdownTerminal(ctx, []byte("fixed-exit")); err != nil {
		t.Fatal(err)
	}
	<-ui.renderDone
	if pending := <-ui.renderExitCommand; len(pending) != 0 {
		t.Fatalf("pending exit command = %q", pending)
	}
	if got := stdout.String(); got != "registered-exitfixed-exit" {
		t.Fatalf("terminal output = %q", got)
	}
	if err := ui.writeTerminal(ctx, []byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("late terminal write error = %v", err)
	}
	if got := stdout.String(); got != "registered-exitfixed-exit" {
		t.Fatalf("late output reached terminal: %q", got)
	}
}

func TestEarlyTerminalExitIsNotRepeatedByShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout lockedBuffer
	ui := &runtimeState{
		stdout:            &stdout,
		events:            make(chan renderEvent, 8),
		renderDone:        make(chan struct{}),
		renderExitCommand: make(chan []byte, 1),
	}
	go ui.renderLoop(ctx, make(chan error, 1))
	if err := ui.registerTerminalExitCommand(ctx, []byte("registered-exit")); err != nil {
		t.Fatal(err)
	}
	if err := ui.executeTerminalExitCommand(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ui.shutdownTerminal(ctx, []byte("fixed-exit")); err != nil {
		t.Fatal(err)
	}
	<-ui.renderDone
	if pending := <-ui.renderExitCommand; len(pending) != 0 {
		t.Fatalf("pending exit command = %q", pending)
	}
	if got := stdout.String(); got != "registered-exitfixed-exit" {
		t.Fatalf("terminal output = %q", got)
	}
}

func TestFixedTerminalExitExplicitlyDisablesFrontendCaptureModes(t *testing.T) {
	exit := string(fixedTerminalExit(80))
	reset := "\x1b[?1003;1006;1004;2004l"
	if !strings.HasPrefix(exit, reset) {
		t.Fatalf("fixed terminal exit %q does not begin with capture reset %q", exit, reset)
	}
	if !strings.HasSuffix(exit, "\x1b[?1049l") {
		t.Fatalf("fixed terminal exit does not leave alternate screen: %q", exit)
	}
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
	controlLoop(protocol.NewDecoder(failingReader{err: err}, protocol.DefaultMaxFrameSize), ui, nil, done, nil)
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
	controlLoop(protocol.NewDecoder(failingReader{err: err}, protocol.DefaultMaxFrameSize), &runtimeState{}, nil, done, nil)
	if result := <-done; !result.graceful || result.err != nil || result.terminalMessage != "session attached elsewhere" {
		t.Fatalf("replacement result = %#v, want graceful terminal result with status message", result)
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

func TestTerminalStatusUsesBottomStatusBar(t *testing.T) {
	state := newScanoutState(true)
	state.cols, state.rows = 80, 24
	state.setTerminalStatus("session taken over by another client")

	out := string(state.takeANSI())
	if !strings.Contains(out, "session taken over by another client") || !strings.Contains(out, "\x1b[24;1H") {
		t.Fatalf("terminal status output = %q", out)
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

func TestStatusOutputAcceptsBarrierlessDisplayFrame(t *testing.T) {
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

func TestDisplayFrameCompilerSplitsImplicitlyWrappedText(t *testing.T) {
	c := displayFrameCompiler{slot: 0, styles: defaultStyles(), cursorVisible: true}
	commands := []protocol.DisplayCommand{
		{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 1, GridCols: 4, GridRows: 2},
		{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 2},
		{Opcode: protocol.DisplayOpcodeWriteTextUTF8Default, Text: []byte("abcdef")},
		{Opcode: protocol.DisplayOpcodePresent},
	}
	for _, command := range commands {
		if _, err := c.apply(command); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.frame.spans) != 2 {
		t.Fatalf("spans = %#v", c.frame.spans)
	}
	first, second := c.frame.spans[0], c.frame.spans[1]
	if first.row != 0 || first.column != 2 || string(first.text) != "ab" || second.row != 1 || second.column != 0 || string(second.text) != "cdef" {
		t.Fatalf("spans = %#v", c.frame.spans)
	}
	if c.row != 2 || c.column != 0 {
		t.Fatalf("insertion state = %d,%d", c.row, c.column)
	}
}

func TestDisplayFrameCompilerSplitsImplicitlyWrappedFill(t *testing.T) {
	c := displayFrameCompiler{slot: 0, styles: defaultStyles(), cursorVisible: true}
	for _, command := range []protocol.DisplayCommand{
		{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 1, GridCols: 4, GridRows: 2},
		{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 0},
		{Opcode: protocol.DisplayOpcodeFill, Fill: protocol.Fill{Columns: 8, Rune: ' ', Width: 1}},
		{Opcode: protocol.DisplayOpcodePresent},
	} {
		if _, err := c.apply(command); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.frame.spans) != 2 || c.frame.spans[0].fillColumns != 4 || c.frame.spans[1].row != 1 || c.frame.spans[1].fillColumns != 4 {
		t.Fatalf("spans = %#v", c.frame.spans)
	}
}

func TestDisplayFrameCompilerRejectsWriteBeyondGrid(t *testing.T) {
	c := displayFrameCompiler{slot: 0, styles: defaultStyles(), cursorVisible: true}
	_, _ = c.apply(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 1, GridCols: 4, GridRows: 2})
	_, _ = c.apply(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 1, Column: 3})
	if _, err := c.apply(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte("ab")}); err == nil {
		t.Fatal("write beyond grid was accepted")
	}
}

func TestDisplayFrameCompilerRejectsMultipleScrolls(t *testing.T) {
	c := displayFrameCompiler{slot: 0, styles: defaultStyles(), cursorVisible: true}
	_, _ = c.apply(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 1, GridCols: 4, GridRows: 2})
	_, _ = c.apply(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeScroll, Delta: -1})
	if _, err := c.apply(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeScroll, Delta: -1}); err == nil {
		t.Fatal("multiple scroll commands in one frame were accepted")
	}
}

func TestDisplayFrameCompilerExpandsWireLatches(t *testing.T) {
	c := displayFrameCompiler{slot: 0, styles: defaultStyles(), cursorVisible: true}
	commands := []protocol.DisplayCommand{
		{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 4, GridCols: 80, GridRows: 24},
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

func TestDisplayFrameCompilerTreatsClusterAsOneDisplayUnit(t *testing.T) {
	c := displayFrameCompiler{slot: 0, styles: defaultStyles(), cursorVisible: true}
	commands := []protocol.DisplayCommand{
		{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 4, GridCols: 80, GridRows: 24},
		{Opcode: protocol.DisplayOpcodeWriteCluster, Text: []byte("👩‍💻"), Width: 2},
		{Opcode: protocol.DisplayOpcodeWriteTextUTF8Default, Text: []byte("X")},
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
	if len(frame.spans) != 2 {
		t.Fatalf("compiled frame = %#v", frame)
	}
	cluster, following := frame.spans[0], frame.spans[1]
	if cluster.kind != paintCluster || cluster.column != 0 || cluster.cellWidth != 2 || string(cluster.text) != "👩‍💻" {
		t.Fatalf("cluster span = %#v", cluster)
	}
	if following.column != 2 || string(following.text) != "X" {
		t.Fatalf("following span = %#v", following)
	}
}

func TestClusterCacheMirrorsAnchorContinuationAndAtomicOverwrite(t *testing.T) {
	cache := newPaneScanoutCache(4, 1)
	evidence := frameEvidence{touched: make(map[cellPosition]authoritativeCellChange)}
	if err := applySpanToCache(cache, paintSpan{
		kind: paintCluster, row: 0, column: 0, cellWidth: 2, text: []byte("👩‍💻"),
	}, &evidence); err != nil {
		t.Fatal(err)
	}
	row := cache.row(0)
	if row[0].Cluster != "👩‍💻" || row[0].Width != 2 || row[1].Cluster != "" || row[1].Width != 0 {
		t.Fatalf("cached cluster = %#v", row[:2])
	}

	// Addressing the continuation column clears the complete old cluster before
	// installing the replacement at the addressed terminal column.
	if err := applySpanToCache(cache, paintSpan{
		kind: paintText, row: 0, column: 1, cellWidth: 1, text: []byte("B"),
	}, &evidence); err != nil {
		t.Fatal(err)
	}
	if row[0].Cluster != "" || row[0].Width != 1 || row[1].Cluster != "B" || row[1].Width != 1 {
		t.Fatalf("cache after continuation overwrite = %#v", row[:2])
	}
}

func TestCachedPaneRepaintEmitsClusterOnce(t *testing.T) {
	cache := newPaneScanoutCache(3, 1)
	evidence := frameEvidence{touched: make(map[cellPosition]authoritativeCellChange)}
	for _, span := range []paintSpan{
		{kind: paintCluster, row: 0, column: 0, cellWidth: 2, text: []byte("👩‍💻")},
		{kind: paintText, row: 0, column: 2, cellWidth: 1, text: []byte("X")},
	} {
		if err := applySpanToCache(cache, span, &evidence); err != nil {
			t.Fatal(err)
		}
	}
	s := newScanoutState(false)
	if err := s.emitCachedPane(protocol.Rect{Width: 3, Height: 1}, cache, defaultStyles()); err != nil {
		t.Fatal(err)
	}
	out := string(s.takeANSI())
	if strings.Count(out, "👩‍💻") != 1 || !strings.Contains(out, "👩‍💻X") {
		t.Fatalf("cached repaint = %q", out)
	}
}

func TestClientCacheAndRepaintPreserveMixedInternationalClusters(t *testing.T) {
	clusters := []struct {
		text  string
		width uint8
	}{
		{text: "a\u030a\u0301", width: 1},
		{text: "שָׁ", width: 1},
		{text: "நி", width: 1},
		{text: "क्ष", width: 2},
		{text: "葛\U000e0100", width: 2},
	}
	columns := 0
	for _, cluster := range clusters {
		columns += int(cluster.width)
	}
	cache := newPaneScanoutCache(columns, 1)
	evidence := frameEvidence{touched: make(map[cellPosition]authoritativeCellChange)}
	column := 0
	var visible strings.Builder
	for _, cluster := range clusters {
		if err := applySpanToCache(cache, paintSpan{
			kind: paintCluster, row: 0, column: column, cellWidth: cluster.width, text: []byte(cluster.text),
		}, &evidence); err != nil {
			t.Fatal(err)
		}
		if anchor := cache.row(0)[column]; anchor.Cluster != cluster.text || anchor.Width != cluster.width {
			t.Fatalf("cached anchor at %d = %#v", column, anchor)
		}
		if cluster.width == 2 && cache.row(0)[column+1].Width != 0 {
			t.Fatalf("cached continuation at %d = %#v", column+1, cache.row(0)[column+1])
		}
		column += int(cluster.width)
		visible.WriteString(cluster.text)
	}

	s := newScanoutState(false)
	if err := s.emitCachedPane(protocol.Rect{Width: columns, Height: 1}, cache, defaultStyles()); err != nil {
		t.Fatal(err)
	}
	out := string(s.takeANSI())
	if strings.Count(out, visible.String()) != 1 {
		t.Fatalf("mixed cached repaint = %q, want one contiguous %q", out, visible.String())
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
	if _, err := s.acceptFrame(0, renderFrame{layoutRevision: 1, scrollDelta: -1}); err != nil {
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
	if _, err := s.acceptFrame(0, renderFrame{layoutRevision: 1, scrollDelta: -1, spans: []paintSpan{{kind: paintText, row: 2, styleID: 0, cellWidth: 1, text: []byte("dddd")}}}); err != nil {
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
