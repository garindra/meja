package terminal

import (
	"unicode/utf8"
)

type Update struct {
	DirtyRows     map[int]struct{}
	FullRedraw    bool
	DefinedStyles map[uint32]Style
	CursorChanged bool
	VisibleChange bool
}

type TerminalState struct {
	Cols          int
	Rows          int
	Cells         []Cell
	CursorX       int
	CursorY       int
	CurrentStyle  Style
	CursorVisible bool
	Parser        Parser
	wrapPending   bool

	styleByID   map[uint32]Style
	styleToID   map[Style]uint32
	nextStyleID uint32
}

func New(cols, rows int) *TerminalState {
	t := &TerminalState{
		Cols:          cols,
		Rows:          rows,
		CurrentStyle:  DefaultStyle,
		CursorVisible: true,
		styleByID:     map[uint32]Style{0: DefaultStyle},
		styleToID:     map[Style]uint32{DefaultStyle: 0},
		nextStyleID:   1,
	}
	t.Cells = make([]Cell, cols*rows)
	for i := range t.Cells {
		t.Cells[i] = blankCell(0)
	}
	return t
}

func (t *TerminalState) Resize(cols, rows int) {
	if cols == t.Cols && rows == t.Rows {
		return
	}
	next := New(cols, rows)
	next.CurrentStyle = t.CurrentStyle
	next.CursorVisible = t.CursorVisible
	next.Parser = t.Parser
	next.wrapPending = t.wrapPending
	next.styleByID = make(map[uint32]Style, len(t.styleByID))
	for id, style := range t.styleByID {
		next.styleByID[id] = style
	}
	next.styleToID = make(map[Style]uint32, len(t.styleToID))
	for style, id := range t.styleToID {
		next.styleToID[style] = id
	}
	next.nextStyleID = t.nextStyleID

	copyCols := min(t.Cols, cols)
	copyRows := min(t.Rows, rows)
	for row := 0; row < copyRows; row++ {
		copy(next.Cells[row*cols:row*cols+copyCols], t.Cells[row*t.Cols:row*t.Cols+copyCols])
	}
	next.CursorX = t.CursorX
	next.CursorY = t.CursorY
	next.clampCursor()
	*t = *next
}

func (t *TerminalState) SnapshotStyles() []protocolStyleDef {
	out := make([]protocolStyleDef, 0, len(t.styleByID))
	for id, style := range t.styleByID {
		out = append(out, protocolStyleDef{ID: id, Style: style})
	}
	return out
}

type protocolStyleDef struct {
	ID    uint32
	Style Style
}

func (t *TerminalState) Apply(data []byte) Update {
	update := Update{
		DirtyRows:     map[int]struct{}{},
		DefinedStyles: map[uint32]Style{},
	}
	for len(data) > 0 {
		switch t.Parser.state {
		case parserText:
			if len(t.Parser.utf8Buf) > 0 || data[0] >= 0x80 {
				var consumed int
				consumed, update = t.consumeUTF8(data, update)
				data = data[consumed:]
				continue
			}
			b := data[0]
			data = data[1:]
			switch b {
			case 0x1b:
				t.Parser.state = parserESC
			case '\r':
				t.CursorX = 0
				t.wrapPending = false
				update.CursorChanged = true
			case '\n':
				if t.wrapPending {
					t.CursorX = 0
				}
				t.lineFeed()
				t.wrapPending = false
				update.FullRedraw = true
				update.CursorChanged = true
			case '\b':
				if t.CursorX > 0 {
					t.CursorX--
				}
				t.wrapPending = false
				update.CursorChanged = true
			case '\t':
				next := ((t.CursorX / 8) + 1) * 8
				if next >= t.Cols {
					next = t.Cols - 1
					if next < 0 {
						next = 0
					}
				}
				t.CursorX = next
				t.wrapPending = false
				update.CursorChanged = true
			case 0x07:
			default:
				if b >= 0x20 && b <= 0x7e {
					t.putRune(rune(b), &update)
				}
			}
		case parserESC:
			switch data[0] {
			case '[':
				t.Parser.state = parserCSI
				t.Parser.csiBuf.Reset()
			case ']':
				t.Parser.state = parserOSC
				t.Parser.oscBuf.Reset()
			default:
				t.Parser.state = parserText
			}
			data = data[1:]
		case parserCSI:
			b := data[0]
			data = data[1:]
			t.Parser.csiBuf.WriteByte(b)
			if b >= 0x40 && b <= 0x7e {
				t.executeCSI(t.Parser.csiBuf.String(), &update)
				t.Parser.state = parserText
				t.Parser.csiBuf.Reset()
			}
		case parserOSC:
			b := data[0]
			data = data[1:]
			switch b {
			case 0x07:
				t.Parser.state = parserText
				t.Parser.oscBuf.Reset()
			case 0x1b:
				t.Parser.state = parserOSCESC
			default:
				t.Parser.oscBuf.WriteByte(b)
			}
		case parserOSCESC:
			b := data[0]
			data = data[1:]
			if b == '\\' {
				t.Parser.state = parserText
				t.Parser.oscBuf.Reset()
				continue
			}
			t.Parser.oscBuf.WriteByte(0x1b)
			t.Parser.oscBuf.WriteByte(b)
			t.Parser.state = parserOSC
		}
	}
	return update
}

func (t *TerminalState) consumeUTF8(data []byte, update Update) (int, Update) {
	for i := 0; i < len(data); i++ {
		t.Parser.utf8Buf = append(t.Parser.utf8Buf, data[i])
		if utf8.FullRune(t.Parser.utf8Buf) {
			r, size := utf8.DecodeRune(t.Parser.utf8Buf)
			if r == utf8.RuneError && size == 1 {
				t.Parser.utf8Buf = t.Parser.utf8Buf[:0]
				return i + 1, update
			}
			t.putRune(r, &update)
			t.Parser.utf8Buf = t.Parser.utf8Buf[:0]
			return i + 1, update
		}
	}
	return len(data), update
}

func (t *TerminalState) putRune(r rune, update *Update) {
	if t.Cols == 0 || t.Rows == 0 {
		return
	}
	if t.wrapPending {
		t.wrapPending = false
		t.CursorX = 0
		if t.CursorY == t.Rows-1 {
			t.scrollUp()
			update.FullRedraw = true
		} else {
			t.CursorY++
		}
	}
	styleID, added := t.styleID(t.CurrentStyle)
	if added {
		update.DefinedStyles[styleID] = t.CurrentStyle
	}
	idx := t.CursorY*t.Cols + t.CursorX
	t.Cells[idx] = Cell{Rune: r, StyleID: styleID, Width: 1}
	update.DirtyRows[t.CursorY] = struct{}{}
	if t.CursorX == t.Cols-1 {
		t.wrapPending = true
	} else {
		t.CursorX++
	}
	update.CursorChanged = true
}

func (t *TerminalState) executeCSI(seq string, update *Update) {
	if seq == "" {
		return
	}
	private := false
	if seq[0] == '?' {
		private = true
		seq = seq[1:]
	}
	if seq == "" {
		return
	}
	final := seq[len(seq)-1]
	params := parseCSIParams(seq[:len(seq)-1])
	if private {
		switch final {
		case 'h':
			if len(params) == 1 && params[0] == 25 {
				t.CursorVisible = true
				update.VisibleChange = true
			}
		case 'l':
			if len(params) == 1 && params[0] == 25 {
				t.CursorVisible = false
				update.VisibleChange = true
			}
		}
		return
	}

	switch final {
	case 'A':
		t.CursorY -= max1(params, 1)
	case 'B':
		t.CursorY += max1(params, 1)
	case 'C':
		t.CursorX += max1(params, 1)
	case 'D':
		t.CursorX -= max1(params, 1)
	case 'H', 'f':
		row := paramOr(params, 0, 1) - 1
		col := paramOr(params, 1, 1) - 1
		t.CursorY, t.CursorX = row, col
	case 'G':
		t.CursorX = paramOr(params, 0, 1) - 1
	case 'd':
		t.CursorY = paramOr(params, 0, 1) - 1
	case 'J':
		t.eraseDisplay(paramOr(params, 0, 0))
		update.FullRedraw = true
	case 'K':
		t.eraseLine(paramOr(params, 0, 0))
		update.FullRedraw = true
	case 'X':
		t.eraseChars(max1(params, 1))
		update.FullRedraw = true
	case 'm':
		t.applySGR(params, update)
	case 'h', 'l':
	default:
		return
	}
	t.clampCursor()
	t.wrapPending = false
	update.CursorChanged = true
}

func (t *TerminalState) applySGR(params []int, update *Update) {
	if len(params) == 0 {
		params = []int{0}
	}
	i := 0
	for i < len(params) {
		p := params[i]
		switch p {
		case 0:
			t.CurrentStyle = DefaultStyle
		case 1:
			t.CurrentStyle.Bold = true
		case 3:
			t.CurrentStyle.Italic = true
		case 4:
			t.CurrentStyle.Underline = true
		case 7:
			t.CurrentStyle.Reverse = true
		case 22:
			t.CurrentStyle.Bold = false
		case 23:
			t.CurrentStyle.Italic = false
		case 24:
			t.CurrentStyle.Underline = false
		case 27:
			t.CurrentStyle.Reverse = false
		case 39:
			t.CurrentStyle.FG = Color{Mode: "default"}
		case 49:
			t.CurrentStyle.BG = Color{Mode: "default"}
		case 38, 48:
			isFG := p == 38
			if i+1 >= len(params) {
				return
			}
			mode := params[i+1]
			switch mode {
			case 5:
				if i+2 >= len(params) {
					return
				}
				if isFG {
					t.CurrentStyle.FG = colorIndexed(params[i+2])
				} else {
					t.CurrentStyle.BG = colorIndexed(params[i+2])
				}
				i += 2
			case 2:
				if i+4 >= len(params) {
					return
				}
				color := colorRGB(params[i+2], params[i+3], params[i+4])
				if isFG {
					t.CurrentStyle.FG = color
				} else {
					t.CurrentStyle.BG = color
				}
				i += 4
			}
		default:
			switch {
			case p >= 30 && p <= 37:
				t.CurrentStyle.FG = colorIndexed(p - 30)
			case p >= 40 && p <= 47:
				t.CurrentStyle.BG = colorIndexed(p - 40)
			case p >= 90 && p <= 97:
				t.CurrentStyle.FG = colorIndexed(p - 90 + 8)
			case p >= 100 && p <= 107:
				t.CurrentStyle.BG = colorIndexed(p - 100 + 8)
			}
		}
		i++
	}
}

func (t *TerminalState) eraseDisplay(mode int) {
	styleID, _ := t.styleID(t.CurrentStyle)
	switch mode {
	case 1:
		for row := 0; row <= t.CursorY; row++ {
			start, end := 0, t.Cols
			if row == t.CursorY {
				end = t.CursorX + 1
			}
			t.fillRow(row, start, end, styleID)
		}
	case 2:
		for i := range t.Cells {
			t.Cells[i] = blankCell(styleID)
		}
	default:
		for row := t.CursorY; row < t.Rows; row++ {
			start, end := 0, t.Cols
			if row == t.CursorY {
				start = t.CursorX
			}
			t.fillRow(row, start, end, styleID)
		}
	}
}

func (t *TerminalState) eraseLine(mode int) {
	styleID, _ := t.styleID(t.CurrentStyle)
	switch mode {
	case 1:
		t.fillRow(t.CursorY, 0, t.CursorX+1, styleID)
	case 2:
		t.fillRow(t.CursorY, 0, t.Cols, styleID)
	default:
		t.fillRow(t.CursorY, t.CursorX, t.Cols, styleID)
	}
}

func (t *TerminalState) eraseChars(n int) {
	styleID, _ := t.styleID(t.CurrentStyle)
	end := t.CursorX + n
	if end > t.Cols {
		end = t.Cols
	}
	t.fillRow(t.CursorY, t.CursorX, end, styleID)
}

func (t *TerminalState) fillRow(row, start, end int, styleID uint32) {
	if row < 0 || row >= t.Rows {
		return
	}
	if start < 0 {
		start = 0
	}
	if end > t.Cols {
		end = t.Cols
	}
	for col := start; col < end; col++ {
		t.Cells[row*t.Cols+col] = blankCell(styleID)
	}
}

func (t *TerminalState) scrollUp() {
	copy(t.Cells, t.Cells[t.Cols:])
	lastRow := (t.Rows - 1) * t.Cols
	for i := 0; i < t.Cols; i++ {
		t.Cells[lastRow+i] = blankCell(0)
	}
}

func (t *TerminalState) lineFeed() {
	if t.CursorY == t.Rows-1 {
		t.scrollUp()
	} else {
		t.CursorY++
	}
}

func (t *TerminalState) styleID(style Style) (uint32, bool) {
	if id, ok := t.styleToID[style]; ok {
		return id, false
	}
	id := t.nextStyleID
	t.nextStyleID++
	t.styleToID[style] = id
	t.styleByID[id] = style
	return id, true
}

func (t *TerminalState) clampCursor() {
	if t.CursorX < 0 {
		t.CursorX = 0
	}
	if t.CursorY < 0 {
		t.CursorY = 0
	}
	if t.Cols > 0 && t.CursorX >= t.Cols {
		t.CursorX = t.Cols - 1
	}
	if t.Rows > 0 && t.CursorY >= t.Rows {
		t.CursorY = t.Rows - 1
	}
}

func paramOr(params []int, idx, def int) int {
	if idx >= len(params) || params[idx] == 0 {
		return def
	}
	return params[idx]
}

func max1(params []int, def int) int {
	v := paramOr(params, 0, def)
	if v <= 0 {
		return def
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
