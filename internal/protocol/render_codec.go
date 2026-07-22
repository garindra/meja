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
