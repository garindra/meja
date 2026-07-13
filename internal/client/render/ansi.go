package render

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"time"

	"tali/internal/protocol"
)

func RenderANSI(state *ClientState) []byte {
	contentRows := state.DrawableRows()
	cursorX, cursorY, cursorVisible := physicalCursor(state)
	fullRedraw := state.fullContentDirty || state.LastRendered.Cols != state.TerminalCols ||
		state.LastRendered.Rows != contentRows
	tabChanged := fullRedraw || state.tabBarDirty
	cursorChanged := !state.hasRenderedCursor || cursorX != state.lastCursorX ||
		cursorY != state.lastCursorY || cursorVisible != state.lastCursorVisible

	var buf bytes.Buffer
	contentChanged := fullRedraw || len(state.pendingDamageRects) > 0
	if contentChanged || tabChanged {
		buf.WriteString("\x1b[?25l")
	}
	if fullRedraw {
		buf.WriteString("\x1b[H\x1b[2J")
		composed := composeContent(state)
		renderFullContent(&buf, composed, state.TerminalCols, contentRows)
	} else if contentChanged {
		renderDamagedContent(&buf, state, state.pendingDamageRects, state.TerminalCols, contentRows)
	}
	if tabChanged {
		writeCursorPosition(&buf, state.TerminalRows, 1)
		if state.Reconnecting {
			renderReconnectStatusBar(&buf, state, time.Now())
		} else {
			renderStatusBar(&buf, state)
		}
	}
	if contentChanged || tabChanged {
		buf.WriteString(sgrForStyle(defaultStyle()))
		writeCursorPosition(&buf, cursorY+1, cursorX+1)
		if cursorVisible {
			buf.WriteString("\x1b[?25h")
		} else {
			buf.WriteString("\x1b[?25l")
		}
	} else if cursorChanged {
		writeCursorPosition(&buf, cursorY+1, cursorX+1)
		if cursorVisible {
			buf.WriteString("\x1b[?25h")
		} else {
			buf.WriteString("\x1b[?25l")
		}
	}

	state.LastRendered = Screen{Cols: state.TerminalCols, Rows: contentRows}
	state.pendingDamageRects = state.pendingDamageRects[:0]
	state.fullContentDirty = false
	state.tabBarDirty = false
	state.lastCursorX = cursorX
	state.lastCursorY = cursorY
	state.lastCursorVisible = cursorVisible
	state.hasRenderedCursor = true
	return buf.Bytes()
}

func reconnectStatusText(state *ClientState, now time.Time) string {
	seconds := int(now.Sub(state.LastContact) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	return "tali is reconnecting... [Last contact " + strconv.Itoa(seconds) + " seconds ago]"
}

func renderReconnectStatusBar(buf *bytes.Buffer, state *ClientState, now time.Time) {
	text := reconnectStatusText(state, now)
	cells := make([]composedCell, state.TerminalCols)
	style := protocol.Style{FG: protocol.Color{Mode: "rgb", R: 255, G: 165, B: 0}, BG: protocol.Color{Mode: "default"}}
	for i := range cells {
		cells[i] = composedCell{Rune: ' ', Width: 1, Style: style}
	}
	for i, r := range []rune(text) {
		if i >= len(cells) {
			break
		}
		cells[i].Rune = r
	}
	renderCellRun(buf, cells)
}

func renderStatusBar(buf *bytes.Buffer, state *ClientState) {
	if state.StatusBar.Cols == state.TerminalCols && len(state.StatusBar.Cells) == state.TerminalCols {
		cells := make([]composedCell, len(state.StatusBar.Cells))
		for i, src := range state.StatusBar.Cells {
			style := defaultStyle()
			if found, ok := state.StatusStyles[src.StyleID]; ok {
				style = found
			}
			r := src.Rune
			if r == 0 {
				r = ' '
			}
			cells[i] = composedCell{Rune: r, Width: src.Width, Style: style}
		}
		renderCellRun(buf, cells)
		return
	}
	for i := 0; i < state.TerminalCols; i++ {
		buf.WriteByte(' ')
	}
}

func renderFullContent(buf *bytes.Buffer, cells []composedCell, cols, rows int) {
	for row := 0; row < rows; row++ {
		writeCursorPosition(buf, row+1, 1)
		renderCellRun(buf, cells[row*cols:(row+1)*cols])
	}
}

type columnSpan struct {
	start int
	end   int
}

func renderDamagedContent(buf *bytes.Buffer, state *ClientState, rects []protocol.Rect, cols, rows int) {
	spans := make(map[int][]columnSpan)
	for _, rect := range rects {
		startX := max(rect.X, 0)
		endX := min(rect.X+rect.Width, cols)
		startY := max(rect.Y, 0)
		endY := min(rect.Y+rect.Height, rows)
		if startX >= endX || startY >= endY {
			continue
		}
		for row := startY; row < endY; row++ {
			spans[row] = append(spans[row], columnSpan{start: startX, end: endX})
		}
	}
	orderedRows := make([]int, 0, len(spans))
	for row := range spans {
		orderedRows = append(orderedRows, row)
	}
	sort.Ints(orderedRows)
	placements := state.orderedLayoutPanes()
	var scratch []composedCell
	for _, row := range orderedRows {
		rowSpans := spans[row]
		sort.Slice(rowSpans, func(i, j int) bool { return rowSpans[i].start < rowSpans[j].start })
		merged := rowSpans[:0]
		for _, span := range rowSpans {
			if len(merged) == 0 || span.start > merged[len(merged)-1].end {
				merged = append(merged, span)
				continue
			}
			merged[len(merged)-1].end = max(merged[len(merged)-1].end, span.end)
		}
		for _, span := range merged {
			width := span.end - span.start
			if cap(scratch) < width {
				scratch = make([]composedCell, width)
			} else {
				scratch = scratch[:width]
			}
			composeRowSpan(scratch, state, placements, row, span.start)
			writeCursorPosition(buf, row+1, span.start+1)
			renderCellRun(buf, scratch)
		}
	}
}

func renderCellRun(buf *bytes.Buffer, cells []composedCell) {
	var currentStyle protocol.Style
	hasStyle := false
	for _, cell := range cells {
		if cell.Width == 0 {
			continue
		}
		if !hasStyle || cell.Style != currentStyle {
			buf.WriteString(sgrForStyle(cell.Style))
			currentStyle = cell.Style
			hasStyle = true
		}
		buf.WriteRune(cell.Rune)
	}
}

func writeCursorPosition(buf *bytes.Buffer, row, col int) {
	buf.WriteString("\x1b[")
	buf.WriteString(strconv.Itoa(row))
	buf.WriteByte(';')
	buf.WriteString(strconv.Itoa(col))
	buf.WriteByte('H')
}

type composedCell struct {
	Rune  rune
	Width uint8
	Style protocol.Style
}

func composeContent(state *ClientState) []composedCell {
	contentRows := state.DrawableRows()
	if state.TerminalCols <= 0 || contentRows <= 0 {
		return nil
	}
	cellCount := state.TerminalCols * contentRows
	if cap(state.composedCells) < cellCount {
		state.composedCells = make([]composedCell, cellCount)
	} else {
		state.composedCells = state.composedCells[:cellCount]
	}
	cells := state.composedCells
	placements := state.orderedLayoutPanes()
	for row := 0; row < contentRows; row++ {
		composeRowSpan(cells[row*state.TerminalCols:(row+1)*state.TerminalCols], state, placements, row, 0)
	}
	return cells
}

func composeRowSpan(dst []composedCell, state *ClientState, placements []protocol.PanePlacement, row, startColumn int) {
	defaultCell := composedCell{Rune: ' ', Width: 1, Style: defaultStyle()}
	for i := range dst {
		column := startColumn + i
		cell := defaultCell
		insidePane := false
		for _, placement := range placements {
			if column < placement.Rect.X || column >= placement.Rect.X+placement.Rect.Width ||
				row < placement.Rect.Y || row >= placement.Rect.Y+placement.Rect.Height {
				continue
			}
			insidePane = true
			pane := state.Panes[placement.PaneID]
			if pane == nil {
				break
			}
			localColumn := column - placement.Rect.X
			localRow := row - placement.Rect.Y
			if localColumn < pane.Grid.Cols && localRow < pane.Grid.Rows {
				src := pane.Grid.Cells[localRow*pane.Grid.Cols+localColumn]
				style := defaultCell.Style
				if found, ok := pane.Styles[src.StyleID]; ok {
					style = found
				}
				r := src.Rune
				if r == 0 {
					r = ' '
				}
				cell = composedCell{Rune: r, Width: src.Width, Style: style}
			}
			break
		}
		if !insidePane {
			if border := paneBorderRune(placements, column, row); border != 0 {
				cell = composedCell{Rune: border, Width: 1, Style: defaultCell.Style}
			}
		}
		dst[i] = cell
	}
}

func paneBorderRune(placements []protocol.PanePlacement, column, row int) rune {
	var left, right, above, below bool
	for _, placement := range placements {
		rect := placement.Rect
		if row >= rect.Y && row < rect.Y+rect.Height {
			left = left || rect.X+rect.Width == column
			right = right || rect.X == column+1
		}
		if column >= rect.X && column < rect.X+rect.Width {
			above = above || rect.Y+rect.Height == row
			below = below || rect.Y == row+1
		}
	}
	vertical := left && right
	horizontal := above && below
	switch {
	case vertical && horizontal:
		return '┼'
	case vertical:
		return '│'
	case horizontal:
		return '─'
	default:
		return 0
	}
}

func physicalCursor(state *ClientState) (int, int, bool) {
	pane := state.Panes[state.FocusedPaneID]
	if pane == nil {
		return 0, 0, false
	}
	x := pane.Rect.X + pane.Cursor.X
	y := pane.Rect.Y + pane.Cursor.Y
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return x, y, pane.CursorVisible
}

func defaultStyle() protocol.Style {
	return protocol.Style{
		FG: protocol.Color{Mode: "default"},
		BG: protocol.Color{Mode: "default"},
	}
}

func sgrForStyle(style protocol.Style) string {
	codes := []string{"0"}
	if style.Bold {
		codes = append(codes, "1")
	}
	if style.Italic {
		codes = append(codes, "3")
	}
	if style.Underline {
		codes = append(codes, "4")
	}
	if style.Reverse {
		codes = append(codes, "7")
	}
	codes = append(codes, colorCodes(style.FG, true)...)
	codes = append(codes, colorCodes(style.BG, false)...)
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

func colorCodes(c protocol.Color, fg bool) []string {
	switch c.Mode {
	case "indexed":
		if c.Index < 8 {
			if fg {
				return []string{strconv.Itoa(30 + int(c.Index))}
			}
			return []string{strconv.Itoa(40 + int(c.Index))}
		}
		if c.Index < 16 {
			if fg {
				return []string{strconv.Itoa(90 + int(c.Index-8))}
			}
			return []string{strconv.Itoa(100 + int(c.Index-8))}
		}
		prefix := "48"
		if fg {
			prefix = "38"
		}
		return []string{prefix, "5", strconv.Itoa(int(c.Index))}
	case "rgb":
		prefix := "48"
		if fg {
			prefix = "38"
		}
		return []string{prefix, "2", strconv.Itoa(int(c.R)), strconv.Itoa(int(c.G)), strconv.Itoa(int(c.B))}
	default:
		if fg {
			return []string{"39"}
		}
		return []string{"49"}
	}
}
