package protocol

import (
	"fmt"
	"path/filepath"
	"strings"
)

// StatusTab is the half-open rune-column range [Start, End) that a window
// occupies in the status text produced by StatusText.
type StatusTab struct {
	WindowIndex int
	Start, End  int
}

// StatusText renders the normal status line prefix and reports where each
// window tab sits within it. The client draws it and the server hit-tests it.
func StatusText(sessionName string, sessionID uint64, windows []ClientStatusWindow) (string, []StatusTab) {
	var text strings.Builder
	column := 0
	write := func(s string) {
		text.WriteString(s)
		column += len([]rune(s))
	}
	if sessionName != "" {
		write(fmt.Sprintf("[%s] ", sessionName))
	} else {
		write(fmt.Sprintf("[%d] ", sessionID))
	}
	tabs := make([]StatusTab, 0, len(windows))
	for _, window := range windows {
		flags := ""
		if window.Active {
			flags += "*"
		}
		if window.Zoomed {
			flags += "Z"
		}
		if flags == "" {
			flags = " "
		}
		start := column
		write(fmt.Sprintf("%d:%s%s", window.Index, window.Title, flags))
		tabs = append(tabs, StatusTab{WindowIndex: window.Index, Start: start, End: column})
		write(" ")
	}
	return text.String(), tabs
}

// StatusTabAt returns the window index of the tab under column x, or false when
// the status line is truncated at that column or no tab is there.
func StatusTabAt(width, x int, text, location string, tabs []StatusTab) (int, bool) {
	left, _ := StatusLineParts(width, text, location)
	if x < 0 || x >= len(left) {
		return 0, false
	}
	truncated := len(left) < len([]rune(text))
	for _, tab := range tabs {
		if x < tab.Start || x >= tab.End {
			continue
		}
		if truncated && tab.End > len(left)-1 {
			return 0, false
		}
		return tab.WindowIndex, true
	}
	return 0, false
}

func StatusLineParts(width int, text, location string) ([]rune, []rune) {
	if width <= 0 {
		return nil, nil
	}
	left, right := []rune(text), []rune(location)
	if len(left)+len(right) <= width {
		return left, right
	}
	leftWidth := width / 2
	rightWidth := width - leftWidth
	if len(left) < leftWidth {
		rightWidth += leftWidth - len(left)
		leftWidth = len(left)
	}
	if len(right) < rightWidth {
		leftWidth += rightWidth - len(right)
		rightWidth = len(right)
	}
	if len(left) > leftWidth {
		left = ellipsizeStatusPrefix(left, leftWidth)
	}
	if len(right) > rightWidth {
		right = ellipsizeStatusLocation(right, rightWidth)
	}
	return left, right
}

func ellipsizeStatusPrefix(value []rune, width int) []rune {
	if width <= 0 {
		return nil
	}
	if width == 1 {
		return []rune{'…'}
	}
	result := make([]rune, width)
	copy(result, value[:width-1])
	result[width-1] = '…'
	return result
}

func ellipsizeStatusLocation(value []rune, width int) []rune {
	if width <= 0 {
		return nil
	}
	if width == 1 {
		return []rune{'…'}
	}
	colon := -1
	for i, r := range value {
		if r == ':' {
			colon = i
			break
		}
	}
	if len(value) >= 3 && value[0] == '[' && value[len(value)-1] == ']' && colon > 0 {
		prefixWidth := colon + 1
		tailWidth := width - prefixWidth - 2
		if tailWidth >= 4 {
			result := make([]rune, 0, width)
			result = append(result, value[:prefixWidth]...)
			result = append(result, '…')
			result = append(result, value[len(value)-1-tailWidth:len(value)-1]...)
			result = append(result, ']')
			return result
		}
	}
	result := make([]rune, width)
	result[0] = '…'
	copy(result[1:], value[len(value)-width+1:])
	return result
}

func StatusLocation(hostname, root, home string) string {
	if root != "" {
		root = filepath.Clean(root)
	}
	if home != "" {
		home = filepath.Clean(home)
	}
	if root == "." {
		root = ""
	}
	if root != "" && home != "" {
		if relative, err := filepath.Rel(home, root); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			if relative == "." {
				root = "~"
			} else {
				root = "~/" + filepath.ToSlash(relative)
			}
		}
	}
	if hostname == "" {
		hostname = "?"
	}
	if root == "" {
		return "[" + hostname + "]"
	}
	return "[" + hostname + ":" + filepath.ToSlash(root) + "]"
}
