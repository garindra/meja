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
	buf.WriteString("\x1b[H\x1b[2J")
	lastStyleID := ^uint32(0)
	for row := 0; row < state.Grid.Rows; row++ {
		buf.WriteString(fmt.Sprintf("\x1b[%d;1H", row+1))
		for col := 0; col < state.Grid.Cols; col++ {
			cell := state.Grid.Cells[row*state.Grid.Cols+col]
			if cell.StyleID != lastStyleID {
				buf.WriteString(sgrForStyle(state.Styles[cell.StyleID]))
				lastStyleID = cell.StyleID
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
	buf.WriteString(fmt.Sprintf("\x1b[%d;%dH", state.Cursor.Y+1, state.Cursor.X+1))
	if state.CursorVisible {
		buf.WriteString("\x1b[?25h")
	} else {
		buf.WriteString("\x1b[?25l")
	}
	state.LastRendered = state.Grid
	return buf.Bytes()
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
		suffix := ""
		if w.WindowID == state.ActiveWindowID {
			suffix = "*"
		}
		entries = append(entries, fmt.Sprintf(" %d:%s%s ", w.Index, w.Title, suffix))
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
