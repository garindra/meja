package server

import (
	"fmt"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

func displayRune(cell protocol.Cell) (rune, bool) {
	if cell.Cluster == "" {
		return ' ', cell.Width == 1
	}
	r, size := utf8.DecodeRuneInString(cell.Cluster)
	return r, size == len(cell.Cluster)
}

type displayCompiler struct {
	output        *renderOutput
	positionValid bool
	row, column   int
	styleValid    bool
	styleID       uint32
	textScratch   []byte
	styles        displayStyleSource
	installStyles bool
}

type displayStyleSource interface {
	LookupStyle(uint32) (protocol.Style, bool)
}

type displayStyleMap map[uint32]protocol.Style

func (m displayStyleMap) LookupStyle(id uint32) (protocol.Style, bool) {
	style, ok := m[id]
	return style, ok
}

func newDisplayCompiler(output *renderOutput, styles map[uint32]protocol.Style) *displayCompiler {
	return &displayCompiler{output: output, styles: displayStyleMap(styles), textScratch: output.textScratch[:0]}
}

func newLiveDisplayCompiler(output *renderOutput, styles displayStyleSource) *displayCompiler {
	return &displayCompiler{output: output, styles: styles, textScratch: output.textScratch[:0], installStyles: true}
}

func (d *displayCompiler) writeCells(row, column int, cells []protocol.Cell) error {
	defer func() { d.output.textScratch = d.textScratch[:0] }()
	for i := 0; i < len(cells); {
		cell := cells[i]
		if cell.Width == 0 {
			i++
			continue
		}
		if err := d.moveTo(row, column+i); err != nil {
			return err
		}
		if _, singleRune := displayRune(cell); !singleRune {
			if err := d.selectStyle(cell.StyleID); err != nil {
				return err
			}
			if err := d.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteCluster, Width: cell.Width, Text: []byte(cell.Cluster)}); err != nil {
				return err
			}
			d.column += int(cell.Width)
			i += int(cell.Width)
			continue
		}
		if cell.Width == 1 {
			j := i + 1
			for j < len(cells) && cells[j] == cell {
				j++
			}
			if j-i >= 3 {
				count := j - i
				if err := d.selectStyle(cell.StyleID); err != nil {
					return err
				}
				r, _ := displayRune(cell)
				if err := d.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeFill, Fill: protocol.Fill{Columns: count, Rune: r, Width: 1}}); err != nil {
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
			r, singleRune := displayRune(current)
			if !singleRune {
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
			d.textScratch = utf8.AppendRune(d.textScratch, r)
			i += int(current.Width)
		}
		if width == 1 && styleID == 0 && (!d.styleValid || d.styleID != 0) {
			if err := d.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8Default, Text: d.textScratch}); err != nil {
				return err
			}
		} else {
			if err := d.selectStyle(styleID); err != nil {
				return err
			}
			opcode := protocol.DisplayOpcodeWriteText
			if width == 1 {
				opcode = protocol.DisplayOpcodeWriteTextUTF8
			}
			if err := d.output.append(protocol.DisplayCommand{Opcode: opcode, Width: width, Text: d.textScratch}); err != nil {
				return err
			}
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
		r, singleRune := displayRune(cell)
		if cell.Width != 1 || !singleRune || r != ' ' {
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
	r, singleRune := displayRune(next)
	return end, next.Width > 0 && singleRune && r != ' ' && next.StyleID == targetStyle
}

func (d *displayCompiler) blankStylesEquivalent(leftID, rightID uint32) bool {
	if leftID == rightID {
		return true
	}
	left, leftOK := d.styles.LookupStyle(leftID)
	right, rightOK := d.styles.LookupStyle(rightID)
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
	if err := d.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: row, Column: column}); err != nil {
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
	if d.installStyles {
		style, ok := d.styles.LookupStyle(styleID)
		if !ok {
			return fmt.Errorf("terminal style %d is undefined", styleID)
		}
		if err := installStyle(d.output, styleID, style); err != nil {
			return err
		}
	}
	if err := d.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWriteStyle, StyleID: styleID}); err != nil {
		return err
	}
	d.styleValid = true
	d.styleID = styleID
	return nil
}
