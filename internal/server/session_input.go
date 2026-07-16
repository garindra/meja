package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const paneResizeRepeatWindow = 500 * time.Millisecond

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
	serverCommandCreateWindow
	serverCommandSplitVertical
	serverCommandSplitHorizontal
	serverCommandDetach
	serverCommandNextWindow
	serverCommandPreviousWindow
	serverCommandLastWindow
	serverCommandClosePane
	serverCommandEnterHistory
	serverCommandSwapPanePrevious
	serverCommandSwapPaneNext
	serverCommandSelectIndex
	serverCommandFocusDirection
	serverCommandResizePane
	serverCommandToggleZoom
	serverCommandBeginWindowPrompt
	serverCommandBeginSessionPrompt
	serverCommandPrompt
)

type serverInputEvent struct {
	Command         serverInputCommand
	Byte            byte
	Data            []byte
	Index           int
	Direction       byte
	ResizeDirection PaneResizeDirection
	ResizeAmount    int
	PromptAction    PromptAction
	PromptKind      PromptKind
	PromptWindowID  uint64
	PromptText      string
}

func (s *Session) ConsumeInputByte(clientID uint64, b byte) serverInputEvent {
	client := s.Clients[clientID]
	if client == nil {
		return serverInputEvent{}
	}
	if client.Prompt != nil {
		return consumePromptByteLocked(client, b)
	}
	return consumeInputByteLockedAt(client, b, time.Now())
}

func consumeInputByteLocked(client *ClientState, b byte) serverInputEvent {
	return consumeInputByteLockedAt(client, b, time.Now())
}

func consumeInputByteLockedAt(client *ClientState, b byte, now time.Time) serverInputEvent {
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
			return serverInputEvent{Command: serverCommandCreateWindow}
		case '%':
			return serverInputEvent{Command: serverCommandSplitVertical}
		case '"':
			return serverInputEvent{Command: serverCommandSplitHorizontal}
		case 'd':
			return serverInputEvent{Command: serverCommandDetach}
		case 'n':
			return serverInputEvent{Command: serverCommandNextWindow}
		case 'p':
			return serverInputEvent{Command: serverCommandPreviousWindow}
		case 'l':
			return serverInputEvent{Command: serverCommandLastWindow}
		case 'x':
			return serverInputEvent{Command: serverCommandClosePane}
		case 'z':
			return serverInputEvent{Command: serverCommandToggleZoom}
		case '[':
			return serverInputEvent{Command: serverCommandEnterHistory}
		case '{':
			return serverInputEvent{Command: serverCommandSwapPanePrevious}
		case '}':
			return serverInputEvent{Command: serverCommandSwapPaneNext}
		case ',':
			return serverInputEvent{Command: serverCommandBeginWindowPrompt}
		case '$':
			return serverInputEvent{Command: serverCommandBeginSessionPrompt}
		default:
			if b >= '0' && b <= '9' {
				return serverInputEvent{Command: serverCommandSelectIndex, Index: int(b - '0')}
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
		if event.Command == serverCommandResizePane {
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
		if event.Command == serverCommandResizePane {
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

func armPaneResizeRepeat(client *ClientState, now time.Time) {
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = now.Add(paneResizeRepeatWindow)
}

func paneResizeRepeatActive(client *ClientState, now time.Time) bool {
	return client != nil && !client.ResizeRepeatUntil.IsZero() && now.Before(client.ResizeRepeatUntil)
}

func cancelPaneResizeRepeat(client *ClientState) {
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
}

func cancelPaneResizeRepeatWithInput(client *ClientState, suffix ...byte) serverInputEvent {
	data := append([]byte(nil), client.PrefixEscape...)
	data = append(data, suffix...)
	cancelPaneResizeRepeat(client)
	return serverInputEvent{Command: serverCommandLiteral, Data: data}
}

func resetPrefixInput(client *ClientState) {
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
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
		return serverInputEvent{Command: serverCommandFocusDirection, Direction: final}
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
	direction := ResizePaneUp
	switch final {
	case 'B':
		direction = ResizePaneDown
	case 'C':
		direction = ResizePaneRight
	case 'D':
		direction = ResizePaneLeft
	}
	return serverInputEvent{Command: serverCommandResizePane, ResizeDirection: direction, ResizeAmount: amount}
}

func consumePromptByteLocked(client *ClientState, b byte) serverInputEvent {
	prompt := client.Prompt
	if prompt.Kind == PromptKindConfirm {
		return consumeConfirmationByteLocked(client, b)
	}
	if len(prompt.PendingEscape) > 0 {
		return consumePromptEscapeByteLocked(client, b)
	}
	if b == 0x1b {
		prompt.PendingEscape = []byte{b}
		return serverInputEvent{}
	}
	event := serverInputEvent{
		Command:        serverCommandPrompt,
		PromptKind:     prompt.Kind,
		PromptWindowID: prompt.TargetWindowID,
	}
	switch b {
	case '\r', '\n':
		prompt.Action = PromptActionSubmit
		prompt.PendingEscape = nil
		prompt.pendingUTF8 = nil
		event.PromptAction = PromptActionSubmit
		event.PromptText = string(prompt.Text)
		if prompt.Kind != PromptKindRenameSession {
			client.Prompt = nil
		}
	case 0x03, 0x1b:
		prompt.Action = PromptActionCancel
		prompt.PendingEscape = nil
		prompt.pendingUTF8 = nil
		event.PromptAction = PromptActionCancel
		client.Prompt = nil
	case 0x08, 0x7f:
		prompt.pendingUTF8 = nil
		deletePromptRune(prompt)
		prompt.Action = PromptActionChanged
		event.PromptAction = PromptActionChanged
	default:
		if b < 0x20 {
			return serverInputEvent{}
		}
		if !appendPromptByte(prompt, b) {
			return serverInputEvent{}
		}
		prompt.Action = PromptActionChanged
		event.PromptAction = PromptActionChanged
	}
	return event
}

func consumeConfirmationByteLocked(client *ClientState, b byte) serverInputEvent {
	prompt := client.Prompt
	event := serverInputEvent{Command: serverCommandPrompt, PromptKind: prompt.Kind}
	switch b {
	case 'y', 'Y':
		event.PromptAction = PromptActionSubmit
		event.PromptText = "y"
		client.Prompt = nil
	case 'n', 'N', '\r', '\n', 0x03, 0x1b:
		event.PromptAction = PromptActionCancel
		client.Prompt = nil
	}
	return event
}

func consumePromptEscapeByteLocked(client *ClientState, b byte) serverInputEvent {
	prompt := client.Prompt
	switch len(prompt.PendingEscape) {
	case 1:
		if b == '[' {
			prompt.PendingEscape = append(prompt.PendingEscape, b)
			return serverInputEvent{}
		}
		return cancelPromptLocked(client)
	case 2:
		if b == '3' {
			prompt.PendingEscape = append(prompt.PendingEscape, b)
			return serverInputEvent{}
		}
		return cancelPromptLocked(client)
	case 3:
		if b == '~' {
			prompt.PendingEscape = nil
			prompt.pendingUTF8 = nil
			deletePromptRune(prompt)
			prompt.Action = PromptActionChanged
			return promptEventLocked(prompt, PromptActionChanged, "")
		}
		return cancelPromptLocked(client)
	default:
		return cancelPromptLocked(client)
	}
}

func cancelPromptLocked(client *ClientState) serverInputEvent {
	prompt := client.Prompt
	prompt.Action = PromptActionCancel
	prompt.PendingEscape = nil
	prompt.pendingUTF8 = nil
	event := promptEventLocked(prompt, PromptActionCancel, "")
	client.Prompt = nil
	return event
}

func promptEventLocked(prompt *PromptState, action PromptAction, text string) serverInputEvent {
	return serverInputEvent{
		Command:        serverCommandPrompt,
		PromptAction:   action,
		PromptKind:     prompt.Kind,
		PromptWindowID: prompt.TargetWindowID,
		PromptText:     text,
	}
}

// ConsumePromptInput keeps incomplete escape sequences in PromptState. Escape
// is resolved as cancel only once its next byte proves it is not CSI Delete.
// Any submit/cancel terminates ownership of the current input payload.
func (s *Session) ConsumePromptInput(clientID uint64, data []byte) (int, []serverInputEvent, bool) {
	client := s.Clients[clientID]
	if client == nil || client.Prompt == nil {
		return 0, nil, false
	}
	events := make([]serverInputEvent, 0, len(data))
	index := 0
	terminated := false
	for index < len(data) && client.Prompt != nil {
		event := consumePromptByteLocked(client, data[index])
		index++
		if event.Command != serverCommandNone {
			events = append(events, event)
			if event.PromptAction == PromptActionSubmit || event.PromptAction == PromptActionCancel {
				terminated = true
				break
			}
		}
	}
	return index, events, terminated
}

func (s *Session) InputIsNormal(clientID uint64) bool {
	client := s.Clients[clientID]
	return client != nil && client.Prompt == nil && client.InputState == serverInputNormal && !paneResizeRepeatActive(client, time.Now())
}

func translateApplicationCursor(data []byte, enabled bool) ([]byte, int, bool) {
	if !enabled || len(data) < 3 || data[0] != 0x1b || data[1] != '[' || data[2] < 'A' || data[2] > 'D' {
		return nil, 0, false
	}
	return []byte{0x1b, 'O', data[2]}, 3, true
}

func (s *Session) RelativeWindowID(clientID uint64, delta int) (uint64, bool) {
	client := s.Clients[clientID]
	if client == nil || len(s.Windows) == 0 {
		return 0, false
	}
	ids := s.windowIDsLocked()
	for i, id := range ids {
		if id == client.ActiveWindowID {
			return ids[(i+delta+len(ids))%len(ids)], true
		}
	}
	return ids[0], true
}

func (s *Session) LastWindowID(clientID uint64) (uint64, bool) {
	client := s.Clients[clientID]
	if client == nil || !client.HasLastWindow || s.Windows[client.LastWindowID] == nil {
		return 0, false
	}
	return client.LastWindowID, true
}

func (s *Session) WindowIDByIndex(index int) (uint64, bool) {
	if index < 0 {
		return 0, false
	}
	for _, window := range s.Windows {
		if window.DisplayIndex == index {
			return window.ID, true
		}
	}
	return 0, false
}

func appendPromptByte(prompt *PromptState, b byte) bool {
	prompt.pendingUTF8 = append(prompt.pendingUTF8, b)
	changed := false
	for len(prompt.pendingUTF8) > 0 {
		if prompt.pendingUTF8[0] < utf8.RuneSelf {
			insertPromptRune(prompt, rune(prompt.pendingUTF8[0]))
			prompt.pendingUTF8 = prompt.pendingUTF8[1:]
			changed = true
			continue
		}
		if !utf8.FullRune(prompt.pendingUTF8) {
			break
		}
		r, size := utf8.DecodeRune(prompt.pendingUTF8)
		if r == utf8.RuneError && size == 1 {
			insertPromptRune(prompt, utf8.RuneError)
			prompt.pendingUTF8 = prompt.pendingUTF8[1:]
			changed = true
			continue
		}
		insertPromptRune(prompt, r)
		prompt.pendingUTF8 = prompt.pendingUTF8[size:]
		changed = true
	}
	return changed
}

func insertPromptRune(prompt *PromptState, r rune) {
	if prompt.Cursor < 0 || prompt.Cursor > len(prompt.Text) {
		prompt.Cursor = len(prompt.Text)
	}
	prompt.Text = append(prompt.Text, 0)
	copy(prompt.Text[prompt.Cursor+1:], prompt.Text[prompt.Cursor:])
	prompt.Text[prompt.Cursor] = r
	prompt.Cursor++
}

func deletePromptRune(prompt *PromptState) {
	if prompt.Cursor <= 0 || len(prompt.Text) == 0 {
		return
	}
	// There is no cursor movement binding yet, so Delete at the end removes
	// the previous rune just like Backspace while retaining cursor semantics.
	prompt.Cursor--
	copy(prompt.Text[prompt.Cursor:], prompt.Text[prompt.Cursor+1:])
	prompt.Text = prompt.Text[:len(prompt.Text)-1]
}

func (s *Session) FocusPaneDirection(clientID uint64, direction byte) (*Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if window.Zoomed {
		window.clearZoom()
		window.LayoutRevision = s.nextLayoutRevisionLocked()
		s.rebuildBindingsLocked(client, window)
	}
	placements := window.Layout.Compute(Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)})
	var current *PanePlacement
	for i := range placements {
		if placements[i].PaneID == client.FocusedPaneID {
			current = &placements[i]
			break
		}
	}
	if current == nil {
		return cloneWindow(window), cloneClientState(client), nil
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
		placement    PanePlacement
		primaryGap   int
		secondaryGap int
	}
	var best *candidate
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
		if best == nil || candidate.secondaryGap < best.secondaryGap ||
			(candidate.secondaryGap == best.secondaryGap && candidate.primaryGap < best.primaryGap) ||
			(candidate.secondaryGap == best.secondaryGap && candidate.primaryGap == best.primaryGap && candidate.placement.PaneID < best.placement.PaneID) {
			copy := candidate
			best = &copy
		}
	}
	if best != nil {
		client.FocusedPaneID = best.placement.PaneID
		window.ActivePaneID = best.placement.PaneID
		if direction == 'A' || direction == 'B' {
			client.FocusX2 = clampToRectAxis(client.FocusX2, best.placement.Rect.X, best.placement.Rect.Width)
			client.FocusY2 = rectCenterY2(best.placement.Rect)
		} else {
			client.FocusX2 = rectCenterX2(best.placement.Rect)
			client.FocusY2 = clampToRectAxis(client.FocusY2, best.placement.Rect.Y, best.placement.Rect.Height)
		}
	}
	return cloneWindow(window), cloneClientState(client), nil
}

func rectCenterX2(rect Rect) int {
	return rect.X*2 + rect.Width
}

func rectCenterY2(rect Rect) int {
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
