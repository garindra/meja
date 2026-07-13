package terminal

import "tali/internal/protocol"

type Style = protocol.Style
type Color = protocol.Color

var DefaultStyle = protocol.CanonicalDefaultStyle()

func colorIndexed(idx int) Color {
	return Color{Mode: "indexed", Index: uint8(idx)}
}

func colorRGB(r, g, b int) Color {
	return Color{Mode: "rgb", R: uint8(r), G: uint8(g), B: uint8(b)}
}
