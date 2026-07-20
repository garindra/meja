package protocol

import "fmt"

func EncodeFrontendInputBytes(dst []byte, msg FrontendInputBytes) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.LayoutRevision)
	w.Raw(msg.Data)
	return w.Buf, nil
}

func DecodeFrontendInputBytes(payload []byte) (FrontendInputBytes, error) {
	r := PayloadReader{Data: payload}
	layoutRevision, err := r.Uvarint()
	if err != nil {
		return FrontendInputBytes{}, fmt.Errorf("decode FrontendInputBytes: %w", err)
	}
	return FrontendInputBytes{LayoutRevision: layoutRevision, Data: r.Remaining()}, nil
}

func EncodeFrontendResize(dst []byte, msg FrontendResize) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}

func DecodeFrontendResize(payload []byte) (FrontendResize, error) {
	r := PayloadReader{Data: payload}
	cols, err := r.Uvarint()
	if err != nil {
		return FrontendResize{}, fmt.Errorf("decode FrontendResize: %w", err)
	}
	rows, err := r.Uvarint()
	if err != nil {
		return FrontendResize{}, fmt.Errorf("decode FrontendResize: %w", err)
	}
	if cols == 0 || rows == 0 || cols > MaxGridCols || rows > MaxGridRows {
		return FrontendResize{}, fmt.Errorf("decode FrontendResize: invalid size %dx%d", cols, rows)
	}
	if err := r.Done(); err != nil {
		return FrontendResize{}, fmt.Errorf("decode FrontendResize: %w", err)
	}
	return FrontendResize{Cols: uint16(cols), Rows: uint16(rows)}, nil
}

func EncodeFrontendTerminalWrite(dst []byte, msg FrontendTerminalWrite) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Raw(msg.Data)
	return w.Buf, nil
}

func DecodeFrontendTerminalWrite(payload []byte) (FrontendTerminalWrite, error) {
	r := PayloadReader{Data: payload}
	return FrontendTerminalWrite{Data: r.Remaining()}, nil
}

func EncodeFrontendRegisterTerminalExitCommand(dst []byte, msg FrontendRegisterTerminalExitCommand) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Raw(msg.Data)
	return w.Buf, nil
}

func DecodeFrontendRegisterTerminalExitCommand(payload []byte) (FrontendRegisterTerminalExitCommand, error) {
	r := PayloadReader{Data: payload}
	return FrontendRegisterTerminalExitCommand{Data: r.Remaining()}, nil
}
