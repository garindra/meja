package server

import (
	"fmt"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

func displayRune(cell cellWord, text string) (rune, bool) {
	if text == "" {
		return ' ', cell.width() == 1
	}
	r, size := utf8.DecodeRuneInString(text)
	return r, size == len(text)
}

type displayCompiler struct {
	output        *renderOutput
	positionValid bool
	row, column   int
	cols, rows    int
	styleValid    bool
	styleID       uint32
	styles        displayStyleSource
	installStyles bool
	cellText      func(cellWord) string
	pendingOpcode protocol.DisplayOpcode
	pendingWidth  uint8
	pendingStyle  uint32
	pendingLength int
	pendingStart  int
	pendingFill   protocol.Fill
}

type displayStyleSource interface {
	LookupStyle(uint32) (protocol.Style, bool)
}

type displayStyleMap map[uint32]protocol.Style

func (m displayStyleMap) LookupStyle(id uint32) (protocol.Style, bool) {
	style, ok := m[id]
	return style, ok
}

func newDisplayCompiler(output *renderOutput, styles displayStyleSource, cellText func(cellWord) string, cols, rows int) *displayCompiler {
	return &displayCompiler{output: output, styles: styles, cellText: cellText, cols: cols, rows: rows}
}

func newLiveDisplayCompiler(output *renderOutput, terminal *TerminalState) *displayCompiler {
	return &displayCompiler{output: output, styles: terminal, installStyles: true, cellText: terminal.cellText, cols: terminal.Cols, rows: terminal.Rows}
}

func (d *displayCompiler) writeCells(row, column int, cells []cellWord) error {
	for i := 0; i < len(cells); {
		cell := cells[i]
		if cell.width() == 0 {
			i++
			continue
		}
		if err := d.moveTo(row, column+i); err != nil {
			return err
		}
		text := d.cellText(cell)
		if _, singleRune := displayRune(cell, text); !singleRune {
			if err := d.finish(); err != nil {
				return err
			}
			if err := d.selectStyle(uint32(cell.styleID())); err != nil {
				return err
			}
			if err := d.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteCluster, Width: cell.width(), Text: []byte(text)}); err != nil {
				return err
			}
			d.advance(int(cell.width()))
			i += int(cell.width())
			continue
		}
		if cell.width() == 1 {
			j := i + 1
			for j < len(cells) && cells[j] == cell {
				j++
			}
			if j-i >= 3 {
				count := j - i
				r, _ := displayRune(cell, text)
				if err := d.queueFill(protocol.Fill{Columns: count, Rune: r, Width: 1}, uint32(cell.styleID())); err != nil {
					return err
				}
				d.advance(count)
				i = j
				continue
			}
		}
		width := cell.width()
		styleID := uint32(cell.styleID())
		start := i
		for i < len(cells) {
			current := cells[i]
			if current.width() != width || current.width() == 0 {
				break
			}
			r, singleRune := displayRune(current, d.cellText(current))
			if !singleRune {
				break
			}
			if uint32(current.styleID()) != styleID {
				end, ok := d.blankBridgeEnd(cells, i, styleID)
				if !ok {
					break
				}
				for i < end {
					if err := d.queueRune(opcodeForWidth(width, styleID, d.styleValid, d.styleID), width, styleID, ' '); err != nil {
						return err
					}
					i++
				}
				continue
			}
			opcode := opcodeForWidth(width, styleID, d.styleValid, d.styleID)
			if err := d.queueRune(opcode, width, styleID, r); err != nil {
				return err
			}
			i += int(current.width())
		}
		d.advance(i - start)
	}
	return nil
}

func opcodeForWidth(width uint8, styleID uint32, styleValid bool, selectedStyle uint32) protocol.DisplayOpcode {
	if width != 1 {
		return protocol.DisplayOpcodeWriteText
	}
	if styleID == 0 && (!styleValid || selectedStyle != 0) {
		return protocol.DisplayOpcodeWriteTextUTF8Default
	}
	return protocol.DisplayOpcodeWriteTextUTF8
}

func (d *displayCompiler) blankBridgeEnd(cells []cellWord, start int, targetStyle uint32) (int, bool) {
	end := start
	var checkedStyle uint32
	checked := false
	for end < len(cells) {
		cell := cells[end]
		r, singleRune := displayRune(cell, d.cellText(cell))
		if cell.width() != 1 || !singleRune || r != ' ' {
			break
		}
		if !checked || uint32(cell.styleID()) != checkedStyle {
			if !d.blankStylesEquivalent(uint32(cell.styleID()), targetStyle) {
				break
			}
			checkedStyle, checked = uint32(cell.styleID()), true
		}
		end++
	}
	if end == start || end >= len(cells) {
		return start, false
	}
	next := cells[end]
	r, singleRune := displayRune(next, d.cellText(next))
	return end, next.width() > 0 && singleRune && r != ' ' && uint32(next.styleID()) == targetStyle
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
	if err := d.finish(); err != nil {
		return err
	}
	if err := d.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: row, Column: column}); err != nil {
		return err
	}
	d.positionValid = true
	d.row = row
	d.column = column
	return nil
}

func (d *displayCompiler) queueRune(opcode protocol.DisplayOpcode, width uint8, styleID uint32, r rune) error {
	if d.pendingOpcode != opcode || d.pendingWidth != width || d.pendingStyle != styleID {
		if err := d.finish(); err != nil {
			return err
		}
		if opcode != protocol.DisplayOpcodeWriteTextUTF8Default {
			if err := d.selectStyle(styleID); err != nil {
				return err
			}
		}
		if err := d.openText(opcode, width, styleID); err != nil {
			return err
		}
	}
	runeBytes := utf8.RuneLen(r)
	if len(d.output.pending)+runeBytes > renderStreamChunkSize && len(d.output.pending) > d.pendingStart {
		if err := d.finish(); err != nil {
			return err
		}
		if err := d.output.commit(); err != nil {
			return err
		}
		if err := d.openText(opcode, width, styleID); err != nil {
			return err
		}
	}
	d.output.pending = utf8.AppendRune(d.output.pending, r)
	return nil
}

func (d *displayCompiler) openText(opcode protocol.DisplayOpcode, width uint8, styleID uint32) error {
	if len(d.output.pending) >= renderStreamChunkSize {
		if err := d.output.commit(); err != nil {
			return err
		}
	}
	d.output.pending = append(d.output.pending, byte(opcode))
	if opcode == protocol.DisplayOpcodeWriteText {
		d.output.pending = append(d.output.pending, width)
	}
	d.pendingLength = len(d.output.pending)
	// Reserve a fixed-size, valid uvarint so the payload can be written directly
	// into the stream buffer and its length backpatched without moving any text.
	d.output.pending = append(d.output.pending, 0, 0, 0, 0, 0)
	d.pendingStart = len(d.output.pending)
	d.pendingOpcode, d.pendingWidth, d.pendingStyle = opcode, width, styleID
	return nil
}

func (d *displayCompiler) queueFill(fill protocol.Fill, styleID uint32) error {
	if d.pendingOpcode == protocol.DisplayOpcodeFill && d.pendingStyle == styleID && d.pendingFill.Rune == fill.Rune && d.pendingFill.Width == fill.Width {
		d.pendingFill.Columns += fill.Columns
		return nil
	}
	if err := d.finish(); err != nil {
		return err
	}
	if err := d.selectStyle(styleID); err != nil {
		return err
	}
	d.pendingOpcode, d.pendingStyle = protocol.DisplayOpcodeFill, styleID
	d.pendingFill = fill
	return nil
}

func (d *displayCompiler) finish() error {
	opcode := d.pendingOpcode
	if opcode == 0 {
		return nil
	}
	d.pendingOpcode = 0
	if opcode == protocol.DisplayOpcodeFill {
		fill := d.pendingFill
		d.pendingFill = protocol.Fill{}
		return d.output.append(protocol.DisplayCommand{Opcode: opcode, Fill: fill})
	}
	putPaddedUvarint5(d.output.pending[d.pendingLength:d.pendingStart], uint32(len(d.output.pending)-d.pendingStart))
	return nil
}

func putPaddedUvarint5(dst []byte, value uint32) {
	for i := 0; i < 4; i++ {
		dst[i] = byte(value) | 0x80
		value >>= 7
	}
	dst[4] = byte(value)
}

func (d *displayCompiler) advance(columns int) {
	d.column += columns
	if d.column == d.cols {
		d.row++
		d.column = 0
	}
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
