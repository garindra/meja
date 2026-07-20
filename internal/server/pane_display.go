package server

import (
	"fmt"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

func displayRune(cell cellWord, clusters *clusterStore) (rune, bool) {
	if cell.isBlank() {
		return ' ', true
	}
	if r, ok := cell.scalar(); ok {
		return r, true
	}
	text := cellTextFromStore(cell, clusters)
	r, size := utf8.DecodeRuneInString(text)
	return r, text != "" && size == len(text)
}

type displayCompiler struct {
	output        *renderOutput
	positionValid bool
	row, column   int
	cols          int
	styleValid    bool
	styleID       uint32
	styles        displayStyleSource
	installStyles bool
	clusters      *clusterStore
	styleMapper   func(uint32) uint32
	pendingOpcode protocol.DisplayOpcode
	pendingWidth  uint8
	pendingStyle  uint32
	pendingLength int
	pendingStart  int
	pendingFill   protocol.Fill
}

func (d *displayCompiler) cellStyleID(cell cellWord) uint32 {
	id := uint32(cell.styleID())
	if d.styleMapper != nil {
		return d.styleMapper(id)
	}
	return id
}

type displayStyleSource interface {
	LookupStyle(uint32) (protocol.Style, bool)
}

func newDisplayCompiler(output *renderOutput, styles displayStyleSource, clusters *clusterStore, cols int) *displayCompiler {
	return &displayCompiler{output: output, styles: styles, clusters: clusters, cols: cols}
}

func newLiveDisplayCompiler(output *renderOutput, terminal *TerminalState) *displayCompiler {
	return &displayCompiler{output: output, styles: terminal, installStyles: true, clusters: &terminal.clusters, cols: terminal.Cols}
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
		if _, singleRune := displayRune(cell, d.clusters); !singleRune {
			text := cellTextFromStore(cell, d.clusters)
			if err := d.finish(); err != nil {
				return err
			}
			if err := d.selectStyle(d.cellStyleID(cell)); err != nil {
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
				r, _ := displayRune(cell, d.clusters)
				if err := d.queueFill(protocol.Fill{Columns: count, Rune: r, Width: 1}, d.cellStyleID(cell)); err != nil {
					return err
				}
				d.advance(count)
				i = j
				continue
			}
		}
		width := cell.width()
		styleID := d.cellStyleID(cell)
		start := i
		for i < len(cells) {
			current := cells[i]
			if current.width() != width || current.width() == 0 {
				break
			}
			r, singleRune := displayRune(current, d.clusters)
			if !singleRune {
				break
			}
			if d.cellStyleID(current) != styleID {
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
		r, singleRune := displayRune(cell, d.clusters)
		if cell.width() != 1 || !singleRune || r != ' ' {
			break
		}
		cellStyle := d.cellStyleID(cell)
		if !checked || cellStyle != checkedStyle {
			if !d.blankStylesEquivalent(cellStyle, targetStyle) {
				break
			}
			checkedStyle, checked = cellStyle, true
		}
		end++
	}
	if end == start || end >= len(cells) {
		return start, false
	}
	next := cells[end]
	r, singleRune := displayRune(next, d.clusters)
	return end, next.width() > 0 && singleRune && r != ' ' && d.cellStyleID(next) == targetStyle
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
