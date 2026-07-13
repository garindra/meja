package protocol

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestDisplayWireBytesHaveNoGenericFrameLength(t *testing.T) {
	encoder := NewDisplayEncoder(nil)
	if err := encoder.AppendRelayoutBarrier(RelayoutBarrier{LayoutRevision: 7}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.AppendSetWritePosition(SetWritePosition{Row: 4, Column: 9}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.AppendWriteText(WriteText{CellWidth: 2, Text: []byte("界")}); err != nil {
		t.Fatal(err)
	}
	encoder.AppendPresent()
	want := []byte{0x01, 0x07, 0x03, 0x04, 0x09, 0x05, 0x02, 0x03, 0xe7, 0x95, 0x8c, 0xff}
	if !bytes.Equal(encoder.Bytes(), want) {
		t.Fatalf("wire=% x want % x", encoder.Bytes(), want)
	}
	if encoder.Bytes()[1] == byte(len(encoder.Bytes())) {
		t.Fatal("render stream contains a generic payload length")
	}
}

func TestDisplayWireBytesCoverAllCommandSchemas(t *testing.T) {
	encoder := NewDisplayEncoder(nil)
	_ = encoder.AppendStyleInstall(StyleInstall{ID: 2, Style: Style{Bold: true, FG: Color{Mode: "indexed", Index: 7}, BG: Color{Mode: "default"}}})
	_ = encoder.AppendSetWritePosition(SetWritePosition{Row: 4, Column: 9})
	_ = encoder.AppendSetWriteStyle(SetWriteStyle{StyleID: 2})
	_ = encoder.AppendWriteTextUTF8([]byte("aé"))
	_ = encoder.AppendWriteTextUTF8Default([]byte(" "))
	_ = encoder.AppendFill(Fill{Columns: 3, Rune: ' ', Width: 1})
	_ = encoder.AppendCursorUpdate(CursorUpdate{Cursor: Cursor{X: 2, Y: 3}, Visible: true})
	_ = encoder.AppendScroll(Scroll{Delta: -1})
	encoder.AppendPresent()
	want := []byte{
		0x02, 0x02, 0x01, 0x01, 0x07, 0x00,
		0x03, 0x04, 0x09,
		0x04, 0x02,
		0x06, 0x03, 'a', 0xc3, 0xa9,
		0x07, 0x01, ' ',
		0x08, 0x03, 0x20, 0x01,
		0x09, 0x02, 0x03, 0x01,
		0x0a, 0x01,
		0xff,
	}
	if !bytes.Equal(encoder.Bytes(), want) {
		t.Fatalf("wire=% x want % x", encoder.Bytes(), want)
	}
}

func TestDisplayCommandRoundTripsAcrossArbitraryReads(t *testing.T) {
	encoder := NewDisplayEncoder(nil)
	commands := []DisplayCommand{
		{Opcode: DisplayOpcodeStyleInstall, StyleID: 2, Style: Style{Bold: true, FG: Color{Mode: "indexed", Index: 7}, BG: Color{Mode: "default"}}},
		{Opcode: DisplayOpcodeSetWritePosition, Row: 4, Column: 9},
		{Opcode: DisplayOpcodeSetWriteStyle, StyleID: 2},
		{Opcode: DisplayOpcodeWriteTextUTF8, Width: 1, Text: []byte("aé")},
		{Opcode: DisplayOpcodeWriteTextUTF8Default, Width: 1, Text: []byte(" ")},
		{Opcode: DisplayOpcodeFill, Fill: Fill{Columns: 3, Rune: ' ', Width: 1}},
		{Opcode: DisplayOpcodeCursorUpdate, Cursor: CursorUpdate{Cursor: Cursor{X: 2, Y: 3}, Visible: true}},
		{Opcode: DisplayOpcodeScroll, Delta: -1},
	}
	if err := encoder.AppendRelayoutBarrier(RelayoutBarrier{LayoutRevision: 7}); err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if err := encoder.AppendCommand(command); err != nil {
			t.Fatal(err)
		}
	}
	encoder.AppendPresent()
	batch, err := NewDisplayDecoder(oneByteReader{Reader: bytes.NewReader(encoder.Bytes())}).ReadBatch()
	if err != nil {
		t.Fatal(err)
	}
	if batch.LayoutRevision != 7 || !reflect.DeepEqual(batch.Commands, commands) {
		t.Fatalf("batch=%#v want commands=%#v", batch, commands)
	}
}

func TestDisplayStyleRoundTripsExtendedAttributesWithoutChangingOldFlags(t *testing.T) {
	style := Style{Bold: true, Dim: true, Blink: true, Italic: true, Underline: true, Reverse: true, Invisible: true, FG: Color{Mode: "default"}, BG: Color{Mode: "default"}}
	encoder := NewDisplayEncoder(nil)
	if err := encoder.AppendRelayoutBarrier(RelayoutBarrier{LayoutRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.AppendStyleInstall(StyleInstall{ID: 1, Style: style}); err != nil {
		t.Fatal(err)
	}
	encoder.AppendPresent()
	batch, err := NewDisplayDecoder(bytes.NewReader(encoder.Bytes())).ReadBatch()
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Commands) != 1 || batch.Commands[0].Style != style {
		t.Fatalf("decoded style=%#v want %#v", batch.Commands, style)
	}

	var writer PayloadWriter
	if err := encodeStyle(&writer, Style{Italic: true}); err != nil {
		t.Fatal(err)
	}
	if got := writer.Buf[0]; got != byte(styleFlagItalic) {
		t.Fatalf("legacy italic flag byte=%#x want %#x", got, styleFlagItalic)
	}
}

func TestDisplayEncoderStyleInstallFailurePreservesBytes(t *testing.T) {
	encoder := NewDisplayEncoder(nil)
	encoder.AppendPresent()
	want := append([]byte(nil), encoder.Bytes()...)
	if err := encoder.AppendStyleInstall(StyleInstall{ID: 4, Style: Style{FG: Color{Mode: "invalid"}}}); err == nil {
		t.Fatal("accepted invalid style")
	}
	if !bytes.Equal(encoder.Bytes(), want) {
		t.Fatalf("bytes=% x want unchanged % x", encoder.Bytes(), want)
	}
}

func TestDisplayDecoderMultipleBatches(t *testing.T) {
	encoder := NewDisplayEncoder(nil)
	for _, text := range []string{"one", "two"} {
		if err := encoder.AppendRelayoutBarrier(RelayoutBarrier{LayoutRevision: 3}); err != nil {
			t.Fatal(err)
		}
		if err := encoder.AppendWriteTextUTF8([]byte(text)); err != nil {
			t.Fatal(err)
		}
		encoder.AppendPresent()
	}
	decoder := NewDisplayDecoder(bytes.NewReader(encoder.Bytes()))
	for _, want := range []string{"one", "two"} {
		batch, err := decoder.ReadBatch()
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Commands) != 1 || string(batch.Commands[0].Text) != want {
			t.Fatalf("batch=%#v", batch)
		}
	}
	if _, err := decoder.ReadBatch(); !errors.Is(err, io.EOF) {
		t.Fatalf("final ReadBatch error=%v", err)
	}
}

func TestDisplayDecoderRejectsMalformedCommands(t *testing.T) {
	for name, data := range map[string][]byte{
		"unknown opcode":         {0x7f},
		"truncated position":     {byte(DisplayOpcodeSetWritePosition), 0x01},
		"invalid width":          {byte(DisplayOpcodeWriteText), 0x03, 0x00},
		"truncated text":         {byte(DisplayOpcodeWriteTextUTF8), 0x03, 'a'},
		"invalid utf8":           {byte(DisplayOpcodeWriteTextUTF8), 0x01, 0xff},
		"present before barrier": {byte(DisplayOpcodePresent)},
		"command before barrier": {byte(DisplayOpcodeWriteTextUTF8), 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewDisplayDecoder(bytes.NewReader(data)).ReadBatch()
			if err == nil {
				t.Fatal("accepted malformed display stream")
			}
		})
	}
}

func TestStatusBarRoundTrip(t *testing.T) {
	cells := make([]Cell, 80)
	for i := range cells {
		cells[i] = Cell{Rune: ' ', Width: 1}
	}
	msg := StatusBar{Cols: 80, Cells: cells, Styles: []StyleDefinition{{ID: 0, Style: Style{BG: Color{Mode: "rgb", R: 42, G: 99, B: 158}}}}}
	payload, err := EncodeStatusBar(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeStatusBar(payload)
	if err != nil {
		t.Fatal(err)
	}
	if out.Cols != msg.Cols || !reflect.DeepEqual(out.Cells, msg.Cells) || len(out.Styles) != 1 || out.Styles[0].Style.BG != msg.Styles[0].Style.BG {
		t.Fatalf("status round trip mismatch: %#v", out)
	}
}

func TestWindowLayoutRoundTripIncludesSlots(t *testing.T) {
	msg := WindowLayout{WindowID: 2, FocusedPaneID: 8, LayoutRevision: 11, Panes: []PanePlacement{{PaneID: 7, Slot: 0, Rect: Rect{Width: 40, Height: 20}}, {PaneID: 8, Slot: 1, Rect: Rect{X: 41, Width: 39, Height: 20}}}}
	payload, err := EncodeWindowLayout(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeWindowLayout(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, msg) {
		t.Fatalf("layout=%#v", out)
	}
}

type oneByteReader struct{ *bytes.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.Reader.Read(p[:1])
}
