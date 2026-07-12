package protocol

import "fmt"

func encodeRect(w *PayloadWriter, rect Rect) error {
	if rect.X < 0 || rect.Y < 0 || rect.Width < 0 || rect.Height < 0 {
		return fmt.Errorf("negative rect")
	}
	w.Uvarint(uint64(rect.X))
	w.Uvarint(uint64(rect.Y))
	w.Uvarint(uint64(rect.Width))
	w.Uvarint(uint64(rect.Height))
	return nil
}

func decodeRect(r *PayloadReader) (Rect, error) {
	x, err := r.Uvarint()
	if err != nil {
		return Rect{}, err
	}
	y, err := r.Uvarint()
	if err != nil {
		return Rect{}, err
	}
	width, err := r.Uvarint()
	if err != nil {
		return Rect{}, err
	}
	height, err := r.Uvarint()
	if err != nil {
		return Rect{}, err
	}
	return Rect{X: int(x), Y: int(y), Width: int(width), Height: int(height)}, nil
}

func EncodeWindowLayout(dst []byte, msg WindowLayout) ([]byte, error) {
	if uint64(len(msg.Panes)) > MaxVisiblePanes {
		return nil, fmt.Errorf("encode WindowLayout: pane count %d exceeds max %d", len(msg.Panes), MaxVisiblePanes)
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.WindowID)
	w.Uvarint(msg.FocusedPaneID)
	w.Uvarint(msg.LayoutRevision)
	w.Uvarint(uint64(len(msg.Panes)))
	for _, pane := range msg.Panes {
		w.Uvarint(pane.PaneID)
		if err := encodeRect(&w, pane.Rect); err != nil {
			return nil, fmt.Errorf("encode WindowLayout: %w", err)
		}
	}
	return w.Buf, nil
}

func DecodeWindowLayout(payload []byte) (WindowLayout, error) {
	r := PayloadReader{Data: payload}
	windowID, err := r.Uvarint()
	if err != nil {
		return WindowLayout{}, fmt.Errorf("decode WindowLayout: %w", err)
	}
	focusedPaneID, err := r.Uvarint()
	if err != nil {
		return WindowLayout{}, fmt.Errorf("decode WindowLayout: %w", err)
	}
	layoutRevision, err := r.Uvarint()
	if err != nil {
		return WindowLayout{}, fmt.Errorf("decode WindowLayout: %w", err)
	}
	n, err := readCount(&r, MaxVisiblePanes)
	if err != nil {
		return WindowLayout{}, fmt.Errorf("decode WindowLayout: %w", err)
	}
	panes := make([]PanePlacement, 0, n)
	for i := 0; i < n; i++ {
		paneID, err := r.Uvarint()
		if err != nil {
			return WindowLayout{}, fmt.Errorf("decode WindowLayout: %w", err)
		}
		rect, err := decodeRect(&r)
		if err != nil {
			return WindowLayout{}, fmt.Errorf("decode WindowLayout: %w", err)
		}
		panes = append(panes, PanePlacement{PaneID: paneID, Rect: rect})
	}
	if err := r.Done(); err != nil {
		return WindowLayout{}, fmt.Errorf("decode WindowLayout: %w", err)
	}
	return WindowLayout{WindowID: windowID, FocusedPaneID: focusedPaneID, LayoutRevision: layoutRevision, Panes: panes}, nil
}

func EncodePing(dst []byte, msg Ping) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.Seq)
	w.Varint(msg.SentUnixMilli)
	return w.Buf, nil
}

func DecodePing(payload []byte) (Ping, error) {
	r := PayloadReader{Data: payload}
	seq, err := r.Uvarint()
	if err != nil {
		return Ping{}, fmt.Errorf("decode Ping: %w", err)
	}
	sentUnixMilli, err := r.Varint()
	if err != nil {
		return Ping{}, fmt.Errorf("decode Ping: %w", err)
	}
	if err := r.Done(); err != nil {
		return Ping{}, fmt.Errorf("decode Ping: %w", err)
	}
	return Ping{Seq: seq, SentUnixMilli: sentUnixMilli}, nil
}

func EncodePong(dst []byte, msg Pong) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.Seq)
	w.Varint(msg.SentUnixMilli)
	return w.Buf, nil
}

func DecodePong(payload []byte) (Pong, error) {
	r := PayloadReader{Data: payload}
	seq, err := r.Uvarint()
	if err != nil {
		return Pong{}, fmt.Errorf("decode Pong: %w", err)
	}
	sentUnixMilli, err := r.Varint()
	if err != nil {
		return Pong{}, fmt.Errorf("decode Pong: %w", err)
	}
	if err := r.Done(); err != nil {
		return Pong{}, fmt.Errorf("decode Pong: %w", err)
	}
	return Pong{Seq: seq, SentUnixMilli: sentUnixMilli}, nil
}
