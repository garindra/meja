package server

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

const frontendWheelReportsPerStep = 3

func (s *Session) coalesceFrontendWheelBursts(c *ClientInstance, events []frontendInputEvent) []frontendInputEvent {
	result := make([]frontendInputEvent, 0, len(events))
	for index := 0; index < len(events); {
		paneID, owned := s.mejaOwnedWheelTarget(c, events[index])
		if !owned {
			result = append(result, events[index])
			index++
			continue
		}
		revision := events[index].LayoutRevision
		delta := 0
		end := index
		for end < len(events) {
			candidatePaneID, candidateOwned := s.mejaOwnedWheelTarget(c, events[end])
			if !candidateOwned || candidatePaneID != paneID || events[end].LayoutRevision != revision {
				break
			}
			switch events[end].Pointer.Action {
			case frontendPointerWheelUp:
				delta--
			case frontendPointerWheelDown:
				delta++
			}
			end++
		}
		magnitude := delta
		if magnitude < 0 {
			magnitude = -magnitude
		}
		steps := (magnitude + frontendWheelReportsPerStep - 1) / frontendWheelReportsPerStep
		for range steps {
			event := events[index]
			if delta < 0 {
				event.Pointer.Action = frontendPointerWheelUp
			} else {
				event.Pointer.Action = frontendPointerWheelDown
			}
			result = append(result, event)
		}
		index = end
	}
	return result
}

func (s *Session) mejaOwnedWheelTarget(c *ClientInstance, event frontendInputEvent) (uint64, bool) {
	if event.Kind != frontendEventPointer || !isFrontendWheelAction(event.Pointer.Action) {
		return 0, false
	}
	paneID, _, found := hitTestFrontendLayout(c.layouts[event.LayoutRevision], event.Pointer.X, event.Pointer.Y)
	if !found {
		return 0, false
	}
	pane := s.Pane(paneID)
	return paneID, pane != nil && (pane.isHistoryMode() || pane.InputMode().mouseTracking == MouseTrackingNone)
}

func isFrontendWheelAction(action frontendPointerAction) bool {
	return action == frontendPointerWheelUp || action == frontendPointerWheelDown ||
		action == frontendPointerWheelLeft || action == frontendPointerWheelRight
}

func (s *Session) handleFrontendInputEvent(c *ClientInstance, event frontendInputEvent) (bool, error) {
	switch event.Kind {
	case frontendEventKey:
		return s.handleFrontendKey(c, event.Key)
	case frontendEventPasteStart:
		if pane, _ := s.ActivePane(clientID0); pane != nil {
			c.pasteCapture = frontendPaneCapture{paneID: pane.ID, active: true}
		}
	case frontendEventPaste:
		capture := c.pasteCapture
		c.pasteCapture = frontendPaneCapture{}
		if !capture.active {
			return false, nil
		}
		if event.PasteDiscarded {
			return false, nil
		}
		pane := s.Pane(capture.paneID)
		if pane == nil {
			return false, nil
		}
		data := event.Paste
		if pane.InputMode().bracketedPaste {
			data = append([]byte("\x1b[200~"), data...)
			data = append(data, []byte("\x1b[201~")...)
		}
		return false, pane.sendInput(data)
	case frontendEventFocus:
		if !event.Focused {
			clear(c.heldKeys)
			if err := s.cancelFrontendPointerCapture(c); err != nil {
				return false, err
			}
		}
		pane, _ := s.ActivePane(clientID0)
		if pane != nil && pane.InputMode().focusReporting {
			if event.Focused {
				return false, pane.sendInput([]byte("\x1b[I"))
			}
			return false, pane.sendInput([]byte("\x1b[O"))
		}
	case frontendEventPointer:
		return false, s.handleFrontendPointer(c, event.LayoutRevision, event.Pointer)
	}
	return false, nil
}

func (s *Session) handleFrontendKey(c *ClientInstance, key frontendKeyEvent) (bool, error) {
	if key.Action != frontendKeyRelease && c.pointerCapture.mejaSelection && c.pointerCapture.autoSelection {
		if err := s.cancelFrontendPointerCapture(c); err != nil {
			return false, err
		}
	}
	if c.heldKeys == nil {
		c.heldKeys = make(map[frontendHeldKey]uint64)
	}
	held := frontendHeldKey{Code: key.Code, Rune: key.Rune, Modifiers: key.Modifiers}
	pane, _ := s.ActivePane(clientID0)
	if key.Action == frontendKeyPress && key.HasEventType && pane != nil {
		c.heldKeys[held] = pane.ID
	} else if key.Action == frontendKeyRepeat || key.Action == frontendKeyRelease {
		if paneID, ok := c.heldKeys[held]; ok {
			pane = s.Pane(paneID)
		}
		if key.Action == frontendKeyRelease {
			delete(c.heldKeys, held)
		}
	}

	commandBytes := encodeLegacyKey(key, paneTerminalMetadata{})
	commandPath := s.ActivePrompt(clientID0) != nil || !s.InputIsNormal(clientID0) ||
		(pane != nil && pane.isHistoryMode()) || isMejaPrefixKey(key)
	if commandPath {
		if key.Action == frontendKeyRelease || len(commandBytes) == 0 {
			return false, nil
		}
		return s.handleLegacyInputBytes(c, commandBytes)
	}
	if pane == nil {
		return false, nil
	}
	data := encodeKeyForPane(key, pane.InputMode())
	if len(data) == 0 {
		return false, nil
	}
	if err := pane.sendInput(data); err != nil {
		return false, fmt.Errorf("write frontend key to pane: %w", err)
	}
	return false, nil
}

func (s *Session) cancelFrontendPointerCapture(c *ClientInstance) error {
	if c == nil {
		return nil
	}
	capture := c.pointerCapture
	c.pointerCapture = frontendPaneCapture{}
	if !capture.active || !capture.mejaSelection || !capture.selecting {
		return nil
	}
	pane := s.Pane(capture.paneID)
	if pane == nil {
		return nil
	}
	return pane.cancelHistorySelection()
}

func isMejaPrefixKey(key frontendKeyEvent) bool {
	return key.Action != frontendKeyRelease && key.Code == frontendKeyRune &&
		(key.Rune == 'b' || key.Rune == 'B') && key.Modifiers&frontendModifierControl != 0
}

func encodeKeyForPane(key frontendKeyEvent, mode paneTerminalMetadata) []byte {
	if mode.kittyFlags != 0 {
		return encodeKittyKey(key, mode.kittyFlags)
	}
	return encodeLegacyKey(key, mode)
}

func encodeLegacyKey(key frontendKeyEvent, mode paneTerminalMetadata) []byte {
	if key.Action == frontendKeyRelease {
		return nil
	}
	if len(key.Raw) > 0 {
		if translated, consumed, ok := translateApplicationCursor(key.Raw, mode.applicationCursorKeys); ok && consumed == len(key.Raw) {
			return translated
		}
		return append([]byte(nil), key.Raw...)
	}
	if key.Modifiers != 0 {
		if data := encodeModifiedLegacyFunctionalKey(key); len(data) > 0 {
			return data
		}
	}
	var data []byte
	switch key.Code {
	case frontendKeyRune:
		r := key.Rune
		if key.Modifiers&frontendModifierShift != 0 && unicode.IsLetter(r) {
			r = unicode.ToUpper(r)
		}
		if key.Modifiers&frontendModifierControl != 0 {
			switch {
			case r >= 'a' && r <= 'z':
				data = []byte{byte(r-'a') + 1}
			case r >= 'A' && r <= 'Z':
				data = []byte{byte(r-'A') + 1}
			case r == ' ':
				data = []byte{0}
			}
		}
		if data == nil && utf8.ValidRune(r) {
			data = []byte(string(r))
		}
	case frontendKeyEscape:
		data = []byte{0x1b}
	case frontendKeyEnter:
		data = []byte{'\r'}
	case frontendKeyTab:
		data = []byte{'\t'}
	case frontendKeyBackspace:
		data = []byte{0x7f}
	case frontendKeyUp:
		data = cursorSequence('A', mode.applicationCursorKeys)
	case frontendKeyDown:
		data = cursorSequence('B', mode.applicationCursorKeys)
	case frontendKeyRight:
		data = cursorSequence('C', mode.applicationCursorKeys)
	case frontendKeyLeft:
		data = cursorSequence('D', mode.applicationCursorKeys)
	case frontendKeyInsert:
		data = []byte("\x1b[2~")
	case frontendKeyDelete:
		data = []byte("\x1b[3~")
	case frontendKeyPageUp:
		data = []byte("\x1b[5~")
	case frontendKeyPageDown:
		data = []byte("\x1b[6~")
	case frontendKeyHome:
		if mode.applicationCursorKeys {
			data = []byte("\x1bOH")
		} else {
			data = []byte("\x1b[H")
		}
	case frontendKeyEnd:
		if mode.applicationCursorKeys {
			data = []byte("\x1bOF")
		} else {
			data = []byte("\x1b[F")
		}
	case frontendKeyF1:
		data = []byte("\x1bOP")
	case frontendKeyF2:
		data = []byte("\x1bOQ")
	case frontendKeyF3:
		data = []byte("\x1bOR")
	case frontendKeyF4:
		data = []byte("\x1bOS")
	case frontendKeyF5:
		data = []byte("\x1b[15~")
	case frontendKeyF6:
		data = []byte("\x1b[17~")
	case frontendKeyF7:
		data = []byte("\x1b[18~")
	case frontendKeyF8:
		data = []byte("\x1b[19~")
	case frontendKeyF9:
		data = []byte("\x1b[20~")
	case frontendKeyF10:
		data = []byte("\x1b[21~")
	case frontendKeyF11:
		data = []byte("\x1b[23~")
	case frontendKeyF12:
		data = []byte("\x1b[24~")
	}
	if len(data) > 0 && key.Modifiers&frontendModifierAlt != 0 {
		data = append([]byte{0x1b}, data...)
	}
	return data
}

func encodeModifiedLegacyFunctionalKey(key frontendKeyEvent) []byte {
	modifier := 1
	if key.Modifiers&frontendModifierShift != 0 {
		modifier += 1
	}
	if key.Modifiers&frontendModifierAlt != 0 {
		modifier += 2
	}
	if key.Modifiers&frontendModifierControl != 0 {
		modifier += 4
	}
	parameter := strconv.Itoa(modifier)
	switch key.Code {
	case frontendKeyUp:
		return []byte("\x1b[1;" + parameter + "A")
	case frontendKeyDown:
		return []byte("\x1b[1;" + parameter + "B")
	case frontendKeyRight:
		return []byte("\x1b[1;" + parameter + "C")
	case frontendKeyLeft:
		return []byte("\x1b[1;" + parameter + "D")
	case frontendKeyHome:
		return []byte("\x1b[1;" + parameter + "H")
	case frontendKeyEnd:
		return []byte("\x1b[1;" + parameter + "F")
	case frontendKeyInsert:
		return []byte("\x1b[2;" + parameter + "~")
	case frontendKeyDelete:
		return []byte("\x1b[3;" + parameter + "~")
	case frontendKeyPageUp:
		return []byte("\x1b[5;" + parameter + "~")
	case frontendKeyPageDown:
		return []byte("\x1b[6;" + parameter + "~")
	case frontendKeyF1:
		return []byte("\x1b[1;" + parameter + "P")
	case frontendKeyF2:
		return []byte("\x1b[1;" + parameter + "Q")
	case frontendKeyF3:
		return []byte("\x1b[1;" + parameter + "R")
	case frontendKeyF4:
		return []byte("\x1b[1;" + parameter + "S")
	case frontendKeyF5:
		return []byte("\x1b[15;" + parameter + "~")
	case frontendKeyF6:
		return []byte("\x1b[17;" + parameter + "~")
	case frontendKeyF7:
		return []byte("\x1b[18;" + parameter + "~")
	case frontendKeyF8:
		return []byte("\x1b[19;" + parameter + "~")
	case frontendKeyF9:
		return []byte("\x1b[20;" + parameter + "~")
	case frontendKeyF10:
		return []byte("\x1b[21;" + parameter + "~")
	case frontendKeyF11:
		return []byte("\x1b[23;" + parameter + "~")
	case frontendKeyF12:
		return []byte("\x1b[24;" + parameter + "~")
	default:
		return nil
	}
}

func cursorSequence(final byte, application bool) []byte {
	if application {
		return []byte{0x1b, 'O', final}
	}
	return []byte{0x1b, '[', final}
}

func encodeKittyKey(key frontendKeyEvent, flags uint32) []byte {
	if key.Action == frontendKeyRelease && flags&2 == 0 {
		return nil
	}
	action := key.Action
	if flags&2 == 0 || action == 0 {
		action = frontendKeyPress
	}
	if key.Code == frontendKeyRune && key.Modifiers == 0 && action == frontendKeyPress {
		return []byte(string(key.Rune))
	}
	modifier := 1
	if key.Modifiers&frontendModifierShift != 0 {
		modifier += 1
	}
	if key.Modifiers&frontendModifierAlt != 0 {
		modifier += 2
	}
	if key.Modifiers&frontendModifierControl != 0 {
		modifier += 4
	}
	eventType := int(action)
	if eventType == 0 {
		eventType = 1
	}
	modifierField := strconv.Itoa(modifier)
	if flags&2 != 0 {
		modifierField += ":" + strconv.Itoa(eventType)
	}
	if final, parameter, ok := kittyFunctionalSequence(key.Code); ok {
		return []byte("\x1b[" + parameter + ";" + modifierField + string(final))
	}
	codepoint := key.Rune
	switch key.Code {
	case frontendKeyEscape:
		codepoint = 27
	case frontendKeyEnter:
		codepoint = 13
	case frontendKeyTab:
		codepoint = 9
	case frontendKeyBackspace:
		codepoint = 127
	}
	if codepoint == 0 {
		return encodeLegacyKey(key, paneTerminalMetadata{})
	}
	return []byte("\x1b[" + strconv.Itoa(int(codepoint)) + ";" + modifierField + "u")
}

func kittyFunctionalSequence(code frontendKeyCode) (byte, string, bool) {
	switch code {
	case frontendKeyUp:
		return 'A', "1", true
	case frontendKeyDown:
		return 'B', "1", true
	case frontendKeyRight:
		return 'C', "1", true
	case frontendKeyLeft:
		return 'D', "1", true
	case frontendKeyHome:
		return 'H', "1", true
	case frontendKeyEnd:
		return 'F', "1", true
	case frontendKeyInsert:
		return '~', "2", true
	case frontendKeyDelete:
		return '~', "3", true
	case frontendKeyPageUp:
		return '~', "5", true
	case frontendKeyPageDown:
		return '~', "6", true
	case frontendKeyF1:
		return 'P', "1", true
	case frontendKeyF2:
		return 'Q', "1", true
	case frontendKeyF3:
		return 'R', "1", true
	case frontendKeyF4:
		return 'S', "1", true
	case frontendKeyF5:
		return '~', "15", true
	case frontendKeyF6:
		return '~', "17", true
	case frontendKeyF7:
		return '~', "18", true
	case frontendKeyF8:
		return '~', "19", true
	case frontendKeyF9:
		return '~', "20", true
	case frontendKeyF10:
		return '~', "21", true
	case frontendKeyF11:
		return '~', "23", true
	case frontendKeyF12:
		return '~', "24", true
	default:
		return 0, "", false
	}
}

func (s *Session) handleFrontendPointer(c *ClientInstance, revision uint64, pointer frontendPointerEvent) error {
	if pointer.Action == frontendPointerPress && c.pointerCapture.active {
		if err := s.cancelFrontendPointerCapture(c); err != nil {
			return err
		}
	}
	paneID, rect, found := hitTestFrontendLayout(c.layouts[revision], pointer.X, pointer.Y)
	capture := c.pointerCapture
	captured := capture.active
	if pointer.Action == frontendPointerMove || pointer.Action == frontendPointerRelease {
		if captured {
			paneID = capture.paneID
			found = true
			rect = capture.rect
			if placement, ok := panePlacement(c.layouts[revision], paneID); ok {
				rect = placement.Rect
			}
		}
	}
	if !found {
		return nil
	}
	pane := s.Pane(paneID)
	if pane == nil {
		c.pointerCapture = frontendPaneCapture{}
		return nil
	}
	if pointer.Action == frontendPointerPress {
		if !s.IsFocusedPane(clientID0, paneID) {
			if _, _, err := s.FocusPane(clientID0, paneID); err != nil {
				return err
			}
			if err := s.publishWindowLayout(); err != nil {
				return err
			}
		}
		mode := pane.InputMode()
		if pointer.Button == 0 && (pane.isHistoryMode() || mode.mouseTracking == MouseTrackingNone) {
			row, column := pointer.Y-rect.Y, pointer.X-rect.X
			c.pointerCapture = frontendPaneCapture{
				paneID:        paneID,
				active:        true,
				button:        pointer.Button,
				mejaSelection: true,
				autoSelection: !pane.isHistoryMode(),
				anchorRow:     row,
				anchorColumn:  column,
				rect:          rect,
			}
			return nil
		}
		c.pointerCapture = frontendPaneCapture{paneID: paneID, active: true, button: pointer.Button, rect: rect}
	}
	if pointer.Action == frontendPointerRelease {
		defer func() { c.pointerCapture = frontendPaneCapture{} }()
	}
	if captured && capture.mejaSelection {
		if pointer.Action == frontendPointerMove && pointer.Button != capture.button {
			return s.cancelFrontendPointerCapture(c)
		}
		row := min(max(pointer.Y-rect.Y, 0), max(rect.Height-1, 0))
		column := min(max(pointer.X-rect.X, 0), max(rect.Width-1, 0))
		if !capture.selecting {
			if pointer.Action != frontendPointerMove || (row == capture.anchorRow && column == capture.anchorColumn) {
				return nil
			}
			if err := pane.beginHistorySelection(capture.anchorRow, capture.anchorColumn, capture.autoSelection); err != nil {
				c.pointerCapture = frontendPaneCapture{}
				return err
			}
			capture.selecting = true
			c.pointerCapture = capture
		}
		switch pointer.Action {
		case frontendPointerMove:
			return pane.updateHistorySelection(row, column)
		case frontendPointerRelease:
			data, err := pane.finishHistorySelection()
			if err != nil || len(data) == 0 {
				return err
			}
			return c.writeFrontendTerminal(osc52ClipboardWrite(data))
		default:
			return nil
		}
	}
	if pane.isHistoryMode() {
		switch pointer.Action {
		case frontendPointerWheelUp:
			_, err := pane.handleHistoryInput([]byte("\x1b[A"))
			return err
		case frontendPointerWheelDown:
			_, err := pane.handleHistoryInput([]byte("\x1b[B"))
			return err
		default:
			return nil
		}
	}
	mode := pane.InputMode()
	if mode.mouseTracking == MouseTrackingNone {
		key := frontendKeyEvent{Action: frontendKeyPress}
		switch pointer.Action {
		case frontendPointerWheelUp:
			key.Code = frontendKeyUp
		case frontendPointerWheelDown:
			key.Code = frontendKeyDown
		default:
			return nil
		}
		return pane.sendInput(encodeKeyForPane(key, mode))
	}
	if !mouseModeAccepts(mode.mouseTracking, pointer.Action, captured || c.pointerCapture.active) {
		return nil
	}
	pointer.X -= rect.X
	pointer.Y -= rect.Y
	if captured {
		pointer.X = min(max(pointer.X, 0), max(rect.Width-1, 0))
		pointer.Y = min(max(pointer.Y, 0), max(rect.Height-1, 0))
	} else if pointer.X < 0 || pointer.Y < 0 || pointer.X >= rect.Width || pointer.Y >= rect.Height {
		return nil
	}
	return pane.sendInput(encodeMouseForPane(pointer, mode.mouseEncoding))
}

func osc52ClipboardWrite(data []byte) []byte {
	encodedLen := base64.StdEncoding.EncodedLen(len(data))
	out := make([]byte, 0, len("\x1b]52;c;\x1b\\")+encodedLen)
	out = append(out, "\x1b]52;c;"...)
	out = base64.StdEncoding.AppendEncode(out, data)
	out = append(out, '\x1b', '\\')
	return out
}

func hitTestFrontendLayout(layout protocol.WindowLayout, x, y int) (uint64, protocol.Rect, bool) {
	for _, placement := range layout.Panes {
		r := placement.Rect
		if x >= r.X && y >= r.Y && x < r.X+r.Width && y < r.Y+r.Height {
			return placement.PaneID, r, true
		}
	}
	return 0, protocol.Rect{}, false
}

func panePlacement(layout protocol.WindowLayout, paneID uint64) (protocol.PanePlacement, bool) {
	for _, placement := range layout.Panes {
		if placement.PaneID == paneID {
			return placement, true
		}
	}
	return protocol.PanePlacement{}, false
}

func mouseModeAccepts(mode MouseTrackingMode, action frontendPointerAction, captured bool) bool {
	switch action {
	case frontendPointerPress, frontendPointerWheelUp, frontendPointerWheelDown,
		frontendPointerWheelLeft, frontendPointerWheelRight:
		return true
	case frontendPointerRelease:
		return mode != MouseTrackingX10
	case frontendPointerMove:
		return mode == MouseTrackingMotion || (mode == MouseTrackingDrag && captured)
	default:
		return false
	}
}

func encodeMouseForPane(pointer frontendPointerEvent, encoding MouseEncoding) []byte {
	code := int(pointer.Button)
	switch pointer.Action {
	case frontendPointerRelease:
		if encoding == MouseEncodingClassic {
			code = 3
		}
	case frontendPointerMove:
		code |= 32
	case frontendPointerWheelUp:
		code = 64
	case frontendPointerWheelDown:
		code = 65
	case frontendPointerWheelLeft:
		code = 66
	case frontendPointerWheelRight:
		code = 67
	}
	if pointer.Modifiers&frontendModifierShift != 0 {
		code |= 4
	}
	if pointer.Modifiers&frontendModifierAlt != 0 {
		code |= 8
	}
	if pointer.Modifiers&frontendModifierControl != 0 {
		code |= 16
	}
	if encoding == MouseEncodingSGR {
		final := 'M'
		if pointer.Action == frontendPointerRelease {
			final = 'm'
		}
		return []byte(fmt.Sprintf("\x1b[<%d;%d;%d%c", code, pointer.X+1, pointer.Y+1, final))
	}
	x := min(max(pointer.X, 0), 222) + 33
	y := min(max(pointer.Y, 0), 222) + 33
	return []byte{0x1b, '[', 'M', byte(code + 32), byte(x), byte(y)}
}
