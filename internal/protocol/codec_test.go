package protocol

import (
	"reflect"
	"testing"
)

func TestDisplayCommandRoundTrips(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{"barrier", func() error {
			in := RelayoutBarrier{LayoutRevision: 7}
			b, e := EncodeRelayoutBarrier(nil, in)
			if e != nil {
				return e
			}
			out, e := DecodeRelayoutBarrier(b)
			if out != in {
				t.Fatalf("barrier=%#v", out)
			}
			return e
		}},
		{"position", func() error {
			in := SetWritePosition{Row: 4, Column: 9}
			b, e := EncodeSetWritePosition(nil, in)
			if e != nil {
				return e
			}
			out, e := DecodeSetWritePosition(b)
			if out != in {
				t.Fatalf("position=%#v", out)
			}
			return e
		}},
		{"text", func() error {
			in := WriteText{CellWidth: 2, Text: []byte("日本")}
			b, e := EncodeWriteText(nil, in)
			if e != nil {
				return e
			}
			out, e := DecodeWriteText(b)
			if !reflect.DeepEqual(out, in) {
				t.Fatalf("text=%#v", out)
			}
			return e
		}},
		{"fill", func() error {
			in := Fill{Columns: 80, Rune: ' ', Width: 1}
			b, e := EncodeFill(nil, in)
			if e != nil {
				return e
			}
			out, e := DecodeFill(b)
			if out != in {
				t.Fatalf("fill=%#v", out)
			}
			return e
		}},
		{"cursor", func() error {
			in := CursorUpdate{Cursor: Cursor{X: 2, Y: 3}, Visible: true}
			b, e := EncodeCursorUpdate(nil, in)
			if e != nil {
				return e
			}
			out, e := DecodeCursorUpdate(b)
			if out != in {
				t.Fatalf("cursor=%#v", out)
			}
			return e
		}},
		{"scroll", func() error {
			in := Scroll{Delta: -1}
			b, e := EncodeScroll(nil, in)
			if e != nil {
				return e
			}
			out, e := DecodeScroll(b)
			if out != in {
				t.Fatalf("scroll=%#v", out)
			}
			return e
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPresentHasEmptyPayload(t *testing.T) {
	b, err := EncodePresent(nil, Present{})
	if err != nil || len(b) != 0 {
		t.Fatalf("present=%v err=%v", b, err)
	}
	if _, err := DecodePresent([]byte{1}); err == nil {
		t.Fatal("accepted nonempty PRESENT")
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
