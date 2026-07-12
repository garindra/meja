package protocol

import "fmt"

func EncodeScrollPane(dst []byte, msg ScrollPane) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Varint(int64(msg.Delta))
	return w.Buf, nil
}

func DecodeScrollPane(payload []byte) (ScrollPane, error) {
	r := PayloadReader{Data: payload}
	delta, err := r.Varint()
	if err != nil {
		return ScrollPane{}, fmt.Errorf("decode ScrollPane: %w", err)
	}
	if delta < -int64(MaxGridRows) || delta > int64(MaxGridRows) {
		return ScrollPane{}, fmt.Errorf("decode ScrollPane: delta %d exceeds max rows %d", delta, MaxGridRows)
	}
	if err := r.Done(); err != nil {
		return ScrollPane{}, fmt.Errorf("decode ScrollPane: %w", err)
	}
	return ScrollPane{Delta: int(delta)}, nil
}

func EncodeStatusBar(dst []byte, msg StatusBar) ([]byte, error) {
	if msg.Cols <= 0 || uint64(msg.Cols) > MaxGridCols || len(msg.Cells) != msg.Cols {
		return nil, fmt.Errorf("encode StatusBar: invalid width %d with %d cells", msg.Cols, len(msg.Cells))
	}
	if uint64(len(msg.Styles)) > MaxStyles {
		return nil, fmt.Errorf("encode StatusBar: style count %d exceeds max %d", len(msg.Styles), MaxStyles)
	}
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Cols))
	w.Uvarint(uint64(len(msg.Styles)))
	for _, def := range msg.Styles {
		w.Uvarint(uint64(def.ID))
		if err := encodeStyle(&w, def.Style); err != nil {
			return nil, fmt.Errorf("encode StatusBar: %w", err)
		}
	}
	if err := encodeCellSequence(&w, msg.Cells); err != nil {
		return nil, fmt.Errorf("encode StatusBar: %w", err)
	}
	return w.Buf, nil
}

func DecodeStatusBar(payload []byte) (StatusBar, error) {
	r := PayloadReader{Data: payload}
	cols, err := readCoord(&r, MaxGridCols)
	if err != nil {
		return StatusBar{}, fmt.Errorf("decode StatusBar: %w", err)
	}
	if cols <= 0 {
		return StatusBar{}, fmt.Errorf("decode StatusBar: invalid width %d", cols)
	}
	styleCount, err := readCount(&r, MaxStyles)
	if err != nil {
		return StatusBar{}, fmt.Errorf("decode StatusBar: %w", err)
	}
	styles := make([]StyleDefinition, 0, styleCount)
	for i := 0; i < styleCount; i++ {
		id, err := r.Uvarint()
		if err != nil {
			return StatusBar{}, fmt.Errorf("decode StatusBar: %w", err)
		}
		style, err := decodeStyle(&r)
		if err != nil {
			return StatusBar{}, fmt.Errorf("decode StatusBar: %w", err)
		}
		styles = append(styles, StyleDefinition{ID: uint32(id), Style: style})
	}
	cells, err := decodeCellSequence(&r, cols)
	if err != nil {
		return StatusBar{}, fmt.Errorf("decode StatusBar: %w", err)
	}
	if err := r.Done(); err != nil {
		return StatusBar{}, fmt.Errorf("decode StatusBar: %w", err)
	}
	return StatusBar{Cols: cols, Cells: cells, Styles: styles}, nil
}

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

const (
	cellSequenceRaw byte = iota
	cellSequenceRLE
)

func EncodeBindRenderStream(dst []byte, msg BindRenderStream) ([]byte, error) {
	w := PayloadWriter{Buf: dst}
	w.Uvarint(uint64(msg.Slot))
	w.Uvarint(msg.SessionID)
	w.Uvarint(msg.WindowID)
	w.Uvarint(msg.PaneID)
	w.Uvarint(msg.BindingGeneration)
	return w.Buf, nil
}

func DecodeBindRenderStream(payload []byte) (BindRenderStream, error) {
	r := PayloadReader{Data: payload}
	slot, err := r.Uvarint()
	if err != nil {
		return BindRenderStream{}, fmt.Errorf("decode BindRenderStream: %w", err)
	}
	if slot >= MaxRenderSlots {
		return BindRenderStream{}, fmt.Errorf("decode BindRenderStream: slot %d exceeds max %d", slot, MaxRenderSlots)
	}
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
		Slot:              uint8(slot),
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
	if err := encodeCellSequence(&w, msg.Cells); err != nil {
		return nil, fmt.Errorf("encode ReplacePane: %w", err)
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
	cells, err := decodeCellSequence(&r, int(cellCount))
	if err != nil {
		return ReplacePane{}, fmt.Errorf("decode ReplacePane: %w", err)
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

const (
	paneUpdateCursorChanged byte = 1 << iota
	paneUpdateCursorVisibleChanged
)

func EncodePaneUpdate(dst []byte, msg PaneUpdate) ([]byte, error) {
	if uint64(len(msg.Styles)) > MaxStyles {
		return nil, fmt.Errorf("encode PaneUpdate: style count %d exceeds max %d", len(msg.Styles), MaxStyles)
	}
	if uint64(len(msg.Runs)) > MaxGridRows {
		return nil, fmt.Errorf("encode PaneUpdate: run count %d exceeds max %d", len(msg.Runs), MaxGridRows)
	}
	if msg.CursorChanged && (msg.Cursor.X < 0 || msg.Cursor.Y < 0) {
		return nil, fmt.Errorf("encode PaneUpdate: negative cursor")
	}

	w := PayloadWriter{Buf: dst}
	w.Uvarint(msg.BindingGeneration)
	w.Uvarint(msg.BaseGeneration)
	w.Uvarint(msg.Generation)
	w.Uvarint(uint64(len(msg.Styles)))
	for _, def := range msg.Styles {
		w.Uvarint(uint64(def.ID))
		if err := encodeStyle(&w, def.Style); err != nil {
			return nil, fmt.Errorf("encode PaneUpdate: %w", err)
		}
	}
	w.Uvarint(uint64(len(msg.Runs)))
	for _, run := range msg.Runs {
		if run.Row < 0 || run.Column < 0 {
			return nil, fmt.Errorf("encode PaneUpdate: negative run coordinates")
		}
		if uint64(len(run.Cells)) > MaxCellRun {
			return nil, fmt.Errorf("encode PaneUpdate: cell count %d exceeds max %d", len(run.Cells), MaxCellRun)
		}
		w.Uvarint(uint64(run.Row))
		w.Uvarint(uint64(run.Column))
		w.Uvarint(uint64(len(run.Cells)))
		if err := encodeCellSequence(&w, run.Cells); err != nil {
			return nil, fmt.Errorf("encode PaneUpdate: %w", err)
		}
	}
	var flags byte
	if msg.CursorChanged {
		flags |= paneUpdateCursorChanged
	}
	if msg.CursorVisibleChanged {
		flags |= paneUpdateCursorVisibleChanged
	}
	w.Byte(flags)
	if msg.CursorChanged {
		w.Uvarint(uint64(msg.Cursor.X))
		w.Uvarint(uint64(msg.Cursor.Y))
	}
	if msg.CursorVisibleChanged {
		w.Bool(msg.CursorVisible)
	}
	return w.Buf, nil
}

func DecodePaneUpdate(payload []byte) (PaneUpdate, error) {
	r := PayloadReader{Data: payload}
	bindingGeneration, err := r.Uvarint()
	if err != nil {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
	}
	baseGeneration, err := r.Uvarint()
	if err != nil {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
	}
	generation, err := r.Uvarint()
	if err != nil {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
	}
	styleCount, err := readCount(&r, MaxStyles)
	if err != nil {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
	}
	styles := make([]StyleDefinition, 0, styleCount)
	for i := 0; i < styleCount; i++ {
		rawID, err := r.Uvarint()
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		if rawID > MaxStyles {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: style id %d exceeds max %d", rawID, MaxStyles)
		}
		style, err := decodeStyle(&r)
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		styles = append(styles, StyleDefinition{ID: uint32(rawID), Style: style})
	}
	runCount, err := readCount(&r, MaxGridRows)
	if err != nil {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
	}
	runs := make([]CellRun, 0, runCount)
	var totalCells uint64
	for i := 0; i < runCount; i++ {
		row, err := readCoord(&r, MaxGridRows)
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		column, err := readCoord(&r, MaxGridCols)
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		cellCount, err := readCount(&r, MaxCellRun)
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		totalCells += uint64(cellCount)
		if totalCells > MaxCells {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: total cell count exceeds max %d", MaxCells)
		}
		cells, err := decodeCellSequence(&r, cellCount)
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		runs = append(runs, CellRun{Row: row, Column: column, Cells: cells})
	}
	flags, err := r.Byte()
	if err != nil {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
	}
	if flags & ^byte(paneUpdateCursorChanged|paneUpdateCursorVisibleChanged) != 0 {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: unknown flags %#x", flags)
	}
	msg := PaneUpdate{
		BindingGeneration:    bindingGeneration,
		BaseGeneration:       baseGeneration,
		Generation:           generation,
		Styles:               styles,
		Runs:                 runs,
		CursorChanged:        flags&paneUpdateCursorChanged != 0,
		CursorVisibleChanged: flags&paneUpdateCursorVisibleChanged != 0,
	}
	if msg.CursorChanged {
		x, err := readCoord(&r, MaxGridCols)
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		y, err := readCoord(&r, MaxGridRows)
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		msg.Cursor = Cursor{X: x, Y: y}
	}
	if msg.CursorVisibleChanged {
		visible, err := r.Bool()
		if err != nil {
			return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
		}
		msg.CursorVisible = visible
	}
	if err := r.Done(); err != nil {
		return PaneUpdate{}, fmt.Errorf("decode PaneUpdate: %w", err)
	}
	return msg, nil
}

func encodeCellSequence(w *PayloadWriter, cells []Cell) error {
	encoding, err := chooseCellSequenceEncoding(cells)
	if err != nil {
		return err
	}
	w.Byte(encoding)
	if encoding == cellSequenceRaw {
		for _, cell := range cells {
			if err := encodeCell(w, cell); err != nil {
				return err
			}
		}
		return nil
	}

	runCount := 0
	for i := 0; i < len(cells); {
		runCount++
		cell := cells[i]
		i++
		for i < len(cells) && cells[i] == cell {
			i++
		}
	}
	w.Uvarint(uint64(runCount))
	for i := 0; i < len(cells); {
		start := i
		cell := cells[i]
		i++
		for i < len(cells) && cells[i] == cell {
			i++
		}
		w.Uvarint(uint64(i - start))
		if err := encodeCell(w, cell); err != nil {
			return err
		}
	}
	return nil
}

func decodeCellSequence(r *PayloadReader, cellCount int) ([]Cell, error) {
	encoding, err := r.Byte()
	if err != nil {
		return nil, err
	}
	cells := make([]Cell, cellCount)
	switch encoding {
	case cellSequenceRaw:
		for i := range cells {
			cell, err := decodeCell(r)
			if err != nil {
				return nil, err
			}
			cells[i] = cell
		}
		return cells, nil
	case cellSequenceRLE:
		runCount, err := readCount(r, uint64(cellCount))
		if err != nil {
			return nil, err
		}
		written := 0
		for i := 0; i < runCount; i++ {
			runLength, err := r.Uvarint()
			if err != nil {
				return nil, err
			}
			if runLength == 0 || runLength > uint64(cellCount-written) {
				return nil, fmt.Errorf("invalid cell run length %d with %d cells remaining", runLength, cellCount-written)
			}
			cell, err := decodeCell(r)
			if err != nil {
				return nil, err
			}
			end := written + int(runLength)
			for written < end {
				cells[written] = cell
				written++
			}
		}
		if written != cellCount {
			return nil, fmt.Errorf("cell runs decoded %d cells, want %d", written, cellCount)
		}
		return cells, nil
	default:
		return nil, fmt.Errorf("unknown cell sequence encoding %d", encoding)
	}
}

func chooseCellSequenceEncoding(cells []Cell) (byte, error) {
	rawSize := 0
	rleBodySize := 0
	runCount := 0
	for i := 0; i < len(cells); {
		cell := cells[i]
		if !validRune(cell.Rune) {
			return 0, ErrInvalidRune
		}
		if !validCellWidth(cell.Width) {
			return 0, ErrInvalidCellWidth
		}
		start := i
		i++
		for i < len(cells) && cells[i] == cell {
			i++
		}
		runLength := i - start
		cellSize := uvarintEncodedLen(uint64(cell.Rune)) + uvarintEncodedLen(uint64(cell.StyleID)) + 1
		rawSize += runLength * cellSize
		rleBodySize += uvarintEncodedLen(uint64(runLength)) + cellSize
		runCount++
	}
	rleSize := uvarintEncodedLen(uint64(runCount)) + rleBodySize
	if len(cells) > 0 && rleSize < rawSize {
		return cellSequenceRLE, nil
	}
	return cellSequenceRaw, nil
}

func uvarintEncodedLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
