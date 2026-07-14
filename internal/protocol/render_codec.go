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
	styleFlagBlink
	styleFlagInvisible
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
	if s.Blink {
		f |= styleFlagBlink
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
	if s.Invisible {
		f |= styleFlagInvisible
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
	return Style{Bold: f&styleFlagBold != 0, Dim: f&styleFlagDim != 0, Blink: f&styleFlagBlink != 0, Italic: f&styleFlagItalic != 0, Underline: f&styleFlagUnderline != 0, Reverse: f&styleFlagReverse != 0, Invisible: f&styleFlagInvisible != 0, FG: fg, BG: bg}, nil
}
