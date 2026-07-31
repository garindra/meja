package server

import (
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPTYAdmissionIsImmediateFirstAndSustainedLimited(t *testing.T) {
	state := newPTYAdmissionState(ptyTurnInterval)
	start := time.Unix(1, 0)
	if !state.canAdmit() {
		t.Fatal("isolated first PTY turn was not immediately eligible")
	}

	state.admit(start)
	for turn := 1; turn <= 5; turn++ {
		if state.canAdmit() {
			t.Fatalf("turn %d became eligible before its 50ms deadline", turn)
		}
		state.timerFired()
		if !state.canAdmit() {
			t.Fatalf("turn %d was not eligible at its 50ms deadline", turn)
		}
		start = start.Add(ptyTurnInterval)
		state.admit(start)
		if got, want := state.deadline, start.Add(ptyTurnInterval); got != want {
			t.Fatalf("turn %d deadline = %v, want %v", turn, got, want)
		}
	}
}

func TestPTYKeyboardOpportunityIsOnePerSustainedBurst(t *testing.T) {
	state := newPTYAdmissionState(ptyTurnInterval)
	start := time.Unix(1, 0)
	state.admit(start)

	state.grantKeyboardOpportunity()
	if !state.canAdmit() {
		t.Fatal("keyboard input did not grant an immediate PTY opportunity")
	}
	state.admit(start.Add(time.Millisecond))
	state.grantKeyboardOpportunity()
	if state.canAdmit() {
		t.Fatal("continuous keyboard input granted a second immediate opportunity")
	}

	state.timerFired()
	state.admit(start.Add(time.Millisecond + ptyTurnInterval))
	state.grantKeyboardOpportunity()
	if state.canAdmit() {
		t.Fatal("sustained ordinary turn incorrectly rearmed the keyboard bypass")
	}

	state.timerFired()
	idleTurn := state.deadline.Add(ptyTurnInterval)
	state.admit(idleTurn)
	state.grantKeyboardOpportunity()
	if !state.canAdmit() {
		t.Fatal("an idle period did not rearm the keyboard opportunity")
	}
}

func TestPTYAttachAndResizeOpportunitiesBypassCurrentDeadline(t *testing.T) {
	now := time.Unix(1, 0)
	attach := newPTYAdmissionState(ptyTurnInterval)
	attach.admit(now)
	attach.grantStructuralOpportunity()
	if !attach.canAdmit() {
		t.Fatal("attach opportunity did not bypass the current deadline")
	}

	resize := newPTYAdmissionState(ptyTurnInterval)
	resize.admit(now)
	resize.grantStructuralOpportunity()
	if !resize.canAdmit() {
		t.Fatal("resize opportunity did not bypass the current deadline")
	}
}

type countingPTYReader struct {
	reads atomic.Int32
}

func (r *countingPTYReader) Read(data []byte) (int, error) {
	r.reads.Add(1)
	data[0] = 'x'
	return 1, nil
}

func (r *countingPTYReader) ptyReadReady() (bool, error) {
	return false, nil
}

type saturatingPTYReader struct {
	reads atomic.Int32
}

func (r *saturatingPTYReader) Read(data []byte) (int, error) {
	r.reads.Add(1)
	for index := range data {
		data[index] = 'x'
	}
	return len(data), nil
}

func (r *saturatingPTYReader) ptyReadReady() (bool, error) {
	return true, nil
}

func TestPTYReaderWaitsForDrainCreditAndShutdownPreservesBuffers(t *testing.T) {
	pane := &Pane{terminal: newTerminal(8, 1)}
	pane.initializeRuntime()
	reader := &countingPTYReader{}
	exited := make(chan struct{})
	go func() {
		relayPTYOutputFrom(pane, reader)
		close(exited)
	}()

	time.Sleep(10 * time.Millisecond)
	if got := reader.reads.Load(); got != 0 {
		t.Fatalf("reader completed %d reads without drain credit, want 0", got)
	}

	pane.stop()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("PTY reader did not stop while waiting for drain credit")
	}
	if got := len(pane.ptyFree); got != ptyReadBufferCount {
		t.Fatalf("buffer ownership after shutdown = %d buffers, want %d", got, ptyReadBufferCount)
	}
}

func TestBlockedOutputStopsPaneAndPTYReaderProgress(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 1)}
	pane.initializeRuntime()
	go pane.run()

	blocked := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	lease := testOutputLease(0, blocked)
	lease.done = pane.done
	if err := attachTestOutputWithRefresh(pane, lease, func(output *renderOutput) error {
		return output.present()
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("initial output did not reach the blocked writer")
	}

	reader := &saturatingPTYReader{}
	relayDone := make(chan struct{})
	go func() {
		relayPTYOutputFrom(pane, reader)
		close(relayDone)
	}()
	deadline := time.Now().Add(time.Second)
	for reader.reads.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("initial bounded drain did not run")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	bounded := reader.reads.Load()
	if bounded > 4 {
		t.Fatalf("blocked publication path allowed %d reads, want at most two 64 KiB drains", bounded)
	}
	time.Sleep(75 * time.Millisecond)
	if got := reader.reads.Load(); got != bounded {
		t.Fatalf("blocked publication path kept draining: before=%d after=%d", bounded, got)
	}

	pane.stop()
	close(blocked.release)
	select {
	case <-pane.mainDone:
	case <-time.After(time.Second):
		t.Fatal("pane actor did not stop")
	}
	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("PTY reader did not stop with the pane")
	}
}

func TestTerminalReplyDoesNotWaitForBlockedRenderOutput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	pane := &Pane{ID: 1, PTY: writer, terminal: newTerminal(8, 1)}
	pane.initializeRuntime()
	go pane.run()
	writerFailed := make(chan error, 1)
	go runPTYWriter(pane, func(err error) { writerFailed <- err })

	blocked := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		pane.stop()
		close(blocked.release)
		<-pane.mainDone
		<-pane.writerDone
	}()
	lease := testOutputLease(0, blocked)
	lease.done = pane.done
	if err := attachTestOutputWithRefresh(pane, lease, func(output *renderOutput) error {
		return output.present()
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("initial output did not reach the blocked writer")
	}

	buffer := takeTestPTYReadBuffer(pane)
	n := copy(buffer, "x\x1b[6n")
	pane.ptyOutput <- buffer[:n]
	reply := make([]byte, len("\x1b[1;2R"))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(reader, reply)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-writerFailed:
		t.Fatalf("PTY writer failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("terminal reply waited for blocked render output")
	}
	if got, want := string(reply), "\x1b[1;2R"; got != want {
		t.Fatalf("terminal reply = %q, want %q", got, want)
	}
}

func TestPTYBufferHandoffSteadyStateDoesNotAllocate(t *testing.T) {
	pane := &Pane{terminal: newTerminal(8, 1)}
	pane.initializeRuntime()
	allocations := testing.AllocsPerRun(1000, func() {
		buffer := <-pane.ptyFree
		pane.ptyOutput <- buffer[:1]
		buffer = <-pane.ptyOutput
		pane.ptyFree <- buffer[:ptyReadBufferSize]
	})
	if allocations != 0 {
		t.Fatalf("PTY buffer ownership cycle allocated %.2f objects, want 0", allocations)
	}
}

func TestPTYParseAndMutationMergeSteadyStateDoesNotAllocate(t *testing.T) {
	pane := &Pane{terminal: newTerminal(8, 1)}
	renderer := newPanePublicationState(pane)
	renderer.lease = &OutputLease{}
	renderer.ensureGeometry()
	var update Update
	chunk := []byte("\n")
	update.ResetFor(pane.terminal.Rows, true)
	pane.terminal.ApplyInto(chunk, &update)
	renderer.merge(update)

	allocations := testing.AllocsPerRun(1000, func() {
		update.ResetFor(pane.terminal.Rows, true)
		pane.terminal.ApplyInto(chunk, &update)
		renderer.merge(update)
	})
	if allocations != 0 {
		t.Fatalf("PTY parse and ViewMutation merge allocated %.2f objects, want 0", allocations)
	}
}

func TestPTYCadenceReusesTimerAndActorStops(t *testing.T) {
	var timers atomic.Int32
	pane := &Pane{
		terminal:          newTerminal(8, 1),
		ptyPacingInterval: 2 * time.Millisecond,
		newTimer: func(delay time.Duration) *time.Timer {
			timers.Add(1)
			return time.NewTimer(delay)
		},
	}
	pane.initializeRuntime()
	go pane.run()
	for _, data := range []string{"a", "b", "c", "d"} {
		sendTestPTYOutput(t, pane, data)
	}
	if got := timers.Load(); got != 1 {
		t.Fatalf("sustained PTY cadence created %d timers, want one reusable timer", got)
	}

	pane.stop()
	select {
	case <-pane.mainDone:
	case <-time.After(time.Second):
		t.Fatal("pane actor leaked after paced shutdown")
	}
}

func TestPaneCommandsStayResponsiveAndResizeGrantsPTYOpportunity(t *testing.T) {
	pane := &Pane{
		terminal:          newTerminal(8, 1),
		ptyPacingInterval: time.Hour,
	}
	pane.initializeRuntime()
	go pane.run()
	sendTestPTYOutput(t, pane, "a")

	buffer := takeTestPTYReadBuffer(pane)
	buffer[0] = 'b'
	pane.ptyOutput <- buffer[:1]
	captured, err := pane.capturePane(capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(captured), "b") {
		t.Fatalf("second PTY turn bypassed pacing before resize: %q", captured)
	}

	if err := pane.resize(8, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(pane.ptyFree) != ptyReadBufferCount {
		if time.Now().After(deadline) {
			t.Fatal("resize did not grant an immediate bounded PTY opportunity")
		}
		time.Sleep(time.Millisecond)
	}
	captured, err = pane.capturePane(capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captured), "ab") {
		t.Fatalf("capture after resize opportunity = %q, want paced PTY data", captured)
	}

	pane.stop()
	select {
	case <-pane.mainDone:
	case <-time.After(time.Second):
		t.Fatal("pane actor did not stop")
	}
}

func TestAcceptedInputGrantsOnlyOneImmediatePTYOpportunity(t *testing.T) {
	pane := &Pane{
		terminal:          newTerminal(8, 1),
		ptyPacingInterval: time.Hour,
	}
	pane.initializeRuntime()
	go pane.run()
	sendTestPTYOutput(t, pane, "a")

	buffer := takeTestPTYReadBuffer(pane)
	buffer[0] = 'b'
	pane.ptyOutput <- buffer[:1]
	if err := pane.sendInput([]byte("key")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(pane.ptyFree) != ptyReadBufferCount {
		if time.Now().After(deadline) {
			t.Fatal("accepted input did not grant an immediate PTY opportunity")
		}
		time.Sleep(time.Millisecond)
	}

	buffer = takeTestPTYReadBuffer(pane)
	buffer[0] = 'c'
	pane.ptyOutput <- buffer[:1]
	if err := pane.sendInput([]byte("more")); err != nil {
		t.Fatal(err)
	}
	captured, err := pane.capturePane(capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captured), "ab") || strings.Contains(string(captured), "c") {
		t.Fatalf("capture after repeated accepted input = %q, want one immediate PTY turn", captured)
	}

	pane.stop()
	select {
	case <-pane.mainDone:
	case <-time.After(time.Second):
		t.Fatal("pane actor did not stop")
	}
}

func TestAttachGrantsImmediatePTYOpportunity(t *testing.T) {
	pane := &Pane{
		terminal:          newTerminal(8, 1),
		ptyPacingInterval: time.Hour,
	}
	pane.initializeRuntime()
	go pane.run()
	sendTestPTYOutput(t, pane, "a")

	buffer := takeTestPTYReadBuffer(pane)
	buffer[0] = 'b'
	pane.ptyOutput <- buffer[:1]
	lease := testOutputLease(0, io.Discard)
	lease.done = pane.done
	if err := pane.installOutputLease(lease, 1, 8, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(pane.ptyFree) != ptyReadBufferCount {
		if time.Now().After(deadline) {
			t.Fatal("attach did not grant an immediate bounded PTY opportunity")
		}
		time.Sleep(time.Millisecond)
	}
	captured, err := pane.capturePane(capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captured), "ab") {
		t.Fatalf("capture after attach opportunity = %q, want paced PTY data", captured)
	}

	pane.stop()
	select {
	case <-pane.mainDone:
	case <-time.After(time.Second):
		t.Fatal("pane actor did not stop")
	}
}
