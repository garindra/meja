package protocol

import "testing"

func BenchmarkEncodeInputBytes(b *testing.B) {
	msg := InputBytes{PaneID: 7, Data: []byte("echo hello world\n")}
	var payload []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := EncodeInputBytes(payload[:0], msg)
		if err != nil {
			b.Fatal(err)
		}
		payload = out
	}
}

func BenchmarkDecodeInputBytes(b *testing.B) {
	payload, _ := EncodeInputBytes(nil, InputBytes{PaneID: 7, Data: []byte("echo hello world\n")})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeInputBytes(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeSetRun(b *testing.B) {
	msg := SetRun{
		BindingGeneration: 1,
		BaseGeneration:    2,
		Generation:        3,
		Row:               0,
		Column:            0,
		Cells:             make([]Cell, 80),
	}
	for i := range msg.Cells {
		msg.Cells[i] = Cell{Rune: 'a', StyleID: 1, Width: 1}
	}
	var payload []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := EncodeSetRun(payload[:0], msg)
		if err != nil {
			b.Fatal(err)
		}
		payload = out
	}
}

func BenchmarkDecodeReplacePane(b *testing.B) {
	msg := ReplacePane{
		BindingGeneration: 1,
		Generation:        2,
		Cols:              80,
		Rows:              24,
		Cursor:            Cursor{X: 3, Y: 4},
		CursorVisible:     true,
		Styles:            []StyleDefinition{{ID: 0, Style: Style{FG: Color{Mode: "default"}, BG: Color{Mode: "default"}}}},
		Cells:             make([]Cell, 80*24),
	}
	for i := range msg.Cells {
		msg.Cells[i] = Cell{Rune: 'x', StyleID: 0, Width: 1}
	}
	payload, _ := EncodeReplacePane(nil, msg)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeReplacePane(payload); err != nil {
			b.Fatal(err)
		}
	}
}
