package protocol

import "fmt"

func EncodeStreamOpen(dst []byte, msg StreamOpen) ([]byte, error) {
	if uint64(len(msg.StreamType)) > MaxStringLen {
		return nil, fmt.Errorf("encode StreamOpen: stream type too long")
	}
	w := PayloadWriter{Buf: dst}
	w.String(msg.StreamType)
	return w.Buf, nil
}

func DecodeStreamOpen(payload []byte) (StreamOpen, error) {
	r := PayloadReader{Data: payload}
	streamType, err := r.String(MaxStringLen)
	if err != nil {
		return StreamOpen{}, fmt.Errorf("decode StreamOpen: %w", err)
	}
	if err := r.Done(); err != nil {
		return StreamOpen{}, fmt.Errorf("decode StreamOpen: %w", err)
	}
	return StreamOpen{StreamType: streamType}, nil
}
