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
	return WindowLayout{WindowID: windowID, LayoutRevision: layoutRevision, Panes: panes}, nil
}

func EncodeCreateSplit(dst []byte, msg CreateSplit) ([]byte, error) {
	if msg.Direction > SplitHorizontal {
		return nil, fmt.Errorf("encode CreateSplit: invalid direction %d", msg.Direction)
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	w.Byte(byte(msg.Direction))
	return w.Buf, nil
}

func DecodeCreateSplit(payload []byte) (CreateSplit, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return CreateSplit{}, fmt.Errorf("decode CreateSplit: %w", err)
	}
	direction, err := r.Byte()
	if err != nil {
		return CreateSplit{}, fmt.Errorf("decode CreateSplit: %w", err)
	}
	if SplitDirection(direction) > SplitHorizontal {
		return CreateSplit{}, fmt.Errorf("decode CreateSplit: invalid direction %d", direction)
	}
	if err := r.Done(); err != nil {
		return CreateSplit{}, fmt.Errorf("decode CreateSplit: %w", err)
	}
	return CreateSplit{PaneID: paneID, Direction: SplitDirection(direction)}, nil
}

func EncodeFocusPane(dst []byte, msg FocusPane) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	return w.Buf, nil
}

func DecodeFocusPane(payload []byte) (FocusPane, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return FocusPane{}, fmt.Errorf("decode FocusPane: %w", err)
	}
	if err := r.Done(); err != nil {
		return FocusPane{}, fmt.Errorf("decode FocusPane: %w", err)
	}
	return FocusPane{PaneID: paneID}, nil
}

func EncodeClosePane(dst []byte, msg ClosePane) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.PaneID)
	return w.Buf, nil
}

func DecodeClosePane(payload []byte) (ClosePane, error) {
	r := PayloadReader{Data: payload}
	paneID, err := r.Uvarint()
	if err != nil {
		return ClosePane{}, fmt.Errorf("decode ClosePane: %w", err)
	}
	if err := r.Done(); err != nil {
		return ClosePane{}, fmt.Errorf("decode ClosePane: %w", err)
	}
	return ClosePane{PaneID: paneID}, nil
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
