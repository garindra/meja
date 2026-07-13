package protocol

import "fmt"

func EncodeStreamOpen(dst []byte, msg StreamOpen) ([]byte, error) {
	if uint64(len(msg.StreamType)) > MaxStringLen {
		return nil, fmt.Errorf("encode StreamOpen: stream type too long")
	}
	w := PayloadWriter{Buf: dst}
	w.String(msg.StreamType)
	w.Uvarint(uint64(msg.Slot))
	w.Uvarint(msg.PaneID)
	return w.Buf, nil
}

func DecodeStreamOpen(payload []byte) (StreamOpen, error) {
	r := PayloadReader{Data: payload}
	streamType, err := r.String(MaxStringLen)
	if err != nil {
		return StreamOpen{}, fmt.Errorf("decode StreamOpen: %w", err)
	}
	slot, err := r.Uvarint()
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
	return StreamOpen{StreamType: streamType, Slot: uint8(slot), PaneID: paneID}, nil
}
