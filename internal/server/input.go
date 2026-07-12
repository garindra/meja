package server

import (
	"fmt"
	"sort"
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
)

type serverInputEvent struct {
	Command   serverInputCommand
	Byte      byte
	Index     int
	Direction byte
}

func (s *Session) ConsumeInputByte(clientID uint64, b byte) serverInputEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.Clients[clientID]
	if client == nil {
		return serverInputEvent{}
	}
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

func (s *Session) InputIsNormal(clientID uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client := s.Clients[clientID]
	return client != nil && client.InputState == serverInputNormal
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
	ids := s.windowIDsLocked()
	if index < 0 || index >= len(ids) {
		return 0, false
	}
	return ids[index], true
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
