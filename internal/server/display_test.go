package server

import (
	"bytes"
	"io"
	"testing"

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
	output := newRenderOutput()
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 0}); err != nil {
		t.Fatal(err)
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := output.present(); err != nil {
		t.Fatal(err)
	}
	select {
	case batch := <-output.batches:
		if len(batch) == 0 || batch[len(batch)-1] != byte(protocol.DisplayOpcodePresent) {
			t.Fatalf("batch=% x", batch)
		}
	default:
		t.Fatal("PRESENT did not publish a batch")
	}
	select {
	case <-output.batches:
		t.Fatal("commands were published as multiple batches")
	default:
	}
}

func TestBindingSnapshotQueuesBarrierAndPresentTogether(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 8
	client.TerminalRows = 3
	pane := &Pane{ID: session.AddPaneID(), Terminal: terminal.New(8, 3)}
	session.CreateWindow(pane, 0)
	output := newRenderOutput()
	state := &sessionState{session: session, outputFrames: map[int]*renderOutput{0: output}}
	ctrl := &controller{state: state, outputFrames: map[int]*renderOutput{0: output}}
	if err := ctrl.publishBindingsAndSnapshots(); err != nil {
		t.Fatal(err)
	}
	var batch []byte
	select {
	case batch = <-output.batches:
	default:
		t.Fatal("snapshot did not queue a batch")
	}
	commands := decodePendingCommands(t, batch)
	if len(commands) < 2 || commands[0].Opcode != protocol.DisplayOpcodeRelayoutBarrier || commands[len(commands)-1].Opcode != protocol.DisplayOpcodePresent {
		t.Fatalf("commands=%#v", commands)
	}
	select {
	case extra := <-output.batches:
		t.Fatalf("standalone write queued before transaction: % x", extra)
	default:
	}
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
	ctrl := &controller{installedStyles: make(map[int]map[uint32]protocol.Style)}
	output := newRenderOutput()
	style := protocol.Style{Bold: true}
	if err := ctrl.installStyle(0, output, 7, style); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.installStyle(0, output, 7, style); err != nil {
		t.Fatal(err)
	}
	if got := countOpcode(commandOpcodes(decodePendingCommands(t, output.pending)), protocol.DisplayOpcodeStyleInstall); got != 1 {
		t.Fatalf("style commands=%d", got)
	}
	ctrl.resetInstalledStyles(0)
	if err := ctrl.installStyle(0, output, 7, style); err != nil {
		t.Fatal(err)
	}
	if got := countOpcode(commandOpcodes(decodePendingCommands(t, output.pending)), protocol.DisplayOpcodeStyleInstall); got != 2 {
		t.Fatalf("style commands after reset=%d", got)
	}
}

func TestStyleZeroMustBeCanonicalDefault(t *testing.T) {
	ctrl := &controller{installedStyles: make(map[int]map[uint32]protocol.Style)}
	output := newRenderOutput()
	if err := ctrl.installStyle(0, output, protocol.CanonicalDefaultStyleID, protocol.Style{Bold: true}); err == nil {
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
	pane := &Pane{ID: session.AddPaneID(), Terminal: terminal.New(8, 3)}
	session.CreateWindow(pane, 0)
	output := newRenderOutput()
	state := &sessionState{session: session, outputFrames: map[int]*renderOutput{0: output}}
	ctrl := &controller{state: state, outputFrames: map[int]*renderOutput{0: output}}
	pane.terminalMu.Lock()
	update := pane.Terminal.Apply([]byte("\x1b[44m\x1b[2K"))
	pane.terminalMu.Unlock()
	if err := ctrl.emitTerminalUpdate(pane, update); err != nil {
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
