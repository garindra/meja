package server

import (
	"fmt"
	"io"
	"sync"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
	"golang.org/x/text/width"
)

const (
	maxCSISequenceBytes  = 256
	maxTerminalStyles    = 4096
	initialStyleCapacity = 32
)

type Cell = protocol.Cell
type Style = protocol.Style
type Color = protocol.Color

var DefaultStyle = protocol.CanonicalDefaultStyle()

var debugLogger struct {
	mu sync.Mutex
	w  io.Writer
}

func setTerminalDebugLogger(w io.Writer) {
	debugLogger.mu.Lock()
	defer debugLogger.mu.Unlock()
	debugLogger.w = w
}

func logUnsupportedf(format string, args ...any) {
	debugLogger.mu.Lock()
	defer debugLogger.mu.Unlock()
	if debugLogger.w == nil {
		return
	}
	_, _ = fmt.Fprintf(debugLogger.w, "meja terminal: "+format+"\n", args...)
}

func blankCell(styleID uint32) Cell {
	return Cell{Rune: ' ', StyleID: styleID, Width: 1}
}

func colorIndexed(idx int) Color {
	return Color{Mode: "indexed", Index: uint8(idx)}
}

func colorRGB(r, g, b int) Color {
	return Color{Mode: "rgb", R: uint8(r), G: uint8(g), B: uint8(b)}
}

func runeCellWidth(r rune) uint8 {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	default:
		return 1
	}
}

type Update struct {
	DirtySpans    []DirtySpan
	ScrollDelta   int
	FullRedraw    bool
	CursorChanged bool
	VisibleChange bool
	Replies       [][]byte
	trackDamage   bool
}

type DirtySpan struct {
	Start int
	End   int
}

type Row struct {
	Cells     []Cell
	WrapsNext bool
}

type TerminalState struct {
	Cols                  int
	Rows                  int
	GridRows              []Row
	CursorX               int
	CursorY               int
	CurrentStyle          Style
	CursorVisible         bool
	Parser                Parser
	wrapPending           bool
	SavedCursor           SavedCursor
	ScrollTop             int
	ScrollBottom          int
	History               []Row
	HistoryLimit          int
	Alternate             bool
	Primary               *screenBuffer
	ApplicationCursorKeys bool
	AutoWrap              bool
	OriginMode            bool
	InsertMode            bool
	G0Charset             byte
	G1Charset             byte
	ActiveCharset         int
	TabStops              []bool
	LastPrintedRune       rune
	LastPrintedValid      bool

	styleByID     []Style
	styleToID     map[Style]uint32
	nextStyleID   uint32
	cachedStyle   Style
	cachedStyleID uint32
	historyStart  int
}

type screenBuffer struct {
	Rows             []Row
	CursorX          int
	CursorY          int
	CurrentStyle     Style
	CursorVisible    bool
	WrapPending      bool
	SavedCursor      SavedCursor
	ScrollTop        int
	ScrollBottom     int
	LastPrintedRune  rune
	LastPrintedValid bool
	G0Charset        byte
	G1Charset        byte
	ActiveCharset    int
}

type SavedCursor struct {
	X           int
	Y           int
	Style       Style
	WrapPending bool
	OriginMode  bool
	G0Charset   byte
	G1Charset   byte
	ActiveGL    int
	Valid       bool
}

type logicalLine struct {
	cells        []Cell
	reflowable   bool
	cursorHere   bool
	cursorOffset int
}

type reflowRow struct {
	cells      []Cell
	continued  bool
	cursorHere bool
	cursorCol  int
}

func newTerminal(cols, rows int) *TerminalState {
	styles := make([]Style, 1, initialStyleCapacity)
	styles[0] = DefaultStyle
	t := &TerminalState{
		Cols:          cols,
		Rows:          rows,
		CurrentStyle:  DefaultStyle,
		cachedStyle:   DefaultStyle,
		CursorVisible: true,
		styleByID:     styles,
		styleToID:     make(map[Style]uint32, initialStyleCapacity),
		nextStyleID:   1,
		ScrollBottom:  rows - 1,
		HistoryLimit:  2000,
		AutoWrap:      true,
		G0Charset:     'B',
		G1Charset:     'B',
	}
	t.styleToID[DefaultStyle] = 0
	t.TabStops = defaultTabStops(cols)
	t.GridRows = make([]Row, rows)
	for row := range t.GridRows {
		t.GridRows[row] = blankRow(cols, 0)
	}
	return t
}

func (t *TerminalState) Resize(cols, rows int) {
	if cols == t.Cols && rows == t.Rows {
		return
	}
	if t.Alternate && t.Primary != nil {
		t.resizeWhileAlternate(cols, rows)
		return
	}
	next := newTerminal(cols, rows)
	next.CurrentStyle = t.CurrentStyle
	next.CursorVisible = t.CursorVisible
	next.Parser = t.Parser.clone()
	next.wrapPending = t.wrapPending
	next.SavedCursor = t.SavedCursor
	next.ApplicationCursorKeys = t.ApplicationCursorKeys
	next.AutoWrap = t.AutoWrap
	next.OriginMode = t.OriginMode
	next.InsertMode = t.InsertMode
	next.G0Charset = t.G0Charset
	next.G1Charset = t.G1Charset
	next.ActiveCharset = t.ActiveCharset
	next.TabStops = resizedTabStops(t.TabStops, t.Cols, cols)
	next.LastPrintedRune = t.LastPrintedRune
	next.LastPrintedValid = t.LastPrintedValid
	next.styleByID = cloneStyles(t.styleByID)
	next.styleToID = cloneStyleIDMap(t.styleToID)
	next.nextStyleID = t.nextStyleID
	next.History = t.cloneHistoryRows()
	next.HistoryLimit = t.HistoryLimit

	lines := t.collectLogicalLines()
	projected := make([]reflowRow, 0, max(rows, len(lines)))
	cursorProjectedRow, cursorProjectedCol := 0, 0
	cursorPlaced := false
	for _, line := range lines {
		rowsForLine := t.projectLogicalLine(line, cols)
		start := len(projected)
		projected = append(projected, rowsForLine...)
		if line.cursorHere {
			cursorProjectedRow, cursorProjectedCol = mapCursorOffset(start, rowsForLine, line.cursorOffset, cols)
			cursorPlaced = true
		}
	}

	start := 0
	if len(projected) > rows {
		start = len(projected) - rows
	}
	visible := projected[start:]
	if len(visible) > rows {
		visible = visible[:rows]
	}
	for row := range next.GridRows {
		next.GridRows[row] = blankRow(cols, 0)
	}
	for row, src := range visible {
		copy(next.GridRows[row].Cells, src.cells)
		next.GridRows[row].WrapsNext = src.continued
		if src.cursorHere {
			next.CursorY = row
			next.CursorX = src.cursorCol
		}
	}
	if cursorPlaced {
		cursorProjectedRow -= start
		if cursorProjectedRow >= 0 && cursorProjectedRow < len(visible) {
			next.CursorY = cursorProjectedRow
			next.CursorX = cursorProjectedCol
		}
	}
	next.clampCursor()
	*t = *next
}

func (t *TerminalState) resizeWhileAlternate(cols, rows int) {
	oldCols := t.Cols
	p := t.Primary
	primary := newTerminal(t.Cols, t.Rows)
	primary.GridRows = cloneRows(p.Rows)
	primary.CursorX = p.CursorX
	primary.CursorY = p.CursorY
	primary.CurrentStyle = p.CurrentStyle
	primary.CursorVisible = p.CursorVisible
	primary.wrapPending = p.WrapPending
	primary.SavedCursor = p.SavedCursor
	primary.ScrollTop = p.ScrollTop
	primary.ScrollBottom = p.ScrollBottom
	primary.LastPrintedRune = p.LastPrintedRune
	primary.LastPrintedValid = p.LastPrintedValid
	primary.History = t.cloneHistoryRows()
	primary.HistoryLimit = t.HistoryLimit
	primary.styleByID = cloneStyles(t.styleByID)
	primary.styleToID = cloneStyleIDMap(t.styleToID)
	primary.nextStyleID = t.nextStyleID
	primary.Resize(cols, rows)
	active := make([]Row, rows)
	for row := range active {
		active[row] = blankRow(cols, 0)
	}
	for row := 0; row < min(rows, len(t.GridRows)); row++ {
		copy(active[row].Cells, t.GridRows[row].Cells[:min(cols, len(t.GridRows[row].Cells))])
		active[row].WrapsNext = t.GridRows[row].WrapsNext
	}
	t.Cols, t.Rows = cols, rows
	t.TabStops = resizedTabStops(t.TabStops, oldCols, cols)
	t.GridRows = active
	t.ScrollTop = 0
	t.ScrollBottom = rows - 1
	t.clampCursor()
	t.History = primary.History
	t.historyStart = 0
	t.Primary = &screenBuffer{Rows: cloneRows(primary.GridRows), CursorX: primary.CursorX, CursorY: primary.CursorY, CurrentStyle: primary.CurrentStyle, CursorVisible: primary.CursorVisible, WrapPending: primary.wrapPending, SavedCursor: primary.SavedCursor, ScrollTop: primary.ScrollTop, ScrollBottom: primary.ScrollBottom, LastPrintedRune: primary.LastPrintedRune, LastPrintedValid: primary.LastPrintedValid, G0Charset: p.G0Charset, G1Charset: p.G1Charset, ActiveCharset: p.ActiveCharset}
}

func (t *TerminalState) SnapshotStyles() []protocolStyleDef {
	out := make([]protocolStyleDef, 0, len(t.styleByID))
	for id, style := range t.styleByID {
		out = append(out, protocolStyleDef{ID: uint32(id), Style: style})
	}
	return out
}

func (t *TerminalState) LookupStyle(id uint32) (Style, bool) {
	if uint64(id) >= uint64(len(t.styleByID)) {
		return Style{}, false
	}
	return t.styleByID[id], true
}

type protocolStyleDef struct {
	ID    uint32
	Style Style
}

func (t *TerminalState) Apply(data []byte) Update {
	var update Update
	update.Reset(t.Rows)
	t.ApplyInto(data, &update)
	return update
}

func (t *TerminalState) ApplyInto(data []byte, update *Update) {
	if update.trackDamage {
		update.ensureRows(t.Rows)
	}
	t.syncCurrentStyleID()
	for len(data) > 0 {
		if t.Parser.state != parserText && (data[0] == 0x18 || data[0] == 0x1a) {
			t.Parser.state = parserText
			t.Parser.csiBuf = t.Parser.csiBuf[:0]
			data = data[1:]
			continue
		}
		switch t.Parser.state {
		case parserText:
			if len(t.Parser.utf8Buf) == 0 && data[0] >= utf8.RuneSelf && invalidUTF8Lead(data[0]) {
				end := 1
				for end < len(data) && invalidUTF8Lead(data[end]) {
					end++
				}
				data = data[end:]
				continue
			}
			if len(t.Parser.utf8Buf) > 0 || data[0] >= 0x80 {
				var consumed int
				consumed = t.consumeUTF8(data, update)
				data = data[consumed:]
				continue
			}
			if data[0] >= 0x20 && data[0] <= 0x7e && t.activeCharset() != '0' {
				end := 1
				for end < len(data) && data[end] >= 0x20 && data[end] <= 0x7e {
					end++
				}
				t.putASCII(data[:end], update)
				data = data[end:]
				continue
			}
			b := data[0]
			data = data[1:]
			switch b {
			case 0x1b:
				t.Parser.state = parserESC
			case '\r':
				t.breakWrapChainAt(t.CursorY)
				t.CursorX = 0
				t.wrapPending = false
				update.CursorChanged = true
			case '\n', '\v', '\f':
				if t.wrapPending {
					t.CursorX = 0
				}
				t.lineFeed(update)
				t.wrapPending = false
				update.CursorChanged = true
			case '\b':
				if t.CursorX > 0 {
					t.CursorX--
				}
				t.wrapPending = false
				update.CursorChanged = true
			case '\t':
				t.CursorX = t.nextTabStop(t.CursorX)
				t.wrapPending = false
				update.CursorChanged = true
			case 0x0e:
				t.ActiveCharset = 1
			case 0x0f:
				t.ActiveCharset = 0
			case 0x07:
			default:
				if b >= 0x20 && b <= 0x7e {
					t.putRune(t.translateByte(b), update)
				}
			}
		case parserESC:
			switch data[0] {
			case '[':
				t.Parser.state = parserCSI
				t.Parser.csiBuf = t.Parser.csiBuf[:0]
			case ']':
				t.Parser.state = parserOSC
			case 'P':
				t.Parser.state = parserDCS
			case '7':
				t.saveCursor()
				t.Parser.state = parserText
			case '8':
				t.restoreCursor()
				t.Parser.state = parserText
			case 'M':
				t.reverseIndex(update)
				t.Parser.state = parserText
			case 'D':
				t.lineFeed(update)
				t.wrapPending = false
				update.CursorChanged = true
				t.Parser.state = parserText
			case 'E':
				t.CursorX = 0
				t.lineFeed(update)
				t.wrapPending = false
				update.CursorChanged = true
				t.Parser.state = parserText
			case 'H':
				t.setTabStop(t.CursorX)
				t.Parser.state = parserText
			case 'c':
				t.reset()
				update.FullRedraw = true
				update.CursorChanged = true
				update.VisibleChange = true
				t.Parser.state = parserText
			case '=', '>':
				// Application/numeric keypad mode does not affect Meja's input protocol.
				t.Parser.state = parserText
			case '(', ')', '*', '+', '-', '.', '/', '%':
				if data[0] == '(' {
					t.Parser.charsetTarget = 0
				} else if data[0] == ')' {
					t.Parser.charsetTarget = 1
				} else {
					t.Parser.charsetTarget = -1
				}
				t.Parser.state = parserESCCharset
			default:
				logUnsupportedf("unsupported ESC %q", data[0])
				t.Parser.state = parserText
			}
			data = data[1:]
		case parserESCCharset:
			switch t.Parser.charsetTarget {
			case 0:
				t.G0Charset = data[0]
			case 1:
				t.G1Charset = data[0]
			}
			t.Parser.state = parserText
			data = data[1:]
		case parserCSI:
			b := data[0]
			data = data[1:]
			if len(t.Parser.csiBuf) == maxCSISequenceBytes {
				t.Parser.csiBuf = t.Parser.csiBuf[:0]
				if b >= 0x40 && b <= 0x7e {
					t.Parser.state = parserText
				} else {
					t.Parser.state = parserCSIDiscard
				}
				continue
			}
			t.Parser.csiBuf = append(t.Parser.csiBuf, b)
			if b >= 0x40 && b <= 0x7e {
				t.executeCSI(t.Parser.csiBuf, update)
				t.Parser.state = parserText
				t.Parser.csiBuf = t.Parser.csiBuf[:0]
			}
		case parserCSIDiscard:
			b := data[0]
			data = data[1:]
			if b >= 0x40 && b <= 0x7e {
				t.Parser.state = parserText
			}
		case parserOSC:
			b := data[0]
			data = data[1:]
			switch b {
			case 0x07:
				t.Parser.state = parserText
			case 0x1b:
				t.Parser.state = parserOSCESC
			}
		case parserOSCESC:
			b := data[0]
			data = data[1:]
			if b == '\\' {
				t.Parser.state = parserText
				continue
			}
			t.Parser.state = parserOSC
		case parserDCS:
			b := data[0]
			data = data[1:]
			if b == 0x1b {
				t.Parser.state = parserDCSESC
			}
		case parserDCSESC:
			b := data[0]
			data = data[1:]
			if b == '\\' {
				t.Parser.state = parserText
			} else if b != 0x1b {
				t.Parser.state = parserDCS
			}
		}
	}
}

func (t *TerminalState) putASCII(data []byte, update *Update) {
	if t.Cols == 0 || t.Rows == 0 {
		return
	}
	if t.InsertMode {
		for _, b := range data {
			t.putRune(rune(b), update)
		}
		return
	}
	styleID := t.currentStyleID()
	for len(data) > 0 {
		if t.wrapPending {
			t.wrapPending = false
			if t.AutoWrap {
				t.setRowWrapped(t.CursorY, true)
				t.CursorX = 0
				t.wrapLine(update)
			}
		}
		start := t.CursorX
		count := min(len(data), t.Cols-start)
		end := start + count
		cells := t.GridRows[t.CursorY].Cells
		dirtyStart, dirtyEnd := start, end
		if start > 0 && cells[start].Width == 0 && cells[start-1].Width == 2 {
			cells[start-1] = blankCell(styleID)
			dirtyStart--
		}
		if end < t.Cols && cells[end-1].Width == 2 {
			cells[end] = blankCell(styleID)
			dirtyEnd++
		}
		t.setRowWrapped(t.CursorY, false)
		for column, b := range data[:count] {
			cells[start+column] = Cell{Rune: rune(b), StyleID: styleID, Width: 1}
		}
		update.markDirty(t.CursorY, dirtyStart, dirtyEnd, t.Cols)
		t.LastPrintedRune = rune(data[count-1])
		t.LastPrintedValid = true
		if end == t.Cols {
			t.CursorX = t.Cols - 1
			t.wrapPending = t.AutoWrap
		} else {
			t.CursorX = end
		}
		data = data[count:]
	}
	update.CursorChanged = true
}

func invalidUTF8Lead(b byte) bool {
	return b >= utf8.RuneSelf && (b < 0xc2 || b > 0xf4)
}

func (t *TerminalState) consumeUTF8(data []byte, update *Update) int {
	if len(t.Parser.utf8Buf) == 0 {
		r, size := utf8.DecodeRune(data)
		if r != utf8.RuneError || size != 1 {
			t.putRune(r, update)
			return size
		}
		if utf8.FullRune(data) {
			return 1
		}
		t.Parser.utf8Buf = append(t.Parser.utf8Buf, data...)
		return len(data)
	}
	for i := 0; i < len(data); i++ {
		t.Parser.utf8Buf = append(t.Parser.utf8Buf, data[i])
		if utf8.FullRune(t.Parser.utf8Buf) {
			r, size := utf8.DecodeRune(t.Parser.utf8Buf)
			if r == utf8.RuneError && size == 1 {
				t.Parser.utf8Buf = t.Parser.utf8Buf[:0]
				return i + 1
			}
			t.putRune(r, update)
			t.Parser.utf8Buf = t.Parser.utf8Buf[:0]
			return i + 1
		}
	}
	return len(data)
}

func (t *TerminalState) putRune(r rune, update *Update) {
	if t.Cols == 0 || t.Rows == 0 {
		return
	}
	if t.wrapPending {
		t.wrapPending = false
		if t.AutoWrap {
			t.setRowWrapped(t.CursorY, true)
			t.CursorX = 0
			t.wrapLine(update)
		}
	}
	cellWidth := 1
	if r >= utf8.RuneSelf {
		cellWidth = int(runeCellWidth(r))
	}
	if cellWidth > t.Cols {
		cellWidth = 1
	}
	if t.CursorX+cellWidth > t.Cols {
		if t.AutoWrap {
			t.setRowWrapped(t.CursorY, true)
			t.CursorX = 0
			t.wrapLine(update)
		} else {
			cellWidth = 1
			t.CursorX = t.Cols - 1
		}
	}
	styleID := t.currentStyleID()
	writtenColumn := t.CursorX
	if t.InsertMode {
		t.insertChars(cellWidth, styleID)
	}
	dirtyStart, dirtyEnd := t.clearGlyphsForWrite(t.CursorY, writtenColumn, cellWidth, styleID)
	t.GridRows[t.CursorY].Cells[writtenColumn] = Cell{Rune: r, StyleID: styleID, Width: uint8(cellWidth)}
	for column := writtenColumn + 1; column < writtenColumn+cellWidth; column++ {
		t.GridRows[t.CursorY].Cells[column] = Cell{StyleID: styleID, Width: 0}
	}
	t.LastPrintedRune = r
	t.LastPrintedValid = true
	update.markDirty(t.CursorY, dirtyStart, dirtyEnd, t.Cols)
	if t.CursorX+cellWidth == t.Cols {
		t.CursorX = t.Cols - 1
		t.wrapPending = t.AutoWrap
	} else {
		t.CursorX += cellWidth
		t.setRowWrapped(t.CursorY, false)
	}
	update.CursorChanged = true
}

func (t *TerminalState) clearGlyphsForWrite(row, start, cellWidth int, styleID uint32) (int, int) {
	dirtyStart, dirtyEnd := start, start+cellWidth
	for column := start; column < start+cellWidth; column++ {
		cell := t.GridRows[row].Cells[column]
		switch {
		case cell.Width == 2:
			t.GridRows[row].Cells[column] = blankCell(styleID)
			if column+1 < t.Cols {
				t.GridRows[row].Cells[column+1] = blankCell(styleID)
				dirtyEnd = max(dirtyEnd, column+2)
			}
		case cell.Width == 0 && column > 0 && t.GridRows[row].Cells[column-1].Width == 2:
			t.GridRows[row].Cells[column-1] = blankCell(styleID)
			t.GridRows[row].Cells[column] = blankCell(styleID)
			dirtyStart = min(dirtyStart, column-1)
		}
	}
	return dirtyStart, dirtyEnd
}

func (t *TerminalState) executeCSI(seq []byte, update *Update) {
	if len(seq) == 0 {
		return
	}
	var parsed CSISequence
	if !parseCSISequence(seq, &parsed) {
		return
	}
	params := parsed.Params[:parsed.ParamCount]
	intermediates := parsed.Intermediates[:parsed.IntermediateCount]
	switch parsed.PrivatePrefix {
	case '?':
		switch parsed.Final {
		case 'h':
			t.setPrivateModes(params, true, update)
		case 'l':
			t.setPrivateModes(params, false, update)
		default:
			logUnsupportedf("unsupported private CSI ?%s", seq)
		}
		return
	case 0:
	default:
		logUnsupportedf("unsupported prefixed CSI %s", seq)
		return
	}

	switch parsed.Final {
	case 'A':
		t.breakWrapChainAt(t.CursorY)
		t.CursorY -= max1(params, 1)
	case 'B':
		t.breakWrapChainAt(t.CursorY)
		t.CursorY += max1(params, 1)
	case 'C':
		t.CursorX += max1(params, 1)
	case 'D':
		t.CursorX -= max1(params, 1)
	case 'H', 'f':
		t.breakWrapChainAt(t.CursorY)
		row := paramOr(params, 0, 1) - 1
		col := paramOr(params, 1, 1) - 1
		if t.OriginMode {
			row += t.ScrollTop
			row = min(max(row, t.ScrollTop), t.ScrollBottom)
		}
		t.CursorY, t.CursorX = row, col
	case 'G':
		t.CursorX = paramOr(params, 0, 1) - 1
	case 'd':
		t.breakWrapChainAt(t.CursorY)
		row := paramOr(params, 0, 1) - 1
		if t.OriginMode {
			row += t.ScrollTop
			row = min(max(row, t.ScrollTop), t.ScrollBottom)
		}
		t.CursorY = row
	case 'J':
		mode := paramOr(params, 0, 0)
		if mode == 3 {
			t.History = nil
			t.historyStart = 0
		} else {
			t.breakAllWrapChains()
			t.eraseDisplay(mode)
			update.FullRedraw = true
		}
	case 'K':
		t.breakWrapChainAt(t.CursorY)
		start, end := t.eraseLine(paramOr(params, 0, 0))
		update.markDirty(t.CursorY, start, end, t.Cols)
	case 'X':
		t.breakWrapChainAt(t.CursorY)
		start, end := t.eraseChars(max1(params, 1))
		update.markDirty(t.CursorY, start, end, t.Cols)
	case 'P':
		t.breakWrapChainAt(t.CursorY)
		styleID := t.currentStyleID()
		t.deleteChars(max1(params, 1), styleID)
		update.markDirty(t.CursorY, t.CursorX, t.Cols, t.Cols)
	case '@':
		t.breakWrapChainAt(t.CursorY)
		styleID := t.currentStyleID()
		t.insertChars(max1(params, 1), styleID)
		update.markDirty(t.CursorY, t.CursorX, t.Cols, t.Cols)
	case 'L':
		styleID := t.eraseStyleID()
		t.insertLines(max1(params, 1), styleID)
		update.FullRedraw = true
	case 'M':
		styleID := t.eraseStyleID()
		t.deleteLines(max1(params, 1), styleID)
		update.FullRedraw = true
	case 'S':
		count := min(max1(params, 1), t.ScrollBottom-t.ScrollTop+1)
		styleID := t.eraseStyleID()
		for range count {
			t.scrollUpRegion(t.ScrollTop, t.ScrollBottom, styleID)
		}
		if t.ScrollTop == 0 && t.ScrollBottom == t.Rows-1 {
			update.recordScroll(-count, t.Rows)
		} else {
			update.FullRedraw = true
		}
		for row := t.ScrollBottom - count + 1; row <= t.ScrollBottom; row++ {
			update.markDirty(row, 0, t.Cols, t.Cols)
		}
	case 'T':
		count := min(max1(params, 1), t.ScrollBottom-t.ScrollTop+1)
		styleID := t.eraseStyleID()
		for range count {
			t.scrollDownRegion(t.ScrollTop, t.ScrollBottom, styleID)
		}
		if t.ScrollTop == 0 && t.ScrollBottom == t.Rows-1 {
			update.recordScroll(count, t.Rows)
		} else {
			update.FullRedraw = true
		}
		for row := t.ScrollTop; row < t.ScrollTop+count; row++ {
			update.markDirty(row, 0, t.Cols, t.Cols)
		}
	case 'Z':
		for range max1(params, 1) {
			t.CursorX = t.previousTabStop(t.CursorX)
		}
	case 'g':
		switch paramOr(params, 0, 0) {
		case 0:
			t.clearTabStop(t.CursorX)
		case 3:
			clear(t.TabStops)
		}
	case 'b':
		if t.LastPrintedValid {
			for count := 0; count < max1(params, 1); count++ {
				t.putRune(t.LastPrintedRune, update)
			}
		}
	case 'r':
		t.setScrollRegion(params)
		t.CursorX = 0
		t.CursorY = 0
		if t.OriginMode {
			t.CursorY = t.ScrollTop
		}
	case 's':
		if len(params) != 0 || len(intermediates) != 0 {
			logUnsupportedf("unsupported CSI %s", seq)
			return
		}
		t.saveCursor()
	case 'u':
		if len(params) != 0 || len(intermediates) != 0 {
			logUnsupportedf("unsupported CSI %s", seq)
			return
		}
		t.restoreCursor()
	case 'n':
		if len(params) == 1 && params[0] == 6 {
			row := t.CursorY + 1
			if t.OriginMode {
				row -= t.ScrollTop
			}
			update.Replies = append(update.Replies, []byte(formatDSR(row, t.CursorX+1)))
			return
		}
		logUnsupportedf("unsupported CSI %s", seq)
		return
	case 'c':
		update.Replies = append(update.Replies, []byte("\x1b[?1;0c"))
		return
	case 'm':
		t.applySGR(params, update)
	case 'h', 'l':
		set := parsed.Final == 'h'
		for _, mode := range params {
			switch mode {
			case 4:
				t.InsertMode = set
			default:
				logUnsupportedf("unsupported mode %d%c", mode, parsed.Final)
			}
		}
	case 'p':
		if len(intermediates) == 1 && intermediates[0] == '!' {
			t.softReset()
			update.CursorChanged = true
			update.VisibleChange = true
			return
		}
		logUnsupportedf("unsupported CSI %s", seq)
		return
	default:
		logUnsupportedf("unsupported CSI %s", seq)
		return
	}
	t.clampCursor()
	t.wrapPending = false
	update.CursorChanged = true
}

func (t *TerminalState) applySGR(params []int, update *Update) {
	defer t.syncCurrentStyleID()
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
		case 2:
			t.CurrentStyle.Dim = true
		case 3:
			t.CurrentStyle.Italic = true
		case 4:
			t.CurrentStyle.Underline = true
		case 5:
			t.CurrentStyle.Blink = true
		case 7:
			t.CurrentStyle.Reverse = true
		case 8:
			t.CurrentStyle.Invisible = true
		case 22:
			t.CurrentStyle.Bold = false
			t.CurrentStyle.Dim = false
		case 23:
			t.CurrentStyle.Italic = false
		case 24:
			t.CurrentStyle.Underline = false
		case 25:
			t.CurrentStyle.Blink = false
		case 27:
			t.CurrentStyle.Reverse = false
		case 28:
			t.CurrentStyle.Invisible = false
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
	styleID := t.currentStyleID()
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
		for row := range t.GridRows {
			t.GridRows[row] = blankRow(t.Cols, styleID)
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

func (t *TerminalState) eraseLine(mode int) (int, int) {
	styleID := t.currentStyleID()
	switch mode {
	case 1:
		t.fillRow(t.CursorY, 0, t.CursorX+1, styleID)
		return 0, t.CursorX + 1
	case 2:
		t.fillRow(t.CursorY, 0, t.Cols, styleID)
		return 0, t.Cols
	default:
		t.fillRow(t.CursorY, t.CursorX, t.Cols, styleID)
		return t.CursorX, t.Cols
	}
}

func (t *TerminalState) eraseChars(n int) (int, int) {
	styleID := t.currentStyleID()
	end := t.CursorX + n
	if end > t.Cols {
		end = t.Cols
	}
	t.fillRow(t.CursorY, t.CursorX, end, styleID)
	return t.CursorX, end
}

func (u *Update) markDirty(row, start, end, cols int) {
	if u == nil || !u.trackDamage || u.FullRedraw || row < 0 || row >= len(u.DirtySpans) || start >= end || cols <= 0 {
		return
	}
	if start < 0 {
		start = 0
	}
	if end > cols {
		end = cols
	}
	if start >= end {
		return
	}
	span := u.DirtySpans[row]
	if span.End == 0 {
		u.DirtySpans[row] = DirtySpan{Start: start, End: end}
		return
	}
	if start < span.Start {
		span.Start = start
	}
	if end > span.End {
		span.End = end
	}
	u.DirtySpans[row] = span
}

func (u *Update) recordScroll(delta, rows int) {
	if u == nil || !u.trackDamage {
		return
	}
	if delta == 0 || (u.ScrollDelta != 0 && (u.ScrollDelta < 0) != (delta < 0)) {
		u.FullRedraw = true
		u.ScrollDelta = 0
		return
	}
	if u.FullRedraw {
		return
	}
	rows = min(rows, len(u.DirtySpans))
	if delta < 0 {
		shift := min(-delta, rows)
		copy(u.DirtySpans[:rows-shift], u.DirtySpans[shift:rows])
		clear(u.DirtySpans[rows-shift : rows])
	} else {
		shift := min(delta, rows)
		copy(u.DirtySpans[shift:rows], u.DirtySpans[:rows-shift])
		clear(u.DirtySpans[:shift])
	}
	u.ScrollDelta += delta
	if u.ScrollDelta < -rows {
		u.ScrollDelta = -rows
	} else if u.ScrollDelta > rows {
		u.ScrollDelta = rows
	}
}

func (u *Update) Merge(next Update, rows int) {
	u.trackDamage = true
	u.ensureRows(rows)
	if next.FullRedraw {
		u.FullRedraw = true
		u.ScrollDelta = 0
	} else if next.ScrollDelta != 0 {
		u.recordScroll(next.ScrollDelta, rows)
	}
	for row, span := range next.DirtySpans {
		if row >= rows || span.End == 0 {
			continue
		}
		current := u.DirtySpans[row]
		if current.End == 0 {
			u.DirtySpans[row] = span
			continue
		}
		if span.Start < current.Start {
			current.Start = span.Start
		}
		if span.End > current.End {
			current.End = span.End
		}
		u.DirtySpans[row] = current
	}
	u.CursorChanged = u.CursorChanged || next.CursorChanged
	u.VisibleChange = u.VisibleChange || next.VisibleChange
}

func (u *Update) ensureRows(rows int) {
	if rows < 0 {
		rows = 0
	}
	if cap(u.DirtySpans) < rows {
		u.DirtySpans = make([]DirtySpan, rows)
	} else {
		u.DirtySpans = u.DirtySpans[:rows]
	}
}

func (u *Update) Reset(rows int) {
	u.ResetFor(rows, true)
}

func (u *Update) ResetFor(rows int, trackDamage bool) {
	u.trackDamage = trackDamage
	if trackDamage {
		u.ensureRows(rows)
		clear(u.DirtySpans)
	} else {
		u.DirtySpans = u.DirtySpans[:0]
	}
	u.ScrollDelta = 0
	u.FullRedraw = false
	u.CursorChanged = false
	u.VisibleChange = false
	u.Replies = u.Replies[:0]
}

func (u *Update) HasDamage() bool {
	for _, span := range u.DirtySpans {
		if span.End != 0 {
			return true
		}
	}
	return false
}

func (u *Update) HasRenderChange() bool {
	return u.FullRedraw || u.ScrollDelta != 0 || u.CursorChanged || u.VisibleChange || u.HasDamage()
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
	t.setRowWrapped(row, false)
	for col := start; col < end; col++ {
		t.GridRows[row].Cells[col] = blankCell(styleID)
	}
}

func (t *TerminalState) scrollUpRegion(top, bottom int, styleID uint32) {
	if top < 0 || bottom >= len(t.GridRows) || top > bottom {
		return
	}
	recycled := t.GridRows[top]
	if !t.Alternate && top == 0 && bottom == len(t.GridRows)-1 {
		recycled = t.rotateHistoryRow(recycled)
	}
	copy(t.GridRows[top:bottom], t.GridRows[top+1:bottom+1])
	clearRow(&recycled, styleID)
	t.GridRows[bottom] = recycled
	t.breakWrapChainAt(top)
	t.breakWrapChainAt(bottom)
}

func (t *TerminalState) enterAlternateScreen() {
	if t.Alternate {
		return
	}
	t.Primary = &screenBuffer{Rows: cloneRows(t.GridRows), CursorX: t.CursorX, CursorY: t.CursorY, CurrentStyle: t.CurrentStyle, CursorVisible: t.CursorVisible, WrapPending: t.wrapPending, SavedCursor: t.SavedCursor, ScrollTop: t.ScrollTop, ScrollBottom: t.ScrollBottom, LastPrintedRune: t.LastPrintedRune, LastPrintedValid: t.LastPrintedValid, G0Charset: t.G0Charset, G1Charset: t.G1Charset, ActiveCharset: t.ActiveCharset}
	t.GridRows = make([]Row, t.Rows)
	for row := range t.GridRows {
		t.GridRows[row] = blankRow(t.Cols, 0)
	}
	t.CursorX, t.CursorY = 0, 0
	t.CurrentStyle = DefaultStyle
	t.cachedStyle = DefaultStyle
	t.cachedStyleID = 0
	t.CursorVisible = true
	t.wrapPending = false
	t.SavedCursor = SavedCursor{}
	t.ScrollTop = 0
	t.ScrollBottom = t.Rows - 1
	t.LastPrintedValid = false
	t.Alternate = true
}

func (t *TerminalState) exitAlternateScreen() {
	if !t.Alternate || t.Primary == nil {
		return
	}
	p := t.Primary
	t.GridRows = cloneRows(p.Rows)
	t.CursorX = p.CursorX
	t.CursorY = p.CursorY
	t.CurrentStyle = p.CurrentStyle
	t.syncCurrentStyleID()
	t.CursorVisible = p.CursorVisible
	t.wrapPending = p.WrapPending
	t.SavedCursor = p.SavedCursor
	t.ScrollTop = p.ScrollTop
	t.ScrollBottom = p.ScrollBottom
	t.LastPrintedRune = p.LastPrintedRune
	t.LastPrintedValid = p.LastPrintedValid
	t.G0Charset = p.G0Charset
	t.G1Charset = p.G1Charset
	t.ActiveCharset = p.ActiveCharset
	t.Alternate = false
	t.Primary = nil
	t.clampCursor()
}

func (t *TerminalState) rotateHistoryRow(row Row) Row {
	if t.HistoryLimit <= 0 {
		return row
	}
	if len(t.History) > t.HistoryLimit {
		ordered := t.orderedHistoryRows()
		t.History = ordered[len(ordered)-t.HistoryLimit:]
		t.historyStart = 0
	} else if t.historyStart != 0 && len(t.History) < t.HistoryLimit {
		t.History = t.orderedHistoryRows()
		t.historyStart = 0
	}
	if len(t.History) >= t.HistoryLimit {
		index := t.historyStart
		recycled := t.History[index]
		t.History[index] = row
		t.historyStart = (t.historyStart + 1) % len(t.History)
		return recycled
	}
	t.History = append(t.History, row)
	return Row{Cells: make([]Cell, len(row.Cells))}
}

func (t *TerminalState) SnapshotRows() (history, primary []Row) {
	history = t.cloneHistoryRows()
	primary = cloneRows(t.GridRows)
	return history, primary
}

func (t *TerminalState) cloneHistoryRows() []Row {
	out := make([]Row, len(t.History))
	for i := range out {
		row := t.History[(t.historyStart+i)%len(t.History)]
		out[i] = Row{Cells: append([]Cell(nil), row.Cells...), WrapsNext: row.WrapsNext}
	}
	return out
}

func (t *TerminalState) orderedHistoryRows() []Row {
	out := make([]Row, len(t.History))
	for i := range out {
		out[i] = t.History[(t.historyStart+i)%len(t.History)]
	}
	return out
}

func cloneRows(rows []Row) []Row {
	out := make([]Row, len(rows))
	for i, row := range rows {
		out[i] = Row{Cells: append([]Cell(nil), row.Cells...), WrapsNext: row.WrapsNext}
	}
	return out
}

func (t *TerminalState) scrollDownRegion(top, bottom int, styleID uint32) {
	if top < 0 || bottom >= len(t.GridRows) || top > bottom {
		return
	}
	recycled := t.GridRows[bottom]
	copy(t.GridRows[top+1:bottom+1], t.GridRows[top:bottom])
	clearRow(&recycled, styleID)
	t.GridRows[top] = recycled
	t.breakWrapChainAt(top)
	t.breakWrapChainAt(bottom)
}

func (t *TerminalState) reverseIndex(update *Update) {
	if t.CursorY != t.ScrollTop {
		if t.CursorY > 0 {
			t.CursorY--
		}
		update.CursorChanged = true
		return
	}
	styleID := t.eraseStyleID()
	t.scrollDownRegion(t.ScrollTop, t.ScrollBottom, styleID)
	delta := t.fullViewportScrollDelta(1)
	update.recordScroll(delta, t.Rows)
	if delta != 0 {
		update.markDirty(t.ScrollTop, 0, t.Cols, t.Cols)
	}
	update.CursorChanged = true
}

func (t *TerminalState) saveCursor() {
	t.SavedCursor = SavedCursor{
		X:           t.CursorX,
		Y:           t.CursorY,
		Style:       t.CurrentStyle,
		WrapPending: t.wrapPending,
		OriginMode:  t.OriginMode,
		G0Charset:   t.G0Charset,
		G1Charset:   t.G1Charset,
		ActiveGL:    t.ActiveCharset,
		Valid:       true,
	}
}

func (t *TerminalState) restoreCursor() {
	if !t.SavedCursor.Valid {
		return
	}
	t.CursorX = t.SavedCursor.X
	t.CursorY = t.SavedCursor.Y
	t.CurrentStyle = t.SavedCursor.Style
	t.syncCurrentStyleID()
	t.wrapPending = t.SavedCursor.WrapPending
	t.OriginMode = t.SavedCursor.OriginMode
	t.G0Charset = t.SavedCursor.G0Charset
	t.G1Charset = t.SavedCursor.G1Charset
	t.ActiveCharset = t.SavedCursor.ActiveGL
	t.clampCursor()
}

func (t *TerminalState) setScrollRegion(params []int) {
	top, bottom := 0, t.Rows-1
	if len(params) > 0 {
		top = paramOr(params, 0, 1) - 1
	}
	if len(params) > 1 {
		bottom = paramOr(params, 1, t.Rows) - 1
	}
	if top < 0 || bottom >= t.Rows || top >= bottom {
		top, bottom = 0, t.Rows-1
	}
	t.ScrollTop = top
	t.ScrollBottom = bottom
	t.wrapPending = false
}

func (t *TerminalState) deleteChars(n int, styleID uint32) {
	if t.CursorY < 0 || t.CursorY >= t.Rows || n <= 0 {
		return
	}
	row := t.GridRows[t.CursorY].Cells
	if t.CursorX < 0 || t.CursorX >= len(row) {
		return
	}
	if n > len(row)-t.CursorX {
		n = len(row) - t.CursorX
	}
	copy(row[t.CursorX:], row[t.CursorX+n:])
	for i := len(row) - n; i < len(row); i++ {
		row[i] = blankCell(styleID)
	}
	t.normalizeRow(t.CursorY)
}

func (t *TerminalState) insertChars(n int, styleID uint32) {
	if t.CursorY < 0 || t.CursorY >= t.Rows || t.CursorX < 0 || t.CursorX >= t.Cols || n <= 0 {
		return
	}
	row := t.GridRows[t.CursorY].Cells
	if n > len(row)-t.CursorX {
		n = len(row) - t.CursorX
	}
	copy(row[t.CursorX+n:], row[t.CursorX:len(row)-n])
	for i := t.CursorX; i < t.CursorX+n; i++ {
		row[i] = blankCell(styleID)
	}
	t.normalizeRow(t.CursorY)
}

func (t *TerminalState) insertLines(n int, styleID uint32) {
	if t.CursorY < t.ScrollTop || t.CursorY > t.ScrollBottom || n <= 0 {
		return
	}
	n = min(n, t.ScrollBottom-t.CursorY+1)
	for range n {
		recycled := t.GridRows[t.ScrollBottom]
		copy(t.GridRows[t.CursorY+1:t.ScrollBottom+1], t.GridRows[t.CursorY:t.ScrollBottom])
		clearRow(&recycled, styleID)
		t.GridRows[t.CursorY] = recycled
	}
	t.breakAllWrapChains()
}

func (t *TerminalState) deleteLines(n int, styleID uint32) {
	if t.CursorY < t.ScrollTop || t.CursorY > t.ScrollBottom || n <= 0 {
		return
	}
	n = min(n, t.ScrollBottom-t.CursorY+1)
	for range n {
		recycled := t.GridRows[t.CursorY]
		copy(t.GridRows[t.CursorY:t.ScrollBottom], t.GridRows[t.CursorY+1:t.ScrollBottom+1])
		clearRow(&recycled, styleID)
		t.GridRows[t.ScrollBottom] = recycled
	}
	t.breakAllWrapChains()
}

func (t *TerminalState) setPrivateModes(modes []int, enabled bool, update *Update) {
	for _, mode := range modes {
		switch mode {
		case 1:
			t.ApplicationCursorKeys = enabled
		case 6:
			t.OriginMode = enabled
			t.CursorX = 0
			t.CursorY = 0
			if enabled {
				t.CursorY = t.ScrollTop
			}
			t.wrapPending = false
			update.CursorChanged = true
		case 7:
			t.AutoWrap = enabled
			if !enabled {
				t.wrapPending = false
			}
		case 25:
			t.CursorVisible = enabled
			update.VisibleChange = true
		case 47, 1047, 1049:
			if enabled {
				t.enterAlternateScreen()
			} else {
				t.exitAlternateScreen()
			}
			update.FullRedraw = true
			update.CursorChanged = true
			update.VisibleChange = true
		case 1048:
			if enabled {
				t.saveCursor()
			} else {
				t.restoreCursor()
			}
			update.CursorChanged = true
		case 3, 4, 12:
			// Pane width, smooth scrolling, and cursor blinking are controlled
			// outside the emulated terminal. Consume common xterm init modes.
		default:
			logUnsupportedf("unsupported private mode ?%d%c", mode, map[bool]byte{true: 'h', false: 'l'}[enabled])
		}
	}
}

func (t *TerminalState) softReset() {
	t.CurrentStyle = DefaultStyle
	t.cachedStyle = DefaultStyle
	t.cachedStyleID = 0
	t.CursorVisible = true
	t.InsertMode = false
	t.OriginMode = false
	t.AutoWrap = true
	t.ApplicationCursorKeys = false
	t.G0Charset = 'B'
	t.G1Charset = 'B'
	t.ActiveCharset = 0
	t.ScrollTop = 0
	t.ScrollBottom = t.Rows - 1
	t.wrapPending = false
}

func (t *TerminalState) reset() {
	cols, rows, historyLimit := t.Cols, t.Rows, t.HistoryLimit
	*t = *newTerminal(cols, rows)
	t.HistoryLimit = historyLimit
}

func defaultTabStops(cols int) []bool {
	stops := make([]bool, max(cols, 0))
	for column := 8; column < cols; column += 8 {
		stops[column] = true
	}
	return stops
}

func resizedTabStops(current []bool, oldCols, cols int) []bool {
	stops := make([]bool, max(cols, 0))
	copy(stops, current)
	for column := 8; column < cols; column += 8 {
		if column >= oldCols {
			stops[column] = true
		}
	}
	return stops
}

func (t *TerminalState) setTabStop(column int) {
	if column >= 0 && column < len(t.TabStops) {
		t.TabStops[column] = true
	}
}

func (t *TerminalState) clearTabStop(column int) {
	if column >= 0 && column < len(t.TabStops) {
		t.TabStops[column] = false
	}
}

func (t *TerminalState) nextTabStop(column int) int {
	for next := column + 1; next < len(t.TabStops); next++ {
		if t.TabStops[next] {
			return next
		}
	}
	return max(t.Cols-1, 0)
}

func (t *TerminalState) previousTabStop(column int) int {
	for previous := column - 1; previous >= 0; previous-- {
		if t.TabStops[previous] {
			return previous
		}
	}
	return 0
}

func (t *TerminalState) translateByte(b byte) rune {
	charset := t.activeCharset()
	if charset != '0' {
		return rune(b)
	}
	if translated, ok := decSpecialGraphics[b]; ok {
		return translated
	}
	return rune(b)
}

func (t *TerminalState) activeCharset() byte {
	if t.ActiveCharset == 1 {
		return t.G1Charset
	}
	return t.G0Charset
}

var decSpecialGraphics = map[byte]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊', 'f': '°', 'g': '±',
	'h': '␤', 'i': '␋', 'j': '┘', 'k': '┐', 'l': '┌', 'm': '└', 'n': '┼', 'o': '⎺',
	'p': '⎻', 'q': '─', 'r': '⎼', 's': '⎽', 't': '├', 'u': '┤', 'v': '┴', 'w': '┬',
	'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠', '}': '£', '~': '·',
}

func (t *TerminalState) normalizeRow(row int) {
	if row < 0 || row >= t.Rows {
		return
	}
	cells := t.GridRows[row].Cells
	for i := 0; i < len(cells); i++ {
		if cells[i].Width == 2 && (i+1 >= len(cells) || cells[i+1].Width != 0) {
			cells[i] = blankCell(0)
		} else if cells[i].Width == 0 {
			if i == 0 || cells[i-1].Width <= 1 {
				cells[i] = blankCell(0)
			}
		}
	}
}

func formatDSR(row, col int) string {
	return "\x1b[" + itoa(row) + ";" + itoa(col) + "R"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func (t *TerminalState) lineFeed(update *Update) {
	t.breakWrapChainAt(t.CursorY)
	if t.CursorY == t.ScrollBottom {
		styleID := t.eraseStyleID()
		t.scrollUpRegion(t.ScrollTop, t.ScrollBottom, styleID)
		delta := t.fullViewportScrollDelta(-1)
		update.recordScroll(delta, t.Rows)
		if delta != 0 {
			update.markDirty(t.ScrollBottom, 0, t.Cols, t.Cols)
		}
		return
	}
	if t.CursorY < t.Rows-1 {
		t.CursorY++
	}
}

func (t *TerminalState) wrapLine(update *Update) {
	if t.CursorY == t.ScrollBottom {
		styleID := t.eraseStyleID()
		t.scrollUpRegion(t.ScrollTop, t.ScrollBottom, styleID)
		delta := t.fullViewportScrollDelta(-1)
		update.recordScroll(delta, t.Rows)
		if delta != 0 {
			update.markDirty(t.ScrollBottom, 0, t.Cols, t.Cols)
		}
		return
	}
	if t.CursorY < t.Rows-1 {
		t.CursorY++
	}
}

func (t *TerminalState) eraseStyleID() uint32 {
	return t.currentStyleID()
}

func (t *TerminalState) fullViewportScrollDelta(delta int) int {
	if t.ScrollTop == 0 && t.ScrollBottom == t.Rows-1 {
		return delta
	}
	return 0
}

func (t *TerminalState) styleID(style Style) (uint32, bool) {
	if id, ok := t.styleToID[style]; ok {
		return id, false
	}
	if len(t.styleByID) >= maxTerminalStyles {
		return 0, false
	}
	id := t.nextStyleID
	t.nextStyleID++
	t.styleToID[style] = id
	t.styleByID = append(t.styleByID, style)
	return id, true
}

func (t *TerminalState) currentStyleID() uint32 {
	return t.cachedStyleID
}

func (t *TerminalState) syncCurrentStyleID() {
	if t.CurrentStyle == t.cachedStyle {
		return
	}
	id, _ := t.styleID(t.CurrentStyle)
	t.cachedStyle = t.CurrentStyle
	t.cachedStyleID = id
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
	if t.OriginMode && t.Rows > 0 {
		t.CursorY = min(max(t.CursorY, t.ScrollTop), t.ScrollBottom)
	}
}

func (t *TerminalState) collectLogicalLines() []logicalLine {
	lines := make([]logicalLine, 0, t.Rows)
	current := logicalLine{reflowable: true}
	for row := 0; row < t.Rows; row++ {
		segment := append([]Cell(nil), t.GridRows[row].Cells...)
		if !t.GridRows[row].WrapsNext {
			segment = trimTrailingBlankCells(segment)
		}
		if row == t.CursorY {
			current.cursorHere = true
			current.cursorOffset = len(current.cells) + min(t.CursorX, len(segment))
		}
		current.cells = append(current.cells, segment...)
		if !t.GridRows[row].WrapsNext {
			lines = append(lines, current)
			current = logicalLine{reflowable: true}
		}
	}
	if len(current.cells) > 0 || current.cursorHere {
		lines = append(lines, current)
	}
	for len(lines) > 0 {
		last := lines[len(lines)-1]
		if last.cursorHere || len(last.cells) > 0 {
			break
		}
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []logicalLine{{reflowable: true}}
	}
	return lines
}

func (t *TerminalState) projectLogicalLine(line logicalLine, cols int) []reflowRow {
	if cols <= 0 {
		return nil
	}
	if len(line.cells) == 0 {
		return []reflowRow{{cursorHere: line.cursorHere, cursorCol: line.cursorOffset}}
	}
	if !line.reflowable {
		row := reflowRow{
			cells:      make([]Cell, cols),
			continued:  false,
			cursorHere: line.cursorHere,
			cursorCol:  min(line.cursorOffset, cols-1),
		}
		for i := range row.cells {
			row.cells[i] = blankCell(0)
		}
		copy(row.cells, line.cells[:min(len(line.cells), cols)])
		return []reflowRow{row}
	}
	out := make([]reflowRow, 0, (len(line.cells)+cols-1)/cols)
	for start := 0; start < len(line.cells); start += cols {
		end := start + cols
		if end > len(line.cells) {
			end = len(line.cells)
		}
		row := reflowRow{
			cells:     make([]Cell, cols),
			continued: end < len(line.cells),
		}
		for i := range row.cells {
			row.cells[i] = blankCell(0)
		}
		copy(row.cells, line.cells[start:end])
		if line.cursorHere && line.cursorOffset >= start && line.cursorOffset <= end {
			row.cursorHere = true
			row.cursorCol = line.cursorOffset - start
			if row.cursorCol >= cols {
				row.cursorCol = cols - 1
			}
		}
		out = append(out, row)
	}
	return out
}

func mapCursorOffset(start int, rows []reflowRow, offset, cols int) (int, int) {
	if cols <= 0 || len(rows) == 0 {
		return start, 0
	}
	if offset < 0 {
		offset = 0
	}
	return start + (offset / cols), offset % cols
}

func (t *TerminalState) setRowWrapped(row int, wrapped bool) {
	if row >= 0 && row < len(t.GridRows) {
		t.GridRows[row].WrapsNext = wrapped
	}
}

func (t *TerminalState) breakWrapChainAt(row int) {
	if row >= 0 && row < len(t.GridRows) {
		t.GridRows[row].WrapsNext = false
	}
	if row > 0 && row-1 < len(t.GridRows) {
		t.GridRows[row-1].WrapsNext = false
	}
}

func (t *TerminalState) breakAllWrapChains() {
	for row := range t.GridRows {
		t.GridRows[row].WrapsNext = false
	}
}

func blankRow(cols int, styleID uint32) Row {
	row := Row{Cells: make([]Cell, cols)}
	clearRow(&row, styleID)
	return row
}

func clearRow(row *Row, styleID uint32) {
	for i := range row.Cells {
		row.Cells[i] = blankCell(styleID)
	}
	row.WrapsNext = false
}

func cloneStyles(in []Style) []Style {
	out := make([]Style, len(in), max(initialStyleCapacity, len(in)))
	copy(out, in)
	return out
}

func cloneStyleIDMap(in map[Style]uint32) map[Style]uint32 {
	out := make(map[Style]uint32, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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

func trimTrailingBlankCells(cells []Cell) []Cell {
	end := len(cells)
	for end > 0 {
		cell := cells[end-1]
		if cell.Rune != 0 && cell.Rune != ' ' {
			break
		}
		end--
	}
	return cells[:end]
}
