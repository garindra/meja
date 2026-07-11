package terminal

import "tali/internal/protocol"

type Cell = protocol.Cell

func blankCell(styleID uint32) Cell {
	return Cell{Rune: ' ', StyleID: styleID, Width: 1}
}
