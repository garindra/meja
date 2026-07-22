package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

// Display streams use this exact grammar, with no generic frame wrapper:
// [one-byte opcode][opcode-specific fields]... [PRESENT]. Text fields are
// [UTF-8 byte length uvarint][raw UTF-8 bytes]. WRITE_TEXT_UTF8 variants are
// for text the server has established as width 1; this decoder intentionally
// does not perform terminal wcwidth validation. Width-2 text uses WRITE_TEXT.
// WRITE_CLUSTER carries exactly one opaque display cluster and its total cell
// width; clients must not split or remeasure its UTF-8 text.
// Style fields are flags uvarint followed by two colors, each [color kind
// byte][kind-specific bytes].
// DisplayOpcode values are independent from framed control message IDs.
type DisplayOpcode byte

const (
	DisplayOpcodeNoop                 DisplayOpcode = 0x00
	DisplayOpcodeStartRender          DisplayOpcode = 0x01
	DisplayOpcodeStyleInstall         DisplayOpcode = 0x02
	DisplayOpcodeSetWritePosition     DisplayOpcode = 0x03
	DisplayOpcodeSetWriteStyle        DisplayOpcode = 0x04
	DisplayOpcodeWriteText            DisplayOpcode = 0x05
	DisplayOpcodeWriteTextUTF8        DisplayOpcode = 0x06
	DisplayOpcodeWriteTextUTF8Default DisplayOpcode = 0x07
	DisplayOpcodeFill                 DisplayOpcode = 0x08
	DisplayOpcodeCursorUpdate         DisplayOpcode = 0x09
	DisplayOpcodeScroll               DisplayOpcode = 0x0a
	DisplayOpcodeWriteCluster         DisplayOpcode = 0x0b
	DisplayOpcodePresent              DisplayOpcode = 0xff
)

const MaxDisplayTextBytes = DefaultMaxFrameSize

// DisplayCommand is a typed union. Only fields belonging to Opcode are used.
type DisplayCommand struct {
	Opcode         DisplayOpcode
	LayoutRevision uint64
	GridCols       int
	GridRows       int
	StyleID        uint32
	Style          Style
	Row, Column    int
	Width          uint8
	Text           []byte
	Fill           Fill
	Cursor         CursorUpdate
	Delta          int
}

type DisplayEncoder struct{ buf []byte }

func NewDisplayEncoder(dst []byte) *DisplayEncoder { return &DisplayEncoder{buf: dst} }
func (e *DisplayEncoder) Bytes() []byte            { return e.buf }

func (e *DisplayEncoder) AppendCommand(cmd DisplayCommand) error {
	switch cmd.Opcode {
	case DisplayOpcodeNoop:
		e.opcode(DisplayOpcodeNoop)
		return nil
	case DisplayOpcodeStartRender:
		return e.AppendStartRender(StartRender{LayoutRevision: cmd.LayoutRevision, Cols: cmd.GridCols, Rows: cmd.GridRows})
	case DisplayOpcodeStyleInstall:
		return e.AppendStyleInstall(StyleInstall{ID: cmd.StyleID, Style: cmd.Style})
	case DisplayOpcodeSetWritePosition:
		return e.AppendSetWritePosition(SetWritePosition{Row: cmd.Row, Column: cmd.Column})
	case DisplayOpcodeSetWriteStyle:
		return e.AppendSetWriteStyle(SetWriteStyle{StyleID: cmd.StyleID})
	case DisplayOpcodeWriteText:
		return e.AppendWriteText(WriteText{CellWidth: cmd.Width, Text: cmd.Text})
	case DisplayOpcodeWriteTextUTF8:
		return e.AppendWriteTextUTF8(cmd.Text)
	case DisplayOpcodeWriteTextUTF8Default:
		return e.AppendWriteTextUTF8Default(cmd.Text)
	case DisplayOpcodeWriteCluster:
		return e.AppendWriteCluster(WriteText{CellWidth: cmd.Width, Text: cmd.Text})
	case DisplayOpcodeFill:
		return e.AppendFill(cmd.Fill)
	case DisplayOpcodeCursorUpdate:
		return e.AppendCursorUpdate(cmd.Cursor)
	case DisplayOpcodeScroll:
		return e.AppendScroll(Scroll{Delta: cmd.Delta})
	case DisplayOpcodePresent:
		e.AppendPresent()
		return nil
	default:
		return fmt.Errorf("unknown display opcode 0x%02x", byte(cmd.Opcode))
	}
}

func (e *DisplayEncoder) opcode(op DisplayOpcode) { e.buf = append(e.buf, byte(op)) }

func (e *DisplayEncoder) AppendStartRender(msg StartRender) error {
	if msg.Cols <= 0 || msg.Rows <= 0 || uint64(msg.Cols) > MaxGridCols || uint64(msg.Rows) > MaxGridRows {
		return fmt.Errorf("invalid display grid %dx%d", msg.Cols, msg.Rows)
	}
	e.opcode(DisplayOpcodeStartRender)
	e.buf = appendUvarint(e.buf, msg.LayoutRevision)
	e.buf = appendUvarint(e.buf, uint64(msg.Cols))
	e.buf = appendUvarint(e.buf, uint64(msg.Rows))
	return nil
}

func (e *DisplayEncoder) AppendStyleInstall(msg StyleInstall) error {
	start := len(e.buf)
	e.opcode(DisplayOpcodeStyleInstall)
	e.buf = appendUvarint(e.buf, uint64(msg.ID))
	w := PayloadWriter{Buf: e.buf}
	if err := encodeStyle(&w, msg.Style); err != nil {
		e.buf = e.buf[:start]
		return err
	}
	e.buf = w.Buf
	return nil
}

func (e *DisplayEncoder) AppendSetWritePosition(msg SetWritePosition) error {
	if msg.Row < 0 || msg.Column < 0 {
		return fmt.Errorf("negative write position")
	}
	e.opcode(DisplayOpcodeSetWritePosition)
	e.buf = appendUvarint(e.buf, uint64(msg.Row))
	e.buf = appendUvarint(e.buf, uint64(msg.Column))
	return nil
}

func (e *DisplayEncoder) AppendSetWriteStyle(msg SetWriteStyle) error {
	e.opcode(DisplayOpcodeSetWriteStyle)
	e.buf = appendUvarint(e.buf, uint64(msg.StyleID))
	return nil
}

func (e *DisplayEncoder) AppendWriteText(msg WriteText) error {
	if msg.CellWidth != 1 && msg.CellWidth != 2 {
		return ErrInvalidCellWidth
	}
	if !utf8.Valid(msg.Text) {
		return ErrInvalidRune
	}
	e.opcode(DisplayOpcodeWriteText)
	e.buf = append(e.buf, msg.CellWidth)
	e.buf = appendText(e.buf, msg.Text)
	return nil
}

func (e *DisplayEncoder) AppendWriteTextUTF8(text []byte) error {
	if !utf8.Valid(text) {
		return ErrInvalidRune
	}
	e.opcode(DisplayOpcodeWriteTextUTF8)
	e.buf = appendText(e.buf, text)
	return nil
}

func (e *DisplayEncoder) AppendWriteTextUTF8Default(text []byte) error {
	if !utf8.Valid(text) {
		return ErrInvalidRune
	}
	e.opcode(DisplayOpcodeWriteTextUTF8Default)
	e.buf = appendText(e.buf, text)
	return nil
}

func (e *DisplayEncoder) AppendWriteCluster(msg WriteText) error {
	if msg.CellWidth != 1 && msg.CellWidth != 2 {
		return ErrInvalidCellWidth
	}
	if len(msg.Text) == 0 || !utf8.Valid(msg.Text) {
		return ErrInvalidRune
	}
	e.opcode(DisplayOpcodeWriteCluster)
	e.buf = append(e.buf, msg.CellWidth)
	e.buf = appendText(e.buf, msg.Text)
	return nil
}

func (e *DisplayEncoder) AppendFill(msg Fill) error {
	if msg.Columns <= 0 || uint64(msg.Columns) > MaxGridCells || !validRune(msg.Rune) || (msg.Width != 1 && msg.Width != 2) || msg.Columns%int(msg.Width) != 0 {
		return fmt.Errorf("invalid fill")
	}
	e.opcode(DisplayOpcodeFill)
	e.buf = appendUvarint(e.buf, uint64(msg.Columns))
	e.buf = appendUvarint(e.buf, uint64(msg.Rune))
	e.buf = append(e.buf, msg.Width)
	return nil
}

func (e *DisplayEncoder) AppendCursorUpdate(msg CursorUpdate) error {
	if msg.Cursor.X < 0 || msg.Cursor.Y < 0 {
		return fmt.Errorf("negative cursor")
	}
	e.opcode(DisplayOpcodeCursorUpdate)
	e.buf = appendUvarint(e.buf, uint64(msg.Cursor.X))
	e.buf = appendUvarint(e.buf, uint64(msg.Cursor.Y))
	if msg.Visible {
		e.buf = append(e.buf, 1)
	} else {
		e.buf = append(e.buf, 0)
	}
	return nil
}

func (e *DisplayEncoder) AppendScroll(msg Scroll) error {
	e.opcode(DisplayOpcodeScroll)
	e.buf = appendVarint(e.buf, int64(msg.Delta))
	return nil
}

func (e *DisplayEncoder) AppendPresent() { e.opcode(DisplayOpcodePresent) }

func appendUvarint(dst []byte, value uint64) []byte {
	var buf [10]byte
	for value >= 0x80 {
		buf[0] = byte(value) | 0x80
		dst = append(dst, buf[0])
		value >>= 7
	}
	return append(dst, byte(value))
}

func appendVarint(dst []byte, value int64) []byte {
	ux := uint64(value) << 1
	if value < 0 {
		ux = ^ux
	}
	return appendUvarint(dst, ux)
}

func appendText(dst, text []byte) []byte {
	dst = appendUvarint(dst, uint64(len(text)))
	return append(dst, text...)
}

type DisplayDecoder struct {
	r         *bufio.Reader
	bytesRead uint64
}

func NewDisplayDecoder(r io.Reader) *DisplayDecoder {
	return &DisplayDecoder{r: bufio.NewReader(r)}
}

func (d *DisplayDecoder) ReadCommand() (DisplayCommand, uint64, error) {
	start := d.bytesRead
	op, err := d.readByte()
	if err != nil {
		if errors.Is(err, io.EOF) && start == d.bytesRead {
			return DisplayCommand{}, 0, io.EOF
		}
		return DisplayCommand{}, d.bytesRead - start, io.ErrUnexpectedEOF
	}
	cmd := DisplayCommand{Opcode: DisplayOpcode(op)}
	switch cmd.Opcode {
	case DisplayOpcodeNoop:
	case DisplayOpcodeStartRender:
		cmd.LayoutRevision, err = d.readUvarint()
		if err == nil {
			cmd.GridCols, err = d.readCoord(MaxGridCols)
		}
		if err == nil && cmd.GridCols <= 0 {
			err = fmt.Errorf("invalid display grid columns")
		}
		if err == nil {
			cmd.GridRows, err = d.readCoord(MaxGridRows)
		}
		if err == nil && cmd.GridRows <= 0 {
			err = fmt.Errorf("invalid display grid rows")
		}
	case DisplayOpcodeStyleInstall:
		var id uint64
		id, err = d.readUvarint()
		if err == nil {
			if id > math.MaxUint32 {
				err = fmt.Errorf("style id %d exceeds uint32", id)
			} else {
				cmd.StyleID = uint32(id)
				cmd.Style, err = d.readStyle()
			}
		}
	case DisplayOpcodeSetWritePosition:
		cmd.Row, err = d.readCoord(MaxGridRows)
		if err == nil {
			cmd.Column, err = d.readCoord(MaxGridCols)
		}
	case DisplayOpcodeSetWriteStyle:
		var id uint64
		id, err = d.readUvarint()
		if err == nil {
			if id > math.MaxUint32 {
				err = fmt.Errorf("style id %d exceeds uint32", id)
			} else {
				cmd.StyleID = uint32(id)
			}
		}
	case DisplayOpcodeWriteText, DisplayOpcodeWriteCluster:
		cmd.Width, err = d.readByte()
		if err == nil && cmd.Width != 1 && cmd.Width != 2 {
			err = ErrInvalidCellWidth
		}
		if err == nil {
			cmd.Text, err = d.readText()
		}
	case DisplayOpcodeWriteTextUTF8, DisplayOpcodeWriteTextUTF8Default:
		cmd.Width = 1
		cmd.Text, err = d.readText()
	case DisplayOpcodeFill:
		cmd.Fill.Columns, err = d.readCoord(MaxGridCells)
		if err == nil && cmd.Fill.Columns <= 0 {
			err = fmt.Errorf("invalid fill count")
		}
		var raw uint64
		if err == nil {
			raw, err = d.readUvarint()
		}
		if err == nil {
			if raw > math.MaxInt32 || !validRune(rune(raw)) {
				err = ErrInvalidRune
			} else {
				cmd.Fill.Rune = rune(raw)
			}
		}
		if err == nil {
			cmd.Fill.Width, err = d.readByte()
			if err == nil && cmd.Fill.Width != 1 && cmd.Fill.Width != 2 {
				err = ErrInvalidCellWidth
			}
			if err == nil && cmd.Fill.Columns%int(cmd.Fill.Width) != 0 {
				err = fmt.Errorf("fill columns split a cell")
			}
		}
	case DisplayOpcodeCursorUpdate:
		cmd.Cursor.Cursor.X, err = d.readCoord(MaxGridCols)
		if err == nil {
			cmd.Cursor.Cursor.Y, err = d.readCoord(MaxGridRows)
		}
		if err == nil {
			cmd.Cursor.Visible, err = d.readBool()
		}
	case DisplayOpcodeScroll:
		var delta int64
		delta, err = d.readVarint()
		if err == nil && (delta < -int64(MaxGridRows) || delta > int64(MaxGridRows)) {
			err = fmt.Errorf("invalid scroll delta")
		}
		cmd.Delta = int(delta)
	case DisplayOpcodePresent:
	default:
		err = fmt.Errorf("unknown display opcode 0x%02x", op)
	}
	if err != nil {
		return DisplayCommand{}, d.bytesRead - start, normalizeDisplayReadError(err)
	}
	if cmd.Opcode == DisplayOpcodeWriteTextUTF8 || cmd.Opcode == DisplayOpcodeWriteTextUTF8Default || cmd.Opcode == DisplayOpcodeWriteText || cmd.Opcode == DisplayOpcodeWriteCluster {
		if !utf8.Valid(cmd.Text) {
			return DisplayCommand{}, d.bytesRead - start, ErrInvalidRune
		}
		if cmd.Opcode == DisplayOpcodeWriteCluster && len(cmd.Text) == 0 {
			return DisplayCommand{}, d.bytesRead - start, ErrInvalidRune
		}
	}
	return cmd, d.bytesRead - start, nil
}

func normalizeDisplayReadError(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (d *DisplayDecoder) readByte() (byte, error) {
	b, err := d.r.ReadByte()
	if err == nil {
		d.bytesRead++
	}
	return b, err
}

func (d *DisplayDecoder) readUvarint() (uint64, error) {
	var value uint64
	for shift := uint(0); shift < 64; shift += 7 {
		b, err := d.readByte()
		if err != nil {
			return 0, err
		}
		if shift == 63 && b > 1 {
			return 0, ErrLengthOverflow
		}
		value |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return value, nil
		}
	}
	return 0, ErrLengthOverflow
}

func (d *DisplayDecoder) readVarint() (int64, error) {
	value, err := d.readUvarint()
	if err != nil {
		return 0, err
	}
	decoded := int64(value >> 1)
	if value&1 != 0 {
		decoded = ^decoded
	}
	return decoded, nil
}

func (d *DisplayDecoder) readCoord(max uint64) (int, error) {
	value, err := d.readUvarint()
	if err != nil {
		return 0, err
	}
	if value > max || value > math.MaxInt {
		return 0, fmt.Errorf("coordinate %d exceeds max %d", value, max)
	}
	return int(value), nil
}

func (d *DisplayDecoder) readText() ([]byte, error) {
	n, err := d.readUvarint()
	if err != nil {
		return nil, err
	}
	if n > MaxDisplayTextBytes || n > uint64(math.MaxInt) {
		return nil, fmt.Errorf("text length %d exceeds max %d", n, MaxDisplayTextBytes)
	}
	text := make([]byte, int(n))
	read, err := io.ReadFull(d.r, text)
	d.bytesRead += uint64(read)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(text) {
		return nil, ErrInvalidRune
	}
	return text, nil
}

func (d *DisplayDecoder) readBool() (bool, error) {
	b, err := d.readByte()
	if err != nil {
		return false, err
	}
	if b > 1 {
		return false, ErrInvalidBoolean
	}
	return b == 1, nil
}

func (d *DisplayDecoder) readStyle() (Style, error) {
	flags, err := d.readUvarint()
	if err != nil {
		return Style{}, err
	}
	fg, err := d.readColor()
	if err != nil {
		return Style{}, err
	}
	bg, err := d.readColor()
	if err != nil {
		return Style{}, err
	}
	return Style{Bold: flags&styleFlagBold != 0, Dim: flags&styleFlagDim != 0, Blink: flags&styleFlagBlink != 0, Italic: flags&styleFlagItalic != 0, Underline: flags&styleFlagUnderline != 0, Reverse: flags&styleFlagReverse != 0, Invisible: flags&styleFlagInvisible != 0, FG: fg, BG: bg}, nil
}

func (d *DisplayDecoder) readColor() (Color, error) {
	kind, err := d.readByte()
	if err != nil {
		return Color{}, err
	}
	switch kind {
	case colorKindDefault:
		return Color{Mode: "default"}, nil
	case colorKindIndexed:
		index, err := d.readByte()
		return Color{Mode: "indexed", Index: index}, err
	case colorKindRGB:
		r, err := d.readByte()
		if err != nil {
			return Color{}, err
		}
		g, err := d.readByte()
		if err != nil {
			return Color{}, err
		}
		b, err := d.readByte()
		return Color{Mode: "rgb", R: r, G: g, B: b}, err
	default:
		return Color{}, fmt.Errorf("unknown color kind %d", kind)
	}
}
