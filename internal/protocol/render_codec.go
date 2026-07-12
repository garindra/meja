package protocol

import "fmt"

const (
	colorKindDefault byte = iota
	colorKindIndexed
	colorKindRGB
)
const (
	styleFlagBold uint64 = 1 << iota
	styleFlagDim
	styleFlagItalic
	styleFlagUnderline
	styleFlagReverse
)

func encodeColor(w *PayloadWriter, c Color) error {
	switch c.Mode {
	case "", "default":
		w.Byte(colorKindDefault)
	case "indexed":
		w.Byte(colorKindIndexed)
		w.Byte(c.Index)
	case "rgb":
		w.Byte(colorKindRGB)
		w.Byte(c.R)
		w.Byte(c.G)
		w.Byte(c.B)
	default:
		return fmt.Errorf("unknown color mode %q", c.Mode)
	}
	return nil
}
func decodeColor(r *PayloadReader) (Color, error) {
	kind, err := r.Byte()
	if err != nil {
		return Color{}, err
	}
	switch kind {
	case colorKindDefault:
		return Color{Mode: "default"}, nil
	case colorKindIndexed:
		v, e := r.Byte()
		return Color{Mode: "indexed", Index: v}, e
	case colorKindRGB:
		a, e := r.Byte()
		if e != nil {
			return Color{}, e
		}
		b, e := r.Byte()
		if e != nil {
			return Color{}, e
		}
		c, e := r.Byte()
		return Color{Mode: "rgb", R: a, G: b, B: c}, e
	default:
		return Color{}, fmt.Errorf("unknown color kind %d", kind)
	}
}
func encodeStyle(w *PayloadWriter, s Style) error {
	var f uint64
	if s.Bold {
		f |= styleFlagBold
	}
	if s.Dim {
		f |= styleFlagDim
	}
	if s.Italic {
		f |= styleFlagItalic
	}
	if s.Underline {
		f |= styleFlagUnderline
	}
	if s.Reverse {
		f |= styleFlagReverse
	}
	w.Uvarint(f)
	if err := encodeColor(w, s.FG); err != nil {
		return err
	}
	return encodeColor(w, s.BG)
}
func decodeStyle(r *PayloadReader) (Style, error) {
	f, e := r.Uvarint()
	if e != nil {
		return Style{}, e
	}
	fg, e := decodeColor(r)
	if e != nil {
		return Style{}, e
	}
	bg, e := decodeColor(r)
	if e != nil {
		return Style{}, e
	}
	return Style{Bold: f&styleFlagBold != 0, Dim: f&styleFlagDim != 0, Italic: f&styleFlagItalic != 0, Underline: f&styleFlagUnderline != 0, Reverse: f&styleFlagReverse != 0, FG: fg, BG: bg}, nil
}

func EncodeStatusBar(dst []byte, msg StatusBar) ([]byte, error) {
	if msg.Cols <= 0 || uint64(msg.Cols) > MaxGridCols || len(msg.Cells) != msg.Cols {
		return nil, fmt.Errorf("invalid status width")
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(len(msg.Styles)))
	for _, d := range msg.Styles {
		w.Uvarint(uint64(d.ID))
		if err := encodeStyle(&w, d.Style); err != nil {
			return nil, err
		}
	}
	for _, c := range msg.Cells {
		if err := encodeCell(&w, c); err != nil {
			return nil, err
		}
	}
	return w.Buf, nil
}
func DecodeStatusBar(payload []byte) (StatusBar, error) {
	r := PayloadReader{Data: payload}
	cols, e := readCoord(&r, MaxGridCols)
	if e != nil || cols <= 0 {
		return StatusBar{}, fmt.Errorf("invalid status width")
	}
	n, e := readCount(&r, MaxStyles)
	if e != nil {
		return StatusBar{}, e
	}
	styles := make([]StyleDefinition, 0, n)
	for i := 0; i < n; i++ {
		id, e := r.Uvarint()
		if e != nil {
			return StatusBar{}, e
		}
		style, e := decodeStyle(&r)
		if e != nil {
			return StatusBar{}, e
		}
		styles = append(styles, StyleDefinition{ID: uint32(id), Style: style})
	}
	cells := make([]Cell, cols)
	for i := range cells {
		cells[i], e = decodeCell(&r)
		if e != nil {
			return StatusBar{}, e
		}
	}
	if e = r.Done(); e != nil {
		return StatusBar{}, e
	}
	return StatusBar{Cols: cols, Cells: cells, Styles: styles}, nil
}
