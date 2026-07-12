package server

import (
	"unicode/utf8"

	"tali/internal/protocol"
)

type displayCompiler struct {
	output        chan<- protocol.Frame
	positionValid bool
	row, column   int
	styleValid    bool
	styleID       uint32
	textScratch   []byte
	styles        map[uint32]protocol.Style
}

func newDisplayCompiler(output chan<- protocol.Frame, styles map[uint32]protocol.Style) *displayCompiler {
	return &displayCompiler{output: output, styles: styles, textScratch: make([]byte, 0, 64)}
}

func (d *displayCompiler) writeCells(row, column int, cells []protocol.Cell) error {
	for i := 0; i < len(cells); {
		cell := cells[i]
		if cell.Width == 0 {
			i++
			continue
		}
		if err := d.moveTo(row, column+i); err != nil {
			return err
		}
		if err := d.selectStyle(cell.StyleID); err != nil {
			return err
		}
		if cell.Width == 1 {
			j := i + 1
			for j < len(cells) && cells[j] == cell {
				j++
			}
			if j-i >= 3 {
				count := j - i
				if err := sendEncoded(d.output, protocol.MsgFill, protocol.Fill{Columns: count, Rune: normalizedRune(cell.Rune), Width: 1}, protocol.EncodeFill); err != nil {
					return err
				}
				d.column += count
				i = j
				continue
			}
		}
		width := cell.Width
		styleID := cell.StyleID
		d.textScratch = d.textScratch[:0]
		start := i
		for i < len(cells) {
			current := cells[i]
			if current.Width != width || current.Width == 0 {
				break
			}
			if current.StyleID != styleID {
				end, ok := d.blankBridgeEnd(cells, i, styleID)
				if !ok {
					break
				}
				for i < end {
					d.textScratch = append(d.textScratch, ' ')
					i++
				}
				continue
			}
			d.textScratch = utf8.AppendRune(d.textScratch, normalizedRune(current.Rune))
			i += int(current.Width)
		}
		if err := sendEncoded(d.output, protocol.MsgWriteText, protocol.WriteText{CellWidth: width, Text: d.textScratch}, protocol.EncodeWriteText); err != nil {
			return err
		}
		d.column += i - start
	}
	return nil
}

func (d *displayCompiler) blankBridgeEnd(cells []protocol.Cell, start int, targetStyle uint32) (int, bool) {
	end := start
	var checkedStyle uint32
	checked := false
	for end < len(cells) {
		cell := cells[end]
		if cell.Width != 1 || normalizedRune(cell.Rune) != ' ' {
			break
		}
		if !checked || cell.StyleID != checkedStyle {
			if !d.blankStylesEquivalent(cell.StyleID, targetStyle) {
				break
			}
			checkedStyle, checked = cell.StyleID, true
		}
		end++
	}
	if end == start || end >= len(cells) {
		return start, false
	}
	next := cells[end]
	return end, next.Width > 0 && normalizedRune(next.Rune) != ' ' && next.StyleID == targetStyle
}

func (d *displayCompiler) blankStylesEquivalent(leftID, rightID uint32) bool {
	if leftID == rightID {
		return true
	}
	left, leftOK := d.styles[leftID]
	right, rightOK := d.styles[rightID]
	if !leftOK || !rightOK || left.Underline || right.Underline {
		return false
	}
	return normalizedColor(effectiveBlankBackground(left)) == normalizedColor(effectiveBlankBackground(right))
}
func effectiveBlankBackground(style protocol.Style) protocol.Color {
	if style.Reverse {
		return style.FG
	}
	return style.BG
}
func normalizedColor(color protocol.Color) protocol.Color {
	if color.Mode == "" {
		color.Mode = "default"
	}
	return color
}

func (d *displayCompiler) moveTo(row, column int) error {
	if d.positionValid && d.row == row && d.column == column {
		return nil
	}
	if err := sendEncoded(d.output, protocol.MsgSetWritePosition, protocol.SetWritePosition{Row: row, Column: column}, protocol.EncodeSetWritePosition); err != nil {
		return err
	}
	d.positionValid = true
	d.row = row
	d.column = column
	return nil
}
func (d *displayCompiler) selectStyle(styleID uint32) error {
	if d.styleValid && d.styleID == styleID {
		return nil
	}
	if err := sendEncoded(d.output, protocol.MsgSetWriteStyle, protocol.SetWriteStyle{StyleID: styleID}, protocol.EncodeSetWriteStyle); err != nil {
		return err
	}
	d.styleValid = true
	d.styleID = styleID
	return nil
}
