package server

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
	"github.com/rivo/uniseg"
)

const (
	maxCSISequenceBytes  = 256
	maxTerminalStyles    = 4096
	initialStyleCapacity = 32
)

var printableASCIIStrings = func() [utf8.RuneSelf]string {
	var strings [utf8.RuneSelf]string
	for b := byte(0x20); b <= 0x7e; b++ {
		strings[b] = string([]byte{b})
	}
	return strings
}()

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

func colorIndexed(idx int) Color {
	return Color{Mode: "indexed", Index: uint8(idx)}
}

func colorRGB(r, g, b int) Color {
	return Color{Mode: "rgb", R: uint8(r), G: uint8(g), B: uint8(b)}
}

// clusterCellWidth is Meja's implicit server-side width policy. The grid
// deliberately stores every printable display cluster in one or two columns.
func clusterCellWidth(cluster string) uint8 {
	// Generic width libraries commonly undercount standardized keycaps.
	if strings.HasSuffix(cluster, "\u20e3") {
		return 2
	}
	// Emoji presentation selectors promote otherwise text-width bases.
	if strings.ContainsRune(cluster, '\ufe0f') {
		return 2
	}
	return clusterBaseCellWidth(cluster)
}

func clusterBaseCellWidth(cluster string) uint8 {
	// Spacing combining marks (notably Indic vowel signs) are part of the
	// anchor's shaping input but do not independently consume terminal cells.
	w := 0
	for _, r := range cluster {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		w += uniseg.StringWidth(string(r))
	}
	if w <= 0 {
		return 1
	}
	if w >= 2 {
		return 2
	}
	return 1
}

func (t *TerminalState) applicationNarrowWideCluster(cell cellWord) bool {
	cluster := t.cellText(cell)
	if cell.width() != 2 || clusterBaseCellWidth(cluster) != 1 {
		return false
	}
	return strings.ContainsRune(cluster, '\ufe0f') || strings.HasSuffix(cluster, "\u20e3")
}

func isSingleGrapheme(cluster string) bool {
	graphemes := uniseg.NewGraphemes(cluster)
	if !graphemes.Next() {
		return false
	}
	return !graphemes.Next() || isIndicConjunctCluster(cluster)
}

func isIndicConjunctCluster(cluster string) bool {
	seenBase, seenVirama, allowLinkedBase, script := false, false, false, 0
	for _, r := range cluster {
		switch {
		case unicode.IsLetter(r):
			currentScript := indicScriptBlock(r)
			if currentScript == 0 || (script != 0 && currentScript != script) {
				return false
			}
			if seenBase && !allowLinkedBase {
				return false
			}
			script = currentScript
			seenBase = true
			allowLinkedBase = false
		case isIndicVirama(r):
			if !seenBase || indicScriptBlock(r) != script {
				return false
			}
			seenVirama = true
			allowLinkedBase = true
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Mc, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Cf, r):
			// Marks and join controls retain the pending virama link.
		default:
			return false
		}
	}
	return seenBase && seenVirama
}

func indicScriptBlock(r rune) int {
	for i, bounds := range [][2]rune{
		{'\u0900', '\u097f'}, {'\u0980', '\u09ff'}, {'\u0a00', '\u0a7f'}, {'\u0a80', '\u0aff'},
		{'\u0b00', '\u0b7f'}, {'\u0b80', '\u0bff'}, {'\u0c00', '\u0c7f'}, {'\u0c80', '\u0cff'},
		{'\u0d00', '\u0d7f'}, {'\u0d80', '\u0dff'}, {'\u1000', '\u109f'}, {'\u1700', '\u177f'},
		{'\u1780', '\u17ff'}, {'\u1a00', '\u1aaf'}, {'\ua800', '\ua82f'}, {'\ua880', '\ua8df'},
		{'\ua930', '\ua95f'}, {'\uaa80', '\uaadf'},
	} {
		if r >= bounds[0] && r <= bounds[1] {
			return i + 1
		}
	}
	return 0
}

func isIndicVirama(r rune) bool {
	switch r {
	case '\u094d', '\u09cd', '\u0a4d', '\u0acd', '\u0b4d', '\u0bcd', '\u0c4d', '\u0ccd', '\u0d4d', '\u0dca',
		'\u1039', '\u1714', '\u1734', '\u17d2', '\u1a60', '\ua806', '\ua8c4', '\ua953', '\uaaf6':
		return true
	default:
		return false
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

type TerminalState struct {
	Cols                  int
	Rows                  int
	grid                  rowStore
	CursorX               int
	CursorY               int
	CurrentStyle          Style
	CursorVisible         bool
	Parser                Parser
	wrapPending           bool
	SavedCursor           SavedCursor
	ScrollTop             int
	ScrollBottom          int
	Alternate             bool
	Primary               *savedScreen
	ApplicationCursorKeys bool
	BracketedPaste        bool
	FocusReporting        bool
	MouseTracking         MouseTrackingMode
	MouseEncoding         MouseEncoding
	KittyFlags            uint32
	KittyStack            []uint32
	AutoWrap              bool
	OriginMode            bool
	InsertMode            bool
	G0Charset             byte
	G1Charset             byte
	ActiveCharset         int
	TabStops              []bool
	LastPrintedCluster    string
	LastPrintedValid      bool
	tailRow               int
	tailColumn            int
	tailValid             bool

	styleByID     []Style
	styleToID     map[Style]uint32
	nextStyleID   uint32
	cachedStyle   Style
	cachedStyleID uint32
	clusters      clusterStore
}

type MouseTrackingMode uint8

const (
	MouseTrackingNone MouseTrackingMode = iota
	MouseTrackingX10
	MouseTrackingButton
	MouseTrackingDrag
	MouseTrackingMotion
)

type MouseEncoding uint8

const (
	MouseEncodingClassic MouseEncoding = iota
	MouseEncodingSGR
)

const supportedKittyKeyboardFlags uint32 = 1 | 2

type savedScreen struct {
	grid               rowStore
	CursorX            int
	CursorY            int
	CurrentStyle       Style
	CursorVisible      bool
	WrapPending        bool
	SavedCursor        SavedCursor
	ScrollTop          int
	ScrollBottom       int
	LastPrintedCluster string
	LastPrintedValid   bool
	G0Charset          byte
	G1Charset          byte
	ActiveCharset      int
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
	cells        []cellWord
	cursorHere   bool
	cursorOffset int
}

type reflowRow struct {
	cells      []cellWord
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
		AutoWrap:      true,
		G0Charset:     'B',
		G1Charset:     'B',
	}
	t.styleToID[DefaultStyle] = 0
	t.TabStops = defaultTabStops(cols)
	t.grid.initialize(cols, rows)
	return t
}

func (t *TerminalState) Resize(cols, rows int) {
	t.invalidateTail()
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
	next.BracketedPaste = t.BracketedPaste
	next.FocusReporting = t.FocusReporting
	next.MouseTracking = t.MouseTracking
	next.MouseEncoding = t.MouseEncoding
	next.KittyFlags = t.KittyFlags
	next.KittyStack = append([]uint32(nil), t.KittyStack...)
	next.AutoWrap = t.AutoWrap
	next.OriginMode = t.OriginMode
	next.InsertMode = t.InsertMode
	next.G0Charset = t.G0Charset
	next.G1Charset = t.G1Charset
	next.ActiveCharset = t.ActiveCharset
	next.TabStops = resizedTabStops(t.TabStops, t.Cols, cols)
	next.LastPrintedCluster = t.LastPrintedCluster
	next.LastPrintedValid = t.LastPrintedValid
	next.styleByID = cloneStyles(t.styleByID)
	next.styleToID = cloneStyleIDMap(t.styleToID)
	next.nextStyleID = t.nextStyleID

	lines := t.collectLogicalLines()
	projected := make([]reflowRow, 0, max(rows, len(lines)))
	cursorProjectedRow, cursorProjectedCol := 0, 0
	cursorPlaced := false
	for _, line := range lines {
		rowsForLine := t.projectLogicalLine(line, cols)
		start := len(projected)
		projected = append(projected, rowsForLine...)
		if line.cursorHere {
			for row, projectedLine := range rowsForLine {
				if projectedLine.cursorHere {
					cursorProjectedRow, cursorProjectedCol = start+row, projectedLine.cursorCol
					cursorPlaced = true
					break
				}
			}
		}
	}

	start := max(0, len(projected)-terminalRowCapacity)
	retained := projected[start:]
	next.grid.initialize(cols, 0)
	for _, src := range retained {
		row := next.grid.appendBlank(cols, 0, &t.clusters)
		copy(row, src.cells)
		retainCellWords(row, &t.clusters)
		next.grid.setLogicalWrapped(int(next.grid.count)-1, src.continued)
	}
	for int(next.grid.count) < rows {
		next.grid.appendBlank(cols, 0, &t.clusters)
	}
	if cursorPlaced {
		cursorProjectedRow -= start
		visibleStart := int(next.grid.count) - rows
		if cursorProjectedRow >= visibleStart && cursorProjectedRow < int(next.grid.count) {
			next.CursorY = cursorProjectedRow - visibleStart
			next.CursorX = cursorProjectedCol
		}
	}
	t.grid.release(t.Cols, &t.clusters)
	next.clusters = t.clusters
	next.clampCursor()
	*t = *next
}

func (t *TerminalState) resizeWhileAlternate(cols, rows int) {
	oldCols := t.Cols
	p := t.Primary
	primary := newTerminal(t.Cols, t.Rows)
	primary.grid = p.grid
	primary.CursorX = p.CursorX
	primary.CursorY = p.CursorY
	primary.CurrentStyle = p.CurrentStyle
	primary.CursorVisible = p.CursorVisible
	primary.wrapPending = p.WrapPending
	primary.SavedCursor = p.SavedCursor
	primary.ScrollTop = p.ScrollTop
	primary.ScrollBottom = p.ScrollBottom
	primary.LastPrintedCluster = p.LastPrintedCluster
	primary.LastPrintedValid = p.LastPrintedValid
	primary.clusters = t.clusters
	primary.styleByID = cloneStyles(t.styleByID)
	primary.styleToID = cloneStyleIDMap(t.styleToID)
	primary.nextStyleID = t.nextStyleID
	primary.Resize(cols, rows)
	active := rowStore{}
	active.initialize(cols, rows)
	for row := 0; row < min(rows, t.Rows); row++ {
		destination := active.visibleRow(row, rows, cols)
		copy(destination, t.gridRow(row)[:min(cols, t.Cols)])
		retainCellWords(destination, &primary.clusters)
		active.setLogicalWrapped(row, t.rowWrapped(row))
	}
	t.grid.release(oldCols, &primary.clusters)
	t.Cols, t.Rows = cols, rows
	t.TabStops = resizedTabStops(t.TabStops, oldCols, cols)
	t.grid = active
	t.clusters = primary.clusters
	t.ScrollTop = 0
	t.ScrollBottom = rows - 1
	t.clampCursor()
	t.Primary = &savedScreen{grid: primary.grid, CursorX: primary.CursorX, CursorY: primary.CursorY, CurrentStyle: primary.CurrentStyle, CursorVisible: primary.CursorVisible, WrapPending: primary.wrapPending, SavedCursor: primary.SavedCursor, ScrollTop: primary.ScrollTop, ScrollBottom: primary.ScrollBottom, LastPrintedCluster: primary.LastPrintedCluster, LastPrintedValid: primary.LastPrintedValid, G0Charset: p.G0Charset, G1Charset: p.G1Charset, ActiveCharset: p.ActiveCharset}
}

func (t *TerminalState) LookupStyle(id uint32) (Style, bool) {
	if uint64(id) >= uint64(len(t.styleByID)) {
		return Style{}, false
	}
	return t.styleByID[id], true
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
				t.invalidateTail()
				t.breakWrapChainAt(t.CursorY)
				t.CursorX = 0
				t.wrapPending = false
				update.CursorChanged = true
			case '\n', '\v', '\f':
				t.invalidateTail()
				if t.wrapPending {
					t.CursorX = 0
				}
				t.lineFeed(update)
				t.wrapPending = false
				update.CursorChanged = true
			case '\b':
				t.invalidateTail()
				if t.CursorX > 0 {
					t.CursorX--
					if t.CursorX > 0 && t.gridRow(t.CursorY)[t.CursorX].width() == 0 && t.applicationNarrowWideCluster(t.gridRow(t.CursorY)[t.CursorX-1]) {
						// Many applications still measure text-default emoji bases per
						// code point and emit one BS even when VS16 made the terminal
						// presentation two cells. Keep literal BS compatible with that
						// convention without changing column-addressed CSI movement.
						t.CursorX--
					}
				}
				t.wrapPending = false
				update.CursorChanged = true
			case '\t':
				t.invalidateTail()
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
				t.invalidateTail()
				t.restoreCursor()
				t.Parser.state = parserText
			case 'M':
				t.invalidateTail()
				t.reverseIndex(update)
				t.Parser.state = parserText
			case 'D':
				t.invalidateTail()
				t.lineFeed(update)
				t.wrapPending = false
				update.CursorChanged = true
				t.Parser.state = parserText
			case 'E':
				t.invalidateTail()
				t.CursorX = 0
				t.lineFeed(update)
				t.wrapPending = false
				update.CursorChanged = true
				t.Parser.state = parserText
			case 'H':
				t.setTabStop(t.CursorX)
				t.Parser.state = parserText
			case 'c':
				t.invalidateTail()
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
	if t.Cols == 0 || t.Rows == 0 || len(data) == 0 {
		return
	}
	if t.InsertMode {
		for _, b := range data {
			t.putASCIIByte(b, update)
		}
		return
	}

	// Printable ASCII always begins a new grapheme. Avoid segmentation, width
	// lookup, per-cell damage updates, and per-byte string conversion here.
	t.invalidateTail()
	styleID := t.currentStyleID()
	lastRow, lastColumn := t.CursorY, t.CursorX
	lastByte := data[len(data)-1]
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
		cells := t.gridRow(t.CursorY)
		dirtyStart, dirtyEnd := start, end
		if start > 0 && cells[start].width() == 0 && cells[start-1].width() == 2 {
			t.replaceBlankCell(cells, start-1, styleID)
			dirtyStart--
		}
		if end < t.Cols && cells[end-1].width() == 2 {
			t.replaceBlankCell(cells, end, styleID)
			dirtyEnd++
		}
		t.setRowWrapped(t.CursorY, false)
		for column, b := range data[:count] {
			t.replaceCell(cells, start+column, printableASCIIWords[b]|cellWord(styleID))
		}
		update.markDirty(t.CursorY, dirtyStart, dirtyEnd, t.Cols)
		lastRow, lastColumn = t.CursorY, end-1
		if end == t.Cols {
			t.CursorX = t.Cols - 1
			t.wrapPending = t.AutoWrap
		} else {
			t.CursorX = end
		}
		data = data[count:]
	}
	t.LastPrintedCluster = printableASCIIStrings[lastByte]
	t.LastPrintedValid = true
	t.tailRow, t.tailColumn = lastRow, lastColumn
	t.tailValid = lastByte != ' '
	update.CursorChanged = true
}

func (t *TerminalState) putASCIIByte(b byte, update *Update) {
	t.invalidateTail()
	if t.wrapPending {
		t.wrapPending = false
		if t.AutoWrap {
			t.setRowWrapped(t.CursorY, true)
			t.CursorX = 0
			t.wrapLine(update)
		}
	}
	styleID := t.currentStyleID()
	writtenColumn := t.CursorX
	t.insertChars(1, styleID)
	dirtyStart, dirtyEnd := t.clearGlyphsForWrite(t.CursorY, writtenColumn, 1, styleID)
	t.replaceCell(t.gridRow(t.CursorY), writtenColumn, printableASCIIWords[b]|cellWord(styleID))
	t.LastPrintedCluster = printableASCIIStrings[b]
	t.LastPrintedValid = true
	t.tailRow, t.tailColumn, t.tailValid = t.CursorY, writtenColumn, b != ' '
	update.markDirty(t.CursorY, dirtyStart, dirtyEnd, t.Cols)
	if t.CursorX+1 == t.Cols {
		t.CursorX = t.Cols - 1
		t.wrapPending = t.AutoWrap
	} else {
		t.CursorX++
		t.setRowWrapped(t.CursorY, false)
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
	if t.extendTail(r, update) {
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
	cluster := string(r)
	if r == ' ' {
		cluster = ""
	}
	cellWidth := int(clusterCellWidth(cluster))
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
	t.replaceTextCell(t.gridRow(t.CursorY), writtenColumn, cluster, uint8(cellWidth), styleID)
	for column := writtenColumn + 1; column < writtenColumn+cellWidth; column++ {
		t.replaceCell(t.gridRow(t.CursorY), column, makeContinuationCellWord(uint16(styleID)))
	}
	t.LastPrintedCluster = string(r)
	t.LastPrintedValid = true
	t.tailRow, t.tailColumn, t.tailValid = t.CursorY, writtenColumn, cluster != ""
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

func (t *TerminalState) extendTail(r rune, update *Update) bool {
	if !t.tailValid || t.tailRow < 0 || t.tailRow >= t.Rows || t.tailColumn < 0 || t.tailColumn >= t.Cols {
		return false
	}
	cells := t.gridRow(t.tailRow)
	anchor := cells[t.tailColumn]
	anchorText := t.cellText(anchor)
	if anchor.width() == 0 || anchorText == "" {
		t.invalidateTail()
		return false
	}
	candidate := anchorText + string(r)
	if !isSingleGrapheme(candidate) {
		return false
	}
	oldWidth := int(anchor.width())
	newWidth := int(clusterCellWidth(candidate))
	if newWidth > t.Cols {
		newWidth = 1
	}
	styleID := uint32(anchor.styleID())

	if oldWidth == 1 && newWidth == 2 && t.tailColumn+1 >= t.Cols {
		if !t.AutoWrap {
			// There is no two-column span available. Keep a deterministic degraded
			// width while preserving the complete cluster text in the anchor.
			t.replaceTextCell(cells, t.tailColumn, candidate, 1, styleID)
			update.markDirty(t.tailRow, t.tailColumn, t.tailColumn+1, t.Cols)
			t.LastPrintedCluster, t.LastPrintedValid = candidate, true
			return true
		}
		// A provisional width-1 tail at the final column became wide. Relocate
		// it exactly as if the complete cluster had arrived atomically.
		t.replaceBlankCell(cells, t.tailColumn, styleID)
		update.markDirty(t.tailRow, t.tailColumn, t.tailColumn+1, t.Cols)
		t.setRowWrapped(t.tailRow, true)
		t.CursorX = 0
		t.CursorY = t.tailRow
		t.wrapPending = false
		t.wrapLine(update)
		t.tailRow, t.tailColumn = t.CursorY, 0
		cells = t.gridRow(t.tailRow)
		t.replaceTextCell(cells, 0, candidate, 2, styleID)
		t.replaceCell(cells, 1, makeContinuationCellWord(uint16(styleID)))
		t.CursorX = 2
		update.markDirty(t.tailRow, 0, 2, t.Cols)
		update.CursorChanged = true
		t.LastPrintedCluster, t.LastPrintedValid = candidate, true
		return true
	}

	t.replaceTextCell(cells, t.tailColumn, candidate, uint8(newWidth), styleID)
	if oldWidth == 1 && newWidth == 2 {
		t.clearGlyphsForWrite(t.tailRow, t.tailColumn+1, 1, styleID)
		t.replaceCell(cells, t.tailColumn+1, makeContinuationCellWord(uint16(styleID)))
		if t.CursorY == t.tailRow && t.CursorX == t.tailColumn+1 {
			t.CursorX++
		}
	} else if oldWidth == 2 && newWidth == 1 {
		t.replaceBlankCell(cells, t.tailColumn+1, styleID)
		if t.CursorY == t.tailRow && t.CursorX == t.tailColumn+2 {
			t.CursorX--
		}
	}
	t.wrapPending = t.AutoWrap && t.tailColumn+newWidth == t.Cols
	if t.wrapPending {
		t.CursorX = t.Cols - 1
	}
	update.markDirty(t.tailRow, t.tailColumn, min(t.Cols, t.tailColumn+max(oldWidth, newWidth)), t.Cols)
	if oldWidth != newWidth {
		update.CursorChanged = true
	}
	t.LastPrintedCluster, t.LastPrintedValid = candidate, true
	return true
}

func (t *TerminalState) invalidateTail() {
	t.tailValid = false
}

func (t *TerminalState) clearGlyphsForWrite(row, start, cellWidth int, styleID uint32) (int, int) {
	dirtyStart, dirtyEnd := start, start+cellWidth
	for column := start; column < start+cellWidth; column++ {
		cell := t.gridRow(row)[column]
		switch {
		case cell.width() == 2:
			t.replaceBlankCell(t.gridRow(row), column, styleID)
			if column+1 < t.Cols {
				t.replaceBlankCell(t.gridRow(row), column+1, styleID)
				dirtyEnd = max(dirtyEnd, column+2)
			}
		case cell.width() == 0 && column > 0 && t.gridRow(row)[column-1].width() == 2:
			t.replaceBlankCell(t.gridRow(row), column-1, styleID)
			t.replaceBlankCell(t.gridRow(row), column, styleID)
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
	if parsed.Final != 'm' && parsed.Final != 'n' && parsed.Final != 'c' {
		t.invalidateTail()
	}
	switch parsed.PrivatePrefix {
	case '?':
		switch parsed.Final {
		case 'h':
			t.setPrivateModes(params, true, update)
		case 'l':
			t.setPrivateModes(params, false, update)
		case 'u':
			update.Replies = append(update.Replies, []byte(fmt.Sprintf("\x1b[?%du", t.KittyFlags)))
		default:
			logUnsupportedf("unsupported private CSI ?%s", seq)
		}
		return
	case '>', '<', '=':
		if parsed.Final == 'u' {
			t.applyKittyKeyboardMode(parsed.PrivatePrefix, params)
			return
		}
		logUnsupportedf("unsupported prefixed CSI %s", seq)
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
			t.grid.discardHistory(t.Rows, t.Cols, &t.clusters)
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
			cluster := t.LastPrintedCluster
			for count := 0; count < max1(params, 1); count++ {
				t.invalidateTail()
				for _, r := range cluster {
					t.putRune(r, update)
				}
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
		for row := 0; row < t.Rows; row++ {
			t.fillRow(row, 0, t.Cols, styleID)
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
		t.replaceBlankCell(t.gridRow(row), col, styleID)
	}
}

func (t *TerminalState) scrollUpRegion(top, bottom int, styleID uint32) {
	if top < 0 || bottom >= t.Rows || top > bottom {
		return
	}
	if !t.Alternate && top == 0 && bottom == t.Rows-1 {
		t.grid.appendBlank(t.Cols, uint16(styleID), &t.clusters)
		return
	}
	discarded := t.gridRow(top)
	releaseCellWords(discarded, &t.clusters)
	for row := top; row < bottom; row++ {
		copy(t.gridRow(row), t.gridRow(row+1))
		t.setRowWrapped(row, t.rowWrapped(row+1))
	}
	fillCellWords(t.gridRow(bottom), makeBlankCellWord(uint16(styleID)))
	t.setRowWrapped(bottom, false)
	t.breakWrapChainAt(top)
	t.breakWrapChainAt(bottom)
}

func (t *TerminalState) enterAlternateScreen() {
	if t.Alternate {
		return
	}
	t.Primary = &savedScreen{grid: t.grid, CursorX: t.CursorX, CursorY: t.CursorY, CurrentStyle: t.CurrentStyle, CursorVisible: t.CursorVisible, WrapPending: t.wrapPending, SavedCursor: t.SavedCursor, ScrollTop: t.ScrollTop, ScrollBottom: t.ScrollBottom, LastPrintedCluster: t.LastPrintedCluster, LastPrintedValid: t.LastPrintedValid, G0Charset: t.G0Charset, G1Charset: t.G1Charset, ActiveCharset: t.ActiveCharset}
	t.grid.initialize(t.Cols, t.Rows)
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
	t.grid.release(t.Cols, &t.clusters)
	t.grid = p.grid
	t.CursorX = p.CursorX
	t.CursorY = p.CursorY
	t.CurrentStyle = p.CurrentStyle
	t.syncCurrentStyleID()
	t.CursorVisible = p.CursorVisible
	t.wrapPending = p.WrapPending
	t.SavedCursor = p.SavedCursor
	t.ScrollTop = p.ScrollTop
	t.ScrollBottom = p.ScrollBottom
	t.LastPrintedCluster = p.LastPrintedCluster
	t.LastPrintedValid = p.LastPrintedValid
	t.G0Charset = p.G0Charset
	t.G1Charset = p.G1Charset
	t.ActiveCharset = p.ActiveCharset
	t.Alternate = false
	t.Primary = nil
	t.clampCursor()
}

func (t *TerminalState) scrollDownRegion(top, bottom int, styleID uint32) {
	if top < 0 || bottom >= t.Rows || top > bottom {
		return
	}
	releaseCellWords(t.gridRow(bottom), &t.clusters)
	for row := bottom; row > top; row-- {
		copy(t.gridRow(row), t.gridRow(row-1))
		t.setRowWrapped(row, t.rowWrapped(row-1))
	}
	fillCellWords(t.gridRow(top), makeBlankCellWord(uint16(styleID)))
	t.setRowWrapped(top, false)
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
	row := t.gridRow(t.CursorY)
	if t.CursorX < 0 || t.CursorX >= len(row) {
		return
	}
	if n > len(row)-t.CursorX {
		n = len(row) - t.CursorX
	}
	releaseCellWords(row[t.CursorX:t.CursorX+n], &t.clusters)
	copy(row[t.CursorX:], row[t.CursorX+n:])
	for i := len(row) - n; i < len(row); i++ {
		row[i] = makeBlankCellWord(uint16(styleID))
	}
	t.normalizeRow(t.CursorY)
}

func (t *TerminalState) insertChars(n int, styleID uint32) {
	if t.CursorY < 0 || t.CursorY >= t.Rows || t.CursorX < 0 || t.CursorX >= t.Cols || n <= 0 {
		return
	}
	row := t.gridRow(t.CursorY)
	if n > len(row)-t.CursorX {
		n = len(row) - t.CursorX
	}
	releaseCellWords(row[len(row)-n:], &t.clusters)
	copy(row[t.CursorX+n:], row[t.CursorX:len(row)-n])
	for i := t.CursorX; i < t.CursorX+n; i++ {
		row[i] = makeBlankCellWord(uint16(styleID))
	}
	t.normalizeRow(t.CursorY)
}

func (t *TerminalState) insertLines(n int, styleID uint32) {
	if t.CursorY < t.ScrollTop || t.CursorY > t.ScrollBottom || n <= 0 {
		return
	}
	n = min(n, t.ScrollBottom-t.CursorY+1)
	for range n {
		t.scrollDownRegion(t.CursorY, t.ScrollBottom, styleID)
	}
	t.breakAllWrapChains()
}

func (t *TerminalState) deleteLines(n int, styleID uint32) {
	if t.CursorY < t.ScrollTop || t.CursorY > t.ScrollBottom || n <= 0 {
		return
	}
	n = min(n, t.ScrollBottom-t.CursorY+1)
	for range n {
		t.scrollUpRegion(t.CursorY, t.ScrollBottom, styleID)
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
		case 9:
			t.setMouseTracking(MouseTrackingX10, enabled)
		case 1000:
			t.setMouseTracking(MouseTrackingButton, enabled)
		case 1002:
			t.setMouseTracking(MouseTrackingDrag, enabled)
		case 1003:
			t.setMouseTracking(MouseTrackingMotion, enabled)
		case 1004:
			t.FocusReporting = enabled
		case 1006:
			if enabled {
				t.MouseEncoding = MouseEncodingSGR
			} else {
				t.MouseEncoding = MouseEncodingClassic
			}
		case 2004:
			t.BracketedPaste = enabled
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
		case 3, 4, 12, 1007:
			// Pane width, smooth scrolling, cursor blinking, and alternate
			// scroll are handled outside terminal screen state. Consume these
			// common xterm modes without retaining duplicate state.
		default:
			logUnsupportedf("unsupported private mode ?%d%c", mode, map[bool]byte{true: 'h', false: 'l'}[enabled])
		}
	}
}

func (t *TerminalState) setMouseTracking(mode MouseTrackingMode, enabled bool) {
	if enabled {
		t.MouseTracking = mode
	} else if t.MouseTracking == mode {
		t.MouseTracking = MouseTrackingNone
	}
}

func (t *TerminalState) applyKittyKeyboardMode(prefix byte, params []int) {
	switch prefix {
	case '>':
		if len(t.KittyStack) < 32 {
			t.KittyStack = append(t.KittyStack, t.KittyFlags)
		}
		t.KittyFlags = uint32(paramOr(params, 0, 0)) & supportedKittyKeyboardFlags
	case '<':
		count := max1(params, 1)
		for count > 0 && len(t.KittyStack) > 0 {
			t.KittyFlags = t.KittyStack[len(t.KittyStack)-1]
			t.KittyStack = t.KittyStack[:len(t.KittyStack)-1]
			count--
		}
	case '=':
		flags := uint32(paramOr(params, 0, 0)) & supportedKittyKeyboardFlags
		switch paramOr(params, 1, 1) {
		case 2:
			t.KittyFlags |= flags
		case 3:
			t.KittyFlags &^= flags
		default:
			t.KittyFlags = flags
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
	t.BracketedPaste = false
	t.FocusReporting = false
	t.MouseTracking = MouseTrackingNone
	t.MouseEncoding = MouseEncodingClassic
	t.KittyFlags = 0
	t.KittyStack = nil
	t.G0Charset = 'B'
	t.G1Charset = 'B'
	t.ActiveCharset = 0
	t.ScrollTop = 0
	t.ScrollBottom = t.Rows - 1
	t.wrapPending = false
}

func (t *TerminalState) reset() {
	cols, rows := t.Cols, t.Rows
	t.grid.release(cols, &t.clusters)
	*t = *newTerminal(cols, rows)
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
	cells := t.gridRow(row)
	for i := 0; i < len(cells); i++ {
		if cells[i].width() == 2 && (i+1 >= len(cells) || cells[i+1].width() != 0) {
			t.replaceBlankCell(cells, i, 0)
		} else if cells[i].width() == 0 {
			if i == 0 || cells[i-1].width() <= 1 {
				t.replaceBlankCell(cells, i, 0)
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
	lines := make([]logicalLine, 0, int(t.grid.count))
	current := logicalLine{}
	cursorLogicalRow := int(t.grid.count) - t.Rows + t.CursorY
	for row := 0; row < int(t.grid.count); row++ {
		segment := append([]cellWord(nil), t.grid.logicalRow(row, t.Cols)...)
		if !t.grid.logicalWrapped(row) {
			segment = trimTrailingBlankCells(segment)
		}
		if row == cursorLogicalRow {
			current.cursorHere = true
			current.cursorOffset = len(current.cells) + min(t.CursorX, len(segment))
		}
		current.cells = append(current.cells, segment...)
		if !t.grid.logicalWrapped(row) {
			lines = append(lines, current)
			current = logicalLine{}
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
		return []logicalLine{{}}
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
	newRow := func() reflowRow {
		row := reflowRow{cells: make([]cellWord, cols)}
		for i := range row.cells {
			row.cells[i] = makeBlankCellWord(0)
		}
		return row
	}
	out := make([]reflowRow, 0, (len(line.cells)+cols-1)/cols)
	row, column := newRow(), 0
	for source := 0; source < len(line.cells); {
		cell := line.cells[source]
		width := 1
		if cell.width() == 2 && source+1 < len(line.cells) && line.cells[source+1].width() == 0 {
			width = 2
		} else if cell.width() == 0 {
			// Never reproduce an orphan continuation from malformed legacy state.
			source++
			continue
		}
		sourceWidth := width
		if width > cols {
			// A one-column viewport cannot hold a two-column cluster. Preserve the
			// text as a width-degraded one-column anchor rather than splitting it.
			width = 1
			cell = cellWordWithWidth(cell, 1)
		}
		if column+width > cols {
			row.continued = true
			out = append(out, row)
			row, column = newRow(), 0
		}
		row.cells[column] = cell
		if width == 2 {
			row.cells[column+1] = makeContinuationCellWord(cell.styleID())
		}
		if line.cursorHere && line.cursorOffset >= source && line.cursorOffset < source+width {
			row.cursorHere = true
			row.cursorCol = column + line.cursorOffset - source
		}
		column += width
		source += sourceWidth
	}
	if line.cursorHere && line.cursorOffset >= len(line.cells) {
		row.cursorHere = true
		row.cursorCol = min(column, cols-1)
	}
	out = append(out, row)
	return out
}

func (t *TerminalState) breakWrapChainAt(row int) {
	if row >= 0 && row < t.Rows {
		t.setRowWrapped(row, false)
	}
	if row > 0 && row-1 < t.Rows {
		t.setRowWrapped(row-1, false)
	}
}

func (t *TerminalState) breakAllWrapChains() {
	for row := 0; row < t.Rows; row++ {
		t.setRowWrapped(row, false)
	}
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

func trimTrailingBlankCells(cells []cellWord) []cellWord {
	end := len(cells)
	for end > 0 {
		cell := cells[end-1]
		if !cell.isBlank() {
			break
		}
		end--
	}
	return cells[:end]
}

type cellWord uint32

const (
	cellStyleBits    = 12
	cellPayloadBits  = 16
	cellStyleMask    = cellWord(1<<cellStyleBits - 1)
	cellPayloadMask  = cellWord(1<<cellPayloadBits - 1)
	cellPayloadShift = cellStyleBits
	cellKindShift    = cellPayloadShift + cellPayloadBits

	maxPaneClusters = 1 << 17
)

const (
	cellKindBlank cellKind = iota
	cellKindContinuation
	cellKindNarrowScalar0
	cellKindNarrowScalar1
	cellKindNarrowScalar2
	cellKindNarrowScalar3
	cellKindWideScalar0
	cellKindWideScalar1
	cellKindWideScalar2
	cellKindWideScalar3
	cellKindNarrowClusterLow
	cellKindNarrowClusterHigh
	cellKindWideClusterLow
	cellKindWideClusterHigh
)

type cellKind uint8

var printableASCIIWords = func() [utf8.RuneSelf]cellWord {
	var words [utf8.RuneSelf]cellWord
	for b := byte('!'); b <= '~'; b++ {
		words[b], _ = makeScalarCellWord(rune(b), 1, 0)
	}
	return words
}()

func makeBlankCellWord(styleID uint16) cellWord {
	return cellWord(styleID) & cellStyleMask
}

func makeContinuationCellWord(styleID uint16) cellWord {
	return makeCellWord(cellKindContinuation, 0, styleID)
}

func makeScalarCellWord(r rune, width uint8, styleID uint16) (cellWord, bool) {
	if r < 0 || r > 0x3ffff || width < 1 || width > 2 {
		return 0, false
	}
	if r == ' ' && width == 1 {
		return makeBlankCellWord(styleID), true
	}
	window := cellKind(uint32(r) >> cellPayloadBits)
	kind := cellKindNarrowScalar0 + window
	if width == 2 {
		kind = cellKindWideScalar0 + window
	}
	return makeCellWord(kind, uint16(r), styleID), true
}

func makeClusterCellWord(handle uint32, width uint8, styleID uint16) (cellWord, bool) {
	if handle >= maxPaneClusters || width < 1 || width > 2 {
		return 0, false
	}
	kind := cellKindNarrowClusterLow
	if handle >= 1<<16 {
		kind = cellKindNarrowClusterHigh
	}
	if width == 2 {
		kind += cellKindWideClusterLow - cellKindNarrowClusterLow
	}
	return makeCellWord(kind, uint16(handle), styleID), true
}

func makeCellWord(kind cellKind, payload uint16, styleID uint16) cellWord {
	return cellWord(kind)<<cellKindShift |
		cellWord(payload)<<cellPayloadShift |
		cellWord(styleID)&cellStyleMask
}

func (c cellWord) kind() cellKind {
	return cellKind(c >> cellKindShift)
}

func (c cellWord) payload() uint16 {
	return uint16(c >> cellPayloadShift & cellPayloadMask)
}

func (c cellWord) styleID() uint16 {
	return uint16(c & cellStyleMask)
}

func (c cellWord) width() uint8 {
	switch c.kind() {
	case cellKindContinuation:
		return 0
	case cellKindWideScalar0, cellKindWideScalar1, cellKindWideScalar2, cellKindWideScalar3,
		cellKindWideClusterLow, cellKindWideClusterHigh:
		return 2
	default:
		return 1
	}
}

func (c cellWord) scalar() (rune, bool) {
	kind := c.kind()
	var window cellKind
	switch {
	case kind >= cellKindNarrowScalar0 && kind <= cellKindNarrowScalar3:
		window = kind - cellKindNarrowScalar0
	case kind >= cellKindWideScalar0 && kind <= cellKindWideScalar3:
		window = kind - cellKindWideScalar0
	default:
		return 0, false
	}
	return rune(uint32(window)<<cellPayloadBits | uint32(c.payload())), true
}

func (c cellWord) clusterHandle() (uint32, bool) {
	kind := c.kind()
	switch kind {
	case cellKindNarrowClusterLow, cellKindWideClusterLow:
		return uint32(c.payload()), true
	case cellKindNarrowClusterHigh, cellKindWideClusterHigh:
		return 1<<16 | uint32(c.payload()), true
	default:
		return 0, false
	}
}

func (c cellWord) isBlank() bool {
	return c.kind() == cellKindBlank
}

type clusterEntry struct {
	text string
	refs uint32
}

type clusterStore struct {
	entries []clusterEntry
	byText  map[string]uint32
	free    []uint32
}

func (s *clusterStore) acquire(text string) (uint32, bool) {
	if text == "" {
		return 0, false
	}
	if handle, ok := s.byText[text]; ok {
		s.entries[handle].refs++
		return handle, true
	}
	var handle uint32
	switch {
	case len(s.free) > 0:
		handle = s.free[len(s.free)-1]
		s.free = s.free[:len(s.free)-1]
	case len(s.entries) < maxPaneClusters:
		handle = uint32(len(s.entries))
		s.entries = append(s.entries, clusterEntry{})
	default:
		return 0, false
	}
	if s.byText == nil {
		s.byText = make(map[string]uint32)
	}
	s.entries[handle] = clusterEntry{text: text, refs: 1}
	s.byText[text] = handle
	return handle, true
}

func (s *clusterStore) retain(handle uint32) bool {
	if handle >= uint32(len(s.entries)) || s.entries[handle].refs == 0 {
		return false
	}
	s.entries[handle].refs++
	return true
}

func (s *clusterStore) release(handle uint32) {
	if handle >= uint32(len(s.entries)) || s.entries[handle].refs == 0 {
		return
	}
	entry := &s.entries[handle]
	entry.refs--
	if entry.refs != 0 {
		return
	}
	delete(s.byText, entry.text)
	entry.text = ""
	s.free = append(s.free, handle)
}

func (s *clusterStore) lookup(handle uint32) (string, bool) {
	if handle >= uint32(len(s.entries)) || s.entries[handle].refs == 0 {
		return "", false
	}
	return s.entries[handle].text, true
}

func (t *TerminalState) makeTextCellWord(text string, width uint8, styleID uint16) cellWord {
	if text == "" || text == " " && width == 1 {
		return makeBlankCellWord(styleID)
	}
	if r, size := utf8.DecodeRuneInString(text); size == len(text) {
		if word, ok := makeScalarCellWord(r, width, styleID); ok {
			return word
		}
	}
	if handle, ok := t.clusters.acquire(text); ok {
		word, _ := makeClusterCellWord(handle, width, styleID)
		return word
	}
	word, _ := makeScalarCellWord(utf8.RuneError, width, styleID)
	return word
}

func (t *TerminalState) cellText(word cellWord) string {
	return cellTextFromStore(word, &t.clusters)
}

func cellTextFromStore(word cellWord, clusters *clusterStore) string {
	if word.isBlank() {
		return ""
	}
	if r, ok := word.scalar(); ok {
		return string(r)
	}
	if handle, ok := word.clusterHandle(); ok && clusters != nil {
		text, _ := clusters.lookup(handle)
		return text
	}
	return ""
}

func (t *TerminalState) replaceCell(cells []cellWord, column int, word cellWord) {
	if handle, ok := cells[column].clusterHandle(); ok {
		t.clusters.release(handle)
	}
	cells[column] = word
}

func (t *TerminalState) gridRow(row int) []cellWord {
	return t.grid.visibleRow(row, t.Rows, t.Cols)
}

func (t *TerminalState) rowWrapped(row int) bool {
	return t.grid.logicalWrapped(int(t.grid.count) - t.Rows + row)
}

func (t *TerminalState) setRowWrapped(row int, wrapped bool) {
	t.grid.setLogicalWrapped(int(t.grid.count)-t.Rows+row, wrapped)
}

func (t *TerminalState) replaceTextCell(cells []cellWord, column int, text string, width uint8, styleID uint32) {
	t.replaceCell(cells, column, t.makeTextCellWord(text, width, uint16(styleID)))
}

func (t *TerminalState) replaceBlankCell(cells []cellWord, column int, styleID uint32) {
	t.replaceCell(cells, column, makeBlankCellWord(uint16(styleID)))
}

func cellWordWithWidth(word cellWord, width uint8) cellWord {
	if r, ok := word.scalar(); ok {
		out, _ := makeScalarCellWord(r, width, word.styleID())
		return out
	}
	if handle, ok := word.clusterHandle(); ok {
		out, _ := makeClusterCellWord(handle, width, word.styleID())
		return out
	}
	return makeBlankCellWord(word.styleID())
}

const (
	terminalRowCapacity = 2048
	terminalBlockRows   = 128
	terminalBlockCount  = terminalRowCapacity / terminalBlockRows
	terminalRowMask     = terminalRowCapacity - 1
)

type rowBlock struct {
	cells []cellWord
	wraps [2]uint64
}

type rowStore struct {
	blocks [terminalBlockCount]rowBlock
	head   uint16
	count  uint16
}

func (s *rowStore) initialize(cols, rows int) {
	*s = rowStore{}
	rows = min(max(rows, 0), terminalRowCapacity)
	for slot := 0; slot < rows; slot++ {
		s.ensureSlot(slot, cols)
	}
	s.count = uint16(rows)
}

func (s *rowStore) ensureSlot(slot, cols int) {
	if slot < 0 || slot >= terminalRowCapacity || cols <= 0 {
		return
	}
	block := &s.blocks[slot/terminalBlockRows]
	if block.cells == nil {
		block.cells = make([]cellWord, terminalBlockRows*cols)
	}
}

func (s *rowStore) physicalRow(slot, cols int) []cellWord {
	if slot < 0 || slot >= terminalRowCapacity || cols <= 0 {
		return nil
	}
	block := &s.blocks[slot/terminalBlockRows]
	if block.cells == nil {
		return nil
	}
	row := slot % terminalBlockRows
	start := row * cols
	return block.cells[start : start+cols]
}

func (s *rowStore) logicalSlot(row int) int {
	if row < 0 || row >= int(s.count) {
		return -1
	}
	return (int(s.head) + row) & terminalRowMask
}

func (s *rowStore) logicalRow(row, cols int) []cellWord {
	return s.physicalRow(s.logicalSlot(row), cols)
}

func (s *rowStore) visibleRow(row, viewportRows, cols int) []cellWord {
	return s.logicalRow(int(s.count)-viewportRows+row, cols)
}

func (s *rowStore) appendBlank(cols int, styleID uint16, clusters *clusterStore) []cellWord {
	var slot int
	if int(s.count) < terminalRowCapacity {
		slot = (int(s.head) + int(s.count)) & terminalRowMask
		s.ensureSlot(slot, cols)
		s.count++
	} else {
		slot = int(s.head)
		s.head = uint16((slot + 1) & terminalRowMask)
	}
	row := s.physicalRow(slot, cols)
	releaseCellWords(row, clusters)
	fillCellWords(row, makeBlankCellWord(styleID))
	s.setPhysicalWrapped(slot, false)
	return row
}

func (s *rowStore) physicalWrapped(slot int) bool {
	if slot < 0 || slot >= terminalRowCapacity {
		return false
	}
	block := &s.blocks[slot/terminalBlockRows]
	row := slot % terminalBlockRows
	return block.wraps[row/64]&(uint64(1)<<uint(row%64)) != 0
}

func (s *rowStore) setPhysicalWrapped(slot int, wrapped bool) {
	if slot < 0 || slot >= terminalRowCapacity {
		return
	}
	block := &s.blocks[slot/terminalBlockRows]
	row := slot % terminalBlockRows
	mask := uint64(1) << uint(row%64)
	if wrapped {
		block.wraps[row/64] |= mask
	} else {
		block.wraps[row/64] &^= mask
	}
}

func (s *rowStore) logicalWrapped(row int) bool {
	return s.physicalWrapped(s.logicalSlot(row))
}

func (s *rowStore) setLogicalWrapped(row int, wrapped bool) {
	s.setPhysicalWrapped(s.logicalSlot(row), wrapped)
}

func (s *rowStore) clone(cols int, clusters *clusterStore) rowStore {
	out := rowStore{head: s.head, count: s.count}
	for i := range s.blocks {
		if s.blocks[i].cells == nil {
			continue
		}
		out.blocks[i].cells = append([]cellWord(nil), s.blocks[i].cells...)
		out.blocks[i].wraps = s.blocks[i].wraps
	}
	for row := 0; row < int(out.count); row++ {
		retainCellWords(out.logicalRow(row, cols), clusters)
	}
	return out
}

func (s *rowStore) release(cols int, clusters *clusterStore) {
	for row := 0; row < int(s.count); row++ {
		releaseCellWords(s.logicalRow(row, cols), clusters)
	}
	*s = rowStore{}
}

func (s *rowStore) discardHistory(viewportRows, cols int, clusters *clusterStore) {
	historyRows := int(s.count) - viewportRows
	if historyRows <= 0 {
		return
	}
	for row := 0; row < historyRows; row++ {
		cells := s.logicalRow(row, cols)
		releaseCellWords(cells, clusters)
		fillCellWords(cells, 0)
		s.setLogicalWrapped(row, false)
	}
	s.head = uint16((int(s.head) + historyRows) & terminalRowMask)
	s.count = uint16(viewportRows)
}

func fillCellWords(cells []cellWord, word cellWord) {
	for i := range cells {
		cells[i] = word
	}
}

func retainCellWords(cells []cellWord, clusters *clusterStore) {
	if clusters == nil {
		return
	}
	for _, word := range cells {
		if handle, ok := word.clusterHandle(); ok {
			clusters.retain(handle)
		}
	}
}

func releaseCellWords(cells []cellWord, clusters *clusterStore) {
	if clusters == nil {
		return
	}
	for _, word := range cells {
		if handle, ok := word.clusterHandle(); ok {
			clusters.release(handle)
		}
	}
}
