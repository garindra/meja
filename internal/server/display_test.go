package server

import (
	"encoding/binary"
	"testing"
	"unicode/utf8"

	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

func TestSendCellCommandsUsesFillAndText(t *testing.T) {
	ch := make(chan protocol.Frame, 16)
	cells := []protocol.Cell{{Rune: ' ', Width: 1}, {Rune: ' ', Width: 1}, {Rune: ' ', Width: 1}, {Rune: 'o', Width: 1}, {Rune: 'k', Width: 1}}
	if err := newDisplayCompiler(ch, map[uint32]protocol.Style{0: {}}).writeCells(2, 4, cells); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var types []uint64
	for frame := range ch {
		types = append(types, frame.Type)
	}
	want := []uint64{protocol.MsgSetWritePosition, protocol.MsgSetWriteStyle, protocol.MsgFill, protocol.MsgWriteText}
	if len(types) != len(want) {
		t.Fatalf("types=%v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types=%v want=%v", types, want)
		}
	}
}

func TestDisplayCompilerSavings(t *testing.T) {
	rows := compilerBenchmarkRows()
	baseline := compileDisplayRows(t, rows, false)
	optimized := compileDisplayRows(t, rows, true)
	baseBytes := displayFramesSize(baseline)
	optimizedBytes := displayFramesSize(optimized)
	t.Logf("display compiler: commands %d -> %d (%.1f%% saved), wire bytes %d -> %d (%.1f%% saved)", len(baseline), len(optimized), savingPercent(len(baseline), len(optimized)), baseBytes, optimizedBytes, savingPercent(baseBytes, optimizedBytes))
	if len(optimized) >= len(baseline) || optimizedBytes >= baseBytes {
		t.Fatal("stateful compiler did not reduce output")
	}
}

func TestCompilerBridgesVisuallyEquivalentBlankStyles(t *testing.T) {
	styles := compilerBenchmarkStyles()
	cells := append(textCells("Desktop", 2), textCells("    ", 0)...)
	cells = append(cells, textCells("Downloads", 2)...)
	ch := make(chan protocol.Frame, 16)
	if err := newDisplayCompiler(ch, styles).writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	close(ch)
	writes := 0
	for frame := range ch {
		if frame.Type == protocol.MsgWriteText {
			writes++
			msg, err := protocol.DecodeWriteText(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if string(msg.Text) != "Desktop    Downloads" {
				t.Fatalf("text=%q", msg.Text)
			}
		}
	}
	if writes != 1 {
		t.Fatalf("WRITE_TEXT commands=%d, want 1", writes)
	}
}

func TestCompilerPreservesVisibleBackgroundBoundary(t *testing.T) {
	styles := map[uint32]protocol.Style{1: {BG: protocol.Color{Mode: "indexed", Index: 4}}, 2: {BG: protocol.Color{Mode: "indexed", Index: 1}}}
	cells := append(textCells("blue", 1), textCells("   ", 2)...)
	cells = append(cells, textCells("panel", 1)...)
	ch := make(chan protocol.Frame, 16)
	if err := newDisplayCompiler(ch, styles).writeCells(0, 0, cells); err != nil {
		t.Fatal(err)
	}
	close(ch)
	writes := 0
	fills := 0
	for frame := range ch {
		if frame.Type == protocol.MsgWriteText {
			writes++
		}
		if frame.Type == protocol.MsgFill {
			fills++
		}
	}
	if writes != 2 || fills != 1 {
		t.Fatalf("writes=%d fills=%d, want 2/1", writes, fills)
	}
}

func textCells(text string, styleID uint32) []protocol.Cell {
	cells := make([]protocol.Cell, 0, len(text))
	for _, r := range text {
		cells = append(cells, protocol.Cell{Rune: r, StyleID: styleID, Width: 1})
	}
	return cells
}

func BenchmarkDisplayCompiler(b *testing.B) {
	rows := compilerBenchmarkRows()
	baseline := compileDisplayRows(b, rows, false)
	optimized := compileDisplayRows(b, rows, true)
	b.Logf("command savings: %d -> %d (%.1f%%); wire savings: %d -> %d bytes (%.1f%%)", len(baseline), len(optimized), savingPercent(len(baseline), len(optimized)), displayFramesSize(baseline), displayFramesSize(optimized), savingPercent(displayFramesSize(baseline), displayFramesSize(optimized)))
	b.Run("stateless", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = compileDisplayRows(b, rows, false)
		}
	})
	b.Run("stateful", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = compileDisplayRows(b, rows, true)
		}
	})
}

type testingTB interface {
	Helper()
	Fatal(...any)
}

func compileDisplayRows(tb testingTB, rows [][]protocol.Cell, optimized bool) []protocol.Frame {
	tb.Helper()
	ch := make(chan protocol.Frame, 2048)
	if optimized {
		compiler := newDisplayCompiler(ch, compilerBenchmarkStyles())
		for row, cells := range rows {
			if err := compiler.writeCells(row, 0, cells); err != nil {
				tb.Fatal(err)
			}
		}
	} else {
		for row, cells := range rows {
			if err := writeCellsStateless(ch, row, 0, cells); err != nil {
				tb.Fatal(err)
			}
		}
	}
	close(ch)
	frames := make([]protocol.Frame, 0, len(ch))
	for frame := range ch {
		frames = append(frames, frame)
	}
	return frames
}

func writeCellsStateless(ch chan<- protocol.Frame, row, column int, cells []protocol.Cell) error {
	for i := 0; i < len(cells); {
		cell := cells[i]
		if cell.Width == 0 {
			i++
			continue
		}
		if err := sendEncoded(ch, protocol.MsgSetWritePosition, protocol.SetWritePosition{Row: row, Column: column + i}, protocol.EncodeSetWritePosition); err != nil {
			return err
		}
		if err := sendEncoded(ch, protocol.MsgSetWriteStyle, protocol.SetWriteStyle{StyleID: cell.StyleID}, protocol.EncodeSetWriteStyle); err != nil {
			return err
		}
		if cell.Width == 1 {
			j := i + 1
			for j < len(cells) && cells[j] == cell {
				j++
			}
			if j-i >= 3 {
				if err := sendEncoded(ch, protocol.MsgFill, protocol.Fill{Columns: j - i, Rune: normalizedRune(cell.Rune), Width: 1}, protocol.EncodeFill); err != nil {
					return err
				}
				i = j
				continue
			}
		}
		width, styleID := cell.Width, cell.StyleID
		text := make([]byte, 0, 64)
		for i < len(cells) {
			current := cells[i]
			if current.Width != width || current.StyleID != styleID || current.Width == 0 {
				break
			}
			text = utf8.AppendRune(text, normalizedRune(current.Rune))
			i += int(current.Width)
		}
		if err := sendEncoded(ch, protocol.MsgWriteText, protocol.WriteText{CellWidth: width, Text: text}, protocol.EncodeWriteText); err != nil {
			return err
		}
	}
	return nil
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
func displayFramesSize(frames []protocol.Frame) int {
	size := 0
	var buf [binary.MaxVarintLen64]byte
	for _, frame := range frames {
		size += binary.PutUvarint(buf[:], frame.Type) + binary.PutUvarint(buf[:], uint64(len(frame.Payload))) + len(frame.Payload)
	}
	return size
}
func savingPercent(before, after int) float64 {
	if before == 0 {
		return 0
	}
	return float64(before-after) * 100 / float64(before)
}

func TestColoredEraseInstallsReferencedStyle(t *testing.T) {
	session := NewSession(0)
	client := session.NewClient(0)
	client.TerminalCols = 8
	client.TerminalRows = 3
	pane := &Pane{ID: session.AddPaneID(), Terminal: terminal.New(8, 3)}
	session.CreateWindow(pane, 0)
	frames := make(chan protocol.Frame, 32)
	state := &sessionState{session: session, outputFrames: map[int]chan protocol.Frame{0: frames}}
	ctrl := &controller{state: state, outputFrames: map[int]chan protocol.Frame{0: frames}}
	pane.terminalMu.Lock()
	update := pane.Terminal.Apply([]byte("\x1b[44m\x1b[2K"))
	pane.terminalMu.Unlock()
	if err := ctrl.emitTerminalUpdate(pane, update); err != nil {
		t.Fatal(err)
	}
	close(frames)
	installed := map[uint32]bool{}
	for frame := range frames {
		switch frame.Type {
		case protocol.MsgStyleInstall:
			m, err := protocol.DecodeStyleInstall(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			installed[m.ID] = true
		case protocol.MsgSetWriteStyle:
			m, err := protocol.DecodeSetWriteStyle(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if !installed[m.StyleID] {
				t.Fatalf("style %d selected before installation", m.StyleID)
			}
		}
	}
}

func TestStyleInstallIsCachedUntilRelayout(t *testing.T) {
	ctrl := &controller{installedStyles: make(map[int]map[uint32]protocol.Style)}
	frames := make(chan protocol.Frame, 4)
	style := protocol.Style{Bold: true}
	if err := ctrl.installStyle(0, frames, 7, style); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.installStyle(0, frames, 7, style); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("style frames=%d, want 1", len(frames))
	}
	ctrl.resetInstalledStyles(0)
	if err := ctrl.installStyle(0, frames, 7, style); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("style frames after relayout=%d, want 2", len(frames))
	}
}
