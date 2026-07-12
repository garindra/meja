package protocol

import (
	"fmt"
	"unicode/utf8"
)

func EncodeRelayoutBarrier(dst []byte, msg RelayoutBarrier) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.LayoutRevision)
	return w.Buf, nil
}

func DecodeRelayoutBarrier(payload []byte) (RelayoutBarrier, error) {
	r := PayloadReader{Data: payload}
	revision, err := r.Uvarint()
	if err != nil {
		return RelayoutBarrier{}, err
	}
	if err := r.Done(); err != nil {
		return RelayoutBarrier{}, err
	}
	return RelayoutBarrier{LayoutRevision: revision}, nil
}

func EncodeStyleInstall(dst []byte, msg StyleInstall) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.ID))
	if err := encodeStyle(&w, msg.Style); err != nil {
		return nil, err
	}
	return w.Buf, nil
}

func DecodeStyleInstall(payload []byte) (StyleInstall, error) {
	r := PayloadReader{Data: payload}
	id, err := r.Uvarint()
	if err != nil {
		return StyleInstall{}, err
	}
	style, err := decodeStyle(&r)
	if err != nil {
		return StyleInstall{}, err
	}
	if err := r.Done(); err != nil {
		return StyleInstall{}, err
	}
	return StyleInstall{ID: uint32(id), Style: style}, nil
}

func EncodeSetWritePosition(dst []byte, msg SetWritePosition) ([]byte, error) {
	if msg.Row < 0 || msg.Column < 0 {
		return nil, fmt.Errorf("negative write position")
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Row))
	w.Uvarint(uint64(msg.Column))
	return w.Buf, nil
}

func DecodeSetWritePosition(payload []byte) (SetWritePosition, error) {
	r := PayloadReader{Data: payload}
	row, err := readCoord(&r, MaxGridRows)
	if err != nil {
		return SetWritePosition{}, err
	}
	col, err := readCoord(&r, MaxGridCols)
	if err != nil {
		return SetWritePosition{}, err
	}
	if err := r.Done(); err != nil {
		return SetWritePosition{}, err
	}
	return SetWritePosition{Row: row, Column: col}, nil
}

func EncodeSetWriteStyle(dst []byte, msg SetWriteStyle) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.StyleID))
	return w.Buf, nil
}
func DecodeSetWriteStyle(payload []byte) (SetWriteStyle, error) {
	r := PayloadReader{Data: payload}
	id, err := r.Uvarint()
	if err != nil {
		return SetWriteStyle{}, err
	}
	if err := r.Done(); err != nil {
		return SetWriteStyle{}, err
	}
	return SetWriteStyle{StyleID: uint32(id)}, nil
}

func EncodeWriteText(dst []byte, msg WriteText) ([]byte, error) {
	if msg.CellWidth != 1 && msg.CellWidth != 2 {
		return nil, ErrInvalidCellWidth
	}
	if !utf8.Valid(msg.Text) {
		return nil, ErrInvalidRune
	}
	w := PayloadWriter{Buf: dst}
	w.Byte(msg.CellWidth)
	w.Bytes(msg.Text)
	return w.Buf, nil
}
func DecodeWriteText(payload []byte) (WriteText, error) {
	r := PayloadReader{Data: payload}
	width, err := r.Byte()
	if err != nil || (width != 1 && width != 2) {
		return WriteText{}, ErrInvalidCellWidth
	}
	data, err := r.Bytes(DefaultMaxFrameSize)
	if err != nil {
		return WriteText{}, err
	}
	if !utf8.Valid(data) {
		return WriteText{}, ErrInvalidRune
	}
	if err := r.Done(); err != nil {
		return WriteText{}, err
	}
	return WriteText{CellWidth: width, Text: data}, nil
}

func EncodeFill(dst []byte, msg Fill) ([]byte, error) {
	if msg.Columns <= 0 || !validRune(msg.Rune) || (msg.Width != 1 && msg.Width != 2) {
		return nil, fmt.Errorf("invalid fill")
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Columns))
	w.Uvarint(uint64(msg.Rune))
	w.Byte(msg.Width)
	return w.Buf, nil
}
func DecodeFill(payload []byte) (Fill, error) {
	r := PayloadReader{Data: payload}
	count, err := readCoord(&r, MaxGridCols)
	if err != nil || count <= 0 {
		return Fill{}, fmt.Errorf("invalid fill count")
	}
	raw, err := r.Uvarint()
	if err != nil || !validRune(rune(raw)) {
		return Fill{}, ErrInvalidRune
	}
	width, err := r.Byte()
	if err != nil || (width != 1 && width != 2) {
		return Fill{}, ErrInvalidCellWidth
	}
	if err := r.Done(); err != nil {
		return Fill{}, err
	}
	return Fill{Columns: count, Rune: rune(raw), Width: width}, nil
}

func EncodeCursorUpdate(dst []byte, msg CursorUpdate) ([]byte, error) {
	if msg.Cursor.X < 0 || msg.Cursor.Y < 0 {
		return nil, fmt.Errorf("negative cursor")
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Cursor.X))
	w.Uvarint(uint64(msg.Cursor.Y))
	w.Bool(msg.Visible)
	return w.Buf, nil
}
func DecodeCursorUpdate(payload []byte) (CursorUpdate, error) {
	r := PayloadReader{Data: payload}
	x, err := readCoord(&r, MaxGridCols)
	if err != nil {
		return CursorUpdate{}, err
	}
	y, err := readCoord(&r, MaxGridRows)
	if err != nil {
		return CursorUpdate{}, err
	}
	visible, err := r.Bool()
	if err != nil {
		return CursorUpdate{}, err
	}
	if err := r.Done(); err != nil {
		return CursorUpdate{}, err
	}
	return CursorUpdate{Cursor: Cursor{X: x, Y: y}, Visible: visible}, nil
}

func EncodeScroll(dst []byte, msg Scroll) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Varint(int64(msg.Delta))
	return w.Buf, nil
}
func DecodeScroll(payload []byte) (Scroll, error) {
	r := PayloadReader{Data: payload}
	delta, err := r.Varint()
	if err != nil || delta < -int64(MaxGridRows) || delta > int64(MaxGridRows) {
		return Scroll{}, fmt.Errorf("invalid scroll delta")
	}
	if err := r.Done(); err != nil {
		return Scroll{}, err
	}
	return Scroll{Delta: int(delta)}, nil
}

func EncodePresent(dst []byte, _ Present) ([]byte, error) { return dst, nil }
func DecodePresent(payload []byte) (Present, error) {
	if len(payload) != 0 {
		return Present{}, fmt.Errorf("present payload is not empty")
	}
	return Present{}, nil
}
