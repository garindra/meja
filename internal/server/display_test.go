package server

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

func TestShiftDirtyRowsWithinRegionPreservesOutsideDamage(t *testing.T) {
	spans := []DirtySpan{
		{Start: 0, End: 1},
		{Start: 1, End: 2},
		{Start: 2, End: 3},
		{Start: 3, End: 4},
		{Start: 4, End: 5},
	}
	if dropped := shiftDirtyRows(spans, 1, 4, -1); dropped != 1 {
		t.Fatalf("dropped dirty rows = %d, want 1", dropped)
	}
	want := []DirtySpan{
		{Start: 0, End: 1},
		{Start: 2, End: 3},
		{Start: 3, End: 4},
		{},
		{Start: 4, End: 5},
	}
	for row := range want {
		if spans[row] != want[row] {
			t.Fatalf("row %d damage = %#v, want %#v", row, spans[row], want[row])
		}
	}
}

func TestPaneRenderStateCoalescesOnlyCompatibleRegions(t *testing.T) {
	pane := &Pane{terminal: newTerminal(4, 5)}
	state := newPanePublicationState(pane)
	state.lease = &OutputLease{}
	state.ensureGeometry()

	first := Update{DirtySpans: make([]DirtySpan, 5), ScrollRegion: &ScrollRegion{Top: 1, Bottom: 4, Delta: -1}}
	second := Update{DirtySpans: make([]DirtySpan, 5), ScrollRegion: &ScrollRegion{Top: 1, Bottom: 4, Delta: -1}}
	state.merge(first)
	state.merge(second)
	if state.scroll == nil || *state.scroll != (ScrollRegion{Top: 1, Bottom: 4, Delta: -2}) {
		t.Fatalf("coalesced render region = %#v", state.scroll)
	}

	state.merge(Update{DirtySpans: make([]DirtySpan, 5), ScrollRegion: &ScrollRegion{Top: 0, Bottom: 4, Delta: -1}})
	if state.scroll != nil || state.dirtyRows != 5 {
		t.Fatalf("incompatible region did not force full redraw: region=%#v dirty=%#v", state.scroll, state.dirty)
	}
}

func TestPaneRenderStatePromotesFullRegionDisplacementToFullRedraw(t *testing.T) {
	pane := &Pane{terminal: newTerminal(4, 5)}
	state := newPanePublicationState(pane)
	state.lease = &OutputLease{}
	state.ensureGeometry()

	update := Update{DirtySpans: make([]DirtySpan, 5), ScrollRegion: &ScrollRegion{Top: 1, Bottom: 4, Delta: -2}}
	state.merge(update)
	update.ScrollRegion = &ScrollRegion{Top: 1, Bottom: 4, Delta: -1}
	state.merge(update)
	if state.scroll != nil || state.dirtyRows != 5 {
		t.Fatalf("full-region displacement did not force full redraw: %#v", state)
	}
}

type renderBatchWriter struct {
	mu      sync.Mutex
	batches [][]byte
}

type synchronizedBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	data bytes.Buffer
}

func (b *synchronizedBuffer) init() {
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}
}

func (b *synchronizedBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	for b.data.Len() == 0 {
		b.cond.Wait()
	}
	return b.data.Read(p)
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	n, err := b.data.Write(p)
	b.cond.Broadcast()
	return n, err
}

func (b *synchronizedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Len()
}

func (b *synchronizedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data.Bytes()...)
}

func (w *renderBatchWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.batches = append(w.batches, append([]byte(nil), data...))
	return len(data), nil
}

func (w *renderBatchWriter) takeBatches() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	batches := w.batches
	w.batches = nil
	return batches
}

func (w *renderBatchWriter) snapshotBatches() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.batches...)
}

func TestConfirmerPresentsLargeRedrawAtomicallyAcrossBoundedWrites(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(int(protocol.MaxGridCols), 64)}
	for row := 0; row < pane.terminal.Rows; row++ {
		cells := pane.terminal.gridRow(row)
		for column := 0; column < pane.terminal.Cols; column++ {
			pane.terminal.replaceTextCell(cells, column, string(rune('a'+column%26)), 1, 0)
		}
	}
	shutdown := startTestPaneLoop(pane)
	defer close(shutdown)

	wire := &renderBatchWriter{}
	if err := pane.installOutputLease(testOutputLease(0, wire), 1, uint16(pane.terminal.Cols), uint16(pane.terminal.Rows)); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	batches := wire.snapshotBatches()
	if len(batches) < 2 {
		t.Fatalf("large redraw used %d physical write, want bounded streaming", len(batches))
	}
	joined := bytes.Join(batches, nil)
	commands := decodePendingCommands(t, joined)
	if got := countOpcode(commandOpcodes(commands), protocol.DisplayOpcodePresent); got != 1 {
		t.Fatalf("PRESENT commands = %d, want exactly one semantic commit", got)
	}
	if commands[len(commands)-1].Opcode != protocol.DisplayOpcodePresent {
		t.Fatalf("final command = %#v, want PRESENT", commands[len(commands)-1])
	}
	renderedCells := 0
	for index, batch := range batches {
		if len(batch) > renderStreamChunkSize {
			t.Fatalf("physical write %d has %d bytes, limit %d", index, len(batch), renderStreamChunkSize)
		}
	}
	for _, command := range commands {
		switch command.Opcode {
		case protocol.DisplayOpcodeWriteText, protocol.DisplayOpcodeWriteTextUTF8, protocol.DisplayOpcodeWriteTextUTF8Default:
			width := int(command.Width)
			if command.Opcode != protocol.DisplayOpcodeWriteText {
				width = 1
			}
			renderedCells += utf8.RuneCount(command.Text) * width
		case protocol.DisplayOpcodeWriteCluster:
			renderedCells += int(command.Width)
		case protocol.DisplayOpcodeFill:
			renderedCells += command.Fill.Columns
		}
	}
	if want := pane.terminal.Cols * pane.terminal.Rows; renderedCells != want {
		t.Fatalf("rendered %d cells across batches, want %d", renderedCells, want)
	}
}

func testOutputLease(slot int, stream io.Writer) *OutputLease {
	return &OutputLease{Slot: slot, Stream: stream}
}

func attachTestOutput(pane *Pane, lease *OutputLease) error {
	return pane.installOutputLease(lease, 0, uint16(pane.terminal.Cols), uint16(pane.terminal.Rows))
}

func republishTestPane(pane *Pane) error {
	if pane.commands == nil {
		return nil
	}
	if err := pane.sendRenderCommand(paneCommand{republish: true}); err != nil {
		return err
	}
	return pane.syncOutput()
}

func resizeTestSessionWindow(state *SessionState, windowID uint64, cols, rows uint16) error {
	if err := resizeSessionWindowModelNow(state, windowID, cols, rows); err != nil {
		return err
	}
	window := state.Windows[windowID]
	placements := visibleWindowPlacementsForSession(state, window, Rect{Width: int(cols), Height: int(rows)})
	for _, placement := range placements {
		if pane := state.Panes[placement.PaneID]; pane != nil && pane.terminal != nil {
			if err := pane.resize(uint16(placement.Rect.Width), uint16(placement.Rect.Height)); err != nil {
				return err
			}
		}
	}
	return nil
}

func testClientInstance(frames chan protocol.Frame, leases map[int]*OutputLease) *ClientInstance {
	connection := newClientInstance(nil, nil)
	controlOut := make(chan protocol.Frame, 256)
	connection.controlOut = controlOut
	if frames != nil {
		go func() {
			for frame := range controlOut {
				if frame.Type != protocol.MsgClientStatus {
					frames <- frame
				}
			}
		}()
	}
	for slot, lease := range leases {
		connection.Output[slot] = lease
	}
	return connection
}

func attachDisplayTestClient(t *testing.T, s *SessionState, client *ClientInstance) {
	t.Helper()
	setLeasedTestClient(t, s, client, 9)
}

func (c *ClientInstance) applyCurrentTestViewWithHandoff(handoff *outputHandoff) error {
	var transition ViewTransition
	c.Daemon.call(func() {
		transition = c.Daemon.prepareViewTransitionNow(viewTransitionLayout, testClientIdentity(c), c.Daemon.sessions[c.sessionID])
	})
	if handoff == nil {
		return c.applyViewTransition(transition)
	}
	if err := c.validateProjectionPlan(transition.Projection); err != nil {
		return err
	}
	return c.installClientView(transition, handoff)
}

func (c *ClientInstance) finishTestHandoff(handoff *outputHandoff, placements []protocol.PanePlacement) error {
	plan := ClientProjectionPlan{SessionID: c.sessionID}
	plan.View.Layout.WindowID = c.currentView.Layout.WindowID
	plan.View.Layout.LayoutRevision = c.currentView.Layout.LayoutRevision
	for _, placement := range placements {
		var pane *Pane
		c.Daemon.call(func() { pane = c.Daemon.panes[placement.PaneID] })
		if pane == nil {
			continue
		}
		cols, rows := pane.TerminalSize()
		placement.Rect = protocol.Rect{Width: cols, Height: rows}
		plan.View.Panes = append(plan.View.Panes, ClientPanePlacement{Placement: placement, Pane: pane})
	}
	return c.finishOutputHandoff(handoff, plan)
}

func TestBindingSnapshotQueuesBarrierAndPresentTogether(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(8, 3)
	pane := &Pane{ID: testAddPaneID(session), terminal: newTerminal(8, 3)}
	createTestWindow(session, pane)
	var wire bytes.Buffer
	state := session
	attachDisplayTestClient(t, state, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, &wire)}))
	if err := clientForState(session).applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, wire.Bytes())
	if len(commands) < 2 || commands[0].Opcode != protocol.DisplayOpcodeStartRender || commands[len(commands)-1].Opcode != protocol.DisplayOpcodePresent {
		t.Fatalf("commands=%#v", commands)
	}
	if commands[0].GridCols != 8 || commands[0].GridRows != 3 {
		t.Fatalf("barrier grid = %dx%d", commands[0].GridCols, commands[0].GridRows)
	}
}

func TestPaneRendererOwnsAndSwapsOutputStream(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	output := startTestPaneLoop(pane)
	defer close(output)

	var first, second bytes.Buffer
	firstLease := testOutputLease(0, &first)
	if err := attachTestOutput(pane, firstLease); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if pane.outputLease != firstLease {
		t.Fatal("pane does not own the attached stream")
	}
	if first.Len() == 0 {
		t.Fatal("first attached stream received no refresh")
	}
	released := make(chan *OutputLease, 1)
	pane.releaseOutputStream(released)
	if got := <-released; got != firstLease {
		t.Fatal("pane returned a different output lease")
	}
	if pane.outputLease != nil {
		t.Fatal("pane retained the released stream")
	}
	firstSize := first.Len()
	if err := republishTestPane(pane); err != nil {
		t.Fatal(err)
	}
	if first.Len() != firstSize {
		t.Fatal("detached stream received output")
	}

	if err := attachTestOutput(pane, testOutputLease(0, &second)); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if second.Len() == 0 {
		t.Fatal("replacement stream received no refresh")
	}
}

func TestOldStreamCleanupDoesNotDetachReplacement(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	output := startTestPaneLoop(pane)
	defer close(output)

	var oldStream, replacement bytes.Buffer
	oldLease := testOutputLease(0, &oldStream)
	if err := attachTestOutput(pane, oldLease); err != nil {
		t.Fatal(err)
	}
	if err := attachTestOutput(pane, testOutputLease(0, &replacement)); err != nil {
		t.Fatal(err)
	}
	if err := pane.detachOutputLease(oldLease); err != nil {
		t.Fatal(err)
	}
	if err := republishTestPane(pane); err != nil {
		t.Fatal(err)
	}
	if replacement.Len() == 0 {
		t.Fatal("old stream cleanup detached the replacement stream")
	}
}

func TestInstallOutputLeaseAtomicallyReplacesAttachedGrid(t *testing.T) {
	pane := &Pane{ID: 7, terminal: newTerminal(91, 49)}
	output := startTestPaneLoop(pane)
	defer close(output)

	var oldWire bytes.Buffer
	var replacementWire renderBatchWriter
	oldLease := testOutputLease(0, &oldWire)
	replacementLease := testOutputLease(0, &replacementWire)
	if err := pane.installOutputLease(oldLease, 1, 91, 49); err != nil {
		t.Fatal(err)
	}
	if err := pane.installOutputLease(replacementLease, 2, 103, 49); err != nil {
		t.Fatalf("replace attached output and resize: %v", err)
	}
	if cols, rows := pane.TerminalSize(); cols != 103 || rows != 49 {
		t.Fatalf("replacement grid = %dx%d, want 103x49", cols, rows)
	}

	// Cleanup from the disconnected transport may arrive after replacement.
	if err := pane.detachOutputLease(oldLease); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(replacementWire.snapshotBatches()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(replacementWire.takeBatches()) == 0 {
		t.Fatal("replacement output did not receive its initial snapshot")
	}
	if err := republishTestPane(pane); err != nil {
		t.Fatal(err)
	}
	if len(replacementWire.takeBatches()) == 0 {
		t.Fatal("stale detach removed the replacement output lease")
	}
}

func TestPaneRendererCanAttachReplacementAfterWriteFailure(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	output := startTestPaneLoop(pane)
	defer close(output)

	writeErr := errors.New("stream closed")
	if err := attachTestOutput(pane, testOutputLease(0, errorWriter{err: writeErr})); err != nil {
		t.Fatal(err)
	}
	_ = pane.syncOutput()

	var replacement bytes.Buffer
	if err := attachTestOutput(pane, testOutputLease(0, &replacement)); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if replacement.Len() == 0 {
		t.Fatal("replacement stream received no output")
	}
}

func TestOutputHandoffAttachesEachReleasedSlotImmediately(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(16, 4)
	first := &Pane{ID: testAddPaneID(session), terminal: newTerminal(8, 4)}
	second := &Pane{ID: testAddPaneID(session), terminal: newTerminal(8, 4)}
	createTestWindow(session, first)
	if _, _, err := splitTestFocusedPane(session, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	firstUpdates := startTestPaneLoop(first)
	secondUpdates := startTestPaneLoop(second)
	defer close(firstUpdates)
	defer close(secondUpdates)

	firstWritten := newSignalWriter()
	secondWritten := newSignalWriter()
	attachDisplayTestClient(t, session, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, firstWritten), 1: testOutputLease(1, secondWritten)}))
	placements, _ := testClientLayoutPanes(session)
	handoff := &outputHandoff{
		released: make(chan *OutputLease, 2),
		pending:  map[int]struct{}{0: {}, 1: {}},
	}
	finished := make(chan error, 1)
	go func() { finished <- clientForState(session).finishTestHandoff(handoff, placements) }()

	handoff.released <- testOutputLease(0, firstWritten)
	select {
	case <-firstWritten.written:
	case <-time.After(time.Second):
		t.Fatal("released slot 0 was not attached while slot 1 remained pending")
	}
	select {
	case err := <-finished:
		t.Fatalf("handoff finished before slot 1 release: %v", err)
	default:
	}

	handoff.released <- testOutputLease(1, secondWritten)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff did not finish after every slot was released")
	}
}

func TestOutputHandoffAttachesReplacementAfterNilRelease(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(16, 4)
	pane := &Pane{ID: testAddPaneID(session), terminal: newTerminal(16, 4)}
	createTestWindow(session, pane)
	updates := startTestPaneLoop(pane)
	defer close(updates)

	written := newSignalWriter()
	attachDisplayTestClient(t, session, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, written)}))
	placements, _ := testClientLayoutPanes(session)
	handoff := &outputHandoff{
		released: make(chan *OutputLease, 1),
		pending:  map[int]struct{}{0: {}},
	}
	handoff.released <- nil

	if err := clientForState(session).finishTestHandoff(handoff, placements); err != nil {
		t.Fatal(err)
	}
	select {
	case <-written.written:
	case <-time.After(time.Second):
		t.Fatal("replacement output was not attached after a nil old-lease release")
	}
}

func TestBindingPublicationWaitsForHandoffCompletion(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(8, 3)
	pane := &Pane{ID: testAddPaneID(session), terminal: newTerminal(8, 3)}
	createTestWindow(session, pane)
	frames := make(chan protocol.Frame, 1)
	var paneWire bytes.Buffer
	attachDisplayTestClient(t, session, testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, &paneWire)}))
	handoff := &outputHandoff{
		released: make(chan *OutputLease, 1),
		pending:  map[int]struct{}{0: {}},
	}
	done := make(chan error, 1)
	go func() {
		done <- clientForState(session).applyCurrentTestViewWithHandoff(handoff)
	}()
	select {
	case frame := <-frames:
		t.Fatalf("layout frame %d published before lease release", frame.Type)
	default:
	}

	handoff.released <- testOutputLease(0, &paneWire)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("binding publication did not finish after lease release")
	}
	select {
	case frame := <-frames:
		if frame.Type != protocol.MsgClientLayout {
			t.Fatalf("published frame type = %d, want CLIENT_LAYOUT", frame.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("layout was not published after binding completion")
	}
}

func TestClientResizeDetachesPaneOutputBeforeChangingGrid(t *testing.T) {
	session := NewSessionState(0)
	fixtureClient := newTestClient(session)
	fixtureClient.setTestTerminalSize(20, 6)
	pane, updates := startTestPaneRenderer(testAddPaneID(session), 20, 6)
	defer close(updates)
	createTestWindow(session, pane)

	frames := make(chan protocol.Frame, 4)
	wire := &renderBatchWriter{}
	client := testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, wire)})
	attachDisplayTestClient(t, session, client)
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	<-frames
	syncPaneRenderer(t, pane)
	// The pane actor and output worker are separate owners. Synchronizing only
	// the pane actor does not prove that its initial batch reached the writer.
	// Drain that batch before asserting the replacement transition's first
	// command, or the test can misclassify delayed old-grid paint as new output.
	deadline := time.Now().Add(time.Second)
	for len(wire.snapshotBatches()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_ = wire.takeBatches()

	// Growing is the dangerous half of a rapid narrow/wide cycle: rendering
	// the wider terminal through the still-attached narrow START_RENDER grid
	// produces commands outside the decoder's current grid and disconnects the
	// frontend before it can process input.
	if err := client.resizeClient(80, 20); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)

	var commands []protocol.DisplayCommand
	deadline = time.Now().Add(time.Second)
	for len(wire.snapshotBatches()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for _, batch := range wire.snapshotBatches() {
		commands = append(commands, decodePendingCommands(t, batch)...)
	}
	if len(commands) == 0 {
		t.Fatal("resize emitted no replacement pane snapshot")
	}
	barrier := -1
	for index, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeStartRender && command.GridCols == 80 && command.GridRows == 20 {
			barrier = index
			break
		}
	}
	if barrier < 0 {
		t.Fatalf("resize emitted no replacement START_RENDER for grid 80x20; opcodes=%v", commandOpcodes(commands))
	}
}

func TestReturningToSplitWindowKeepsFirstPaneAttached(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	second, secondUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	firstWindow, _ := createTestWindow(session, first)
	if _, _, err := splitTestFocusedPane(session, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 2)
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(t, session, testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, &slot0), 1: testOutputLease(1, &slot1)}))
	if err := clientForState(session).applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	<-frames
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	handoff := clientForState(session).beginOutputHandoff()
	third, thirdUpdates := startTestPaneRenderer(testAddPaneID(session), 16, 4)
	defer close(thirdUpdates)
	createTestWindow(session, third)
	if err := installTestCurrentProjection(clientForState(session)); err != nil {
		t.Fatal(err)
	}
	if err := clientForState(session).applyCurrentTestViewWithHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, third)

	handoff = clientForState(session).beginOutputHandoff()
	if _, _, err := selectTestSessionWindow(session, firstWindow.ID); err != nil {
		t.Fatal(err)
	}
	if err := clientForState(session).applyCurrentTestViewWithHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	before := slot0.Len()
	if err := republishTestPane(first); err != nil {
		t.Fatal(err)
	}
	if slot0.Len() <= before {
		t.Fatal("first pane had no live output after returning to its split window")
	}
}

func TestSplitCommandPublishesNewPaneAsFocused(t *testing.T) {
	session := NewSessionState(1)
	fixtureClient := newTestClient(session)
	fixtureClient.setTestTerminalSize(80, 24)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(session), 80, 24)
	defer close(firstUpdates)
	createTestWindow(session, first)

	frames := make(chan protocol.Frame, 2)
	var slot0, slot1 bytes.Buffer
	client := testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &slot0),
		1: testOutputLease(1, &slot1),
	})
	client.shell = "/bin/sh"
	attachDisplayTestClient(t, session, client)
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	initial := decodeTestClientLayout(t, <-frames)

	detach, err := client.handleInputBytes(initial.LayoutRevision, []byte{0x02, '"'})
	if err != nil || detach {
		t.Fatalf("prefix split-window detach=%v err=%v", detach, err)
	}
	layout := decodeTestClientLayout(t, <-frames)
	if len(layout.Panes) != 2 {
		t.Fatalf("split layout panes = %#v", layout.Panes)
	}
	if layout.FocusedPaneID == first.ID || layout.FocusedPaneID != layout.Panes[1].PaneID {
		t.Fatalf("published focused pane = %d, panes = %#v; want new pane", layout.FocusedPaneID, layout.Panes)
	}
	if got := client.currentView.Layout.FocusedPaneID; got != layout.FocusedPaneID {
		t.Fatalf("client focused pane = %d, published = %d", got, layout.FocusedPaneID)
	}
	newPane := session.Pane(layout.FocusedPaneID)
	if newPane == nil {
		t.Fatalf("split pane %d is not in canonical graph", layout.FocusedPaneID)
	}
	syncPaneRenderer(t, newPane)
	if newPane.outputLease == nil || newPane.outputLease.Slot != 1 {
		t.Fatalf("split pane output lease = %#v, want slot 1", newPane.outputLease)
	}
	outputBeforeInput := slot1.Len()
	const marker = "MEJA_SPLIT_INPUT_731"
	if detach, err = client.handleInputBytes(layout.LayoutRevision, []byte("printf '"+marker+"\\n'\r")); err != nil || detach {
		t.Fatalf("split pane input detach=%v err=%v", detach, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		captured, captureErr := newPane.capturePane(capturePaneOptions{})
		if captureErr != nil {
			t.Fatal(captureErr)
		}
		if bytes.Contains(captured, []byte(marker)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("split pane never rendered typed marker; capture=%q", captured)
		}
		time.Sleep(10 * time.Millisecond)
	}
	syncPaneRenderer(t, newPane)
	if slot1.Len() <= outputBeforeInput {
		t.Fatalf("split pane render stream did not advance after input: before=%d after=%d", outputBeforeInput, slot1.Len())
	}
	select {
	case got := <-first.ptyInput:
		t.Fatalf("split input was also routed to old pane: %q", got)
	default:
	}

	_ = terminatePane(newPane)
}

func TestPrefixDirectionalFocusPublishesAndRoutesInput(t *testing.T) {
	session := NewSessionState(1)
	fixtureClient := newTestClient(session)
	fixtureClient.setTestTerminalSize(16, 6)
	top, topUpdates := startTestPaneRenderer(testAddPaneID(session), 16, 3)
	bottom, bottomUpdates := startTestPaneRenderer(testAddPaneID(session), 16, 3)
	defer close(topUpdates)
	defer close(bottomUpdates)
	window, _ := createTestWindow(session, top)
	if _, _, err := splitTestFocusedPane(session, bottom, SplitHorizontal); err != nil {
		t.Fatal(err)
	}

	frames := make(chan protocol.Frame, 2)
	var slot0, slot1 bytes.Buffer
	client := testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &slot0),
		1: testOutputLease(1, &slot1),
	})
	attachDisplayTestClient(t, session, client)
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	initial := decodeTestClientLayout(t, <-frames)
	if initial.FocusedPaneID != bottom.ID {
		t.Fatalf("initial focus = %d, want bottom pane %d", initial.FocusedPaneID, bottom.ID)
	}

	detach, err := client.handleInputBytes(initial.LayoutRevision, []byte{0x02, 0x1b, '[', 'A'})
	if err != nil || detach {
		t.Fatalf("prefix focus-up detach=%v err=%v", detach, err)
	}
	published := decodeTestClientLayout(t, <-frames)
	if published.WindowID != window.ID || published.FocusedPaneID != top.ID {
		t.Fatalf("focus-up projection = %#v; want window %d pane %d", published, window.ID, top.ID)
	}
	if published.LayoutRevision != initial.LayoutRevision {
		t.Fatalf("focus-only layout revision = %d, want existing rendered revision %d", published.LayoutRevision, initial.LayoutRevision)
	}
	if got := client.currentView.Layout.FocusedPaneID; got != top.ID {
		t.Fatalf("installed focus = %d, want %d", got, top.ID)
	}
	if view := session.WindowViews[window.ID]; view.FocusedPaneID != top.ID {
		t.Fatalf("daemon session-view focus = %d, want %d", view.FocusedPaneID, top.ID)
	}
	if detach, err = client.handleInputBytes(published.LayoutRevision, []byte("focused-input")); err != nil || detach {
		t.Fatalf("focused input detach=%v err=%v", detach, err)
	}
	assertOnlyPaneInput(t, top.ptyInput, "focused-input")
	select {
	case got := <-bottom.ptyInput:
		t.Fatalf("input remained on old focused pane: %q", got)
	default:
	}
}

func TestPrefixPaneResizePublishesGeometryAndKeepsInputTarget(t *testing.T) {
	session := NewSessionState(1)
	fixtureClient := newTestClient(session)
	fixtureClient.setTestTerminalSize(20, 6)
	left, leftUpdates := startTestPaneRenderer(testAddPaneID(session), 10, 6)
	right, rightUpdates := startTestPaneRenderer(testAddPaneID(session), 9, 6)
	defer close(leftUpdates)
	defer close(rightUpdates)
	createTestWindow(session, left)
	if _, _, err := splitTestFocusedPane(session, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(session, left.ID); err != nil {
		t.Fatal(err)
	}

	frames := make(chan protocol.Frame, 2)
	var slot0, slot1 bytes.Buffer
	client := testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &slot0),
		1: testOutputLease(1, &slot1),
	})
	attachDisplayTestClient(t, session, client)
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	initial := decodeTestClientLayout(t, <-frames)
	leftBefore, ok := panePlacement(initial, left.ID)
	if !ok {
		t.Fatalf("left pane missing from initial layout: %#v", initial)
	}

	detach, err := client.handleInputBytes(initial.LayoutRevision, []byte{0x02, 0x1b, '[', '1', ';', '5', 'C'})
	if err != nil || detach {
		t.Fatalf("prefix resize-right detach=%v err=%v", detach, err)
	}
	published := decodeTestClientLayout(t, <-frames)
	leftAfter, ok := panePlacement(published, left.ID)
	if !ok || leftAfter.Rect.Width != leftBefore.Rect.Width+1 {
		t.Fatalf("resized layout = %#v; left width before=%d", published, leftBefore.Rect.Width)
	}
	if published.FocusedPaneID != left.ID || client.currentView.Layout.FocusedPaneID != left.ID {
		t.Fatalf("focus moved during resize: layout=%d client=%d want=%d", published.FocusedPaneID, client.currentView.Layout.FocusedPaneID, left.ID)
	}
	leftCols, _ := left.TerminalSize()
	rightCols, _ := right.TerminalSize()
	if leftCols != leftAfter.Rect.Width || rightCols != published.Panes[1].Rect.Width {
		t.Fatalf("terminal widths left=%d right=%d layout=%#v", leftCols, rightCols, published.Panes)
	}
	if detach, err = client.handleInputBytes(published.LayoutRevision, []byte("resized-input")); err != nil || detach {
		t.Fatalf("input after resize detach=%v err=%v", detach, err)
	}
	assertPaneInputStream(t, left.ptyInput, "resized-input")
}

func TestPrefixWindowNavigationMovesTheLiveProjectionAndInputTarget(t *testing.T) {
	session := NewSessionState(1)
	fixtureClient := newTestClient(session)
	fixtureClient.setTestTerminalSize(80, 24)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(session), 80, 24)
	defer close(firstUpdates)
	firstWindow, _ := createTestWindow(session, first)

	frames := make(chan protocol.Frame, 5)
	var output synchronizedBuffer
	client := testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, &output)})
	client.shell = "/bin/sh"
	attachDisplayTestClient(t, session, client)
	client.terminalCols = 80
	client.terminalRows = 24
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	initial := decodeTestClientLayout(t, <-frames)
	if initial.WindowID != firstWindow.ID || initial.FocusedPaneID != first.ID {
		t.Fatalf("initial projection = %#v; want window %d pane %d", initial, firstWindow.ID, first.ID)
	}

	detach, err := client.handleInputBytes(initial.LayoutRevision, []byte{0x02, 'c'})
	if err != nil || detach {
		t.Fatalf("prefix new-window detach=%v err=%v", detach, err)
	}
	created := decodeTestClientLayout(t, <-frames)
	if created.WindowID == firstWindow.ID || created.FocusedPaneID == first.ID {
		t.Fatalf("new-window projection = %#v; want a new window and pane", created)
	}
	createdPane := session.Pane(created.FocusedPaneID)
	if createdPane == nil {
		t.Fatalf("new-window pane %d is not in the canonical graph", created.FocusedPaneID)
	}
	syncPaneRenderer(t, createdPane)
	if createdPane.outputLease == nil || createdPane.outputLease.Slot != 0 {
		t.Fatalf("new-window output lease = %#v; want live slot 0", createdPane.outputLease)
	}
	outputBeforeInput := output.Len()
	const marker = "MEJA_NEW_WINDOW_INPUT_417"
	if detach, err = client.handleInputBytes(created.LayoutRevision, []byte("printf '"+marker+"\\n'\r")); err != nil || detach {
		t.Fatalf("new-window input detach=%v err=%v", detach, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		captured, captureErr := createdPane.capturePane(capturePaneOptions{})
		if captureErr != nil {
			t.Fatal(captureErr)
		}
		if bytes.Contains(captured, []byte(marker)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new-window terminal never rendered typed marker; capture=%q", captured)
		}
		time.Sleep(10 * time.Millisecond)
	}
	syncPaneRenderer(t, createdPane)
	if output.Len() <= outputBeforeInput {
		t.Fatalf("new-window render stream did not advance after typed output: before=%d after=%d", outputBeforeInput, output.Len())
	}
	select {
	case got := <-first.ptyInput:
		t.Fatalf("new-window input was also routed to the old pane: %q", got)
	default:
	}
	// Model a window created from a nested CLI before the live viewport was
	// installed: its pane grid is one row shorter while another window is
	// active. Selecting it must reconcile its geometry before publication.
	if err := resizeTestSessionWindow(session, firstWindow.ID, 80, 23); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)

	renderOffset := output.Len()
	detach, err = client.handleInputBytes(created.LayoutRevision, []byte{0x02, 'p'})
	if err != nil || detach {
		t.Fatalf("prefix previous-window detach=%v err=%v", detach, err)
	}
	previous := decodeTestClientLayout(t, <-frames)
	if previous.WindowID != firstWindow.ID || previous.FocusedPaneID != first.ID {
		t.Fatalf("previous-window projection = %#v; want window %d pane %d", previous, firstWindow.ID, first.ID)
	}
	if previous.LayoutRevision <= created.LayoutRevision {
		t.Fatalf("previous-window revision = %d, want newer than %d", previous.LayoutRevision, created.LayoutRevision)
	}
	syncPaneRenderer(t, first)
	firstCols, firstRows := first.TerminalSize()
	if firstCols != previous.Panes[0].Rect.Width || firstRows != previous.Panes[0].Rect.Height {
		t.Fatalf("selected pane grid = %dx%d, layout = %dx%d", firstCols, firstRows, previous.Panes[0].Rect.Width, previous.Panes[0].Rect.Height)
	}
	assertRenderRevision(t, output.Bytes()[renderOffset:], previous.LayoutRevision)

	renderOffset = output.Len()
	detach, err = client.handleInputBytes(previous.LayoutRevision, []byte{0x02, 'n'})
	if err != nil || detach {
		t.Fatalf("prefix next-window detach=%v err=%v", detach, err)
	}
	next := decodeTestClientLayout(t, <-frames)
	if next.WindowID != created.WindowID || next.FocusedPaneID != created.FocusedPaneID {
		t.Fatalf("next-window projection = %#v; want window %d pane %d", next, created.WindowID, created.FocusedPaneID)
	}
	if next.LayoutRevision <= previous.LayoutRevision {
		t.Fatalf("next-window revision = %d, want newer than %d", next.LayoutRevision, previous.LayoutRevision)
	}
	syncPaneRenderer(t, createdPane)
	assertRenderRevision(t, output.Bytes()[renderOffset:], next.LayoutRevision)

	renderOffset = output.Len()
	detach, err = client.handleInputBytes(next.LayoutRevision, []byte{0x02, 'l'})
	if err != nil || detach {
		t.Fatalf("prefix last-window detach=%v err=%v", detach, err)
	}
	returned := decodeTestClientLayout(t, <-frames)
	if returned.WindowID != firstWindow.ID || returned.FocusedPaneID != first.ID {
		t.Fatalf("last-window projection = %#v; want window %d pane %d", returned, firstWindow.ID, first.ID)
	}
	if returned.LayoutRevision <= next.LayoutRevision {
		t.Fatalf("last-window revision = %d, want newer than %d", returned.LayoutRevision, next.LayoutRevision)
	}
	syncPaneRenderer(t, first)
	assertRenderRevision(t, output.Bytes()[renderOffset:], returned.LayoutRevision)
	if current := client.currentView.Layout; current.WindowID != returned.WindowID || current.FocusedPaneID != returned.FocusedPaneID {
		t.Fatalf("installed client layout window=%d pane=%d; published window=%d pane=%d", current.WindowID, current.FocusedPaneID, returned.WindowID, returned.FocusedPaneID)
	}
	if client.ViewLeaseWindowID != firstWindow.ID || client.ViewLeaseGeneration == 0 {
		t.Fatalf("installed lease window=%d generation=%d; want window %d", client.ViewLeaseWindowID, client.ViewLeaseGeneration, firstWindow.ID)
	}
	if panes := client.currentView.Placements(); len(panes) != 1 || panes[0].PaneID != first.ID {
		t.Fatalf("installed client layout panes = %#v; want pane %d", panes, first.ID)
	}
	if detach, err = client.handleInputBytes(returned.LayoutRevision, []byte("input-target")); err != nil || detach {
		t.Fatalf("input after last-window detach=%v err=%v", detach, err)
	}
	assertOnlyPaneInput(t, first.ptyInput, "input-target")

	if pane := session.Pane(created.FocusedPaneID); pane != nil {
		_ = terminatePane(pane)
	}
}

func TestSwapPaneCommandMovesLiveOutputsToRevisedSlots(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	second, secondUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	createTestWindow(session, first)
	if _, _, err := splitTestFocusedPane(session, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 1)
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(t, session, testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &slot0),
		1: testOutputLease(1, &slot1),
	}))
	if err := clientForState(session).applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	<-frames // Initial layout is now published after the initial binding succeeds.
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	if _, err := executeTestClientCommand(clientForState(session), []string{"swap-pane", "-U"}); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)
	if first.outputLease == nil || first.outputLease.Slot != 1 || second.outputLease == nil || second.outputLease.Slot != 0 {
		t.Fatalf("leases after swap: first=%#v second=%#v", first.outputLease, second.outputLease)
	}
	placements, state := testClientLayoutPanes(session)
	if len(placements) != 2 || placements[0].PaneID != second.ID || placements[1].PaneID != first.ID || state.FocusedPaneID != second.ID {
		t.Fatalf("placements after swap=%#v state=%#v", placements, state)
	}

	frame := <-frames
	if frame.Type != protocol.MsgClientLayout {
		t.Fatalf("frame type = %d, want CLIENT_LAYOUT", frame.Type)
	}
	layout, err := protocol.DecodeClientLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].PaneID != second.ID || layout.Panes[0].Slot != 0 || layout.Panes[1].PaneID != first.ID || layout.Panes[1].Slot != 1 {
		t.Fatalf("published layout after swap=%#v", layout)
	}
}

func TestZoomCommandRebindsOnlyFocusedPaneAndRestoresSplit(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	second, secondUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	createTestWindow(session, first)
	if _, _, err := splitTestFocusedPane(session, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(session, first.ID); err != nil {
		t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 2)
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(t, session, testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &slot0),
		1: testOutputLease(1, &slot1),
	}))
	if err := clientForState(session).applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	<-frames // Initial layout is now published after the initial binding succeeds.
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	if _, err := executeTestClientCommand(clientForState(session), []string{"resize-pane", "-Z"}); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)
	placements, state := testClientLayoutPanes(session)
	if len(placements) != 1 || placements[0].PaneID != first.ID || placements[0].Slot != 0 || state.FocusedPaneID != first.ID {
		t.Fatalf("zoomed placements=%#v state=%#v", placements, state)
	}
	if first.outputLease == nil || first.outputLease.Slot != 0 || second.outputLease != nil {
		t.Fatalf("zoomed leases: first=%#v second=%#v", first.outputLease, second.outputLease)
	}
	frame := <-frames
	layout, err := protocol.DecodeClientLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 1 || layout.Panes[0].PaneID != first.ID || layout.Panes[0].Rect != (protocol.Rect{Width: 16, Height: 4}) {
		t.Fatalf("zoomed layout = %#v", layout)
	}

	if _, err := executeTestClientCommand(clientForState(session), []string{"resize-pane", "-Z"}); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)
	placements, _ = testClientLayoutPanes(session)
	if len(placements) != 2 || first.outputLease == nil || first.outputLease.Slot != 0 || second.outputLease == nil || second.outputLease.Slot != 1 {
		t.Fatalf("unzoomed placements=%#v leases: first=%#v second=%#v", placements, first.outputLease, second.outputLease)
	}
	frame = <-frames
	layout, err = protocol.DecodeClientLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Rect.Width != 7 || layout.Panes[1].Rect.Width != 8 {
		t.Fatalf("unzoomed layout = %#v", layout)
	}
}

func TestClosingSplitPaneDoesNotLetDuplicateProcessExitDetachRemainingPane(t *testing.T) {
	session := NewSessionState(0)
	client := newTestClient(session)
	client.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	second, secondUpdates := startTestPaneRenderer(testAddPaneID(session), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	createTestWindow(session, first)
	if _, _, err := splitTestFocusedPane(session, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 2)
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(t, session, testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, &slot0), 1: testOutputLease(1, &slot1)}))
	if err := clientForState(session).applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	<-frames
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	instance := clientForState(session)
	removal, err := session.daemon.removeClientPane(instance.clientID, instance.activePane().ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = terminatePane(removal.Pane)
	if err := instance.applyViewTransition(removal.Transition); err != nil {
		t.Fatal(err)
	}
	layout, err := protocol.DecodeClientLayout((<-frames).Payload)
	if err != nil {
		t.Fatal(err)
	}
	if layout.FocusedPaneID != first.ID || clientForState(session).currentView.Layout.FocusedPaneID != first.ID {
		t.Fatalf("focus after close: layout=%d client=%d, want %d", layout.FocusedPaneID, clientForState(session).currentView.Layout.FocusedPaneID, first.ID)
	}
	syncPaneRenderer(t, first)
	if detach, inputErr := clientForState(session).handleInputBytes(layout.LayoutRevision, []byte("after-close")); inputErr != nil || detach {
		t.Fatalf("input after close detach=%v err=%v", detach, inputErr)
	}
	assertOnlyPaneInput(t, first.ptyInput, "after-close")
	session.daemon.postPaneProcessExit(second.ID)
	session.daemon.call(func() {})
	syncPaneRenderer(t, first)

	before := slot0.Len()
	if err := republishTestPane(first); err != nil {
		t.Fatal(err)
	}
	if slot0.Len() <= before {
		t.Fatal("duplicate process exit detached the remaining pane")
	}
}

func TestDaemonPostedExitOfOnlyPaneDestroysSession(t *testing.T) {
	state := NewSessionState(1)
	client := clientForState(state)
	pane, updates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(updates)
	createTestWindow(state, pane)

	state.daemon.postPaneProcessExit(pane.ID)
	deadline := time.Now().Add(time.Second)
	for testDaemonSession(state.daemon, state.ID) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if testDaemonSession(state.daemon, state.ID) != nil {
		t.Fatal("daemon-posted final pane exit left an empty session registered")
	}
	clientSnapshot := snapshotTestClientActor(client)
	if !clientSnapshot.Ended || state.HasWindows() {
		t.Fatalf("ended=%v windows=%v", clientSnapshot.Ended, state.HasWindows())
	}
}

func TestExitedShellProcessRemovesWindowAndSelectsFallback(t *testing.T) {
	state := NewSessionState(1)
	client := clientForState(state)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(firstUpdates)
	firstWindow, _ := createTestWindow(state, first)

	exitingID := testAddPaneID(state)
	exiting, err := startPaneProcess(exitingID, state.contextualPaneRequest(paneRequest{
		Cwd: t.TempDir(), Command: []string{"/bin/sh", "-c", "exit 0"}, Cols: 16, Rows: 4, Shell: "/bin/sh",
	}))
	if err != nil {
		t.Fatal(err)
	}
	exitingWindow, _ := createTestWindow(state, exiting)
	if exitingWindow == nil {
		_ = terminatePane(exiting)
		t.Fatal("failed to insert exiting shell window")
	}
	previousProjection := snapshotTestClientActor(client).AppliedProjectionRevision
	client.Daemon.startPane(client.sessionID, exiting)

	deadline := time.Now().Add(2 * time.Second)
	var exitingPresent bool
	var activeWindowID uint64
	for {
		state.daemon.call(func() {
			exitingPresent = state.Windows[exitingWindow.ID] != nil
			activeWindowID = state.ActiveWindowID
		})
		clientSnapshot := snapshotTestClientActor(client)
		if (!exitingPresent && clientSnapshot.AppliedProjectionRevision > previousProjection) || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if exitingPresent || activeWindowID != firstWindow.ID {
		t.Fatalf("shell exit left exiting-window=%v active=%d, want fallback %d", exitingPresent, activeWindowID, firstWindow.ID)
	}
	clientSnapshot := snapshotTestClientActor(client)
	if testDaemonSession(state.daemon, state.ID) != state || clientSnapshot.Ended {
		t.Fatal("shell exit destroyed the session despite a surviving fallback window")
	}
	if clientSnapshot.View.Layout.WindowID != firstWindow.ID || clientSnapshot.View.Layout.FocusedPaneID != first.ID {
		t.Fatalf("installed fallback window=%d pane=%d, want window=%d pane=%d", clientSnapshot.View.Layout.WindowID, clientSnapshot.View.Layout.FocusedPaneID, firstWindow.ID, first.ID)
	}
}

func TestPaneExitReclaimsRemovedPaneOutputBeforeBindingFallback(t *testing.T) {
	state := NewSessionState(1)
	base := newTestClient(state)
	base.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(firstUpdates)
	createTestWindow(state, first)

	frames := make(chan protocol.Frame, 4)
	lease := testOutputLease(0, &synchronizedBuffer{})
	client := testClientInstance(frames, map[int]*OutputLease{0: lease})
	attachDisplayTestClient(t, state, client)
	client.terminalCols = 16
	client.terminalRows = 4
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	<-frames

	second, secondUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(secondUpdates)
	handoff := client.beginOutputHandoff()
	secondWindow, plan, err := state.daemon.createClientWindow(client.clientID, second, 16, 4)
	if err != nil {
		t.Fatal(err)
	}
	plan.delivery.cancel()
	if err := client.commitProjectionPlan(plan.Projection); err != nil {
		t.Fatal(err)
	}
	if err := client.applyCurrentTestViewWithHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	<-frames

	// Model the process-exit ordering: graph removal happens before the client
	// actor performs its output handoff, while the removed pane actor can still
	// own the physical output lease.
	state.daemon.postPaneProcessExit(second.ID)
	state.daemon.call(func() {})
	if state.Windows[secondWindow.ID] != nil {
		t.Fatal("pane-exit transaction did not remove the active window")
	}
	select {
	case <-frames:
	case <-time.After(time.Second):
		t.Fatal("pane-exit handoff did not publish fallback layout")
	}

	released := make(chan *OutputLease, 1)
	second.releaseOutputStream(released)
	if got := <-released; got != nil {
		t.Fatalf("removed pane retained output lease after fallback handoff: got %#v", got)
	}
}

func TestSplitPaneExitRelayoutsSurvivorBeforePublishingProjection(t *testing.T) {
	state := NewSessionState(1)
	base := newTestClient(state)
	base.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(state), 8, 4)
	defer close(firstUpdates)
	createTestWindow(state, first)
	second, secondUpdates := startTestPaneRenderer(testAddPaneID(state), 8, 4)
	defer close(secondUpdates)
	if _, _, err := splitTestFocusedPane(state, second, SplitVertical); err != nil {
		t.Fatal(err)
	}

	frames := make(chan protocol.Frame, 4)
	var firstOutput synchronizedBuffer
	client := testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &firstOutput),
		1: testOutputLease(1, &synchronizedBuffer{}),
	})
	attachDisplayTestClient(t, state, client)
	client.terminalCols = 16
	client.terminalRows = 4
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	<-frames

	state.daemon.postPaneProcessExit(second.ID)
	state.daemon.call(func() {})
	var fallback protocol.ClientLayout
	select {
	case frame := <-frames:
		fallback = decodeTestClientLayout(t, frame)
	case <-time.After(time.Second):
		t.Fatal("split-pane exit did not publish replacement layout")
	}
	if len(fallback.Panes) != 1 || fallback.Panes[0].PaneID != first.ID {
		t.Fatalf("fallback layout = %#v, want sole pane %d", fallback, first.ID)
	}
	cols, rows := first.TerminalSize()
	if cols != fallback.Panes[0].Rect.Width || rows != fallback.Panes[0].Rect.Height {
		t.Fatalf("surviving pane grid = %dx%d, replacement layout = %dx%d",
			cols, rows, fallback.Panes[0].Rect.Width, fallback.Panes[0].Rect.Height)
	}
	syncPaneRenderer(t, first)
	firstOutput.mu.Lock()
	outputBytes := append([]byte(nil), firstOutput.data.Bytes()...)
	firstOutput.mu.Unlock()
	commands := decodePendingCommands(t, outputBytes)
	foundBarrier := false
	for _, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeStartRender &&
			command.LayoutRevision == fallback.LayoutRevision &&
			command.GridCols == fallback.Panes[0].Rect.Width &&
			command.GridRows == fallback.Panes[0].Rect.Height {
			foundBarrier = true
			break
		}
	}
	if !foundBarrier {
		t.Fatalf("surviving pane emitted no matching START_RENDER for replacement layout %#v", fallback)
	}
}

func TestLoginShellLogoutRemovesWindowAndSelectsFallback(t *testing.T) {
	state := NewSessionState(1)
	client := clientForState(state)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(firstUpdates)
	firstWindow, _ := createTestWindow(state, first)

	exitingID := testAddPaneID(state)
	exiting, err := startPaneProcess(exitingID, state.contextualPaneRequest(paneRequest{
		Cwd: t.TempDir(), Cols: 16, Rows: 4, Shell: defaultShell(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	exitingWindow, _ := createTestWindow(state, exiting)
	if exitingWindow == nil {
		_ = terminatePane(exiting)
		t.Fatal("failed to insert login-shell window")
	}
	client.Daemon.startPane(client.sessionID, exiting)
	if err := exiting.sendInput([]byte("logout\n")); err != nil {
		t.Fatalf("send logout: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var exitingPresent bool
	var activeWindowID uint64
	for {
		state.daemon.call(func() {
			exitingPresent = state.Windows[exitingWindow.ID] != nil
			activeWindowID = state.ActiveWindowID
		})
		if !exitingPresent || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if exitingPresent || activeWindowID != firstWindow.ID {
		t.Fatalf("logout left exiting-window=%v active=%d, want fallback %d", exitingPresent, activeWindowID, firstWindow.ID)
	}
}

func TestDaemonPostedVisiblePaneExitTransfersPhysicalOutputToFallback(t *testing.T) {
	state := NewSessionState(1)
	base := newTestClient(state)
	base.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(firstUpdates)
	firstWindow, _ := createTestWindow(state, first)
	firstCanonicalRevision := firstWindow.LayoutRevision

	frames := make(chan protocol.Frame, 4)
	var output synchronizedBuffer
	client := testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, &output)})
	attachDisplayTestClient(t, state, client)
	client.terminalCols = 16
	client.terminalRows = 4
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	initial := decodeTestClientLayout(t, <-frames)
	syncPaneRenderer(t, first)
	if initial.WindowID != firstWindow.ID {
		t.Fatalf("initial window = %d, want %d", initial.WindowID, firstWindow.ID)
	}

	exiting, exitingUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(exitingUpdates)
	handoff := client.beginOutputHandoff()
	exitingWindow, plan, err := state.daemon.createClientWindow(client.clientID, exiting, 16, 4)
	if err != nil {
		_ = terminatePane(exiting)
		t.Fatal(err)
	}
	plan.delivery.cancel()
	if err := client.commitProjectionPlan(plan.Projection); err != nil {
		_ = terminatePane(exiting)
		t.Fatal(err)
	}
	if err := client.applyCurrentTestViewWithHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	exitingLayout := decodeTestClientLayout(t, <-frames)
	syncPaneRenderer(t, exiting)
	if exitingLayout.WindowID != exitingWindow.ID {
		t.Fatalf("exiting window = %d, want %d", exitingLayout.WindowID, exitingWindow.ID)
	}

	output.mu.Lock()
	outputOffset := output.data.Len()
	output.mu.Unlock()
	state.daemon.postPaneProcessExit(exiting.ID)
	var fallback protocol.ClientLayout
	select {
	case frame := <-frames:
		fallback = decodeTestClientLayout(t, frame)
	case <-time.After(2 * time.Second):
		t.Fatal("shell exit did not publish a fallback layout")
	}
	if fallback.WindowID != firstWindow.ID || fallback.FocusedPaneID != first.ID {
		t.Fatalf("fallback layout = %#v, want window %d pane %d", fallback, firstWindow.ID, first.ID)
	}
	if fallback.LayoutRevision <= exitingLayout.LayoutRevision {
		t.Fatalf("fallback layout revision = %d, want newer than exited window revision %d", fallback.LayoutRevision, exitingLayout.LayoutRevision)
	}
	if firstWindow.LayoutRevision != firstCanonicalRevision {
		t.Fatalf("forced projection changed canonical fallback revision from %d to %d", firstCanonicalRevision, firstWindow.LayoutRevision)
	}
	syncPaneRenderer(t, first)
	output.mu.Lock()
	outputBytes := append([]byte(nil), output.data.Bytes()...)
	output.mu.Unlock()
	assertRenderRevision(t, outputBytes[outputOffset:], fallback.LayoutRevision)
}

func TestPaneProcessExitResizesFallbackAndTransfersPhysicalOutput(t *testing.T) {
	state := NewSessionState(1)
	base := newTestClient(state)
	base.setTestTerminalSize(16, 4)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(firstUpdates)
	firstWindow, _ := createTestWindow(state, first)

	frames := make(chan protocol.Frame, 4)
	var output synchronizedBuffer
	client := testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, &output)})
	attachDisplayTestClient(t, state, client)
	client.terminalCols = 16
	client.terminalRows = 4
	if err := client.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	initial := decodeTestClientLayout(t, <-frames)
	if initial.WindowID != firstWindow.ID {
		t.Fatalf("initial window = %d, want %d", initial.WindowID, firstWindow.ID)
	}

	exitingID := testAddPaneID(state)
	exiting, err := startPaneProcess(exitingID, state.contextualPaneRequest(paneRequest{
		Cwd: t.TempDir(), Command: []string{"/bin/sleep", "30"}, Cols: 16, Rows: 4, Shell: "/bin/sh",
	}))
	if err != nil {
		t.Fatal(err)
	}
	handoff := client.beginOutputHandoff()
	exitingWindow, plan, err := state.daemon.createClientWindow(client.clientID, exiting, 16, 4)
	if err != nil {
		_ = terminatePane(exiting)
		t.Fatal(err)
	}
	state.daemon.startPane(state.ID, exiting)
	plan.delivery.cancel()
	if err := client.commitProjectionPlan(plan.Projection); err != nil {
		t.Fatal(err)
	}
	if err := client.applyCurrentTestViewWithHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	exitingLayout := decodeTestClientLayout(t, <-frames)
	if exitingLayout.WindowID != exitingWindow.ID {
		t.Fatalf("exiting layout window = %d, want %d", exitingLayout.WindowID, exitingWindow.ID)
	}
	// Inactive windows legitimately retain their previous canonical geometry.
	// Falling back after pane exit must resize the survivor to the live client
	// viewport before publishing it.
	if err := resizeTestSessionWindow(state, firstWindow.ID, 16, 3); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)

	output.mu.Lock()
	outputOffset := output.data.Len()
	output.mu.Unlock()
	previousProjection := snapshotTestClientActor(client).AppliedProjectionRevision
	if err := terminatePane(exiting); err != nil {
		t.Fatalf("terminate exiting pane: %v", err)
	}

	var fallback protocol.ClientLayout
	select {
	case frame := <-frames:
		fallback = decodeTestClientLayout(t, frame)
	case <-time.After(2 * time.Second):
		t.Fatal("interactive shell exit did not publish a fallback layout")
	}
	if fallback.WindowID != firstWindow.ID || fallback.FocusedPaneID != first.ID {
		t.Fatalf("fallback layout = %#v, want window %d pane %d", fallback, firstWindow.ID, first.ID)
	}
	if state.Windows[exitingWindow.ID] != nil || state.Panes[exiting.ID] != nil {
		t.Fatalf("exited shell remained in graph: window=%v pane=%v", state.Windows[exitingWindow.ID] != nil, state.Panes[exiting.ID] != nil)
	}
	deadline := time.Now().Add(2 * time.Second)
	clientSnapshot := snapshotTestClientActor(client)
	for clientSnapshot.AppliedProjectionRevision <= previousProjection && time.Now().Before(deadline) {
		runtime.Gosched()
		clientSnapshot = snapshotTestClientActor(client)
	}
	if clientSnapshot.AppliedProjectionRevision <= previousProjection {
		t.Fatal("fallback projection was published but not installed")
	}
	installed := clientSnapshot.View.Layout
	if installed.WindowID != firstWindow.ID || installed.FocusedPaneID != first.ID {
		t.Fatalf("installed fallback window=%d pane=%d, want window=%d pane=%d",
			installed.WindowID, installed.FocusedPaneID, firstWindow.ID, first.ID)
	}
	syncPaneRenderer(t, first)
	firstCols, firstRows := first.TerminalSize()
	if firstCols != fallback.Panes[0].Rect.Width || firstRows != fallback.Panes[0].Rect.Height {
		t.Fatalf("fallback pane grid = %dx%d, layout = %dx%d", firstCols, firstRows, fallback.Panes[0].Rect.Width, fallback.Panes[0].Rect.Height)
	}
	output.mu.Lock()
	outputBytes := append([]byte(nil), output.data.Bytes()...)
	output.mu.Unlock()
	commands := decodePendingCommands(t, outputBytes[outputOffset:])
	foundFallbackRender := false
	for _, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeStartRender && command.LayoutRevision == fallback.LayoutRevision {
			foundFallbackRender = true
			break
		}
	}
	if !foundFallbackRender {
		t.Fatalf("render stream contains no START_RENDER for fallback revision %d: %#v", fallback.LayoutRevision, commands)
	}
}

func TestMissingPrefixWindowShowsStatusWithoutDetaching(t *testing.T) {
	state := NewSessionState(1)
	fixtureClient := newTestClient(state)
	fixtureClient.setTestTerminalSize(16, 4)
	createTestWindow(state, &Pane{ID: testAddPaneID(state), terminal: newTerminal(16, 4)})
	client := clientForState(state)
	detach, err := client.handleInputBytes(client.currentView.Layout.LayoutRevision, []byte{0x02, '2'})
	if err != nil || detach {
		t.Fatalf("missing prefix window detach=%v err=%v", detach, err)
	}
	message := client.statusMessage
	if message != "unknown window 2" {
		t.Fatalf("status message = %q, want unknown window 2", message)
	}
}

func TestDaemonPostedPaneExitCannotDetachNewerFallbackProjection(t *testing.T) {
	state := NewSessionState(1)
	client := clientForState(state)
	state.daemon.windowLeases = make(map[uint64]*WindowViewLease)
	first, firstUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	second, secondUpdates := startTestPaneRenderer(testAddPaneID(state), 16, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	firstWindow, _ := createTestWindow(state, first)
	secondWindow, _ := createTestWindow(state, second)
	if firstWindow == nil || secondWindow == nil {
		t.Fatal("failed to create pane-exit test windows")
	}

	lease := testOutputLease(0, &bytes.Buffer{})
	if err := attachTestOutput(first, lease); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	client.Output[0] = lease
	firstPlacement := protocol.PanePlacement{PaneID: first.ID, Slot: 0}
	client.currentView.Layout.Panes = []protocol.PanePlacement{firstPlacement}
	client.currentView.Panes = []ClientPanePlacement{{Pane: first, Placement: firstPlacement}}

	state.daemon.postPaneProcessExit(second.ID)
	// Barrier: the graph removal has completed. Client delivery starts only
	// after the daemon transaction returns and may not be queued yet.
	state.daemon.call(func() {})
	if state.Windows[secondWindow.ID] != nil || state.ActiveWindowID != firstWindow.ID {
		var paneIndexed, windowIndexed, groupIndexed bool
		state.daemon.call(func() {
			paneIndexed = state.daemon.panes[second.ID] != nil
			windowIndexed = state.daemon.windows[secondWindow.ID] != nil
			groupIndexed = state.daemon.groups[secondWindow.GroupID] != nil
		})
		t.Fatalf("pane exit left windows=%v active=%d, want fallback %d (pane=%v window=%v group=%v members=%v)", state.orderedWindowIDs(), state.ActiveWindowID, firstWindow.ID, paneIndexed, windowIndexed, groupIndexed, state.group.memberIDsSnapshot())
	}

	var newer ViewTransition
	state.daemon.call(func() {
		newer = state.daemon.prepareViewTransitionNow(viewTransitionSelectWindow, testClientIdentity(client), state)
	})
	if err := sendClientCommand(client.connection, clientInstanceCommand{Transition: &newer}); err != nil {
		t.Fatal(err)
	}
	if err := sendClientCommand(client.connection, clientInstanceCommand{RefreshStatus: true}); err != nil {
		t.Fatal(err)
	}

	released := make(chan *OutputLease, 1)
	first.releaseOutputStream(released)
	if got := <-released; got != lease {
		t.Fatalf("stale pane-exit projection detached live fallback output: got %#v, want %#v", got, lease)
	}
}

type testPaneDrainRuntime struct {
	pane      *Pane
	reader    *queuedPTYReader
	relayDone chan struct{}
}

var testPaneDrainRuntimes sync.Map

func (r *testPaneDrainRuntime) enqueue(data string) {
	r.reader.enqueue([]byte(data))
}

func startTestPaneRenderer(id uint64, cols, rows int) (*Pane, chan struct{}) {
	pane := &Pane{ID: id, terminal: newTerminal(cols, rows)}
	return pane, startTestPaneLoop(pane)
}

func startTestPaneLoop(pane *Pane) chan struct{} {
	pane.initializeRuntime()
	runtime := &testPaneDrainRuntime{
		pane: pane, reader: &queuedPTYReader{}, relayDone: make(chan struct{}),
	}
	testPaneDrainRuntimes.Store(pane, runtime)
	go func() {
		relayPTYOutputFrom(runtime.pane, runtime.reader)
		close(runtime.relayDone)
	}()
	go pane.run()
	shutdown := make(chan struct{})
	go func() {
		<-shutdown
		pane.stop()
		testPaneDrainRuntimes.Delete(pane)
		<-runtime.relayDone
	}()
	return shutdown
}

func queueTestPTYOutput(t *testing.T, pane *Pane, data string) uint64 {
	t.Helper()
	value, ok := testPaneDrainRuntimes.Load(pane)
	if !ok {
		t.Fatal("pane has no test PTY drain runtime")
	}
	before := pane.renderMetricsSnapshot().PTYBytes
	value.(*testPaneDrainRuntime).enqueue(data)
	return before + uint64(len(data))
}

func sendTestPTYOutput(t *testing.T, pane *Pane, data string) {
	t.Helper()
	want := queueTestPTYOutput(t, pane, data)
	waitTestPTYBytes(t, pane, want)
}

func waitTestPTYBytes(t *testing.T, pane *Pane, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for pane.renderMetricsSnapshot().PTYBytes < want {
		if time.Now().After(deadline) {
			t.Fatalf("pane PTY bytes = %d, want at least %d", pane.renderMetricsSnapshot().PTYBytes, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertPaneInputStream(t *testing.T, inputs <-chan []byte, want string) {
	t.Helper()
	var got []byte
	for len(got) < len(want) {
		select {
		case chunk := <-inputs:
			got = append(got, chunk...)
		default:
			t.Fatalf("pane input stream = %q, want %q", got, want)
		}
	}
	if string(got) != want {
		t.Fatalf("pane input stream = %q, want %q", got, want)
	}
	select {
	case chunk := <-inputs:
		t.Fatalf("unexpected extra pane input %q", chunk)
	default:
	}
}

func assertRenderRevision(t *testing.T, data []byte, want protocol.ClientLayoutRevision) {
	t.Helper()
	commands := decodePendingCommands(t, data)
	for _, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeStartRender {
			if command.LayoutRevision != want {
				t.Fatalf("START_RENDER revision = %d, want published layout revision %d", command.LayoutRevision, want)
			}
			return
		}
	}
	t.Fatalf("render stream contains no START_RENDER for layout revision %d: %#v", want, commands)
}

func TestPaneAttachmentDoesNotWaitForSnapshotWrite(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	output := startTestPaneLoop(pane)
	defer close(output)

	stream := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	attached := make(chan error, 1)
	go func() {
		attached <- attachTestOutput(pane, testOutputLease(0, stream))
	}()
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("snapshot write did not start")
	}
	select {
	case err := <-attached:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("attach waited for the snapshot write")
	}
	sendTestPTYOutput(t, pane, "still-live")
	captured := make(chan []byte, 1)
	go func() {
		data, _ := pane.capturePane(capturePaneOptions{})
		captured <- data
	}()
	select {
	case data := <-captured:
		joined := bytes.ReplaceAll(data, []byte{'\n'}, nil)
		if !bytes.Contains(joined, []byte("still-live")) {
			t.Fatalf("pane actor processed capture but not blocked-write PTY data: %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked output writer also blocked pane PTY processing")
	}
	released := make(chan *OutputLease, 1)
	pane.releaseOutputStream(released)
	select {
	case lease := <-released:
		if lease == nil || lease.Stream != stream {
			t.Fatalf("released lease = %#v, want blocked output lease", lease)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked output writer also blocked lease handoff")
	}
	close(stream.release)
	syncPaneRenderer(t, pane)
}

func TestPaneReleaseAcknowledgesRendererExit(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
		output := startTestPaneLoop(pane)
		released := make(chan *OutputLease, 1)
		pane.releaseOutputStream(released)
		close(output)
		select {
		case lease := <-released:
			if lease != nil && lease.Slot != 0 {
				t.Fatalf("released slot = %d", lease.Slot)
			}
		case <-time.After(time.Second):
			t.Fatal("renderer exit lost release acknowledgment")
		}
	}
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(data), nil
}

type signalWriter struct {
	once    sync.Once
	written chan struct{}
}

func newSignalWriter() *signalWriter {
	return &signalWriter{written: make(chan struct{})}
}

func (w *signalWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.written) })
	return len(data), nil
}

type errorWriter struct {
	err error
}

func syncPaneRenderer(t *testing.T, pane *Pane) {
	t.Helper()
	if err := pane.syncOutput(); err != nil {
		t.Fatal(err)
	}
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func decodePendingCommands(tb testing.TB, data []byte) []protocol.DisplayCommand {
	tb.Helper()
	decoder := protocol.NewDisplayDecoder(bytes.NewReader(data))
	var commands []protocol.DisplayCommand
	for {
		command, _, err := decoder.ReadCommand()
		if err != nil {
			if err == io.EOF {
				return commands
			}
			tb.Fatal(err)
		}
		commands = append(commands, command)
	}
}

func commandOpcodes(commands []protocol.DisplayCommand) []protocol.DisplayOpcode {
	opcodes := make([]protocol.DisplayOpcode, len(commands))
	for i, command := range commands {
		opcodes[i] = command.Opcode
	}
	return opcodes
}

func containsOpcode(opcodes []protocol.DisplayOpcode, want protocol.DisplayOpcode) bool {
	return countOpcode(opcodes, want) > 0
}

func countOpcode(opcodes []protocol.DisplayOpcode, want protocol.DisplayOpcode) int {
	count := 0
	for _, opcode := range opcodes {
		if opcode == want {
			count++
		}
	}
	return count
}
