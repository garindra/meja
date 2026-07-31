package server

import (
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type scriptedPTYReader struct {
	chunks     [][]byte
	chunk      int
	offset     int
	reads      atomic.Int32
	readyCalls atomic.Int32
}

type gatedPTYReader struct {
	data []byte
	gate <-chan struct{}
	read bool
}

type eagainPTYReader struct {
	data  []byte
	reads atomic.Int32
}

type interruptedPTYReader struct {
	reads atomic.Int32
}

func (r *interruptedPTYReader) ptyReadReady() (bool, error) {
	return true, nil
}

func (r *interruptedPTYReader) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, unix.EINTR
}

type queuedPTYReader struct {
	mu     sync.Mutex
	chunks [][]byte
	chunk  int
	offset int
	reads  atomic.Int32
}

func (r *queuedPTYReader) enqueue(chunks ...[]byte) {
	r.mu.Lock()
	r.chunks = append(r.chunks, chunks...)
	r.mu.Unlock()
}

func (r *queuedPTYReader) ptyReadReady() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chunk < len(r.chunks), nil
}

func (r *queuedPTYReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads.Add(1)
	source := r.chunks[r.chunk][r.offset:]
	n := copy(buffer, source)
	r.offset += n
	if r.offset == len(r.chunks[r.chunk]) {
		r.chunk++
		r.offset = 0
	}
	return n, nil
}

func (r *eagainPTYReader) ptyReadReady() (bool, error) {
	return true, nil
}

func (r *eagainPTYReader) Read(buffer []byte) (int, error) {
	r.reads.Add(1)
	if len(r.data) == 0 {
		return 0, unix.EAGAIN
	}
	n := copy(buffer, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *gatedPTYReader) ptyReadReady() (bool, error) {
	if !r.read {
		return true, nil
	}
	<-r.gate
	return false, nil
}

func (r *gatedPTYReader) Read(buffer []byte) (int, error) {
	r.read = true
	return copy(buffer, r.data), nil
}

func (r *scriptedPTYReader) ptyReadReady() (bool, error) {
	r.readyCalls.Add(1)
	return r.chunk < len(r.chunks), nil
}

func (r *scriptedPTYReader) Read(buffer []byte) (int, error) {
	r.reads.Add(1)
	if r.chunk >= len(r.chunks) {
		return 0, io.EOF
	}
	source := r.chunks[r.chunk][r.offset:]
	n := copy(buffer, source)
	r.offset += n
	if r.offset == len(r.chunks[r.chunk]) {
		r.chunk++
		r.offset = 0
	}
	return n, nil
}

func runTestPTYDrain(t *testing.T, pane *Pane, reader io.Reader, request ptyDrainRequest) ([]byte, ptyDrainEvent) {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		done <- drainPTYOpportunity(pane, reader, request)
	}()
	var output []byte
	for {
		select {
		case event := <-pane.ptyDrainEvents:
			if event.buffer != nil {
				output = append(output, event.buffer...)
				pane.ptyFree <- event.buffer[:ptyReadBufferSize]
				continue
			}
			if event.reason != 0 {
				<-done
				return output, event
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for PTY drain")
		}
	}
}

func TestPTYDrainConsumesAllReadyReadsAndEndsExplicitly(t *testing.T) {
	pane := &Pane{terminal: newTerminal(16, 1)}
	pane.initializeRuntime()
	reader := &scriptedPTYReader{chunks: [][]byte{[]byte("abc"), []byte("def"), []byte("ghi")}}

	output, complete := runTestPTYDrain(t, pane, reader, ptyDrainRequest{})
	if got, want := string(output), "abcdefghi"; got != want {
		t.Fatalf("drained output = %q, want %q", got, want)
	}
	if complete.reason != ptyDrainStoppedEmpty || complete.reads != 3 || len(output) != 9 {
		t.Fatalf("completion = %#v, want 3 reads/9 bytes ending empty", complete)
	}
	if got := reader.reads.Load(); got != 3 {
		t.Fatalf("reads = %d, want 3", got)
	}
	before := reader.reads.Load()
	time.Sleep(time.Millisecond)
	if got := reader.reads.Load(); got != before {
		t.Fatalf("reader progressed without another credit: before=%d after=%d", before, got)
	}
}

func TestPTYDrainEAGAINEndsWithoutPollingOrSpinning(t *testing.T) {
	pane := &Pane{terminal: newTerminal(16, 1)}
	pane.initializeRuntime()
	reader := &eagainPTYReader{data: []byte("ready")}

	output, complete := runTestPTYDrain(t, pane, reader, ptyDrainRequest{})
	if got, want := string(output), "ready"; got != want {
		t.Fatalf("drained output = %q, want %q", got, want)
	}
	if complete.reason != ptyDrainStoppedEmpty || complete.reads != 2 || len(output) != 5 {
		t.Fatalf("completion = %#v, want data followed by EAGAIN", complete)
	}
	before := reader.reads.Load()
	time.Sleep(time.Millisecond)
	if got := reader.reads.Load(); got != before {
		t.Fatalf("reader spun after EAGAIN: before=%d after=%d", before, got)
	}
	_, next := runTestPTYDrain(t, pane, reader, ptyDrainRequest{})
	if next.reason != ptyDrainStoppedEmpty || reader.reads.Load() != before+1 {
		t.Fatalf("next credit completion=%#v reads=%d, want one new EAGAIN read", next, reader.reads.Load())
	}
}

func TestPTYDrainInterruptedReadEndsOpportunityWithoutTerminatingReader(t *testing.T) {
	pane := &Pane{terminal: newTerminal(16, 1)}
	pane.initializeRuntime()
	reader := &interruptedPTYReader{}

	_, first := runTestPTYDrain(t, pane, reader, ptyDrainRequest{})
	if first.reason != ptyDrainStoppedEmpty {
		t.Fatalf("interrupted read completion = %#v, want temporary empty", first)
	}
	_, second := runTestPTYDrain(t, pane, reader, ptyDrainRequest{})
	if second.reason != ptyDrainStoppedEmpty {
		t.Fatalf("second interrupted completion = %#v, want temporary empty", second)
	}
	if reader.reads.Load() != 2 {
		t.Fatalf("reads after two interrupted opportunities = %d, want 2", reader.reads.Load())
	}
}

func TestPTYReadinessInterruptionIsTemporary(t *testing.T) {
	ready, err := ptyPollReadinessResult(0, unix.EINTR)
	if err != nil || ready {
		t.Fatalf("interrupted poll = ready %t, err %v; want temporary empty", ready, err)
	}
}

func TestPTYReadinessPollSteadyStateDoesNotAllocate(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	run := func() {
		ready, err := ptyReadImmediatelyAvailable(reader)
		if err != nil || ready {
			panic("empty pipe unexpectedly became ready")
		}
	}
	run()
	if allocations := testing.AllocsPerRun(1000, run); allocations != 0 {
		t.Fatalf("PTY readiness poll allocated %.2f objects, want 0", allocations)
	}
}

func TestPTYDrainByteBudgetLeavesDataForNextOpportunity(t *testing.T) {
	pane := &Pane{terminal: newTerminal(16, 1)}
	pane.initializeRuntime()
	reader := &scriptedPTYReader{chunks: [][]byte{
		bytes.Repeat([]byte{'a'}, ptyReadBufferSize),
		bytes.Repeat([]byte{'b'}, ptyReadBufferSize),
		bytes.Repeat([]byte{'c'}, ptyReadBufferSize),
	}}
	request := ptyDrainRequest{maxBytes: ptyDrainByteBudget, maxDuration: time.Hour}

	first, complete := runTestPTYDrain(t, pane, reader, request)
	if len(first) != ptyDrainByteBudget || complete.reason != ptyDrainStoppedByteBudget || complete.reads != 2 {
		t.Fatalf("first drain bytes=%d completion=%#v", len(first), complete)
	}
	second, complete := runTestPTYDrain(t, pane, reader, request)
	if len(second) != ptyReadBufferSize || complete.reason != ptyDrainStoppedEmpty || complete.reads != 1 {
		t.Fatalf("second drain bytes=%d completion=%#v", len(second), complete)
	}
	if second[0] != 'c' {
		t.Fatal("byte-budget drain dropped or duplicated remaining data")
	}
}

func TestPTYDrainTimeBudgetUsesInjectableClock(t *testing.T) {
	pane := &Pane{terminal: newTerminal(16, 1)}
	pane.initializeRuntime()
	var ticks atomic.Int32
	base := time.Unix(1, 0)
	pane.ptyDrainNow = func() time.Time {
		return base.Add(time.Duration(ticks.Add(1)-1) * 3 * time.Millisecond)
	}
	reader := &scriptedPTYReader{chunks: [][]byte{[]byte("a"), []byte("b")}}

	output, complete := runTestPTYDrain(t, pane, reader, ptyDrainRequest{
		maxBytes: ptyDrainByteBudget, maxDuration: 2 * time.Millisecond,
	})
	if got := string(output); got != "a" {
		t.Fatalf("time-bounded output = %q, want one chunk", got)
	}
	if complete.reason != ptyDrainStoppedTimeBudget || complete.reads != 1 {
		t.Fatalf("completion = %#v, want time budget after one read", complete)
	}
}

func TestPTYImmediateOpportunityIsOneBoundedMultiReadDrain(t *testing.T) {
	pane := &Pane{
		terminal:          newTerminal(8, 1),
		ptyPacingInterval: time.Hour,
	}
	pane.initializeRuntime()
	go pane.run()
	reader := &queuedPTYReader{}
	relayDone := make(chan struct{})
	go func() {
		relayPTYOutputFrom(pane, reader)
		close(relayDone)
	}()
	deadline := time.Now().Add(time.Second)
	for pane.renderMetricsSnapshot().PTYDrainsCompleted == 0 {
		if time.Now().After(deadline) {
			t.Fatal("initial empty drain did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	before := pane.renderMetricsSnapshot()
	reader.enqueue([]byte("a"), []byte("b"), []byte("c"))
	if err := pane.sendInput([]byte("key")); err != nil {
		t.Fatal(err)
	}
	for pane.renderMetricsSnapshot().PTYDrainsCompleted == before.PTYDrainsCompleted {
		if time.Now().After(deadline) {
			t.Fatal("accepted input did not grant an immediate drain")
		}
		time.Sleep(time.Millisecond)
	}
	after := pane.renderMetricsSnapshot()
	if got := after.PTYDrainReads - before.PTYDrainReads; got != 3 {
		t.Fatalf("immediate drain reads = %d, want 3", got)
	}
	captured, err := pane.capturePane(capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(captured, []byte("abc")) {
		t.Fatalf("capture after immediate drain = %q, want all ready chunks", captured)
	}

	reader.enqueue([]byte("d"))
	if err := pane.sendInput([]byte("more")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := reader.reads.Load(); got != 3 {
		t.Fatalf("repeated input bypassed sustained cadence: reads=%d, want 3", got)
	}
	pane.stop()
	<-pane.mainDone
	<-relayDone
}

func TestPTYDrainHandoffsDoNotAllocate(t *testing.T) {
	for name, chunks := range map[string][][]byte{
		"empty": nil,
		"one chunk": {
			[]byte("a"),
		},
		"multiple chunks": {
			[]byte("a"),
			[]byte("b"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			pane := &Pane{terminal: newTerminal(8, 1)}
			pane.initializeRuntime()
			pane.ptyDrainEvents = make(chan ptyDrainEvent, len(chunks)+1)
			reader := &scriptedPTYReader{chunks: chunks}
			run := func() {
				reader.chunk, reader.offset = 0, 0
				if !drainPTYOpportunity(pane, reader, ptyDrainRequest{
					maxBytes: ptyDrainByteBudget, maxDuration: time.Hour,
				}) {
					panic("drain stopped unexpectedly")
				}
				for range len(chunks) + 1 {
					event := <-pane.ptyDrainEvents
					if event.buffer != nil {
						pane.ptyFree <- event.buffer[:ptyReadBufferSize]
					}
				}
			}
			run()
			if allocations := testing.AllocsPerRun(1000, run); allocations != 0 {
				t.Fatalf("PTY drain handoff allocated %.2f objects, want 0", allocations)
			}
		})
	}
}

func TestPTYDrainShutdownWhileWaitingForPaneReturnsHeldBuffer(t *testing.T) {
	pane := &Pane{terminal: newTerminal(8, 1)}
	pane.initializeRuntime()
	reader := &scriptedPTYReader{chunks: [][]byte{[]byte("a"), []byte("b")}}
	exited := make(chan struct{})
	go func() {
		relayPTYOutputFrom(pane, reader)
		close(exited)
	}()
	go func() {
		pane.ptyDrainRequests <- ptyDrainRequest{}
	}()
	first := <-pane.ptyDrainEvents
	if first.buffer == nil {
		t.Fatalf("first event = %#v, want chunk", first)
	}
	pane.stop()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("PTY reader did not stop while waiting for pane handoff")
	}
	pane.ptyFree <- first.buffer[:ptyReadBufferSize]
	if got := len(pane.ptyFree); got != ptyReadBufferCount {
		t.Fatalf("buffer ownership after shutdown = %d, want %d", got, ptyReadBufferCount)
	}
	metrics := pane.renderMetricsSnapshot()
	if metrics.PTYDrainStoppedCancelled != 1 || metrics.PTYDrainsCompleted != 1 {
		t.Fatalf("cancelled drain metrics = %#v", metrics)
	}
}

func TestPTYDrainRoutesTerminalReplyBeforeVisualCompletion(t *testing.T) {
	replyReader, replyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer replyReader.Close()
	pane := &Pane{ID: 1, PTY: replyWriter, terminal: newTerminal(8, 1)}
	pane.initializeRuntime()
	go pane.run()
	writerFailed := make(chan error, 1)
	go runPTYWriter(pane, func(err error) { writerFailed <- err })
	lease := testOutputLease(0, io.Discard)
	lease.done = pane.done
	if err := pane.installOutputLease(lease, 1, 8, 1); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	before := pane.renderMetricsSnapshot()

	gate := make(chan struct{})
	reader := &gatedPTYReader{data: []byte("x\x1b[6n"), gate: gate}
	relayDone := make(chan struct{})
	go func() {
		relayPTYOutputFrom(pane, reader)
		close(relayDone)
	}()
	reply := make([]byte, len("\x1b[1;2R"))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(replyReader, reply)
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
		t.Fatal("terminal reply waited for PTY drain completion")
	}
	if got, want := string(reply), "\x1b[1;2R"; got != want {
		t.Fatalf("terminal reply = %q, want %q", got, want)
	}
	during := pane.renderMetricsSnapshot()
	if during.PTYDrainsCompleted != before.PTYDrainsCompleted ||
		during.PTYDrainPublications != before.PTYDrainPublications {
		t.Fatalf("visual work escaped active drain: before=%#v during=%#v", before, during)
	}

	close(gate)
	deadline := time.Now().Add(time.Second)
	for pane.renderMetricsSnapshot().PTYDrainsCompleted == before.PTYDrainsCompleted {
		if time.Now().After(deadline) {
			t.Fatal("PTY drain did not complete after readiness became empty")
		}
		time.Sleep(time.Millisecond)
	}
	syncPaneRenderer(t, pane)
	after := pane.renderMetricsSnapshot()
	if after.PTYDrainPublications-before.PTYDrainPublications != 1 ||
		after.PTYDrainPresents-before.PTYDrainPresents != 1 {
		t.Fatalf("completed drain metrics = %#v", after)
	}

	pane.stop()
	<-pane.mainDone
	<-pane.writerDone
	<-relayDone
}

func TestPTYDrainSplitRedrawProducesOnePublicationAndPresent(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(9, 1)}
	pane.initializeRuntime()
	go pane.run()
	lease := testOutputLease(0, io.Discard)
	lease.done = pane.done
	if err := pane.installOutputLease(lease, 1, 9, 1); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	before := pane.renderMetricsSnapshot()

	reader := &scriptedPTYReader{chunks: [][]byte{
		[]byte("\x1b[2J\x1b[HABC"),
		[]byte("DEF"),
		[]byte("GHI"),
	}}
	relayDone := make(chan struct{})
	go func() {
		relayPTYOutputFrom(pane, reader)
		close(relayDone)
	}()
	deadline := time.Now().Add(time.Second)
	for pane.renderMetricsSnapshot().PTYDrainsCompleted == before.PTYDrainsCompleted {
		if time.Now().After(deadline) {
			t.Fatal("bounded PTY drain did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	syncPaneRenderer(t, pane)

	after := pane.renderMetricsSnapshot()
	if got := after.PTYDrainReads - before.PTYDrainReads; got != 3 {
		t.Fatalf("reads in drain = %d, want 3", got)
	}
	if got := after.PTYDrainPublications - before.PTYDrainPublications; got != 1 {
		t.Fatalf("publications for split redraw = %d, want 1", got)
	}
	if got := after.PTYDrainPresents - before.PTYDrainPresents; got != 1 {
		t.Fatalf("PRESENTs for split redraw = %d, want 1", got)
	}
	captured, err := pane.capturePane(capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(captured, []byte("ABCDEFGHI")) {
		t.Fatalf("captured split redraw = %q, want complete logical redraw", captured)
	}

	pane.stop()
	select {
	case <-pane.mainDone:
	case <-time.After(time.Second):
		t.Fatal("pane actor did not stop")
	}
	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("PTY reader did not stop")
	}
}
