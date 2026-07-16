package protocol

import "fmt"

func EncodeInputBytes(dst []byte, msg InputBytes) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Raw(msg.Data)
	return w.Buf, nil
}

func DecodeInputBytes(payload []byte) (InputBytesView, error) {
	r := PayloadReader{Data: payload}
	return InputBytesView{Data: r.Remaining()}, nil
}

func EncodeResizePane(dst []byte, msg ResizePane) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}

func DecodeResizePane(payload []byte) (ResizePane, error) {
	r := PayloadReader{Data: payload}
	cols, err := r.Uvarint()
	if err != nil {
		return ResizePane{}, fmt.Errorf("decode ResizePane: %w", err)
	}
	rows, err := r.Uvarint()
	if err != nil {
		return ResizePane{}, fmt.Errorf("decode ResizePane: %w", err)
	}
	if cols == 0 || rows == 0 || cols > MaxGridCols || rows > MaxGridRows {
		return ResizePane{}, fmt.Errorf("decode ResizePane: invalid size %dx%d", cols, rows)
	}
	if err := r.Done(); err != nil {
		return ResizePane{}, fmt.Errorf("decode ResizePane: %w", err)
	}
	return ResizePane{Cols: uint16(cols), Rows: uint16(rows)}, nil
}
