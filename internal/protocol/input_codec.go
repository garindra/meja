package protocol

import "fmt"

func EncodeCreatePane(dst []byte, msg CreatePane) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.Cwd)
	var err error
	w.Buf, err = encodeArgv(w.Buf, msg.Argv)
	if err != nil {
		return nil, fmt.Errorf("encode CreatePane: %w", err)
	}
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}

func DecodeCreatePane(payload []byte) (CreatePane, error) {
	r := PayloadReader{Data: payload}
	cwd, err := r.String(MaxStringLen)
	if err != nil {
		return CreatePane{}, fmt.Errorf("decode CreatePane: %w", err)
	}
	argv, err := decodeArgv(&r)
	if err != nil {
		return CreatePane{}, fmt.Errorf("decode CreatePane: %w", err)
	}
	cols, err := r.Uvarint()
	if err != nil {
		return CreatePane{}, fmt.Errorf("decode CreatePane: %w", err)
	}
	rows, err := r.Uvarint()
	if err != nil {
		return CreatePane{}, fmt.Errorf("decode CreatePane: %w", err)
	}
	if cols == 0 || rows == 0 || cols > MaxGridCols || rows > MaxGridRows {
		return CreatePane{}, fmt.Errorf("decode CreatePane: invalid size %dx%d", cols, rows)
	}
	if err := r.Done(); err != nil {
		return CreatePane{}, fmt.Errorf("decode CreatePane: %w", err)
	}
	return CreatePane{Cwd: cwd, Argv: argv, Cols: uint16(cols), Rows: uint16(rows)}, nil
}

func EncodePaneCreated(dst []byte, msg PaneCreated) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	return w.Buf, nil
}

func DecodePaneCreated(payload []byte) (PaneCreated, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return PaneCreated{}, fmt.Errorf("decode PaneCreated: %w", err)
	}
	if err := r.Done(); err != nil {
		return PaneCreated{}, fmt.Errorf("decode PaneCreated: %w", err)
	}
	return PaneCreated{PaneID: paneID}, nil
}

func EncodePaneExited(dst []byte, msg PaneExited) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	w.Varint(int64(msg.ExitCode))
	w.String(msg.Signal)
	return w.Buf, nil
}

func DecodePaneExited(payload []byte) (PaneExited, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return PaneExited{}, fmt.Errorf("decode PaneExited: %w", err)
	}
	exitCode, err := r.Varint()
	if err != nil {
		return PaneExited{}, fmt.Errorf("decode PaneExited: %w", err)
	}
	signal, err := r.String(MaxStringLen)
	if err != nil {
		return PaneExited{}, fmt.Errorf("decode PaneExited: %w", err)
	}
	if err := r.Done(); err != nil {
		return PaneExited{}, fmt.Errorf("decode PaneExited: %w", err)
	}
	return PaneExited{PaneID: paneID, ExitCode: int(exitCode), Signal: signal}, nil
}

func EncodeInputBytes(dst []byte, msg InputBytes) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	w.Raw(msg.Data)
	return w.Buf, nil
}

func DecodeInputBytes(payload []byte) (InputBytesView, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return InputBytesView{}, fmt.Errorf("decode InputBytes: %w", err)
	}
	return InputBytesView{PaneID: paneID, Data: r.Remaining()}, nil
}

func EncodeResizePane(dst []byte, msg ResizePane) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}

func DecodeResizePane(payload []byte) (ResizePane, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return ResizePane{}, fmt.Errorf("decode ResizePane: %w", err)
	}
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
	return ResizePane{PaneID: paneID, Cols: uint16(cols), Rows: uint16(rows)}, nil
}

func EncodeRequestPaneSnapshot(dst []byte, msg RequestPaneSnapshot) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	return w.Buf, nil
}

func DecodeRequestPaneSnapshot(payload []byte) (RequestPaneSnapshot, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return RequestPaneSnapshot{}, fmt.Errorf("decode RequestPaneSnapshot: %w", err)
	}
	if err := r.Done(); err != nil {
		return RequestPaneSnapshot{}, fmt.Errorf("decode RequestPaneSnapshot: %w", err)
	}
	return RequestPaneSnapshot{PaneID: paneID}, nil
}
