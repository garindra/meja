package protocol

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestGoldenPayloads(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{
			name: "InputBytes",
			got:  mustEncode(t, EncodeInputBytes, InputBytes{PaneID: 7, Data: []byte("ls\n")}),
			want: []byte{0x07, 'l', 's', '\n'},
		},
		{
			name: "ResizePane",
			got:  mustEncode(t, EncodeResizePane, ResizePane{PaneID: 9, Cols: 80, Rows: 23}),
			want: []byte{0x09, 0x50, 0x17},
		},
		{
			name: "BindRenderStream",
			got:  mustEncode(t, EncodeBindRenderStream, BindRenderStream{Slot: 0, SessionID: 0, WindowID: 1, PaneID: 2, BindingGeneration: 3}),
			want: []byte{0x00, 0x00, 0x01, 0x02, 0x03},
		},
		{
			name: "DefineStyle",
			got: mustEncode(t, EncodeDefineStyle, DefineStyle{
				BindingGeneration: 5,
				ID:                9,
				Style: Style{
					Bold: true,
					FG:   Color{Mode: "indexed", Index: 7},
					BG:   Color{Mode: "rgb", R: 42, G: 99, B: 158},
				},
			}),
			want: []byte{0x05, 0x09, 0x01, 0x01, 0x07, 0x02, 0x2a, 0x63, 0x9e},
		},
		{
			name: "SetRun",
			got: mustEncode(t, EncodeSetRun, SetRun{
				BindingGeneration: 1,
				BaseGeneration:    2,
				Generation:        3,
				Row:               4,
				Column:            5,
				Cells: []Cell{
					{Rune: 'A', StyleID: 0, Width: 1},
					{Rune: 'B', StyleID: 1, Width: 1},
				},
			}),
			want: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x02, 0x41, 0x00, 0x01, 0x42, 0x01, 0x01},
		},
		{
			name: "WindowList",
			got: mustEncode(t, EncodeWindowList, WindowList{
				ActiveWindowID: 2,
				Windows: []WindowInfo{
					{WindowID: 1, PaneID: 11, Index: 0, Title: "bash", Active: false},
					{WindowID: 2, PaneID: 12, Index: 1, Title: "logs", Active: true},
				},
			}),
			want: []byte{
				0x02, 0x02,
				0x01, 0x0b, 0x00, 0x04, 'b', 'a', 's', 'h', 0x00,
				0x02, 0x0c, 0x01, 0x04, 'l', 'o', 'g', 's', 0x01,
			},
		},
		{
			name: "WindowLayout",
			got: mustEncode(t, EncodeWindowLayout, WindowLayout{
				WindowID:       2,
				LayoutRevision: 3,
				Panes: []PanePlacement{
					{PaneID: 11, Rect: Rect{X: 0, Y: 0, Width: 59, Height: 39}},
					{PaneID: 12, Rect: Rect{X: 60, Y: 0, Width: 60, Height: 39}},
				},
			}),
			want: []byte{
				0x02, 0x03, 0x02,
				0x0b, 0x00, 0x00, 0x3b, 0x27,
				0x0c, 0x3c, 0x00, 0x3c, 0x27,
			},
		},
		{
			name: "Ping",
			got:  mustEncode(t, EncodePing, Ping{Seq: 7, SentUnixMilli: 0}),
			want: []byte{0x07, 0x00},
		},
	}
	for _, tc := range tests {
		if !bytes.Equal(tc.got, tc.want) {
			t.Fatalf("%s payload = %#v, want %#v", tc.name, tc.got, tc.want)
		}
	}
}

func TestRoundTripMessages(t *testing.T) {
	expiry := time.Unix(0, 123456789).UTC()
	streamOpen := mustRoundTrip(t, EncodeStreamOpen, DecodeStreamOpen, StreamOpen{StreamType: StreamTypeManagement, Slot: 2, PaneID: 3})
	if streamOpen.StreamType != StreamTypeManagement || streamOpen.Slot != 2 || streamOpen.PaneID != 3 {
		t.Fatalf("StreamOpen round-trip = %#v", streamOpen)
	}
	authChallenge := mustRoundTrip(t, EncodeAuthChallenge, DecodeAuthChallenge, AuthChallenge{ChallengeID: "c1", Nonce: "n1", ExpiresAt: expiry})
	if !authChallenge.ExpiresAt.Equal(expiry) {
		t.Fatalf("AuthChallenge round-trip = %#v", authChallenge)
	}
	pong := mustRoundTrip(t, EncodePong, DecodePong, Pong{Seq: 4, SentUnixMilli: 12345})
	if pong.Seq != 4 || pong.SentUnixMilli != 12345 {
		t.Fatalf("Pong round-trip = %#v", pong)
	}
	setCursor := mustRoundTrip(t, EncodeSetCursor, DecodeSetCursor, SetCursor{
		BindingGeneration: 3,
		BaseGeneration:    4,
		Generation:        5,
		Cursor:            Cursor{X: 1, Y: 2},
	})
	if setCursor.Cursor.X != 1 || setCursor.Cursor.Y != 2 {
		t.Fatalf("SetCursor round-trip = %#v", setCursor)
	}
	paneUpdate := mustRoundTrip(t, EncodePaneUpdate, DecodePaneUpdate, PaneUpdate{
		BindingGeneration:    2,
		BaseGeneration:       3,
		Generation:           4,
		Styles:               []StyleDefinition{{ID: 1, Style: Style{Bold: true, FG: Color{Mode: "indexed", Index: 2}}}},
		Runs:                 []CellRun{{Row: 5, Column: 6, Cells: []Cell{{Rune: 'x', StyleID: 1, Width: 1}}}},
		CursorChanged:        true,
		Cursor:               Cursor{X: 7, Y: 8},
		CursorVisibleChanged: true,
		CursorVisible:        false,
	})
	if len(paneUpdate.Runs) != 1 || paneUpdate.Runs[0].Cells[0].Rune != 'x' || paneUpdate.Cursor.X != 7 || !paneUpdate.CursorVisibleChanged || paneUpdate.CursorVisible {
		t.Fatalf("PaneUpdate round-trip = %#v", paneUpdate)
	}
	replace := mustRoundTrip(t, EncodeReplacePane, DecodeReplacePane, ReplacePane{
		SessionID:         4,
		WindowID:          5,
		PaneID:            6,
		BindingGeneration: 9,
		Generation:        10,
		Cols:              2,
		Rows:              1,
		Cursor:            Cursor{X: 1, Y: 0},
		CursorVisible:     true,
		Styles:            []StyleDefinition{{ID: 0, Style: Style{FG: Color{Mode: "default"}, BG: Color{Mode: "default"}}}},
		Cells: []Cell{
			{Rune: 'x', StyleID: 0, Width: 1},
			{Rune: 'y', StyleID: 0, Width: 1},
		},
	})
	if replace.SessionID != 4 || replace.WindowID != 5 || replace.PaneID != 6 || replace.Generation != 10 || len(replace.Cells) != 2 || replace.Cells[1].Rune != 'y' {
		t.Fatalf("ReplacePane round-trip = %#v", replace)
	}
}

func TestMalformedPayloads(t *testing.T) {
	if _, err := DecodeDefineStyle([]byte{0x01, 0x01, 0x00, 0x07}); err == nil {
		t.Fatal("DecodeDefineStyle() accepted unknown color kind")
	}
	if _, err := DecodeSetCursorVisible([]byte{0x01, 0x02, 0x03, 0x02}); !errors.Is(err, ErrInvalidBoolean) && err == nil {
		t.Fatalf("DecodeSetCursorVisible() error = %v, want invalid boolean", err)
	}
	if _, err := DecodeSetRun([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x01, 0xff}); err == nil {
		t.Fatal("DecodeSetRun() accepted truncated cell")
	}
	if _, err := DecodeReplacePane([]byte{0x01, 0x02, 0x02, 0x02, 0x00, 0x00, 0x01, 0x00}); err == nil {
		t.Fatal("DecodeReplacePane() accepted missing cells")
	}
}

func TestReplacePaneCompressesRepeatedCells(t *testing.T) {
	cells := make([]Cell, 80*24)
	for i := range cells {
		cells[i] = Cell{Rune: ' ', StyleID: 0, Width: 1}
	}
	msg := ReplacePane{
		BindingGeneration: 1,
		Generation:        2,
		Cols:              80,
		Rows:              24,
		Cursor:            Cursor{X: 0, Y: 0},
		CursorVisible:     true,
		Cells:             cells,
	}
	payload, err := EncodeReplacePane(nil, msg)
	if err != nil {
		t.Fatalf("EncodeReplacePane() error = %v", err)
	}
	if len(payload) >= 100 {
		t.Fatalf("compressed ReplacePane payload = %d bytes, want less than 100", len(payload))
	}
	decoded, err := DecodeReplacePane(payload)
	if err != nil {
		t.Fatalf("DecodeReplacePane() error = %v", err)
	}
	if len(decoded.Cells) != len(cells) || decoded.Cells[0] != cells[0] || decoded.Cells[len(decoded.Cells)-1] != cells[len(cells)-1] {
		t.Fatalf("decoded cells do not match repeated-cell snapshot")
	}
}

func TestPaneUpdateCompressesRepeatedCells(t *testing.T) {
	cells := make([]Cell, 80)
	for i := range cells {
		cells[i] = Cell{Rune: ' ', StyleID: 0, Width: 1}
	}
	msg := PaneUpdate{
		BindingGeneration: 1,
		BaseGeneration:    2,
		Generation:        3,
		Runs:              []CellRun{{Row: 4, Column: 0, Cells: cells}},
	}
	payload, err := EncodePaneUpdate(nil, msg)
	if err != nil {
		t.Fatalf("EncodePaneUpdate() error = %v", err)
	}
	if len(payload) >= 40 {
		t.Fatalf("compressed PaneUpdate payload = %d bytes, want less than 40", len(payload))
	}
	decoded, err := DecodePaneUpdate(payload)
	if err != nil {
		t.Fatalf("DecodePaneUpdate() error = %v", err)
	}
	if len(decoded.Runs) != 1 || len(decoded.Runs[0].Cells) != len(cells) || decoded.Runs[0].Cells[79] != cells[79] {
		t.Fatalf("decoded run does not match repeated-cell update")
	}
}

func mustEncode[T any](t *testing.T, encode func([]byte, T) ([]byte, error), msg T) []byte {
	t.Helper()
	out, err := encode(nil, msg)
	if err != nil {
		t.Fatalf("encode error = %v", err)
	}
	return out
}

func mustRoundTrip[T any](t *testing.T, encode func([]byte, T) ([]byte, error), decode func([]byte) (T, error), msg T) T {
	t.Helper()
	payload, err := encode(nil, msg)
	if err != nil {
		t.Fatalf("encode error = %v", err)
	}
	out, err := decode(payload)
	if err != nil {
		t.Fatalf("decode error = %v", err)
	}
	return out
}
