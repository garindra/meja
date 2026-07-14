package server

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

func TestDisplayCompilerUsesSpecializedTextAndFill(t *testing.T) {
	output := newRenderOutput()
	cells := []protocol.Cell{{Rune: ' ', Width: 1}, {Rune: ' ', Width: 1}, {Rune: ' ', Width: 1}, {Rune: 'o', Width: 1}, {Rune: 'k', Width: 1}}
	if err := newDisplayCompiler(output, map[uint32]protocol.Style{0: {}}).writeCells(2, 4, cells); err != nil {
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

type countingBuffer struct {
	bytes.Buffer
	writes int
}

func (b *countingBuffer) Write(data []byte) (int, error) {
	b.writes++
	return b.Buffer.Write(data)
}

func TestBindingSnapshotQueuesBarrierAndPresentTogether(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 8
	client.TerminalRows = 3
	pane := &Pane{ID: session.AddPaneID(), terminal: terminal.New(8, 3)}
	session.CreateWindow(pane, 0)
	var wire bytes.Buffer
	state := &sessionState{session: session}
	handler := &connectionHandler{state: state}
	state.attachConnection(nil, map[int]io.Writer{0: &wire})
	if err := handler.state.publishBindingsAndSnapshots(nil); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, wire.Bytes())
	if len(commands) < 2 || commands[0].Opcode != protocol.DisplayOpcodeRelayoutBarrier || commands[len(commands)-1].Opcode != protocol.DisplayOpcodePresent {
		t.Fatalf("commands=%#v", commands)
	}
}

func TestPaneRendererOwnsAndSwapsOutputStream(t *testing.T) {
	pane := &Pane{ID: 1, terminal: terminal.New(8, 3)}
	handler := &connectionHandler{state: &sessionState{session: NewSession(0)}}
	output := startTestPaneLoop(handler.state, pane)
	defer close(output)

	var first, second bytes.Buffer
	if err := pane.attachOutput(&first, func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 1}); err != nil {
			return err
		}
		return output.present()
	}); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)
	if pane.outputStream != &first {
		t.Fatal("pane does not own the attached stream")
	}
	if first.Len() == 0 {
		t.Fatal("first attached stream received no refresh")
	}
	released := make(chan int, 1)
	pane.releaseOutput(0, released)
	<-released
	if pane.outputStream != nil {
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

	if err := pane.attachOutput(&second, func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeRelayoutBarrier, LayoutRevision: 2}); err != nil {
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
	pane := &Pane{ID: 1, terminal: terminal.New(8, 3)}
	handler := &connectionHandler{state: &sessionState{session: NewSession(0)}}
	output := startTestPaneLoop(handler.state, pane)
	defer close(output)

	var oldStream, replacement bytes.Buffer
	if err := pane.attachOutput(&oldStream, nil); err != nil {
		t.Fatal(err)
	}
	if err := pane.attachOutput(&replacement, nil); err != nil {
		t.Fatal(err)
	}
	if err := pane.detachOutput(&oldStream); err != nil {
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
	pane := &Pane{ID: 1, terminal: terminal.New(8, 3)}
	handler := &connectionHandler{state: &sessionState{session: NewSession(0)}}
	output := startTestPaneLoop(handler.state, pane)
	defer close(output)

	writeErr := errors.New("stream closed")
	if err := pane.attachOutput(errorWriter{err: writeErr}, func(output *renderOutput) error {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
			return err
		}
		return output.commit()
	}); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, pane)

	var replacement bytes.Buffer
	if err := pane.attachOutput(&replacement, func(output *renderOutput) error {
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
	first := &Pane{ID: session.AddPaneID(), terminal: terminal.New(8, 4)}
	second := &Pane{ID: session.AddPaneID(), terminal: terminal.New(8, 4)}
	session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	handler := &connectionHandler{state: &sessionState{session: session}}
	firstUpdates := startTestPaneLoop(handler.state, first)
	secondUpdates := startTestPaneLoop(handler.state, second)
	defer close(firstUpdates)
	defer close(secondUpdates)

	firstWritten := newSignalWriter()
	secondWritten := newSignalWriter()
	handler.state.attachConnection(nil, map[int]io.Writer{0: firstWritten, 1: secondWritten})
	bindings, _ := session.RenderBindings(0)
	handoff := &outputHandoff{
		released: make(chan int, 2),
		pending:  map[int]struct{}{0: {}, 1: {}},
	}
	finished := make(chan error, 1)
	go func() { finished <- handler.state.finishOutputHandoff(handoff, bindings) }()

	handoff.released <- 0
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

	handoff.released <- 1
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff did not finish after every slot was released")
	}
}

func TestReturningToSplitWindowKeepsFirstPaneAttached(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 16
	client.TerminalRows = 4
	handler := &connectionHandler{state: &sessionState{session: session}}

	first, firstUpdates := startTestPaneRenderer(handler, session.AddPaneID(), 8, 4)
	second, secondUpdates := startTestPaneRenderer(handler, session.AddPaneID(), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	firstWindow, _ := session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	var slot0, slot1 bytes.Buffer
	handler.state.attachConnection(nil, map[int]io.Writer{0: &slot0, 1: &slot1})
	if err := handler.state.publishBindingsAndSnapshots(nil); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	handoff := handler.state.beginOutputHandoff()
	third, thirdUpdates := startTestPaneRenderer(handler, session.AddPaneID(), 16, 4)
	defer close(thirdUpdates)
	session.CreateWindow(third, 0)
	if err := handler.state.publishBindingsAndSnapshots(handoff); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, third)

	handoff = handler.state.beginOutputHandoff()
	if _, _, err := session.SelectWindow(0, firstWindow.ID); err != nil {
		t.Fatal(err)
	}
	if err := handler.state.publishBindingsAndSnapshots(handoff); err != nil {
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

func TestClosingSplitPaneDoesNotLetDuplicateProcessExitDetachRemainingPane(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 16
	client.TerminalRows = 4
	handler := &connectionHandler{state: &sessionState{session: session}}

	first, firstUpdates := startTestPaneRenderer(handler, session.AddPaneID(), 8, 4)
	second, secondUpdates := startTestPaneRenderer(handler, session.AddPaneID(), 8, 4)
	defer close(firstUpdates)
	defer close(secondUpdates)
	session.CreateWindow(first, 0)
	if _, _, err := session.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	var slot0, slot1 bytes.Buffer
	handler.state.attachConnection(nil, map[int]io.Writer{0: &slot0, 1: &slot1})
	if err := handler.state.publishBindingsAndSnapshots(nil); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	syncPaneRenderer(t, second)

	if err := handler.commandClosePaneNow(); err != nil {
		t.Fatal(err)
	}
	syncPaneRenderer(t, first)
	if err := handler.handlePaneProcessExitNow(second.ID); err != nil {
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
	state := newSessionState(1, "work")
	d := &daemon{sessions: map[uint64]*sessionState{1: state}}
	handler := &connectionHandler{state: state, daemon: d}
	state.session.NewClient(clientID0)
	pane, updates := startTestPaneRenderer(handler, state.session.AddPaneID(), 16, 4)
	defer close(updates)
	state.session.CreateWindow(pane, clientID0)

	if err := handler.handlePaneProcessExit(pane.ID); err != nil {
		t.Fatal(err)
	}
	if d.session(1) != nil {
		t.Fatal("session remained registered after its only pane exited")
	}
	if !state.ended || state.session.HasWindows() {
		t.Fatalf("ended=%v windows=%v", state.ended, state.session.HasWindows())
	}
}

func startTestPaneRenderer(handler *connectionHandler, id uint64, cols, rows int) (*Pane, chan []byte) {
	pane := &Pane{ID: id, terminal: terminal.New(cols, rows)}
	return pane, startTestPaneLoop(handler.state, pane)
}

func startTestPaneLoop(state *sessionState, pane *Pane) chan []byte {
	pane.initializeRuntime()
	go state.runPane(pane)
	return pane.ptyOutput
}

func TestPaneAttachmentDoesNotWaitForSnapshotWrite(t *testing.T) {
	pane := &Pane{ID: 1, terminal: terminal.New(8, 3)}
	handler := &connectionHandler{state: &sessionState{session: NewSession(0)}}
	output := startTestPaneLoop(handler.state, pane)
	defer close(output)

	stream := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	attached := make(chan error, 1)
	go func() {
		attached <- pane.attachOutput(stream, func(output *renderOutput) error {
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
		pane := &Pane{ID: 1, terminal: terminal.New(8, 3)}
		handler := &connectionHandler{state: &sessionState{session: NewSession(0)}}
		output := startTestPaneLoop(handler.state, pane)
		released := make(chan int, 1)
		pane.releaseOutput(0, released)
		close(output)
		select {
		case slot := <-released:
			if slot != 0 {
				t.Fatalf("released slot = %d", slot)
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
	compiler := newDisplayCompiler(output, styles)
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
	cells := []protocol.Cell{{Rune: '界', Width: 2, StyleID: 0}}
	if err := newDisplayCompiler(output, map[uint32]protocol.Style{0: {}}).writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, output.pending)
	if len(commands) == 0 || commands[len(commands)-1].Opcode != protocol.DisplayOpcodeWriteText || commands[len(commands)-1].Width != 2 {
		t.Fatalf("commands=%#v", commands)
	}
}

func TestCompilerBridgesVisuallyEquivalentBlankStyles(t *testing.T) {
	styles := compilerBenchmarkStyles()
	cells := append(textCells("Desktop", 2), textCells("    ", 0)...)
	cells = append(cells, textCells("Downloads", 2)...)
	output := newRenderOutput()
	if err := newDisplayCompiler(output, styles).writeCells(0, 0, cells); err != nil {
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
	if err := newDisplayCompiler(output, styles).writeCells(0, 0, cells); err != nil {
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
	pane := &Pane{ID: session.AddPaneID(), terminal: terminal.New(8, 3)}
	session.CreateWindow(pane, 0)
	output := newRenderOutput()
	state := &sessionState{session: session}
	handler := &connectionHandler{state: state}
	update := pane.terminal.Apply([]byte("\x1b[44m\x1b[2K"))
	if err := handler.state.emitTerminalUpdate(output, pane, update); err != nil {
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
	pane := &Pane{ID: session.AddPaneID(), terminal: terminal.New(3, 2)}
	session.CreateWindow(pane, 0)
	pane.terminal.Apply([]byte("aaa\r\nbbb"))
	update := pane.terminal.Apply([]byte("\r\nccc"))

	var wire bytes.Buffer
	state := &sessionState{session: session}
	if err := state.emitTerminalUpdate(newRenderOutput(&wire), pane, update); err != nil {
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
	pane := &Pane{ID: session.AddPaneID(), terminal: terminal.New(8, 1)}
	session.CreateWindow(pane, 0)
	update := pane.terminal.Apply([]byte("界"))

	var wire bytes.Buffer
	state := &sessionState{session: session}
	if err := state.emitTerminalUpdate(newRenderOutput(&wire), pane, update); err != nil {
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
	aggregate := terminal.Update{}
	aggregate.Merge(terminal.Update{
		DirtyRows:  map[int]struct{}{1: {}},
		DirtySpans: map[int]terminal.DirtySpan{1: {Start: 0, End: 3}},
	}, 3)
	aggregate.Merge(terminal.Update{ScrollDelta: -1}, 3)

	if aggregate.ScrollDelta != -1 {
		t.Fatalf("scroll delta = %d, want -1", aggregate.ScrollDelta)
	}
	if _, ok := aggregate.DirtySpans[0]; !ok {
		t.Fatalf("damage was not shifted with scroll: %#v", aggregate.DirtySpans)
	}
	if _, ok := aggregate.DirtySpans[1]; ok {
		t.Fatalf("old damage row survived scroll: %#v", aggregate.DirtySpans)
	}
}

func TestDisplayWireMeasurement(t *testing.T) {
	rows := compilerBenchmarkRows()
	output := newRenderOutput()
	compiler := newDisplayCompiler(output, compilerBenchmarkStyles())
	for row, cells := range rows {
		if err := compiler.writeCells(row, 0, cells); err != nil {
			t.Fatal(err)
		}
	}
	actual := append(append([]byte(nil), output.pending...), byte(protocol.DisplayOpcodePresent))
	conceptual := conceptualFramedDisplaySize(t, actual)
	t.Logf("TUI-like batch commands=%d before_framed=%d after_display=%d savings=%d", len(decodePendingCommands(t, output.pending)), conceptual, len(actual), conceptual-len(actual))
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

func textCells(text string, styleID uint32) []protocol.Cell {
	cells := make([]protocol.Cell, 0, len(text))
	for _, r := range text {
		cells = append(cells, protocol.Cell{Rune: r, StyleID: styleID, Width: 1})
	}
	return cells
}

func compilerBenchmarkRows() [][]protocol.Cell {
	rows := make([][]protocol.Cell, 39)
	for row := range rows {
		cells := make([]protocol.Cell, 120)
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
			cells[col] = protocol.Cell{Rune: r, StyleID: style, Width: 1}
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
