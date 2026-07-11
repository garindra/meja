package protocol

import "fmt"

func EncodeCreateWindow(dst []byte, msg CreateWindow) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.String(msg.Cwd)
	var err error
	w.Buf, err = encodeArgv(w.Buf, msg.Argv)
	if err != nil {
		return nil, fmt.Errorf("encode CreateWindow: %w", err)
	}
	return w.Buf, nil
}

func DecodeCreateWindow(payload []byte) (CreateWindow, error) {
	r := PayloadReader{Data: payload}
	cwd, err := r.String(MaxStringLen)
	if err != nil {
		return CreateWindow{}, fmt.Errorf("decode CreateWindow: %w", err)
	}
	argv, err := decodeArgv(&r)
	if err != nil {
		return CreateWindow{}, fmt.Errorf("decode CreateWindow: %w", err)
	}
	if err := r.Done(); err != nil {
		return CreateWindow{}, fmt.Errorf("decode CreateWindow: %w", err)
	}
	return CreateWindow{Cwd: cwd, Argv: argv}, nil
}

func encodeWindowInfo(w *PayloadWriter, info WindowInfo) error {
	w.Uvarint(info.WindowID)
	w.Uvarint(info.PaneID)
	if info.Index < 0 {
		return fmt.Errorf("negative index")
	}
	w.Uvarint(uint64(info.Index))
	w.String(info.Title)
	w.Bool(info.Active)
	return nil
}

func decodeWindowInfo(r *PayloadReader) (WindowInfo, error) {
	windowID, err := r.Uvarint()
	if err != nil {
		return WindowInfo{}, err
	}
	paneID, err := r.Uvarint()
	if err != nil {
		return WindowInfo{}, err
	}
	index, err := r.Uvarint()
	if err != nil {
		return WindowInfo{}, err
	}
	title, err := r.String(MaxStringLen)
	if err != nil {
		return WindowInfo{}, err
	}
	active, err := r.Bool()
	if err != nil {
		return WindowInfo{}, err
	}
	return WindowInfo{
		WindowID: windowID,
		PaneID:   paneID,
		Index:    int(index),
		Title:    title,
		Active:   active,
	}, nil
}

func EncodeWindowCreated(dst []byte, msg WindowCreated) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	if err := encodeWindowInfo(&w, msg.Window); err != nil {
		return nil, fmt.Errorf("encode WindowCreated: %w", err)
	}
	return w.Buf, nil
}

func DecodeWindowCreated(payload []byte) (WindowCreated, error) {
	r := PayloadReader{Data: payload}
	info, err := decodeWindowInfo(&r)
	if err != nil {
		return WindowCreated{}, fmt.Errorf("decode WindowCreated: %w", err)
	}
	if err := r.Done(); err != nil {
		return WindowCreated{}, fmt.Errorf("decode WindowCreated: %w", err)
	}
	return WindowCreated{Window: info}, nil
}

func EncodeCloseWindow(dst []byte, msg CloseWindow) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.WindowID)
	return w.Buf, nil
}

func DecodeCloseWindow(payload []byte) (CloseWindow, error) {
	r := PayloadReader{Data: payload}
	windowID, err := r.Uvarint()
	if err != nil {
		return CloseWindow{}, fmt.Errorf("decode CloseWindow: %w", err)
	}
	if err := r.Done(); err != nil {
		return CloseWindow{}, fmt.Errorf("decode CloseWindow: %w", err)
	}
	return CloseWindow{WindowID: windowID}, nil
}

func EncodeWindowClosed(dst []byte, msg WindowClosed) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.WindowID)
	return w.Buf, nil
}

func DecodeWindowClosed(payload []byte) (WindowClosed, error) {
	r := PayloadReader{Data: payload}
	windowID, err := r.Uvarint()
	if err != nil {
		return WindowClosed{}, fmt.Errorf("decode WindowClosed: %w", err)
	}
	if err := r.Done(); err != nil {
		return WindowClosed{}, fmt.Errorf("decode WindowClosed: %w", err)
	}
	return WindowClosed{WindowID: windowID}, nil
}

func EncodeSelectWindow(dst []byte, msg SelectWindow) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.WindowID)
	return w.Buf, nil
}

func DecodeSelectWindow(payload []byte) (SelectWindow, error) {
	r := PayloadReader{Data: payload}
	windowID, err := r.Uvarint()
	if err != nil {
		return SelectWindow{}, fmt.Errorf("decode SelectWindow: %w", err)
	}
	if err := r.Done(); err != nil {
		return SelectWindow{}, fmt.Errorf("decode SelectWindow: %w", err)
	}
	return SelectWindow{WindowID: windowID}, nil
}

func EncodeWindowSelected(dst []byte, msg WindowSelected) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.WindowID)
	w.Uvarint(msg.PaneID)
	return w.Buf, nil
}

func DecodeWindowSelected(payload []byte) (WindowSelected, error) {
	r := PayloadReader{Data: payload}
	windowID, err := r.Uvarint()
	if err != nil {
		return WindowSelected{}, fmt.Errorf("decode WindowSelected: %w", err)
	}
	paneID, err := r.Uvarint()
	if err != nil {
		return WindowSelected{}, fmt.Errorf("decode WindowSelected: %w", err)
	}
	if err := r.Done(); err != nil {
		return WindowSelected{}, fmt.Errorf("decode WindowSelected: %w", err)
	}
	return WindowSelected{WindowID: windowID, PaneID: paneID}, nil
}

func EncodeListWindows(dst []byte, _ ListWindows) ([]byte, error) { return dst, nil }

func DecodeListWindows(payload []byte) (ListWindows, error) {
	if len(payload) != 0 {
		return ListWindows{}, fmt.Errorf("decode ListWindows: %w", ErrTrailingBytes)
	}
	return ListWindows{}, nil
}

func EncodeWindowList(dst []byte, msg WindowList) ([]byte, error) {
	if uint64(len(msg.Windows)) > MaxWindows {
		return nil, fmt.Errorf("encode WindowList: window count %d exceeds max %d", len(msg.Windows), MaxWindows)
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.ActiveWindowID)
	w.Uvarint(uint64(len(msg.Windows)))
	for _, info := range msg.Windows {
		if err := encodeWindowInfo(&w, info); err != nil {
			return nil, fmt.Errorf("encode WindowList: %w", err)
		}
	}
	return w.Buf, nil
}

func DecodeWindowList(payload []byte) (WindowList, error) {
	r := PayloadReader{Data: payload}
	activeWindowID, err := r.Uvarint()
	if err != nil {
		return WindowList{}, fmt.Errorf("decode WindowList: %w", err)
	}
	n, err := readCount(&r, MaxWindows)
	if err != nil {
		return WindowList{}, fmt.Errorf("decode WindowList: %w", err)
	}
	windows := make([]WindowInfo, 0, n)
	for i := 0; i < n; i++ {
		info, err := decodeWindowInfo(&r)
		if err != nil {
			return WindowList{}, fmt.Errorf("decode WindowList: %w", err)
		}
		windows = append(windows, info)
	}
	if err := r.Done(); err != nil {
		return WindowList{}, fmt.Errorf("decode WindowList: %w", err)
	}
	return WindowList{Windows: windows, ActiveWindowID: activeWindowID}, nil
}

func EncodeWindowTitleChanged(dst []byte, msg WindowTitleChanged) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.WindowID)
	w.String(msg.Title)
	return w.Buf, nil
}

func DecodeWindowTitleChanged(payload []byte) (WindowTitleChanged, error) {
	r := PayloadReader{Data: payload}
	windowID, err := r.Uvarint()
	if err != nil {
		return WindowTitleChanged{}, fmt.Errorf("decode WindowTitleChanged: %w", err)
	}
	title, err := r.String(MaxStringLen)
	if err != nil {
		return WindowTitleChanged{}, fmt.Errorf("decode WindowTitleChanged: %w", err)
	}
	if err := r.Done(); err != nil {
		return WindowTitleChanged{}, fmt.Errorf("decode WindowTitleChanged: %w", err)
	}
	return WindowTitleChanged{WindowID: windowID, Title: title}, nil
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
