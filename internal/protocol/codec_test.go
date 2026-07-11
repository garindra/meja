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
			got:  mustEncode(t, EncodeBindRenderStream, BindRenderStream{SessionID: 0, WindowID: 1, PaneID: 2, BindingGeneration: 3}),
			want: []byte{0x00, 0x01, 0x02, 0x03},
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
	streamOpen := mustRoundTrip(t, EncodeStreamOpen, DecodeStreamOpen, StreamOpen{StreamType: StreamTypeManagement, PaneID: 3})
	if streamOpen.StreamType != StreamTypeManagement || streamOpen.PaneID != 3 {
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
