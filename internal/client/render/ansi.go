package render

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"tali/internal/protocol"
)

var tabBarBG = protocol.Color{Mode: "rgb", R: 42, G: 99, B: 158}

func RenderANSI(state *ClientState) []byte {
	var buf bytes.Buffer
	buf.WriteString("\x1b[?25l")
	contentRows := state.DrawableRows()
	if state.LastRendered.Cols != state.TerminalCols || state.LastRendered.Rows != contentRows {
		buf.WriteString("\x1b[H\x1b[2J")
	}
	composed := composeContent(state)
	lastStyleKey := ""
	for row := 0; row < contentRows; row++ {
		buf.WriteString(fmt.Sprintf("\x1b[%d;1H", row+1))
		for col := 0; col < state.TerminalCols; col++ {
			cell := composed[row*state.TerminalCols+col]
			styleKey := styleCacheKey(cell.Style)
			if styleKey != lastStyleKey {
				buf.WriteString(sgrForStyle(cell.Style))
				lastStyleKey = styleKey
			}
			r := cell.Rune
			if r == 0 {
				r = ' '
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteString(sgrForStyle(protocol.Style{
		FG: protocol.Color{Mode: "default"},
		BG: protocol.Color{Mode: "default"},
	}))
	buf.WriteString(fmt.Sprintf("\x1b[%d;1H", state.TerminalRows))
	buf.WriteString(renderTabBar(state))
	buf.WriteString(sgrForStyle(protocol.Style{
		FG: protocol.Color{Mode: "default"},
		BG: protocol.Color{Mode: "default"},
	}))
	cursorX, cursorY, cursorVisible := physicalCursor(state)
	buf.WriteString(fmt.Sprintf("\x1b[%d;%dH", cursorY+1, cursorX+1))
	if cursorVisible {
		buf.WriteString("\x1b[?25h")
	} else {
		buf.WriteString("\x1b[?25l")
	}
	state.LastRendered = Screen{Cols: state.TerminalCols, Rows: contentRows}
	return buf.Bytes()
}

type composedCell struct {
	Rune  rune
	Style protocol.Style
}

func composeContent(state *ClientState) []composedCell {
	contentRows := state.DrawableRows()
	if state.TerminalCols <= 0 || contentRows <= 0 {
		return nil
	}
	cells := make([]composedCell, state.TerminalCols*contentRows)
	defaultStyle := protocol.Style{
		FG: protocol.Color{Mode: "default"},
		BG: protocol.Color{Mode: "default"},
	}
	for i := range cells {
		cells[i] = composedCell{Rune: ' ', Style: defaultStyle}
	}
	for _, placement := range state.orderedLayoutPanes() {
		pane := state.Panes[placement.PaneID]
		if pane == nil || pane.Grid.Cols == 0 || pane.Grid.Rows == 0 {
			continue
		}
		for row := 0; row < placement.Rect.Height && row < pane.Grid.Rows; row++ {
			for col := 0; col < placement.Rect.Width && col < pane.Grid.Cols; col++ {
				dstX := placement.Rect.X + col
				dstY := placement.Rect.Y + row
				if dstX < 0 || dstY < 0 || dstX >= state.TerminalCols || dstY >= contentRows {
					continue
				}
				src := pane.Grid.Cells[row*pane.Grid.Cols+col]
				style := defaultStyle
				if pane.Styles != nil {
					if found, ok := pane.Styles[src.StyleID]; ok {
						style = found
					}
				}
				r := src.Rune
				if r == 0 {
					r = ' '
				}
				cells[dstY*state.TerminalCols+dstX] = composedCell{Rune: r, Style: style}
			}
		}
	}
	if len(state.Layout.Panes) == 2 {
		panes := state.orderedLayoutPanes()
		borderX := panes[0].Rect.X + panes[0].Rect.Width
		for y := 0; y < contentRows; y++ {
			if borderX >= 0 && borderX < state.TerminalCols {
				cells[y*state.TerminalCols+borderX] = composedCell{Rune: '│', Style: defaultStyle}
			}
		}
	}
	return cells
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

func renderTabBar(state *ClientState) string {
	width := state.TerminalCols
	if width <= 0 {
		return ""
	}
	var buf strings.Builder
	defaultStyle := protocol.Style{
		FG: protocol.Color{Mode: "default"},
		BG: tabBarBG,
	}

	prefix := fmt.Sprintf("[%d] ", state.SessionID)
	if len(prefix) > width {
		prefix = truncateToWidth(prefix, width)
	}

	entries := make([]string, 0, len(state.Windows))
	for _, w := range state.Windows {
		marker := ' '
		if w.WindowID == state.ActiveWindowID {
			marker = '*'
		}
		entries = append(entries, fmt.Sprintf("%d:%s%c ", w.Index, w.Title, marker))
	}

	used := 0
	buf.WriteString(sgrForStyle(defaultStyle))
	buf.WriteString(prefix)
	used += len(prefix)
	for _, entry := range entries {
		remaining := width - used
		if remaining <= 0 {
			break
		}
		if len(entry) > remaining {
			entry = truncateToWidth(entry, remaining)
		}
		buf.WriteString(entry)
		used += len(entry)
	}
	if used < width {
		buf.WriteString(strings.Repeat(" ", width-used))
	}
	return buf.String()
}

func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(s) <= width {
		return s
	}
	return s[:width]
}

func styleCacheKey(style protocol.Style) string {
	return fmt.Sprintf("%#v", style)
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
