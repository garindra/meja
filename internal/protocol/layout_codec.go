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

func EncodeClientLayout(dst []byte, msg ClientLayout) ([]byte, error) {
	if uint64(len(msg.Panes)) > MaxVisiblePanes {
		return nil, fmt.Errorf("encode ClientLayout: pane count %d exceeds max %d", len(msg.Panes), MaxVisiblePanes)
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.WindowID)
	w.Uvarint(msg.FocusedPaneID)
	w.Uvarint(uint64(msg.LayoutRevision))
	w.Uvarint(uint64(len(msg.Panes)))
	for _, pane := range msg.Panes {
		w.Uvarint(pane.PaneID)
		w.Uvarint(uint64(pane.Slot))
		if err := encodeRect(&w, pane.Rect); err != nil {
			return nil, fmt.Errorf("encode ClientLayout: %w", err)
		}
	}
	return w.Buf, nil
}

func DecodeClientLayout(payload []byte) (ClientLayout, error) {
	r := PayloadReader{Data: payload}
	windowID, err := r.Uvarint()
	if err != nil {
		return ClientLayout{}, fmt.Errorf("decode ClientLayout: %w", err)
	}
	focusedPaneID, err := r.Uvarint()
	if err != nil {
		return ClientLayout{}, fmt.Errorf("decode ClientLayout: %w", err)
	}
	layoutRevision, err := r.Uvarint()
	if err != nil {
		return ClientLayout{}, fmt.Errorf("decode ClientLayout: %w", err)
	}
	n, err := readCount(&r, MaxVisiblePanes)
	if err != nil {
		return ClientLayout{}, fmt.Errorf("decode ClientLayout: %w", err)
	}
	panes := make([]PanePlacement, 0, n)
	for i := 0; i < n; i++ {
		paneID, err := r.Uvarint()
		if err != nil {
			return ClientLayout{}, fmt.Errorf("decode ClientLayout: %w", err)
		}
		slot, err := r.Uvarint()
		if err != nil || slot >= MaxRenderSlots {
			return ClientLayout{}, fmt.Errorf("decode ClientLayout: invalid slot %d", slot)
		}
		rect, err := decodeRect(&r)
		if err != nil {
			return ClientLayout{}, fmt.Errorf("decode ClientLayout: %w", err)
		}
		panes = append(panes, PanePlacement{PaneID: paneID, Slot: uint8(slot), Rect: rect})
	}
	if err := r.Done(); err != nil {
		return ClientLayout{}, fmt.Errorf("decode ClientLayout: %w", err)
	}
	return ClientLayout{WindowID: windowID, FocusedPaneID: focusedPaneID, LayoutRevision: ClientLayoutRevision(layoutRevision), Panes: panes}, nil
}
