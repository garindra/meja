package server

import (
	"io"
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

	state.grantImmediateOpportunity()
	if !state.canAdmit() {
		t.Fatal("keyboard input did not grant an immediate PTY opportunity")
	}
	state.admit(start.Add(time.Millisecond))
	state.grantImmediateOpportunity()
	if state.canAdmit() {
		t.Fatal("continuous keyboard input granted a second immediate opportunity")
	}

	state.timerFired()
	state.admit(start.Add(time.Millisecond + ptyTurnInterval))
	state.grantImmediateOpportunity()
	if state.canAdmit() {
		t.Fatal("sustained ordinary turn incorrectly rearmed the keyboard bypass")
	}

	state.timerFired()
	idleTurn := state.deadline.Add(ptyTurnInterval)
	state.admit(idleTurn)
	state.grantImmediateOpportunity()
	if !state.canAdmit() {
		t.Fatal("an idle period did not rearm the keyboard opportunity")
	}
}

func TestPTYAttachAndResizeOpportunitiesBypassCurrentDeadline(t *testing.T) {
	now := time.Unix(1, 0)
	attach := newPTYAdmissionState(ptyTurnInterval)
	attach.admit(now)
	attach.grantImmediateOpportunity()
	if !attach.canAdmit() {
		t.Fatal("attach opportunity did not bypass the current deadline")
	}

	resize := newPTYAdmissionState(ptyTurnInterval)
	resize.admit(now)
	resize.grantImmediateOpportunity()
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
	if err := attachTestOutput(pane, lease); err != nil {
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
	shutdown := startTestPaneLoop(pane)
	for _, data := range []string{"a", "b", "c", "d"} {
		sendTestPTYOutput(t, pane, data)
	}
	if got := timers.Load(); got != 1 {
		t.Fatalf("sustained PTY cadence created %d timers, want one reusable timer", got)
	}

	close(shutdown)
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
	shutdown := startTestPaneLoop(pane, "a")
	waitTestPTYBytes(t, pane, 1)

	wantBytes := queueTestPTYOutput(t, pane, "b")
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
	for pane.renderMetricsSnapshot().PTYBytes < wantBytes {
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

	close(shutdown)
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
	shutdown := startTestPaneLoop(pane, "a")
	waitTestPTYBytes(t, pane, 1)

	wantBytes := queueTestPTYOutput(t, pane, "b")
	lease := testOutputLease(0, io.Discard)
	lease.done = pane.done
	if err := pane.installOutputLease(lease, 1, 8, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for pane.renderMetricsSnapshot().PTYBytes < wantBytes {
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

	close(shutdown)
	select {
	case <-pane.mainDone:
	case <-time.After(time.Second):
		t.Fatal("pane actor did not stop")
	}
}
