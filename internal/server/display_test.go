package server

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func TestDisplayCompilerUsesSpecializedTextAndFill(t *testing.T) {
	output := newRenderOutput()
	cells := []decodedTestCell{{Width: 1}, {Width: 1}, {Width: 1}, {Cluster: "o", Width: 1}, {Cluster: "k", Width: 1}}
	if err := newTestDisplayCompiler(output, map[uint32]protocol.Style{0: {}}).writeCells(2, 4, cells); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	want := []protocol.DisplayOpcode{protocol.DisplayOpcodeSetWritePosition, protocol.DisplayOpcodeSetWriteStyle, protocol.DisplayOpcodeFill, protocol.DisplayOpcodeWriteTextUTF8}
	if len(commands) != len(want) {
		t.Fatalf("commands=%#v", commands)
	}
	for i, opcode := range want {
		if commands[i].Opcode != opcode {
			t.Fatalf("commands[%d]=0x%02x want 0x%02x", i, commands[i].Opcode, opcode)
		}
	}
}

func TestDisplayCompilerMergesCompatibleRows(t *testing.T) {
	word := func(r rune) cellWord {
		value, _ := makeScalarCellWord(r, 1, 0)
		return value
	}
	output := newRenderOutput()
	compiler := newDisplayCompiler(output, displayStyleMap{0: protocol.CanonicalDefaultStyle()}, func(cell cellWord) string {
		if r, ok := cell.scalar(); ok {
			return string(r)
		}
		return ""
	}, 4, 2)
	if err := compiler.writeCells(0, 0, []cellWord{word('a'), word('b'), word('c'), word('d')}); err != nil {
		t.Fatal(err)
	}
	if err := compiler.writeCells(1, 0, []cellWord{word('e'), word('f'), word('g'), word('h')}); err != nil {
		t.Fatal(err)
	}
	if err := compiler.finish(); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	if len(commands) != 2 || commands[0].Opcode != protocol.DisplayOpcodeSetWritePosition || commands[1].Opcode != protocol.DisplayOpcodeWriteTextUTF8Default || string(commands[1].Text) != "abcdefgh" {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestDisplayCompilerStreamsBoundedCompatibleText(t *testing.T) {
	word := func(r rune) cellWord {
		value, _ := makeScalarCellWord(r, 1, 0)
		return value
	}
	const (
		cols = 1024
		rows = 12
	)
	row := make([]cellWord, cols)
	for column := range row {
		row[column] = word(rune('a' + column%26))
	}
	var wire countingBuffer
	output := newRenderOutput(&wire)
	compiler := newDisplayCompiler(output, displayStyleMap{0: protocol.CanonicalDefaultStyle()}, func(cell cellWord) string {
		r, _ := cell.scalar()
		return string(r)
	}, cols, rows)
	for rowIndex := 0; rowIndex < rows; rowIndex++ {
		if err := compiler.writeCells(rowIndex, 0, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := compiler.finish(); err != nil {
		t.Fatal(err)
	}
	if err := output.present(); err != nil {
		t.Fatal(err)
	}
	if wire.writes < 2 {
		t.Fatalf("physical writes = %d, want incremental streaming", wire.writes)
	}
	if wire.maxWrite > renderStreamChunkSize {
		t.Fatalf("largest write = %d, want at most %d", wire.maxWrite, renderStreamChunkSize)
	}
	commands := decodePendingCommands(t, wire.Bytes())
	if got := countOpcode(commandOpcodes(commands), protocol.DisplayOpcodeSetWritePosition); got != 1 {
		t.Fatalf("position commands = %d, want 1", got)
	}
	if got := countOpcode(commandOpcodes(commands), protocol.DisplayOpcodeWriteTextUTF8Default); got < 2 {
		t.Fatalf("text commands = %d, want split commands", got)
	}
	textBytes := 0
	for _, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeWriteTextUTF8Default {
			textBytes += len(command.Text)
		}
	}
	if textBytes != cols*rows {
		t.Fatalf("text bytes = %d, want %d", textBytes, cols*rows)
	}
	if len(output.pending) != 0 || cap(output.pending) > maxRetainedRenderBuffer {
		t.Fatalf("reusable buffer len=%d cap=%d", len(output.pending), cap(output.pending))
	}
}

func TestDisplayCompilerMergesFillAcrossRows(t *testing.T) {
	output := newRenderOutput()
	compiler := newDisplayCompiler(output, displayStyleMap{0: protocol.CanonicalDefaultStyle()}, func(cellWord) string { return "" }, 4, 2)
	row := []cellWord{0, 0, 0, 0}
	if err := compiler.writeCells(0, 0, row); err != nil {
		t.Fatal(err)
	}
	if err := compiler.writeCells(1, 0, row); err != nil {
		t.Fatal(err)
	}
	if err := compiler.finish(); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	if len(commands) != 3 || commands[2].Opcode != protocol.DisplayOpcodeFill || commands[2].Fill.Columns != 8 {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestRenderOutputPublishesOnePhysicalBatchAtPresent(t *testing.T) {
	var wire countingBuffer
	output := newRenderOutput(&wire)
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 0}); err != nil {
		t.Fatal(err)
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := output.present(); err != nil {
		t.Fatal(err)
	}
	batch := wire.Bytes()
	if len(batch) == 0 || batch[len(batch)-1] != byte(protocol.DisplayOpcodePresent) {
		t.Fatalf("batch=% x", batch)
	}
	if wire.writes != 1 {
		t.Fatalf("physical writes = %d, want 1", wire.writes)
	}
}

func TestRenderOutputRetainsOnlyBoundedScratchBuffer(t *testing.T) {
	output := newRenderOutput(io.Discard)
	if err := output.present(); err != nil {
		t.Fatal(err)
	}
	if len(output.pending) != 0 || cap(output.pending) == 0 || cap(output.pending) > maxRetainedRenderBuffer {
		t.Fatalf("small render buffer len=%d cap=%d", len(output.pending), cap(output.pending))
	}

	output.pending = make([]byte, maxRetainedRenderBuffer+1)
	if err := output.commit(); err != nil {
		t.Fatal(err)
	}
	if output.pending != nil {
		t.Fatalf("oversized render buffer was retained with cap=%d", cap(output.pending))
	}
}

func TestPaneOutputRateLimiterAllowsBurstThenLimitsSustainedOutput(t *testing.T) {
	start := time.Unix(1, 0)
	limiter := newPaneOutputRateLimiter(start)
	if delay := limiter.reserve(start, paneOutputBurstBytes); delay != 0 {
		t.Fatalf("initial burst delay = %v, want 0", delay)
	}
	halfSecondBytes := paneOutputBytesPerSecond / 2
	if delay := limiter.reserve(start, halfSecondBytes); delay != 500*time.Millisecond {
		t.Fatalf("first sustained delay = %v, want 500ms", delay)
	}
	if delay := limiter.reserve(start.Add(500*time.Millisecond), halfSecondBytes); delay != 500*time.Millisecond {
		t.Fatalf("second sustained delay = %v, want 500ms", delay)
	}
	if delay := limiter.reserve(start.Add(1500*time.Millisecond), paneOutputBurstBytes); delay != 0 {
		t.Fatalf("refilled burst delay = %v, want 0", delay)
	}
}

type countingBuffer struct {
	bytes.Buffer
	writes   int
	maxWrite int
}

func (b *countingBuffer) Write(data []byte) (int, error) {
	b.writes++
	b.maxWrite = max(b.maxWrite, len(data))
	return b.Buffer.Write(data)
}

func testOutputLease(slot int, stream io.Writer) *OutputLease {
	return &OutputLease{Slot: slot, Stream: stream}
}

func testClientInstance(frames chan protocol.Frame, leases map[int]*OutputLease, status ...io.Writer) *ClientInstance {
	connection := &ClientInstance{managementOut: frames}
	if len(status) > 0 {
		connection.StatusOutput = status[0]
	}
	for slot, lease := range leases {
		connection.Output[slot] = lease
	}
	return connection
}

func attachDisplayTestClient(s *Session, client *ClientInstance) {
	s.clientInstance = client
}

func TestBindingSnapshotQueuesBarrierAndPresentTogether(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 8
	client.TerminalRows = 3
	pane := &Pane{ID: session.AddPaneID(), terminal: newTerminal(8, 3)}
	session.CreateWindow(pane, 0)
	var wire bytes.Buffer
	state := session
	attachDisplayTestClient(state, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, &wire)}))
	if err := session.rebindOutputsAndPublishLayout(nil); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, wire.Bytes())
	if len(commands) < 2 || commands[0].Opcode != protocol.DisplayOpcodeRelayoutBarrier || commands[len(commands)-1].Opcode != protocol.DisplayOpcodePresent {
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
	if err := pane.attachOutputWithRefresh(firstLease, func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 1, GridCols: 80, GridRows: 24}); err != nil {
			return err
		}
		return output.present()
	}); err != nil {
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
	if err := pane.applyRender(func(output *renderOutput) error {
		return output.present()
	}); err != nil {
		t.Fatal(err)
	}
	if first.Len() != firstSize {
		t.Fatal("detached stream received output")
	}

	if err := pane.attachOutputWithRefresh(testOutputLease(0, &second), func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 2, GridCols: 80, GridRows: 24}); err != nil {
			return err
		}
		return output.present()
	}); err != nil {
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
	if err := pane.attachOutputWithRefresh(testOutputLease(0, &oldStream), nil); err != nil {
		t.Fatal(err)
	}
	if err := pane.attachOutputWithRefresh(testOutputLease(0, &replacement), nil); err != nil {
		t.Fatal(err)
	}
	if err := pane.detachOutputStream(&oldStream); err != nil {
		t.Fatal(err)
	}
	if err := pane.applyRender(func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
			return err
		}
		return output.commit()
	}); err != nil {
		t.Fatal(err)
	}
	if replacement.Len() == 0 {
		t.Fatal("old stream cleanup detached the replacement stream")
	}
}

func TestPaneRendererCanAttachReplacementAfterWriteFailure(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	output := startTestPaneLoop(pane)
	defer close(output)

	writeErr := errors.New("stream closed")
	if err := pane.attachOutputWithRefresh(testOutputLease(0, errorWriter{err: writeErr}), func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
			return err
		}
		return output.commit()
	}); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)

	var replacement bytes.Buffer
	if err := pane.attachOutputWithRefresh(testOutputLease(0, &replacement), func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
			return err
		}
		return output.commit()
	}); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if replacement.Len() == 0 {
		t.Fatal("replacement stream received no output")
	}
}

func TestOutputHandoffAttachesEachReleasedSlotImmediately(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 16
	client.TerminalRows = 4
	first := &Pane{ID: session.AddPaneID(), terminal: newTerminal(8, 4)}
	second := &Pane{ID: session.AddPaneID(), terminal: newTerminal(8, 4)}
	session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	firstUpdates := startTestPaneLoop(first)
	secondUpdates := startTestPaneLoop(second)
	defer close(firstUpdates)
	defer close(secondUpdates)

	firstWritten := newSignalWriter()
	secondWritten := newSignalWriter()
	attachDisplayTestClient(session, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, firstWritten), 1: testOutputLease(1, secondWritten)}))
	bindings, _ := session.RenderBindings(0)
	handoff := &outputHandoff{
		released: make(chan *OutputLease, 2),
		pending:  map[int]struct{}{0: {}, 1: {}},
	}
	finished := make(chan error, 1)
	go func() { finished <- session.finishOutputHandoff(handoff, bindings) }()

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
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 16
	client.TerminalRows = 4
	pane := &Pane{ID: session.AddPaneID(), terminal: newTerminal(16, 4)}
	session.CreateWindow(pane, 0)
	updates := startTestPaneLoop(pane)
	defer close(updates)

	written := newSignalWriter()
	attachDisplayTestClient(session, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, written)}))
	bindings, _ := session.RenderBindings(0)
	handoff := &outputHandoff{
		released: make(chan *OutputLease, 1),
		pending:  map[int]struct{}{0: {}},
	}
	handoff.released <- nil

	if err := session.finishOutputHandoff(handoff, bindings); err != nil {
		t.Fatal(err)
	}
	select {
	case <-written.written:
	case <-time.After(time.Second):
		t.Fatal("replacement output was not attached after a nil old-lease release")
	}
}

func TestBindingPublicationWaitsForHandoffCompletion(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols, client.TerminalRows = 8, 3
	pane := &Pane{ID: session.AddPaneID(), terminal: newTerminal(8, 3)}
	session.CreateWindow(pane, 0)
	frames := make(chan protocol.Frame, 1)
	var paneWire bytes.Buffer
	attachDisplayTestClient(session, testClientInstance(frames, map[int]*OutputLease{0: testOutputLease(0, &paneWire)}))
	handoff := &outputHandoff{
		released: make(chan *OutputLease, 1),
		pending:  map[int]struct{}{0: {}},
	}
	done := make(chan error, 1)
	go func() {
		done <- session.rebindOutputsAndPublishLayout(handoff)
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
		if frame.Type != protocol.MsgWindowLayout {
			t.Fatalf("published frame type = %d, want WINDOW_LAYOUT", frame.Type)
		}
	default:
		t.Fatal("layout was not published after binding completion")
	}
}

func TestReturningToSplitWindowKeepsFirstPaneAttached(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 16
	client.TerminalRows = 4
	first, firstUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	second, secondUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	firstWindow, _ := session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(session, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, &slot0), 1: testOutputLease(1, &slot1)}))
	if err := session.rebindOutputsAndPublishLayout(nil); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	handoff := session.beginOutputHandoff()
	third, thirdUpdates := startTestPaneRenderer(session.AddPaneID(), 16, 4)
	defer close(thirdUpdates)
	session.CreateWindow(third, 0)
	if err := session.rebindOutputsAndPublishLayout(handoff); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, third)

	handoff = session.beginOutputHandoff()
	if _, _, err := session.SelectWindow(0, firstWindow.ID); err != nil {
		t.Fatal(err)
	}
	if err := session.rebindOutputsAndPublishLayout(handoff); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	before := slot0.Len()
	if err := first.applyRender(func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
			return err
		}
		return output.commit()
	}); err != nil {
		t.Fatal(err)
	}
	if slot0.Len() <= before {
		t.Fatal("first pane had no live output after returning to its split window")
	}
}

func TestSwapPaneCommandMovesLiveOutputsToRevisedSlots(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 16
	client.TerminalRows = 4
	first, firstUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	second, secondUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 1)
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(session, testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &slot0),
		1: testOutputLease(1, &slot1),
	}))
	if err := session.rebindOutputsAndPublishLayout(nil); err != nil {
		t.Fatal(err)
	}
	<-frames // Initial layout is now published after the initial binding succeeds.
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	if err := session.commandSwapPane(SwapPanePrevious); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)
	if first.outputLease == nil || first.outputLease.Slot != 1 || second.outputLease == nil || second.outputLease.Slot != 0 {
		t.Fatalf("leases after swap: first=%#v second=%#v", first.outputLease, second.outputLease)
	}
	bindings, state := session.RenderBindings(0)
	if len(bindings) != 2 || bindings[0].PaneID != second.ID || bindings[1].PaneID != first.ID || state.FocusedPaneID != second.ID {
		t.Fatalf("bindings after swap=%#v state=%#v", bindings, state)
	}

	frame := <-frames
	if frame.Type != protocol.MsgWindowLayout {
		t.Fatalf("frame type = %d, want WINDOW_LAYOUT", frame.Type)
	}
	layout, err := protocol.DecodeWindowLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].PaneID != second.ID || layout.Panes[0].Slot != 0 || layout.Panes[1].PaneID != first.ID || layout.Panes[1].Slot != 1 {
		t.Fatalf("published layout after swap=%#v", layout)
	}
}

func TestZoomCommandRebindsOnlyFocusedPaneAndRestoresSplit(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols, client.TerminalRows = 16, 4
	first, firstUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	second, secondUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.FocusPane(0, first.ID); err != nil {
		t.Fatal(err)
	}
	frames := make(chan protocol.Frame, 2)
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(session, testClientInstance(frames, map[int]*OutputLease{
		0: testOutputLease(0, &slot0),
		1: testOutputLease(1, &slot1),
	}))
	if err := session.rebindOutputsAndPublishLayout(nil); err != nil {
		t.Fatal(err)
	}
	<-frames // Initial layout is now published after the initial binding succeeds.
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	if err := session.commandToggleZoom(); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)
	bindings, state := session.RenderBindings(0)
	if len(bindings) != 1 || bindings[0].PaneID != first.ID || bindings[0].Slot != 0 || state.FocusedPaneID != first.ID {
		t.Fatalf("zoomed bindings=%#v state=%#v", bindings, state)
	}
	if first.outputLease == nil || first.outputLease.Slot != 0 || second.outputLease != nil {
		t.Fatalf("zoomed leases: first=%#v second=%#v", first.outputLease, second.outputLease)
	}
	frame := <-frames
	layout, err := protocol.DecodeWindowLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 1 || layout.Panes[0].PaneID != first.ID || layout.Panes[0].Rect != (protocol.Rect{Width: 16, Height: 4}) {
		t.Fatalf("zoomed layout = %#v", layout)
	}

	if err := session.commandToggleZoom(); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)
	bindings, _ = session.RenderBindings(0)
	if len(bindings) != 2 || first.outputLease == nil || first.outputLease.Slot != 0 || second.outputLease == nil || second.outputLease.Slot != 1 {
		t.Fatalf("unzoomed bindings=%#v leases: first=%#v second=%#v", bindings, first.outputLease, second.outputLease)
	}
	frame = <-frames
	layout, err = protocol.DecodeWindowLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Rect.Width != 7 || layout.Panes[1].Rect.Width != 8 {
		t.Fatalf("unzoomed layout = %#v", layout)
	}
}

func TestClosingSplitPaneDoesNotLetDuplicateProcessExitDetachRemainingPane(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 16
	client.TerminalRows = 4
	first, firstUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	second, secondUpdates := startTestPaneRenderer(session.AddPaneID(), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	var slot0, slot1 bytes.Buffer
	attachDisplayTestClient(session, testClientInstance(nil, map[int]*OutputLease{0: testOutputLease(0, &slot0), 1: testOutputLease(1, &slot1)}))
	if err := session.rebindOutputsAndPublishLayout(nil); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	if err := session.commandClosePaneNow(); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	if err := session.handlePaneProcessExitNow(second.ID); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)

	before := slot0.Len()
	if err := first.applyRender(func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
			return err
		}
		return output.commit()
	}); err != nil {
		t.Fatal(err)
	}
	if slot0.Len() <= before {
		t.Fatal("duplicate process exit detached the remaining pane")
	}
}

func TestExitedOnlyPaneDestroysSession(t *testing.T) {
	state := newSession(1, "work")
	d := &Daemon{sessions: map[uint64]*Session{1: state}}
	state.daemon = d
	state.NewClient(clientID0)
	pane, updates := startTestPaneRenderer(state.AddPaneID(), 16, 4)
	defer close(updates)
	state.CreateWindow(pane, clientID0)

	if err := state.handlePaneProcessExit(pane.ID); err != nil {
		t.Fatal(err)
	}
	if d.session(1) != nil {
		t.Fatal("session remained registered after its only pane exited")
	}
	if !state.ended || state.HasWindows() {
		t.Fatalf("ended=%v windows=%v", state.ended, state.HasWindows())
	}
}

func startTestPaneRenderer(id uint64, cols, rows int) (*Pane, chan []byte) {
	pane := &Pane{ID: id, terminal: newTerminal(cols, rows)}
	return pane, startTestPaneLoop(pane)
}

func startTestPaneLoop(pane *Pane) chan []byte {
	pane.initializeRuntime()
	go pane.run()
	return pane.ptyOutput
}

func TestPaneAttachmentDoesNotWaitForSnapshotWrite(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	output := startTestPaneLoop(pane)
	defer close(output)

	stream := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	attached := make(chan error, 1)
	go func() {
		attached <- pane.attachOutputWithRefresh(testOutputLease(0, stream), func(output *renderOutput) error {
			if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
				return err
			}
			return output.commit()
		})
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
	if err := pane.applyRender(func(*renderOutput) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestDisplayCompilerDefaultOverrideDoesNotLatchStyle(t *testing.T) {
	output := newRenderOutput()
	styles := map[uint32]protocol.Style{0: {}, 2: {Bold: true}}
	compiler := newTestDisplayCompiler(output, styles)
	cells := append(textCells("bold", 2), textCells(" default", 0)...)
	cells = append(cells, textCells("bold", 2)...)
	if err := compiler.writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	var opcodes []protocol.DisplayOpcode
	for _, command := range commands {
		opcodes = append(opcodes, command.Opcode)
	}
	if !containsOpcode(opcodes, protocol.DisplayOpcodeWriteTextUTF8Default) || countOpcode(opcodes, protocol.DisplayOpcodeSetWriteStyle) != 1 {
		t.Fatalf("opcodes=%v", opcodes)
	}
}

func TestDisplayCompilerKeepsWidthTwoFallback(t *testing.T) {
	output := newRenderOutput()
	cells := []decodedTestCell{{Cluster: "界", Width: 2, StyleID: 0}, {Width: 0}}
	if err := newTestDisplayCompiler(output, map[uint32]protocol.Style{0: {}}).writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	if len(commands) == 0 || commands[len(commands)-1].Opcode != protocol.DisplayOpcodeWriteText || commands[len(commands)-1].Width != 2 {
		t.Fatalf("commands=%#v", commands)
	}
}

func TestDisplayCompilerWritesMultiRuneClusterAtomically(t *testing.T) {
	output := newRenderOutput()
	cells := []decodedTestCell{{Cluster: "👩‍💻", Width: 2}, {Width: 0}, {Cluster: "X", Width: 1}}
	if err := newTestDisplayCompiler(output, map[uint32]protocol.Style{0: {}}).writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	var cluster *protocol.DisplayCommand
	for i := range commands {
		if commands[i].Opcode == protocol.DisplayOpcodeWriteCluster {
			cluster = &commands[i]
			break
		}
	}
	if cluster == nil || string(cluster.Text) != "👩‍💻" || cluster.Width != 2 {
		t.Fatalf("commands=%#v", commands)
	}
	for _, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeSetWritePosition && command.Column == 2 {
			t.Fatal("compiler unnecessarily repositioned after atomic cluster")
		}
	}
}

func TestDisplayCompilerPreservesRepresentativeInternationalClusters(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		width   uint8
	}{
		{name: "hebrew points", cluster: "שָׁ", width: 1},
		{name: "tamil vowel sign", cluster: "நி", width: 1},
		{name: "devanagari conjunct", cluster: "क्ष", width: 2},
		{name: "cjk variation selector", cluster: "葛\U000e0100", width: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := newRenderOutput()
			cells := []decodedTestCell{{Cluster: tt.cluster, Width: tt.width}}
			if tt.width == 2 {
				cells = append(cells, decodedTestCell{Width: 0})
			}
			cells = append(cells, decodedTestCell{Cluster: "X", Width: 1})
			if err := newTestDisplayCompiler(output, map[uint32]protocol.Style{0: {}}).writeCells(0, 0, cells); err != nil {
				t.Fatal(err)
			}
			commands := decodePendingCommands(t, output.pending)
			var clusterCommands []protocol.DisplayCommand
			for _, command := range commands {
				if command.Opcode == protocol.DisplayOpcodeWriteCluster {
					clusterCommands = append(clusterCommands, command)
				}
			}
			if len(clusterCommands) != 1 || string(clusterCommands[0].Text) != tt.cluster || clusterCommands[0].Width != tt.width {
				t.Fatalf("cluster commands = %#v", clusterCommands)
			}
		})
	}
}

func TestCompilerBridgesVisuallyEquivalentBlankStyles(t *testing.T) {
	styles := compilerBenchmarkStyles()
	cells := append(textCells("Desktop", 2), textCells("    ", 0)...)
	cells = append(cells, textCells("Downloads", 2)...)
	output := newRenderOutput()
	if err := newTestDisplayCompiler(output, styles).writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	texts := 0
	for _, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeWriteTextUTF8 {
			texts++
			if string(command.Text) != "Desktop    Downloads" {
				t.Fatalf("text=%q", command.Text)
			}
		}
	}
	if texts != 1 {
		t.Fatalf("text commands=%d, want 1", texts)
	}
}

func TestDisplayCompilerPreservesVisibleBackgroundBoundary(t *testing.T) {
	styles := map[uint32]protocol.Style{1: {BG: protocol.Color{Mode: "indexed", Index: 4}}, 2: {BG: protocol.Color{Mode: "indexed", Index: 1}}}
	cells := append(textCells("blue", 1), textCells("   ", 2)...)
	cells = append(cells, textCells("panel", 1)...)
	output := newRenderOutput()
	if err := newTestDisplayCompiler(output, styles).writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	if countOpcode(commandOpcodes(commands), protocol.DisplayOpcodeWriteTextUTF8) != 2 || countOpcode(commandOpcodes(commands), protocol.DisplayOpcodeFill) != 1 {
		t.Fatalf("commands=%#v", commands)
	}
}

func TestStyleInstallIsCachedUntilRelayout(t *testing.T) {
	output := newRenderOutput()
	style := protocol.Style{Bold: true}
	if err := installStyle(output, 7, style); err != nil {
		t.Fatal(err)
	}
	if err := installStyle(output, 7, style); err != nil {
		t.Fatal(err)
	}
	if got := countOpcode(commandOpcodes(decodePendingCommands(t, output.pending)), protocol.DisplayOpcodeStyleInstall); got != 1 {
		t.Fatalf("style commands=%d", got)
	}
	output.installedStyles = make(map[uint32]protocol.Style)
	if err := installStyle(output, 7, style); err != nil {
		t.Fatal(err)
	}
	if got := countOpcode(commandOpcodes(decodePendingCommands(t, output.pending)), protocol.DisplayOpcodeStyleInstall); got != 2 {
		t.Fatalf("style commands after reset=%d", got)
	}
}

func TestStyleZeroMustBeCanonicalDefault(t *testing.T) {
	output := newRenderOutput()
	if err := installStyle(output, protocol.CanonicalDefaultStyleID, protocol.Style{Bold: true}); err == nil {
		t.Fatal("accepted noncanonical style 0")
	}
	if len(output.pending) != 0 {
		t.Fatalf("invalid style changed pending bytes: %x", output.pending)
	}
}

func TestColoredEraseInstallsReferencedStyle(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 8
	client.TerminalRows = 3
	pane := &Pane{ID: session.AddPaneID(), terminal: newTerminal(8, 3)}
	session.CreateWindow(pane, 0)
	output := newRenderOutput()
	update := pane.terminal.Apply([]byte("\x1b[44m\x1b[2K"))
	if err := emitTerminalUpdate(output, pane, update); err != nil {
		t.Fatal(err)
	}
	installed := map[uint32]bool{}
	for _, command := range decodePendingCommands(t, output.pending) {
		switch command.Opcode {
		case protocol.DisplayOpcodeStyleInstall:
			installed[command.StyleID] = true
		case protocol.DisplayOpcodeSetWriteStyle:
			if !installed[command.StyleID] {
				t.Fatalf("style %d selected before installation", command.StyleID)
			}
		}
	}
}

func TestBottomEdgeOutputEmitsScrollBeforeNewRow(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 3
	client.TerminalRows = 3
	pane := &Pane{ID: session.AddPaneID(), terminal: newTerminal(3, 2)}
	session.CreateWindow(pane, 0)
	pane.terminal.Apply([]byte("aaa\r\nbbb"))
	update := pane.terminal.Apply([]byte("\r\nccc"))

	var wire bytes.Buffer
	if err := emitTerminalUpdate(newRenderOutput(&wire), pane, update); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, wire.Bytes())
	if len(commands) == 0 || commands[0].Opcode != protocol.DisplayOpcodeScroll || commands[0].Delta != -1 {
		t.Fatalf("first command = %#v, want scroll -1", commands)
	}
	positions := 0
	for _, command := range commands {
		if command.Opcode != protocol.DisplayOpcodeSetWritePosition {
			continue
		}
		positions++
		if command.Row != 1 {
			t.Fatalf("write position row = %d, want only bottom row 1", command.Row)
		}
	}
	if positions != 1 {
		t.Fatalf("write positions = %d, want one bottom-row write", positions)
	}
}

func TestChineseTerminalOutputUsesWidthTwoDisplayCommand(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 8
	client.TerminalRows = 2
	pane := &Pane{ID: session.AddPaneID(), terminal: newTerminal(8, 1)}
	session.CreateWindow(pane, 0)
	update := pane.terminal.Apply([]byte("界"))

	var wire bytes.Buffer
	if err := emitTerminalUpdate(newRenderOutput(&wire), pane, update); err != nil {
		t.Fatal(err)
	}
	for _, command := range decodePendingCommands(t, wire.Bytes()) {
		if command.Opcode == protocol.DisplayOpcodeWriteText && command.Width == 2 && string(command.Text) == "界" {
			return
		}
	}
	t.Fatalf("display commands did not contain width-two Chinese output: %#v", decodePendingCommands(t, wire.Bytes()))
}

func TestMergedDamageMovesWithLaterScroll(t *testing.T) {
	aggregate := Update{}
	aggregate.Merge(Update{
		DirtySpans: []DirtySpan{{}, {Start: 0, End: 3}, {}},
	}, 3)
	aggregate.Merge(Update{ScrollDelta: -1}, 3)

	if aggregate.ScrollDelta != -1 {
		t.Fatalf("scroll delta = %d, want -1", aggregate.ScrollDelta)
	}
	if aggregate.DirtySpans[0].End == 0 {
		t.Fatalf("damage was not shifted with scroll: %#v", aggregate.DirtySpans)
	}
	if aggregate.DirtySpans[1].End != 0 {
		t.Fatalf("old damage row survived scroll: %#v", aggregate.DirtySpans)
	}
}

func TestDisplayWireMeasurement(t *testing.T) {
	rows := compilerBenchmarkRows()
	output := newRenderOutput()
	compiler := newTestDisplayCompiler(output, compilerBenchmarkStyles())
	for row, cells := range rows {
		if err := compiler.writeCells(row, 0, cells); err != nil {
			t.Fatal(err)
		}
	}
	actual := append(append([]byte(nil), output.pending...), byte(protocol.DisplayOpcodePresent))
	conceptual := conceptualFramedDisplaySize(t, actual)
	t.Logf("TUI-like batch commands=%d before_framed=%d after_display=%d savings=%d", len(decodePendingCommands(t, output.pending)), conceptual, len(actual), conceptual-len(actual))
}

func BenchmarkPaneOutputHotPath(b *testing.B) {
	pane := &Pane{terminal: newTerminal(80, 24)}
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	pane.terminal.Apply(bytes.Repeat([]byte{'w'}, 80*terminalRowCapacity))
	output := newRenderOutput(io.Discard)
	var update Update
	update.Reset(pane.terminal.Rows)
	pane.terminal.ApplyInto(chunk, &update)
	if err := emitTerminalUpdate(output, pane, update); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		update.Reset(pane.terminal.Rows)
		pane.terminal.ApplyInto(chunk, &update)
		if err := emitTerminalUpdate(output, pane, update); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFullPaneRender(b *testing.B) {
	pane := &Pane{terminal: newTerminal(80, 24)}
	pane.terminal.Apply(bytes.Repeat([]byte("styled \x1b[32mtext\x1b[0m "), 200))
	output := newRenderOutput(io.Discard)
	if err := sendFullRender(output, pane); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := sendFullRender(output, pane); err != nil {
			b.Fatal(err)
		}
	}
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

func textCells(text string, styleID uint32) []decodedTestCell {
	cells := make([]decodedTestCell, 0, len(text))
	for _, r := range text {
		cells = append(cells, decodedTestCell{Cluster: string(r), StyleID: styleID, Width: 1})
	}
	return cells
}

func compilerBenchmarkRows() [][]decodedTestCell {
	rows := make([][]decodedTestCell, 39)
	for row := range rows {
		cells := make([]decodedTestCell, 120)
		for col := range cells {
			style := uint32(0)
			r := ' '
			if col >= 12 && col < 42 {
				style = 2
			}
			if col >= 18 && col < 35 {
				style = 3
				r = rune('a' + col%26)
			}
			cells[col] = decodedTestCell{Cluster: string(r), StyleID: style, Width: 1}
		}
		rows[row] = cells
	}
	return rows
}

func compilerBenchmarkStyles() map[uint32]protocol.Style {
	return map[uint32]protocol.Style{
		0: {FG: protocol.Color{Mode: "default"}, BG: protocol.Color{Mode: "default"}},
		2: {FG: protocol.Color{Mode: "indexed", Index: 4}, BG: protocol.Color{Mode: "default"}},
		3: {FG: protocol.Color{Mode: "indexed", Index: 1}, BG: protocol.Color{Mode: "default"}},
	}
}

func conceptualFramedDisplaySize(tb testing.TB, stream []byte) int {
	tb.Helper()
	decoder := protocol.NewDisplayDecoder(bytes.NewReader(stream))
	total := 0
	for {
		start := decoder.BytesRead()
		command, _, err := decoder.ReadCommand()
		if err != nil {
			return total
		}
		payload := int(decoder.BytesRead()-start) - 1
		var scratch [10]byte
		typeBytes := putUvarint(scratch[:], uint64(command.Opcode))
		lengthBytes := putUvarint(scratch[:], uint64(payload))
		total += typeBytes + lengthBytes + payload
	}
}

func putUvarint(dst []byte, value uint64) int {
	count := 0
	for value >= 0x80 {
		dst[count] = byte(value) | 0x80
		value >>= 7
		count++
	}
	dst[count] = byte(value)
	return count + 1
}
