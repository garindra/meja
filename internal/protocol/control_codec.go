package protocol

import "fmt"

func EncodeSessionAttach(dst []byte, msg SessionAttach) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.Token)
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}

func DecodeSessionAttach(payload []byte) (SessionAttach, error) {
	r := PayloadReader{Data: payload}
	token, err := r.String(MaxStringLen)
	if err != nil {
		return SessionAttach{}, fmt.Errorf("decode session attach: %w", err)
	}
	cols, err := r.Uvarint()
	if err != nil {
		return SessionAttach{}, fmt.Errorf("decode session attach columns: %w", err)
	}
	if cols > 65535 {
		return SessionAttach{}, fmt.Errorf("decode session attach columns: %d", cols)
	}
	rows, err := r.Uvarint()
	if err != nil {
		return SessionAttach{}, fmt.Errorf("decode session attach rows: %w", err)
	}
	if rows > 65535 {
		return SessionAttach{}, fmt.Errorf("decode session attach rows: %d", rows)
	}
	if err := r.Done(); err != nil {
		return SessionAttach{}, fmt.Errorf("decode session attach: %w", err)
	}
	return SessionAttach{Token: token, Cols: uint16(cols), Rows: uint16(rows)}, nil
}

func EncodeSessionAttachOK(dst []byte, msg SessionAttachOK) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.ResumeToken)
	return w.Buf, nil
}

func DecodeSessionAttachOK(payload []byte) (SessionAttachOK, error) {
	r := PayloadReader{Data: payload}
	token, err := r.String(MaxStringLen)
	if err != nil {
		return SessionAttachOK{}, err
	}
	if err := r.Done(); err != nil {
		return SessionAttachOK{}, err
	}
	return SessionAttachOK{ResumeToken: token}, nil
}

func EncodeSessionAttachFailed(dst []byte, msg SessionAttachFailed) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.Reason)
	return w.Buf, nil
}

func DecodeSessionAttachFailed(payload []byte) (SessionAttachFailed, error) {
	r := PayloadReader{Data: payload}
	reason, err := r.String(MaxStringLen)
	if err != nil {
		return SessionAttachFailed{}, err
	}
	if err := r.Done(); err != nil {
		return SessionAttachFailed{}, err
	}
	return SessionAttachFailed{Reason: reason}, nil
}

func EncodeClientResume(dst []byte, msg ClientResume) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.ResumeToken)
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}

func DecodeClientResume(payload []byte) (ClientResume, error) {
	r := PayloadReader{Data: payload}
	token, err := r.String(MaxStringLen)
	if err != nil {
		return ClientResume{}, err
	}
	cols, err := r.Uvarint()
	if err != nil {
		return ClientResume{}, fmt.Errorf("decode client resume columns: %w", err)
	}
	if cols > 65535 {
		return ClientResume{}, fmt.Errorf("decode client resume columns: %d", cols)
	}
	rows, err := r.Uvarint()
	if err != nil {
		return ClientResume{}, fmt.Errorf("decode client resume rows: %w", err)
	}
	if rows > 65535 {
		return ClientResume{}, fmt.Errorf("decode client resume rows: %d", rows)
	}
	if err := r.Done(); err != nil {
		return ClientResume{}, err
	}
	return ClientResume{ResumeToken: token, Cols: uint16(cols), Rows: uint16(rows)}, nil
}

func EncodeClientResumeOK(dst []byte, _ ClientResumeOK) ([]byte, error) {
	return dst, nil
}

func DecodeClientResumeOK(payload []byte) (ClientResumeOK, error) {
	r := PayloadReader{Data: payload}
	if err := r.Done(); err != nil {
		return ClientResumeOK{}, err
	}
	return ClientResumeOK{}, nil
}

func EncodeFrontendInputBytes(dst []byte, msg FrontendInputBytes) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.LayoutRevision))
	w.Bool(msg.SourceIdle)
	w.Raw(msg.Data)
	return w.Buf, nil
}

func DecodeFrontendInputBytes(payload []byte) (FrontendInputBytes, error) {
	r := PayloadReader{Data: payload}
	layoutRevision, err := r.Uvarint()
	if err != nil {
		return FrontendInputBytes{}, fmt.Errorf("decode FrontendInputBytes: %w", err)
	}
	sourceIdle, err := r.Bool()
	if err != nil {
		return FrontendInputBytes{}, fmt.Errorf("decode FrontendInputBytes: %w", err)
	}
	return FrontendInputBytes{LayoutRevision: ClientLayoutRevision(layoutRevision), SourceIdle: sourceIdle, Data: r.Remaining()}, nil
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
