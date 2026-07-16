package protocol

import "fmt"

func EncodeSessionAttach(dst []byte, msg SessionAttach) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Version))
	w.Uvarint(msg.SessionID)
	w.String(msg.Token)
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}

func DecodeSessionAttach(payload []byte) (SessionAttach, error) {
	r := PayloadReader{Data: payload}
	version, err := r.Uvarint()
	if err != nil {
		return SessionAttach{}, fmt.Errorf("decode session attach: %w", err)
	}
	sessionID, err := r.Uvarint()
	if err != nil {
		return SessionAttach{}, fmt.Errorf("decode session attach: %w", err)
	}
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
	return SessionAttach{Version: int(version), SessionID: sessionID, Token: token, Cols: uint16(cols), Rows: uint16(rows)}, nil
}

func EncodeSessionAttachOK(dst []byte, msg SessionAttachOK) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Version))
	w.Uvarint(msg.SessionID)
	w.String(msg.ResumeToken)
	w.Uvarint(msg.Generation)
	return w.Buf, nil
}

func DecodeSessionAttachOK(payload []byte) (SessionAttachOK, error) {
	r := PayloadReader{Data: payload}
	version, err := r.Uvarint()
	if err != nil {
		return SessionAttachOK{}, err
	}
	id, err := r.Uvarint()
	if err != nil {
		return SessionAttachOK{}, err
	}
	token, err := r.String(MaxStringLen)
	if err != nil {
		return SessionAttachOK{}, err
	}
	generation, err := r.Uvarint()
	if err != nil {
		return SessionAttachOK{}, err
	}
	if err := r.Done(); err != nil {
		return SessionAttachOK{}, err
	}
	return SessionAttachOK{Version: int(version), SessionID: id, ResumeToken: token, Generation: generation}, nil
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

func EncodeSessionResume(dst []byte, msg SessionResume) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Version))
	w.Uvarint(msg.SessionID)
	w.String(msg.ResumeToken)
	w.Uvarint(msg.Generation)
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	return w.Buf, nil
}
func DecodeSessionResume(payload []byte) (SessionResume, error) {
	r := PayloadReader{Data: payload}
	version, err := r.Uvarint()
	if err != nil {
		return SessionResume{}, err
	}
	id, err := r.Uvarint()
	if err != nil {
		return SessionResume{}, err
	}
	token, err := r.String(MaxStringLen)
	if err != nil {
		return SessionResume{}, err
	}
	generation, err := r.Uvarint()
	if err != nil {
		return SessionResume{}, err
	}
	cols, err := r.Uvarint()
	if err != nil {
		return SessionResume{}, fmt.Errorf("decode session resume columns: %w", err)
	}
	if cols > 65535 {
		return SessionResume{}, fmt.Errorf("decode session resume columns: %d", cols)
	}
	rows, err := r.Uvarint()
	if err != nil {
		return SessionResume{}, fmt.Errorf("decode session resume rows: %w", err)
	}
	if rows > 65535 {
		return SessionResume{}, fmt.Errorf("decode session resume rows: %d", rows)
	}
	if err := r.Done(); err != nil {
		return SessionResume{}, err
	}
	return SessionResume{Version: int(version), SessionID: id, ResumeToken: token, Generation: generation, Cols: uint16(cols), Rows: uint16(rows)}, nil
}

func EncodeSessionResumeOK(dst []byte, msg SessionResumeOK) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Version))
	w.Uvarint(msg.SessionID)
	w.String(msg.ResumeToken)
	w.Uvarint(msg.Generation)
	return w.Buf, nil
}
func DecodeSessionResumeOK(payload []byte) (SessionResumeOK, error) {
	r := PayloadReader{Data: payload}
	version, err := r.Uvarint()
	if err != nil {
		return SessionResumeOK{}, err
	}
	id, err := r.Uvarint()
	if err != nil {
		return SessionResumeOK{}, err
	}
	token, err := r.String(MaxStringLen)
	if err != nil {
		return SessionResumeOK{}, err
	}
	generation, err := r.Uvarint()
	if err != nil {
		return SessionResumeOK{}, err
	}
	if err := r.Done(); err != nil {
		return SessionResumeOK{}, err
	}
	return SessionResumeOK{Version: int(version), SessionID: id, ResumeToken: token, Generation: generation}, nil
}
