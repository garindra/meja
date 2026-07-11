package protocol

import "fmt"

const (
	colorKindDefault byte = iota
	colorKindIndexed
	colorKindRGB
)

const (
	styleFlagBold uint64 = 1 << iota
	styleFlagDim
	styleFlagItalic
	styleFlagUnderline
	styleFlagReverse
)

func EncodeBindRenderStream(dst []byte, msg BindRenderStream) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.SessionID)
	w.Uvarint(msg.WindowID)
	w.Uvarint(msg.PaneID)
	w.Uvarint(msg.BindingGeneration)
	return w.Buf, nil
}

func DecodeBindRenderStream(payload []byte) (BindRenderStream, error) {
	r := PayloadReader{Data: payload}
	sessionID, err := r.Uvarint()
	if err != nil {
		return BindRenderStream{}, fmt.Errorf("decode BindRenderStream: %w", err)
	}
	windowID, err := r.Uvarint()
	if err != nil {
		return BindRenderStream{}, fmt.Errorf("decode BindRenderStream: %w", err)
	}
	paneID, err := r.Uvarint()
	if err != nil {
		return BindRenderStream{}, fmt.Errorf("decode BindRenderStream: %w", err)
	}
	bindingGeneration, err := r.Uvarint()
	if err != nil {
		return BindRenderStream{}, fmt.Errorf("decode BindRenderStream: %w", err)
	}
	if err := r.Done(); err != nil {
		return BindRenderStream{}, fmt.Errorf("decode BindRenderStream: %w", err)
	}
	return BindRenderStream{
		SessionID:         sessionID,
		WindowID:          windowID,
		PaneID:            paneID,
		BindingGeneration: bindingGeneration,
	}, nil
}

func encodeColor(w *PayloadWriter, c Color) error {
	switch c.Mode {
	case "", "default":
		w.Byte(colorKindDefault)
	case "indexed":
		w.Byte(colorKindIndexed)
		w.Byte(c.Index)
	case "rgb":
		w.Byte(colorKindRGB)
		w.Byte(c.R)
		w.Byte(c.G)
		w.Byte(c.B)
	default:
		return fmt.Errorf("unknown color mode %q", c.Mode)
	}
	return nil
}

func decodeColor(r *PayloadReader) (Color, error) {
	kind, err := r.Byte()
	if err != nil {
		return Color{}, err
	}
	switch kind {
	case colorKindDefault:
		return Color{Mode: "default"}, nil
	case colorKindIndexed:
		index, err := r.Byte()
		if err != nil {
			return Color{}, err
		}
		return Color{Mode: "indexed", Index: index}, nil
	case colorKindRGB:
		red, err := r.Byte()
		if err != nil {
			return Color{}, err
		}
		green, err := r.Byte()
		if err != nil {
			return Color{}, err
		}
		blue, err := r.Byte()
		if err != nil {
			return Color{}, err
		}
		return Color{Mode: "rgb", R: red, G: green, B: blue}, nil
	default:
		return Color{}, fmt.Errorf("unknown color kind %d", kind)
	}
}

func encodeStyle(w *PayloadWriter, style Style) error {
	var flags uint64
	if style.Bold {
		flags |= styleFlagBold
	}
	if style.Dim {
		flags |= styleFlagDim
	}
	if style.Italic {
		flags |= styleFlagItalic
	}
	if style.Underline {
		flags |= styleFlagUnderline
	}
	if style.Reverse {
		flags |= styleFlagReverse
	}
	w.Uvarint(flags)
	if err := encodeColor(w, style.FG); err != nil {
		return err
	}
	return encodeColor(w, style.BG)
}

func decodeStyle(r *PayloadReader) (Style, error) {
	flags, err := r.Uvarint()
	if err != nil {
		return Style{}, err
	}
	fg, err := decodeColor(r)
	if err != nil {
		return Style{}, err
	}
	bg, err := decodeColor(r)
	if err != nil {
		return Style{}, err
	}
	return Style{
		Bold:      flags&styleFlagBold != 0,
		Dim:       flags&styleFlagDim != 0,
		Italic:    flags&styleFlagItalic != 0,
		Underline: flags&styleFlagUnderline != 0,
		Reverse:   flags&styleFlagReverse != 0,
		FG:        fg,
		BG:        bg,
	}, nil
}

func EncodeDefineStyle(dst []byte, msg DefineStyle) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.BindingGeneration)
	w.Uvarint(uint64(msg.ID))
	if err := encodeStyle(&w, msg.Style); err != nil {
		return nil, fmt.Errorf("encode DefineStyle: %w", err)
	}
	return w.Buf, nil
}

func DecodeDefineStyle(payload []byte) (DefineStyle, error) {
	r := PayloadReader{Data: payload}
	bindingGeneration, err := r.Uvarint()
	if err != nil {
		return DefineStyle{}, fmt.Errorf("decode DefineStyle: %w", err)
	}
	rawID, err := r.Uvarint()
	if err != nil {
		return DefineStyle{}, fmt.Errorf("decode DefineStyle: %w", err)
	}
	style, err := decodeStyle(&r)
	if err != nil {
		return DefineStyle{}, fmt.Errorf("decode DefineStyle: %w", err)
	}
	if err := r.Done(); err != nil {
		return DefineStyle{}, fmt.Errorf("decode DefineStyle: %w", err)
	}
	return DefineStyle{
		BindingGeneration: bindingGeneration,
		ID:                uint32(rawID),
		Style:             style,
	}, nil
}

func EncodeSetCursor(dst []byte, msg SetCursor) ([]byte, error) {
	if msg.Cursor.X < 0 || msg.Cursor.Y < 0 {
		return nil, fmt.Errorf("encode SetCursor: negative cursor")
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.BindingGeneration)
	w.Uvarint(msg.BaseGeneration)
	w.Uvarint(msg.Generation)
	w.Uvarint(uint64(msg.Cursor.X))
	w.Uvarint(uint64(msg.Cursor.Y))
	return w.Buf, nil
}

func DecodeSetCursor(payload []byte) (SetCursor, error) {
	r := PayloadReader{Data: payload}
	bindingGeneration, err := r.Uvarint()
	if err != nil {
		return SetCursor{}, fmt.Errorf("decode SetCursor: %w", err)
	}
	baseGeneration, err := r.Uvarint()
	if err != nil {
		return SetCursor{}, fmt.Errorf("decode SetCursor: %w", err)
	}
	generation, err := r.Uvarint()
	if err != nil {
		return SetCursor{}, fmt.Errorf("decode SetCursor: %w", err)
	}
	x, err := readCoord(&r, MaxGridCols)
	if err != nil {
		return SetCursor{}, fmt.Errorf("decode SetCursor: %w", err)
	}
	y, err := readCoord(&r, MaxGridRows)
	if err != nil {
		return SetCursor{}, fmt.Errorf("decode SetCursor: %w", err)
	}
	if err := r.Done(); err != nil {
		return SetCursor{}, fmt.Errorf("decode SetCursor: %w", err)
	}
	return SetCursor{
		BindingGeneration: bindingGeneration,
		BaseGeneration:    baseGeneration,
		Generation:        generation,
		Cursor:            Cursor{X: x, Y: y},
	}, nil
}

func EncodeSetCursorVisible(dst []byte, msg SetCursorVisible) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.BindingGeneration)
	w.Uvarint(msg.BaseGeneration)
	w.Uvarint(msg.Generation)
	w.Bool(msg.Visible)
	return w.Buf, nil
}

func DecodeSetCursorVisible(payload []byte) (SetCursorVisible, error) {
	r := PayloadReader{Data: payload}
	bindingGeneration, err := r.Uvarint()
	if err != nil {
		return SetCursorVisible{}, fmt.Errorf("decode SetCursorVisible: %w", err)
	}
	baseGeneration, err := r.Uvarint()
	if err != nil {
		return SetCursorVisible{}, fmt.Errorf("decode SetCursorVisible: %w", err)
	}
	generation, err := r.Uvarint()
	if err != nil {
		return SetCursorVisible{}, fmt.Errorf("decode SetCursorVisible: %w", err)
	}
	visible, err := r.Bool()
	if err != nil {
		return SetCursorVisible{}, fmt.Errorf("decode SetCursorVisible: %w", err)
	}
	if err := r.Done(); err != nil {
		return SetCursorVisible{}, fmt.Errorf("decode SetCursorVisible: %w", err)
	}
	return SetCursorVisible{
		BindingGeneration: bindingGeneration,
		BaseGeneration:    baseGeneration,
		Generation:        generation,
		Visible:           visible,
	}, nil
}

func EncodeSetRun(dst []byte, msg SetRun) ([]byte, error) {
	if msg.Row < 0 || msg.Column < 0 {
		return nil, fmt.Errorf("encode SetRun: negative coordinates")
	}
	if uint64(len(msg.Cells)) > MaxCellRun {
		return nil, fmt.Errorf("encode SetRun: cell count %d exceeds max %d", len(msg.Cells), MaxCellRun)
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.BindingGeneration)
	w.Uvarint(msg.BaseGeneration)
	w.Uvarint(msg.Generation)
	w.Uvarint(uint64(msg.Row))
	w.Uvarint(uint64(msg.Column))
	w.Uvarint(uint64(len(msg.Cells)))
	for _, cell := range msg.Cells {
		if err := encodeCell(&w, cell); err != nil {
			return nil, fmt.Errorf("encode SetRun: %w", err)
		}
	}
	return w.Buf, nil
}

func DecodeSetRun(payload []byte) (SetRun, error) {
	r := PayloadReader{Data: payload}
	bindingGeneration, err := r.Uvarint()
	if err != nil {
		return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
	}
	baseGeneration, err := r.Uvarint()
	if err != nil {
		return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
	}
	generation, err := r.Uvarint()
	if err != nil {
		return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
	}
	row, err := readCoord(&r, MaxGridRows)
	if err != nil {
		return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
	}
	column, err := readCoord(&r, MaxGridCols)
	if err != nil {
		return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
	}
	cellCount, err := readCount(&r, MaxCellRun)
	if err != nil {
		return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
	}
	cells := make([]Cell, 0, cellCount)
	for i := 0; i < cellCount; i++ {
		cell, err := decodeCell(&r)
		if err != nil {
			return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
		}
		cells = append(cells, cell)
	}
	if err := r.Done(); err != nil {
		return SetRun{}, fmt.Errorf("decode SetRun: %w", err)
	}
	return SetRun{
		BindingGeneration: bindingGeneration,
		BaseGeneration:    baseGeneration,
		Generation:        generation,
		Row:               row,
		Column:            column,
		Cells:             cells,
	}, nil
}

func EncodeReplacePane(dst []byte, msg ReplacePane) ([]byte, error) {
	if msg.Cols < 0 || msg.Rows < 0 {
		return nil, fmt.Errorf("encode ReplacePane: negative dimensions")
	}
	if msg.Cursor.X < 0 || msg.Cursor.Y < 0 {
		return nil, fmt.Errorf("encode ReplacePane: negative cursor")
	}
	if uint64(len(msg.Styles)) > MaxStyles {
		return nil, fmt.Errorf("encode ReplacePane: style count %d exceeds max %d", len(msg.Styles), MaxStyles)
	}
	if want := msg.Cols * msg.Rows; want < 0 || len(msg.Cells) != want {
		return nil, fmt.Errorf("encode ReplacePane: cell count %d does not match %dx%d", len(msg.Cells), msg.Cols, msg.Rows)
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.SessionID)
	w.Uvarint(msg.WindowID)
	w.Uvarint(msg.PaneID)
	w.Uvarint(msg.BindingGeneration)
	w.Uvarint(msg.Generation)
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(msg.Rows))
	w.Uvarint(uint64(msg.Cursor.X))
	w.Uvarint(uint64(msg.Cursor.Y))
	w.Bool(msg.CursorVisible)
	w.Uvarint(uint64(len(msg.Styles)))
	for _, def := range msg.Styles {
		w.Uvarint(uint64(def.ID))
		if err := encodeStyle(&w, def.Style); err != nil {
			return nil, fmt.Errorf("encode ReplacePane: %w", err)
		}
	}
	for _, cell := range msg.Cells {
		if err := encodeCell(&w, cell); err != nil {
			return nil, fmt.Errorf("encode ReplacePane: %w", err)
		}
	}
	return w.Buf, nil
}

func DecodeReplacePane(payload []byte) (ReplacePane, error) {
	r := PayloadReader{Data: payload}
	sessionID, err := r.Uvarint()
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	windowID, err := r.Uvarint()
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	paneID, err := r.Uvarint()
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	bindingGeneration, err := r.Uvarint()
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	generation, err := r.Uvarint()
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	cols, err := readCoord(&r, MaxGridCols)
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	rows, err := readCoord(&r, MaxGridRows)
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	cellCount := uint64(cols) * uint64(rows)
	if cols <= 0 || rows <= 0 || cellCount > MaxCells {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: invalid dimensions %dx%d", cols, rows)
	}
	cursorX, err := readCoord(&r, uint64(cols))
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	cursorY, err := readCoord(&r, uint64(rows))
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	cursorVisible, err := r.Bool()
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	styleCount, err := readCount(&r, MaxStyles)
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	styles := make([]StyleDefinition, 0, styleCount)
	for i := 0; i < styleCount; i++ {
		styleID, err := r.Uvarint()
		if err != nil {
			return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
		}
		style, err := decodeStyle(&r)
		if err != nil {
			return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
		}
		styles = append(styles, StyleDefinition{ID: uint32(styleID), Style: style})
	}
	cells := make([]Cell, int(cellCount))
	for i := range cells {
		cell, err := decodeCell(&r)
		if err != nil {
			return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
		}
		cells[i] = cell
	}
	if err := r.Done(); err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
	}
	return ReplacePane{
		SessionID:         sessionID,
		WindowID:          windowID,
		PaneID:            paneID,
		BindingGeneration: bindingGeneration,
		Generation:        generation,
		Cols:              cols,
		Rows:              rows,
		Cursor:            Cursor{X: cursorX, Y: cursorY},
		CursorVisible:     cursorVisible,
		Styles:            styles,
		Cells:             cells,
	}, nil
}
