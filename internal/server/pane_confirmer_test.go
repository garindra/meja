package server

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestPaneConfirmerSelectsRawOrZlibPerFrame(t *testing.T) {
	confirmer := newPaneConfirmer()
	tinyCommands := protocol.NewDisplayEncoder(nil)
	if err := tinyCommands.AppendStartRender(protocol.StartRender{LayoutRevision: 1, Cols: 1, Rows: 1}); err != nil {
		t.Fatal(err)
	}
	tiny, err := confirmer.encodeRecord(tinyCommands.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if tiny.header.Encoding != protocol.RenderEncodingRaw {
		t.Fatalf("tiny encoding=%d, want raw", tiny.header.Encoding)
	}
	compressible, err := confirmer.encodeRecord(bytes.Repeat([]byte("render-command-payload-"), 256))
	if err != nil {
		t.Fatal(err)
	}
	if compressible.header.Encoding != protocol.RenderEncodingZlib {
		t.Fatalf("compressible encoding=%d, want zlib", compressible.header.Encoding)
	}
	if int(compressible.header.EncodedSize)+compressible.headerSize != len(compressible.bytes) {
		t.Fatalf("header=%#v header_size=%d record_bytes=%d", compressible.header, compressible.headerSize, len(compressible.bytes))
	}
}

func TestRenderPayloadSelectionFallsBackToRawOnEqualSize(t *testing.T) {
	raw := []byte("raw")
	encoding, selected := selectRenderPayload(raw, []byte("zip"))
	if encoding != protocol.RenderEncodingRaw || !bytes.Equal(selected, raw) {
		t.Fatalf("selection=%d %q, want raw", encoding, selected)
	}
}

func TestPaneConfirmerZlibRecordsDecodeIndependently(t *testing.T) {
	confirmer := newPaneConfirmer()
	wants := [][]byte{bytes.Repeat([]byte("alpha"), 200), bytes.Repeat([]byte("beta"), 200)}
	var wire []byte
	for _, raw := range wants {
		record, err := confirmer.encodeRecord(raw)
		if err != nil {
			t.Fatal(err)
		}
		if record.header.Encoding != protocol.RenderEncodingZlib {
			t.Fatalf("encoding=%d, want zlib", record.header.Encoding)
		}
		wire = append(wire, record.bytes...)
	}
	reader := protocol.NewRenderFrameReader(bytes.NewReader(wire))
	for _, want := range wants {
		frame, err := reader.ReadFrame()
		if err != nil || !bytes.Equal(frame.Payload, want) {
			t.Fatalf("decoded frame error=%v", err)
		}
	}
	if _, err := reader.ReadFrame(); err != io.EOF {
		t.Fatalf("trailing read error=%v", err)
	}
}

func BenchmarkPaneConfirmerEncodeRecord(b *testing.B) {
	for _, size := range []int{32, 4096} {
		b.Run(fmt.Sprintf("bytes-%d", size), func(b *testing.B) {
			confirmer := newPaneConfirmer()
			raw := bytes.Repeat([]byte("render"), (size+5)/6)[:size]
			if _, err := confirmer.encodeRecord(raw); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				if _, err := confirmer.encodeRecord(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type compilerDiagnosticWorkload struct {
	name  string
	frame func(*testing.T) []byte
}

func compileDiagnosticKeyframe(t *testing.T, publication viewPublication) []byte {
	t.Helper()
	publication.Epoch = 1
	publication.TargetVersion = 1
	publication.Kind = PublicationKeyframe
	publication.Barrier = true
	confirmer := newPaneConfirmer()
	frame, err := confirmer.compile(&publication)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func compilerDiagnosticWorkloads() []compilerDiagnosticWorkload {
	defaultStyle := protocol.CanonicalDefaultStyle()
	red := protocol.Style{Bold: true, FG: protocol.Color{Mode: "indexed", Index: 1}, BG: protocol.Color{Mode: "default"}}
	return []compilerDiagnosticWorkload{
		{name: "deterministic-random", frame: func(t *testing.T) []byte {
			const cols, rows = 80, 8
			cells := make([]semanticCell, cols*rows)
			runs := make([]publishedRun, rows)
			state := uint32(0x9e3779b9)
			for index := range cells {
				state ^= state << 13
				state ^= state >> 17
				state ^= state << 5
				r := rune('a' + state%26)
				if state%11 == 0 {
					r = ' '
				}
				cells[index] = semanticCell{kind: semanticScalar, width: 1, payload: uint32(r)}
			}
			for row := range rows {
				runs[row] = publishedRun{Row: uint16(row), Columns: cols, CellStart: uint32(row * cols)}
			}
			return compileDiagnosticKeyframe(t, viewPublication{Cols: cols, Rows: rows, Styles: []protocol.Style{defaultStyle}, Cells: cells, Runs: runs})
		}},
		{name: "fragmented-same-style", frame: func(t *testing.T) []byte {
			confirmer := newPaneConfirmer()
			seed := viewPublication{
				Epoch: 1, TargetVersion: 1, Kind: PublicationKeyframe, Barrier: true, Cols: 24, Rows: 1,
				Styles: []protocol.Style{defaultStyle},
				Cells:  []semanticCell{{kind: semanticBlank, width: 1}},
				Runs:   []publishedRun{{Columns: 1}},
			}
			if _, err := confirmer.compile(&seed); err != nil {
				t.Fatal(err)
			}
			cells := make([]semanticCell, 12)
			for index := range cells {
				cells[index] = semanticCell{kind: semanticScalar, width: 1, payload: uint32('a' + index)}
			}
			delta := viewPublication{
				Epoch: 1, BaseVersion: 1, TargetVersion: 2, Kind: PublicationDelta, Cols: 24, Rows: 1,
				Styles: []protocol.Style{defaultStyle}, Cells: cells,
				Runs: []publishedRun{
					{Column: 0, Columns: 2, CellStart: 0},
					{Column: 2, Columns: 2, CellStart: 2},
					{Column: 4, Columns: 2, CellStart: 4},
					{Column: 10, Columns: 2, CellStart: 6},
					{Column: 12, Columns: 2, CellStart: 8},
					{Column: 20, Columns: 2, CellStart: 10},
				},
			}
			frame, err := confirmer.compile(&delta)
			if err != nil {
				t.Fatal(err)
			}
			return frame
		}},
		{name: "repeated-fill", frame: func(t *testing.T) []byte {
			cells := make([]semanticCell, 80)
			for index := range cells {
				cells[index] = semanticCell{kind: semanticScalar, width: 1, payload: 'x'}
			}
			return compileDiagnosticKeyframe(t, viewPublication{Cols: 80, Rows: 1, Styles: []protocol.Style{defaultStyle}, Cells: cells, Runs: []publishedRun{{Columns: 80}}})
		}},
		{name: "style-alternating", frame: func(t *testing.T) []byte {
			cells := make([]semanticCell, 80)
			for index := range cells {
				cells[index] = semanticCell{kind: semanticScalar, width: 1, style: uint16(index % 2), payload: uint32('a' + index%26)}
			}
			return compileDiagnosticKeyframe(t, viewPublication{Cols: 80, Rows: 1, Styles: []protocol.Style{defaultStyle, red}, Cells: cells, Runs: []publishedRun{{Columns: 80}}})
		}},
		{name: "cluster-wide", frame: func(t *testing.T) []byte {
			clusters := []byte("e\u0301")
			cells := []semanticCell{
				{kind: semanticCluster, width: 1, clusterLen: uint16(len(clusters))},
				{kind: semanticScalar, width: 2, payload: '界'}, {kind: semanticContinuation},
				{kind: semanticScalar, width: 1, payload: 'a'},
				{kind: semanticScalar, width: 1, payload: 'b'},
				{kind: semanticBlank, width: 1}, {kind: semanticBlank, width: 1},
				{kind: semanticScalar, width: 1, payload: 'c'},
				{kind: semanticScalar, width: 2, payload: '語'}, {kind: semanticContinuation},
				{kind: semanticScalar, width: 1, payload: 'd'},
				{kind: semanticScalar, width: 1, payload: 'e'},
				{kind: semanticBlank, width: 1}, {kind: semanticBlank, width: 1},
				{kind: semanticScalar, width: 1, payload: 'f'},
				{kind: semanticScalar, width: 1, payload: 'g'},
			}
			return compileDiagnosticKeyframe(t, viewPublication{Cols: 16, Rows: 1, Styles: []protocol.Style{defaultStyle}, Cells: cells, Clusters: clusters, Runs: []publishedRun{{Columns: 16}}})
		}},
	}
}

func TestPaneConfirmerWireDiagnostic(t *testing.T) {
	if os.Getenv("MEJA_RUN_COMPILER_DIAGNOSTIC") != "1" {
		t.Skip("set MEJA_RUN_COMPILER_DIAGNOSTIC=1 to run compiler wire diagnostics")
	}
	for _, workload := range compilerDiagnosticWorkloads() {
		t.Run(workload.name, func(t *testing.T) {
			frame := workload.frame(t)
			commands := decodePendingCommands(t, bytes.Clone(frame))
			counts := map[protocol.DisplayOpcode]int{}
			for _, command := range commands {
				counts[command.Opcode]++
			}
			text := counts[protocol.DisplayOpcodeWriteText] + counts[protocol.DisplayOpcodeWriteTextUTF8] + counts[protocol.DisplayOpcodeWriteTextUTF8Default]
			t.Logf("position=%d style=%d text=%d cluster=%d fill=%d install=%d bytes=%d",
				counts[protocol.DisplayOpcodeSetWritePosition], counts[protocol.DisplayOpcodeSetWriteStyle], text,
				counts[protocol.DisplayOpcodeWriteCluster], counts[protocol.DisplayOpcodeFill],
				counts[protocol.DisplayOpcodeStyleInstall], len(frame))
		})
	}
}

func compileConfirmerTestFrame(t *testing.T, publication viewPublication) ([]byte, []protocol.DisplayCommand) {
	t.Helper()
	frame := bytes.Clone(compileDiagnosticKeyframe(t, publication))
	commands := decodePendingCommands(t, frame)
	return frame, commands
}

func scalarCell(r rune, style uint16) semanticCell {
	return semanticCell{kind: semanticScalar, width: 1, style: style, payload: uint32(r)}
}

func TestFrameCompilerSuppressesContiguousPositionsAndTracksExactWrap(t *testing.T) {
	cells := []semanticCell{
		scalarCell('a', 0), scalarCell('b', 0),
		{kind: semanticScalar, width: 2, payload: '界'}, {kind: semanticContinuation},
		scalarCell('z', 0),
	}
	_, commands := compileConfirmerTestFrame(t, viewPublication{
		Cols: 4, Rows: 2, Styles: []protocol.Style{protocol.CanonicalDefaultStyle()}, Cells: cells,
		Runs: []publishedRun{
			{Row: 0, Column: 0, Columns: 2, CellStart: 0},
			{Row: 0, Column: 2, Columns: 2, CellStart: 2},
			{Row: 1, Column: 0, Columns: 1, CellStart: 4},
		},
	})
	opcodes := commandOpcodes(commands)
	if got := countOpcode(opcodes, protocol.DisplayOpcodeSetWritePosition); got != 1 {
		t.Fatalf("position commands = %d, want one across contiguous runs and exact wrap; opcodes=%v", got, opcodes)
	}
	if got := countOpcode(opcodes, protocol.DisplayOpcodeWriteText) + countOpcode(opcodes, protocol.DisplayOpcodeWriteTextUTF8) + countOpcode(opcodes, protocol.DisplayOpcodeWriteTextUTF8Default); got != 3 {
		t.Fatalf("text commands = %d, want width-1, width-2, width-1 groups", got)
	}
}

func TestFrameCompilerEmitsPositionForDiscontinuity(t *testing.T) {
	_, commands := compileConfirmerTestFrame(t, viewPublication{
		Cols: 8, Rows: 1, Styles: []protocol.Style{protocol.CanonicalDefaultStyle()},
		Cells: []semanticCell{scalarCell('a', 0), scalarCell('b', 0)},
		Runs: []publishedRun{
			{Column: 0, Columns: 1, CellStart: 0},
			{Column: 5, Columns: 1, CellStart: 1},
		},
	})
	if got := countOpcode(commandOpcodes(commands), protocol.DisplayOpcodeSetWritePosition); got != 2 {
		t.Fatalf("position commands = %d, want one initial and one after gap", got)
	}
}

func TestFrameCompilerMergesTextAcrossPublicationRunsWithCanonicalLength(t *testing.T) {
	frame, commands := compileConfirmerTestFrame(t, viewPublication{
		Cols: 4, Rows: 1, Styles: []protocol.Style{protocol.CanonicalDefaultStyle()},
		Cells: []semanticCell{scalarCell('a', 0), scalarCell('b', 0), scalarCell('c', 0), scalarCell('d', 0)},
		Runs: []publishedRun{
			{Column: 0, Columns: 2, CellStart: 0},
			{Column: 2, Columns: 2, CellStart: 2},
		},
	})
	var text []byte
	textCommands := 0
	for _, command := range commands {
		switch command.Opcode {
		case protocol.DisplayOpcodeWriteText, protocol.DisplayOpcodeWriteTextUTF8, protocol.DisplayOpcodeWriteTextUTF8Default:
			textCommands++
			text = command.Text
		}
	}
	if textCommands != 1 || string(text) != "abcd" {
		t.Fatalf("text commands=%d payload=%q, want one merged command", textCommands, text)
	}
	if !bytes.Contains(frame, []byte{byte(protocol.DisplayOpcodeWriteTextUTF8Default), 4, 'a', 'b', 'c', 'd'}) {
		t.Fatalf("frame does not contain canonical one-byte text length: %x", frame)
	}
}

func TestFrameCompilerSuppressesStylesAndPreservesDefaultSpecialization(t *testing.T) {
	red := protocol.Style{Bold: true, FG: protocol.Color{Mode: "indexed", Index: 1}, BG: protocol.Color{Mode: "default"}}
	_, commands := compileConfirmerTestFrame(t, viewPublication{
		Cols: 3, Rows: 1, Styles: []protocol.Style{protocol.CanonicalDefaultStyle(), red},
		Cells: []semanticCell{scalarCell('r', 1), scalarCell('d', 0), scalarCell('r', 1)},
		Runs:  []publishedRun{{Columns: 3}},
	})
	opcodes := commandOpcodes(commands)
	if got := countOpcode(opcodes, protocol.DisplayOpcodeSetWriteStyle); got != 1 {
		t.Fatalf("style selections = %d, want one retained nondefault selection; opcodes=%v", got, opcodes)
	}
	if got := countOpcode(opcodes, protocol.DisplayOpcodeWriteTextUTF8Default); got != 1 {
		t.Fatalf("default-specialized writes = %d, want one", got)
	}
}

func TestFrameCompilerSelectsEachRealStyleChangeOnce(t *testing.T) {
	red := protocol.Style{FG: protocol.Color{Mode: "indexed", Index: 1}, BG: protocol.Color{Mode: "default"}}
	blue := protocol.Style{FG: protocol.Color{Mode: "indexed", Index: 4}, BG: protocol.Color{Mode: "default"}}
	clusters := []byte("e\u0301")
	_, commands := compileConfirmerTestFrame(t, viewPublication{
		Cols: 3, Rows: 1, Styles: []protocol.Style{protocol.CanonicalDefaultStyle(), red, blue}, Clusters: clusters,
		Cells: []semanticCell{
			scalarCell('a', 1),
			{kind: semanticCluster, width: 1, style: 1, clusterLen: uint16(len(clusters))},
			scalarCell('b', 2),
		},
		Runs: []publishedRun{{Columns: 3}},
	})
	if got := countOpcode(commandOpcodes(commands), protocol.DisplayOpcodeSetWriteStyle); got != 2 {
		t.Fatalf("style selections = %d, want red then blue", got)
	}
}

func TestFrameCompilerChoosesAndMergesSmallerFills(t *testing.T) {
	cells := make([]semanticCell, 10)
	for index := range cells {
		cells[index] = scalarCell('x', 0)
	}
	_, commands := compileConfirmerTestFrame(t, viewPublication{
		Cols: 10, Rows: 1, Styles: []protocol.Style{protocol.CanonicalDefaultStyle()}, Cells: cells,
		Runs: []publishedRun{
			{Column: 0, Columns: 5, CellStart: 0},
			{Column: 5, Columns: 5, CellStart: 5},
		},
	})
	var fills []protocol.Fill
	for _, command := range commands {
		if command.Opcode == protocol.DisplayOpcodeFill {
			fills = append(fills, command.Fill)
		}
	}
	if len(fills) != 1 || fills[0] != (protocol.Fill{Columns: 10, Rune: 'x', Width: 1}) {
		t.Fatalf("fills = %#v, want one merged ten-column fill", fills)
	}
}

func TestFrameCompilerBridgesOnlyVisuallyEquivalentBlanks(t *testing.T) {
	defaultStyle := protocol.CanonicalDefaultStyle()
	equivalent := defaultStyle
	equivalent.Bold = true
	underlined := equivalent
	underlined.Underline = true
	for _, test := range []struct {
		name      string
		blank     protocol.Style
		wantTexts int
	}{
		{name: "equivalent", blank: equivalent, wantTexts: 1},
		{name: "underlined", blank: underlined, wantTexts: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, commands := compileConfirmerTestFrame(t, viewPublication{
				Cols: 3, Rows: 1, Styles: []protocol.Style{defaultStyle, test.blank},
				Cells: []semanticCell{scalarCell('a', 0), {kind: semanticBlank, width: 1, style: 1}, scalarCell('b', 0)},
				Runs:  []publishedRun{{Columns: 3}},
			})
			opcodes := commandOpcodes(commands)
			texts := countOpcode(opcodes, protocol.DisplayOpcodeWriteText) + countOpcode(opcodes, protocol.DisplayOpcodeWriteTextUTF8) + countOpcode(opcodes, protocol.DisplayOpcodeWriteTextUTF8Default)
			if texts != test.wantTexts {
				t.Fatalf("text commands = %d, want %d; opcodes=%v", texts, test.wantTexts, opcodes)
			}
		})
	}
}

func TestFrameCompilerKeepsClustersAndWideCellsAtomic(t *testing.T) {
	red := protocol.Style{FG: protocol.Color{Mode: "indexed", Index: 1}, BG: protocol.Color{Mode: "default"}}
	clusters := []byte("e\u0301")
	_, commands := compileConfirmerTestFrame(t, viewPublication{
		Cols: 5, Rows: 1, Styles: []protocol.Style{protocol.CanonicalDefaultStyle(), red}, Clusters: clusters,
		Cells: []semanticCell{
			{kind: semanticCluster, width: 1, style: 1, clusterLen: uint16(len(clusters))},
			scalarCell('a', 1), scalarCell('b', 1),
			{kind: semanticScalar, width: 2, style: 1, payload: '界'}, {kind: semanticContinuation},
		},
		Runs: []publishedRun{{Columns: 5}},
	})
	opcodes := commandOpcodes(commands)
	if countOpcode(opcodes, protocol.DisplayOpcodeSetWritePosition) != 1 || countOpcode(opcodes, protocol.DisplayOpcodeSetWriteStyle) != 1 ||
		countOpcode(opcodes, protocol.DisplayOpcodeWriteCluster) != 1 || countOpcode(opcodes, protocol.DisplayOpcodeWriteText) != 1 {
		t.Fatalf("cluster/wide command sequence = %v", opcodes)
	}
}

func TestFrameCompilerResetsPenAndStyleAtPublicationBoundary(t *testing.T) {
	red := protocol.Style{FG: protocol.Color{Mode: "indexed", Index: 1}, BG: protocol.Color{Mode: "default"}}
	confirmer := newPaneConfirmer()
	first := viewPublication{
		Epoch: 1, TargetVersion: 1, Kind: PublicationKeyframe, Barrier: true, Cols: 1, Rows: 1,
		Styles: []protocol.Style{red}, Cells: []semanticCell{scalarCell('a', 0)}, Runs: []publishedRun{{Columns: 1}},
	}
	if _, err := confirmer.compile(&first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.BaseVersion = 1
	second.TargetVersion = 2
	second.Kind = PublicationDelta
	second.Barrier = false
	frame, err := confirmer.compile(&second)
	if err != nil {
		t.Fatal(err)
	}
	commands := decodePendingCommands(t, bytes.Clone(frame))
	opcodes := commandOpcodes(commands)
	if countOpcode(opcodes, protocol.DisplayOpcodeSetWritePosition) != 1 || countOpcode(opcodes, protocol.DisplayOpcodeSetWriteStyle) != 1 {
		t.Fatalf("second publication depended on prior frame latches: %v", opcodes)
	}
	if countOpcode(opcodes, protocol.DisplayOpcodeStyleInstall) != 0 {
		t.Fatalf("persistent style dictionary was reset without a barrier: %v", opcodes)
	}
}
