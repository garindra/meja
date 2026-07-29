package server

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

const paneResizeRepeatWindow = 500 * time.Millisecond

func (c *ClientInstance) BeginPrompt(mode PromptMode, label, initial string) (*activePrompt, error) {
	if c == nil {
		return nil, errors.New("client is unavailable")
	}
	windowID := c.currentView.Layout.WindowID
	if windowID == 0 {
		return nil, errors.New("client has no active window")
	}
	if c.activePrompt != nil {
		return nil, errors.New("client already has an active prompt")
	}
	if c.nextPromptID == ^uint64(0) {
		return nil, errors.New("client prompt ID space exhausted")
	}
	c.nextPromptID++
	client := &c.inputState
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	c.activePrompt = &activePrompt{ID: c.nextPromptID, Mode: mode, Label: label, Initial: initial}
	return cloneActivePrompt(c.activePrompt), nil
}

func (c *ClientInstance) BeginCommandPrompt() (*activePrompt, error) {
	prompt, err := c.BeginPrompt(PromptModeText, ":", "")
	if err != nil {
		return nil, err
	}
	c.promptContinuation = c.runCommandPromptAnswer
	return prompt, nil
}

func (c *ClientInstance) showStatusMessage(message string) {
	if c == nil || c.currentView.Layout.WindowID == 0 {
		return
	}
	c.statusMessageID++
	messageID := c.statusMessageID
	c.statusMessage = message
	duration := c.statusMessageDuration
	if duration <= 0 {
		duration = time.Second
	}
	time.AfterFunc(duration, func() {
		c.postCommand(clientInstanceCommand{ClearStatusMessage: messageID})
	})
}

func (c *ClientInstance) beginPrompt(mode PromptMode, label, initial string, continuation promptContinuation) (*activePrompt, error) {
	prompt, err := c.BeginPrompt(mode, label, initial)
	if err != nil {
		return nil, err
	}
	c.promptContinuation = continuation
	return prompt, nil
}

func (c *ClientInstance) resolvePrompt(result protocol.FrontendPromptResult) (bool, error) {
	prompt := c.activePrompt
	if prompt == nil || prompt.ID != result.PromptID {
		return false, nil
	}
	c.activePrompt = nil
	continuation := c.promptContinuation
	c.promptContinuation = nil
	if continuation == nil {
		return false, c.publishClientStatus()
	}
	return continuation(promptResult{Submitted: result.Submitted, Text: result.Text})
}

func (c *ClientInstance) ActivePrompt() *activePrompt {
	if c == nil {
		return nil
	}
	return cloneActivePrompt(c.activePrompt)
}

type serverInputState uint8

const (
	serverInputNormal serverInputState = iota
	serverInputPrefix
	serverInputPrefixESC
	serverInputPrefixCSI
	serverInputResizeRepeatESC
	serverInputResizeRepeatCSI
)

type serverInputCommand uint8

const (
	serverCommandNone serverInputCommand = iota
	serverCommandLiteral
	serverCommandExecute
	serverCommandOpenCommandPrompt
)

type serverInputEvent struct {
	Command     serverInputCommand
	Byte        byte
	Data        []byte
	CommandArgs []string
}

func (c *ClientInstance) ConsumeInputByte(b byte) serverInputEvent {
	if c == nil {
		return serverInputEvent{}
	}
	return consumeInputByteAt(&c.inputState, b, time.Now())
}

func consumeInputByteAt(client *clientInputState, b byte, now time.Time) serverInputEvent {
	switch client.InputState {
	case serverInputPrefix:
		if b == 0x1b {
			client.InputState = serverInputPrefixESC
			client.PrefixEscape = []byte{b}
			return serverInputEvent{}
		}
		resetPrefixInput(client)
		switch b {
		case 0x02:
			return serverInputEvent{Command: serverCommandLiteral, Byte: 0x02}
		case 'c':
			return commandInputEvent("new-window")
		case ' ':
			return commandInputEvent("next-layout")
		case '%':
			return commandInputEvent("split-window", "-h")
		case '"':
			return commandInputEvent("split-window", "-v")
		case 'd':
			return commandInputEvent("detach-client")
		case 'n':
			return commandInputEvent("next-window")
		case 'p':
			return commandInputEvent("previous-window")
		case 'l':
			return commandInputEvent("last-window")
		case 'x':
			return commandInputEvent("kill-pane")
		case 'z':
			return commandInputEvent("resize-pane", "-Z")
		case '[':
			return commandInputEvent("copy-mode")
		case ']':
			return commandInputEvent("paste-buffer")
		case '{':
			return commandInputEvent("swap-pane", "-U")
		case '}':
			return commandInputEvent("swap-pane", "-D")
		case ',':
			return commandInputEvent("rename-window")
		case '$':
			return commandInputEvent("rename-session")
		case ':':
			return serverInputEvent{Command: serverCommandOpenCommandPrompt}
		default:
			if b >= '0' && b <= '9' {
				return commandInputEvent("select-window", "-t", ":"+string(b))
			}
		}
	case serverInputPrefixESC:
		if b == '[' {
			client.PrefixEscape = append(client.PrefixEscape, b)
			client.InputState = serverInputPrefixCSI
			return serverInputEvent{}
		}
		if b == 0x1b && len(client.PrefixEscape) == 1 {
			client.PrefixEscape = append(client.PrefixEscape, b)
			return serverInputEvent{}
		}
		resetPrefixInput(client)
	case serverInputPrefixCSI:
		client.PrefixEscape = append(client.PrefixEscape, b)
		if len(client.PrefixEscape) > 32 {
			resetPrefixInput(client)
			return serverInputEvent{}
		}
		if b < 0x40 || b > 0x7e {
			return serverInputEvent{}
		}
		sequence := append([]byte(nil), client.PrefixEscape...)
		resetPrefixInput(client)
		event := decodePrefixCSI(sequence)
		if isResizeCommandEvent(event) {
			armPaneResizeRepeat(client, now)
		}
		return event
	case serverInputResizeRepeatESC:
		if !paneResizeRepeatActive(client, now) {
			return cancelPaneResizeRepeatWithInput(client, b)
		}
		if b == '[' {
			client.PrefixEscape = append(client.PrefixEscape, b)
			client.InputState = serverInputResizeRepeatCSI
			return serverInputEvent{}
		}
		if b == 0x1b && len(client.PrefixEscape) == 1 {
			client.PrefixEscape = append(client.PrefixEscape, b)
			return serverInputEvent{}
		}
		return cancelPaneResizeRepeatWithInput(client, b)
	case serverInputResizeRepeatCSI:
		client.PrefixEscape = append(client.PrefixEscape, b)
		if !paneResizeRepeatActive(client, now) || len(client.PrefixEscape) > 32 {
			return cancelPaneResizeRepeatWithInput(client)
		}
		if b < 0x40 || b > 0x7e {
			return serverInputEvent{}
		}
		sequence := append([]byte(nil), client.PrefixEscape...)
		resetPrefixInput(client)
		event := decodePrefixCSI(sequence)
		if isResizeCommandEvent(event) {
			armPaneResizeRepeat(client, now)
			return event
		}
		cancelPaneResizeRepeat(client)
		return serverInputEvent{Command: serverCommandLiteral, Data: sequence}
	default:
		if paneResizeRepeatActive(client, now) {
			if b == 0x1b {
				client.InputState = serverInputResizeRepeatESC
				client.PrefixEscape = []byte{b}
				return serverInputEvent{}
			}
			cancelPaneResizeRepeat(client)
		} else if !client.ResizeRepeatUntil.IsZero() {
			cancelPaneResizeRepeat(client)
		}
		if b == 0x02 {
			client.InputState = serverInputPrefix
			client.PrefixEscape = nil
			return serverInputEvent{}
		}
		return serverInputEvent{Command: serverCommandLiteral, Byte: b}
	}
	return serverInputEvent{}
}

func armPaneResizeRepeat(client *clientInputState, now time.Time) {
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = now.Add(paneResizeRepeatWindow)
}

func paneResizeRepeatActive(client *clientInputState, now time.Time) bool {
	return client != nil && !client.ResizeRepeatUntil.IsZero() && now.Before(client.ResizeRepeatUntil)
}

func cancelPaneResizeRepeat(client *clientInputState) {
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
}

func cancelPaneResizeRepeatWithInput(client *clientInputState, suffix ...byte) serverInputEvent {
	data := append([]byte(nil), client.PrefixEscape...)
	data = append(data, suffix...)
	cancelPaneResizeRepeat(client)
	return serverInputEvent{Command: serverCommandLiteral, Data: data}
}

func resetPrefixInput(client *clientInputState) {
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
}

func commandInputEvent(args ...string) serverInputEvent {
	return serverInputEvent{Command: serverCommandExecute, CommandArgs: args}
}

func isResizeCommandEvent(event serverInputEvent) bool {
	return event.Command == serverCommandExecute && len(event.CommandArgs) > 0 && event.CommandArgs[0] == "resize-pane"
}

func decodePrefixCSI(sequence []byte) serverInputEvent {
	index := 0
	meta := false
	if index >= len(sequence) || sequence[index] != 0x1b {
		return serverInputEvent{}
	}
	index++
	if index < len(sequence) && sequence[index] == 0x1b {
		meta = true
		index++
	}
	if index >= len(sequence) || sequence[index] != '[' || len(sequence)-index < 2 {
		return serverInputEvent{}
	}
	index++
	final := sequence[len(sequence)-1]
	if final < 'A' || final > 'D' {
		return serverInputEvent{}
	}
	modifier := 1
	params := string(sequence[index : len(sequence)-1])
	if params != "" {
		parts := strings.Split(params, ";")
		parsed, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			return serverInputEvent{}
		}
		modifier = parsed
	}
	if meta {
		modifier = 3
	}
	if modifier == 1 {
		return commandInputEvent("select-pane", directionFlag(final))
	}
	amount := 0
	if modifier == 5 {
		amount = 1
	} else if modifier == 3 {
		amount = 5
	}
	if amount == 0 {
		return serverInputEvent{}
	}
	return commandInputEvent("resize-pane", directionFlag(final), strconv.Itoa(amount))
}

func directionFlag(final byte) string {
	switch final {
	case 'A':
		return "-U"
	case 'B':
		return "-D"
	case 'C':
		return "-R"
	default:
		return "-L"
	}
}

func (c *ClientInstance) InputIsNormal() bool {
	return c != nil && c.inputState.InputState == serverInputNormal && !paneResizeRepeatActive(&c.inputState, time.Now())
}

func translateApplicationCursor(data []byte, enabled bool) ([]byte, int, bool) {
	if !enabled || len(data) < 3 || data[0] != 0x1b || data[1] != '[' || data[2] < 'A' || data[2] > 'D' {
		return nil, 0, false
	}
	return []byte{0x1b, 'O', data[2]}, 3, true
}

func (c *ClientInstance) FocusPaneDirection(direction byte) (*Window, protocol.ClientLayout, error) {
	if c == nil {
		return nil, protocol.ClientLayout{}, errors.New("client is unavailable")
	}
	client := &c.inputState
	windowID := c.currentView.Layout.WindowID
	placements := c.currentView.NavigationPanes
	if len(placements) == 0 {
		placements = c.currentView.Layout.Panes
	}
	if windowID == 0 || len(placements) == 0 {
		return nil, protocol.ClientLayout{}, fmt.Errorf("unknown window %d", windowID)
	}
	var current *protocol.PanePlacement
	for i := range placements {
		if placements[i].PaneID == c.currentView.Layout.FocusedPaneID {
			current = &placements[i]
			break
		}
	}
	if current == nil {
		return nil, c.currentView.Layout, nil
	}
	if !client.HasFocusPoint {
		client.FocusX2 = rectCenterX2(current.Rect)
		client.FocusY2 = rectCenterY2(current.Rect)
		client.HasFocusPoint = true
	} else {
		client.FocusX2 = clampToRectAxis(client.FocusX2, current.Rect.X, current.Rect.Width)
		client.FocusY2 = clampToRectAxis(client.FocusY2, current.Rect.Y, current.Rect.Height)
	}
	type candidate struct {
		placement    protocol.PanePlacement
		primaryGap   int
		secondaryGap int
	}
	var best candidate
	hasBest := false
	for _, placement := range placements {
		if placement.PaneID == current.PaneID {
			continue
		}
		candidate := candidate{placement: placement}
		candidateRight := placement.Rect.X + placement.Rect.Width
		candidateBottom := placement.Rect.Y + placement.Rect.Height
		currentRight := current.Rect.X + current.Rect.Width
		currentBottom := current.Rect.Y + current.Rect.Height
		switch direction {
		case 'A':
			if candidateBottom > current.Rect.Y {
				continue
			}
			candidate.primaryGap = current.Rect.Y - candidateBottom
			candidate.secondaryGap = distanceToRectAxis(client.FocusX2, placement.Rect.X, placement.Rect.Width)
		case 'B':
			if placement.Rect.Y < currentBottom {
				continue
			}
			candidate.primaryGap = placement.Rect.Y - currentBottom
			candidate.secondaryGap = distanceToRectAxis(client.FocusX2, placement.Rect.X, placement.Rect.Width)
		case 'C':
			if placement.Rect.X < currentRight {
				continue
			}
			candidate.primaryGap = placement.Rect.X - currentRight
			candidate.secondaryGap = distanceToRectAxis(client.FocusY2, placement.Rect.Y, placement.Rect.Height)
		case 'D':
			if candidateRight > current.Rect.X {
				continue
			}
			candidate.primaryGap = current.Rect.X - candidateRight
			candidate.secondaryGap = distanceToRectAxis(client.FocusY2, placement.Rect.Y, placement.Rect.Height)
		default:
			continue
		}
		if !hasBest || candidate.secondaryGap < best.secondaryGap ||
			(candidate.secondaryGap == best.secondaryGap && candidate.primaryGap < best.primaryGap) ||
			(candidate.secondaryGap == best.secondaryGap && candidate.primaryGap == best.primaryGap && candidate.placement.PaneID < best.placement.PaneID) {
			best = candidate
			hasBest = true
		}
	}
	if hasBest {
		if direction == 'A' || direction == 'B' {
			client.FocusX2 = clampToRectAxis(client.FocusX2, best.placement.Rect.X, best.placement.Rect.Width)
			client.FocusY2 = rectCenterY2(best.placement.Rect)
		} else {
			client.FocusX2 = rectCenterX2(best.placement.Rect)
			client.FocusY2 = clampToRectAxis(client.FocusY2, best.placement.Rect.Y, best.placement.Rect.Height)
		}
		focusedWindow, err := c.focusPane(best.placement.PaneID)
		if err != nil {
			return nil, protocol.ClientLayout{}, err
		}
		return focusedWindow, c.currentView.Layout, nil
	}
	return nil, c.currentView.Layout, nil
}

func rectCenterX2(rect protocol.Rect) int {
	return rect.X*2 + rect.Width
}

func rectCenterY2(rect protocol.Rect) int {
	return rect.Y*2 + rect.Height
}

func clampToRectAxis(point, start, size int) int {
	minimum := start * 2
	maximum := (start+size)*2 - 1
	if point < minimum {
		return minimum
	}
	if point > maximum {
		return maximum
	}
	return point
}

func distanceToRectAxis(point, start, size int) int {
	clamped := clampToRectAxis(point, start, size)
	if point < clamped {
		return clamped - point
	}
	return point - clamped
}
