package terminal

import "golang.org/x/text/width"

func runeCellWidth(r rune) uint8 {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	default:
		return 1
	}
}
