package protocol

import "fmt"

func EncodeStreamOpen(dst []byte, msg StreamOpen) ([]byte, error) {
	if uint64(len(msg.StreamType)) > MaxStringLen {
		return nil, fmt.Errorf("encode StreamOpen: stream type too long")
	}
	w := PayloadWriter{Buf: dst}
	w.String(msg.StreamType)
	w.Uvarint(msg.PaneID)
	return w.Buf, nil
}

func DecodeStreamOpen(payload []byte) (StreamOpen, error) {
	r := PayloadReader{Data: payload}
	streamType, err := r.String(MaxStringLen)
	if err != nil {
		return StreamOpen{}, fmt.Errorf("decode StreamOpen: %w", err)
	}
	paneID, err := r.Uvarint()
	if err != nil {
		return StreamOpen{}, fmt.Errorf("decode StreamOpen: %w", err)
	}
	if err := r.Done(); err != nil {
		return StreamOpen{}, fmt.Errorf("decode StreamOpen: %w", err)
	}
	return StreamOpen{StreamType: streamType, PaneID: paneID}, nil
}

func EncodeClientHello(dst []byte, msg ClientHello) ([]byte, error) {
	if msg.Version < 0 {
		return nil, fmt.Errorf("encode ClientHello: negative version")
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Version))
	return w.Buf, nil
}

func DecodeClientHello(payload []byte) (ClientHello, error) {
	r := PayloadReader{Data: payload}
	version, err := r.Uvarint()
	if err != nil {
		return ClientHello{}, fmt.Errorf("decode ClientHello: %w", err)
	}
	if err := r.Done(); err != nil {
		return ClientHello{}, fmt.Errorf("decode ClientHello: %w", err)
	}
	return ClientHello{Version: int(version)}, nil
}
