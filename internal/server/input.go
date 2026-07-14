package server

import (
	"fmt"
	"sort"
	"unicode/utf8"
)

type serverInputState uint8

const (
	serverInputNormal serverInputState = iota
	serverInputPrefix
	serverInputPrefixESC
	serverInputPrefixCSI
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
	serverCommandSelectIndex
	serverCommandFocusDirection
	serverCommandBeginWindowPrompt
	serverCommandBeginSessionPrompt
	serverCommandPrompt
)

type serverInputEvent struct {
	Command        serverInputCommand
	Byte           byte
	Index          int
	Direction      byte
	PromptAction   PromptAction
	PromptKind     PromptKind
	PromptWindowID uint64
	PromptText     string
}

func (s *Session) ConsumeInputByte(clientID uint64, b byte) serverInputEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil {
		return serverInputEvent{}
	}
	if client.Prompt != nil {
		return consumePromptByteLocked(client, b)
	}
	return consumeInputByteLocked(client, b)
}

func consumeInputByteLocked(client *ClientState, b byte) serverInputEvent {
	switch client.InputState {
	case serverInputPrefix:
		if b == 0x1b {
			client.InputState = serverInputPrefixESC
			return serverInputEvent{}
		}
		client.InputState = serverInputNormal
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
		case '[':
			return serverInputEvent{Command: serverCommandEnterHistory}
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
			client.InputState = serverInputPrefixCSI
			return serverInputEvent{}
		}
		client.InputState = serverInputNormal
	case serverInputPrefixCSI:
		client.InputState = serverInputNormal
		if b == 'A' || b == 'B' || b == 'C' || b == 'D' {
			return serverInputEvent{Command: serverCommandFocusDirection, Direction: b}
		}
	default:
		if b == 0x02 {
			client.InputState = serverInputPrefix
			return serverInputEvent{}
		}
		return serverInputEvent{Command: serverCommandLiteral, Byte: b}
	}
	return serverInputEvent{}
}

func consumePromptByteLocked(client *ClientState, b byte) serverInputEvent {
	prompt := client.Prompt
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
		client.Prompt = nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
			}
		}
	}
	return index, events, terminated
}

func (s *Session) InputIsNormal(clientID uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	return client != nil && client.Prompt == nil && client.InputState == serverInputNormal
}

func translateApplicationCursor(data []byte, enabled bool) ([]byte, int, bool) {
	if !enabled || len(data) < 3 || data[0] != 0x1b || data[1] != '[' || data[2] < 'A' || data[2] > 'D' {
		return nil, 0, false
	}
	return []byte{0x1b, 'O', data[2]}, 3, true
}

func (s *Session) RelativeWindowID(clientID uint64, delta int) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	if client == nil || !client.HasLastWindow || s.Windows[client.LastWindowID] == nil {
		return 0, false
	}
	return client.LastWindowID, true
}

func (s *Session) WindowIDByIndex(index int) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
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
	cx, cy := current.Rect.X*2+current.Rect.Width, current.Rect.Y*2+current.Rect.Height
	type candidate struct {
		paneID uint64
		score  int
	}
	var candidates []candidate
	for _, placement := range placements {
		if placement.PaneID == current.PaneID {
			continue
		}
		x, y := placement.Rect.X*2+placement.Rect.Width, placement.Rect.Y*2+placement.Rect.Height
		dx, dy := x-cx, y-cy
		primary, secondary := 0, 0
		switch direction {
		case 'A':
			if dy >= 0 {
				continue
			}
			primary, secondary = -dy, serverAbs(dx)
		case 'B':
			if dy <= 0 {
				continue
			}
			primary, secondary = dy, serverAbs(dx)
		case 'C':
			if dx <= 0 {
				continue
			}
			primary, secondary = dx, serverAbs(dy)
		case 'D':
			if dx >= 0 {
				continue
			}
			primary, secondary = -dx, serverAbs(dy)
		default:
			continue
		}
		candidates = append(candidates, candidate{placement.PaneID, primary*10000 + secondary})
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].score < candidates[j].score })
		client.FocusedPaneID = candidates[0].paneID
	}
	return cloneWindow(window), cloneClientState(client), nil
}

func serverAbs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
