package server

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

type frontendEventKind uint8

const (
	frontendEventKey frontendEventKind = iota + 1
	frontendEventPasteStart
	frontendEventPaste
	frontendEventFocus
	frontendEventPointer
)

type frontendKeyCode uint16

const (
	frontendKeyRune frontendKeyCode = iota + 1
	frontendKeyEscape
	frontendKeyEnter
	frontendKeyTab
	frontendKeyBackspace
	frontendKeyUp
	frontendKeyDown
	frontendKeyRight
	frontendKeyLeft
	frontendKeyInsert
	frontendKeyDelete
	frontendKeyPageUp
	frontendKeyPageDown
	frontendKeyHome
	frontendKeyEnd
	frontendKeyF1
	frontendKeyF2
	frontendKeyF3
	frontendKeyF4
	frontendKeyF5
	frontendKeyF6
	frontendKeyF7
	frontendKeyF8
	frontendKeyF9
	frontendKeyF10
	frontendKeyF11
	frontendKeyF12
)

type frontendModifiers uint8

const (
	frontendModifierShift frontendModifiers = 1 << iota
	frontendModifierAlt
	frontendModifierControl
)

type frontendKeyAction uint8

const (
	frontendKeyPress frontendKeyAction = iota + 1
	frontendKeyRepeat
	frontendKeyRelease
)

type frontendKeyEvent struct {
	Code         frontendKeyCode
	Rune         rune
	Modifiers    frontendModifiers
	Action       frontendKeyAction
	Raw          []byte
	HasEventType bool
}

type frontendHeldKey struct {
	Code      frontendKeyCode
	Rune      rune
	Modifiers frontendModifiers
}

type frontendPointerAction uint8

const (
	frontendPointerPress frontendPointerAction = iota + 1
	frontendPointerRelease
	frontendPointerMove
	frontendPointerWheelUp
	frontendPointerWheelDown
	frontendPointerWheelLeft
	frontendPointerWheelRight
)

type frontendPointerEvent struct {
	Action    frontendPointerAction
	Button    uint8
	Modifiers frontendModifiers
	X         int
	Y         int
}

type frontendInputEvent struct {
	Kind           frontendEventKind
	LayoutRevision protocol.ClientLayoutRevision
	Key            frontendKeyEvent
	Paste          []byte
	PasteDiscarded bool
	Focused        bool
	Pointer        frontendPointerEvent
}

type frontendParserState uint8

const (
	frontendParserGround frontendParserState = iota
	frontendParserEscape
	frontendParserCSI
	frontendParserCSIDiscard
	frontendParserSS3
	frontendParserUTF8
	frontendParserPaste
)

type frontendInputParser struct {
	state         frontendParserState
	pending       []byte
	revision      protocol.ClientLayoutRevision
	paste         []byte
	pasteOverflow bool
}

const maxFrontendSequenceBytes = 512
const maxFrontendPasteBytes = 8 << 20

var bracketedPasteEnd = []byte("\x1b[201~")

func (p *frontendInputParser) Feed(layoutRevision protocol.ClientLayoutRevision, data []byte) []frontendInputEvent {
	events := make([]frontendInputEvent, 0, min(len(data), 64))
	for _, b := range data {
		// Outside bracketed paste, Escape always starts a new input
		// transaction. In particular, it abandons an incomplete or malformed
		// sequence instead of allowing that sequence to consume later input.
		if b == 0x1b && p.state != frontendParserPaste {
			p.startEscape(layoutRevision)
			continue
		}
		switch p.state {
		case frontendParserPaste:
			p.paste = append(p.paste, b)
			if !p.pasteOverflow && len(p.paste) > maxFrontendPasteBytes {
				p.pasteOverflow = true
				p.paste = append([]byte(nil), p.paste[len(p.paste)-len(bracketedPasteEnd):]...)
			} else if p.pasteOverflow && len(p.paste) > len(bracketedPasteEnd) {
				p.paste = append(p.paste[:0], p.paste[len(p.paste)-len(bracketedPasteEnd):]...)
			}
			if bytes.HasSuffix(p.paste, bracketedPasteEnd) {
				payload := append([]byte(nil), p.paste[:len(p.paste)-len(bracketedPasteEnd)]...)
				events = append(events, frontendInputEvent{Kind: frontendEventPaste, LayoutRevision: p.revision, Paste: payload, PasteDiscarded: p.pasteOverflow})
				p.reset()
			}
			continue
		case frontendParserGround:
			p.revision = layoutRevision
			switch {
			case b < utf8.RuneSelf:
				events = append(events, keyInputEvent(layoutRevision, decodeGroundByte(b)))
			default:
				p.state = frontendParserUTF8
				p.pending = append(p.pending[:0], b)
			}
		case frontendParserEscape:
			p.pending = append(p.pending, b)
			switch b {
			case '[':
				p.state = frontendParserCSI
			case 'O':
				p.state = frontendParserSS3
			default:
				events = append(events, keyInputEvent(p.revision, decodeLegacySequence(p.pending)))
				p.reset()
			}
		case frontendParserSS3:
			p.pending = append(p.pending, b)
			key := decodeLegacySequence(p.pending)
			if key.Code != 0 {
				events = append(events, keyInputEvent(p.revision, key))
			}
			p.reset()
		case frontendParserCSI:
			p.pending = append(p.pending, b)
			if len(p.pending) > maxFrontendSequenceBytes {
				p.pending = p.pending[:0]
				if isFrontendSequenceFinal(b) {
					p.reset()
				} else {
					p.state = frontendParserCSIDiscard
				}
				continue
			}
			if isFrontendSequenceFinal(b) {
				if string(p.pending) == "\x1b[200~" {
					events = append(events, frontendInputEvent{Kind: frontendEventPasteStart, LayoutRevision: p.revision})
					p.state = frontendParserPaste
					p.paste = p.paste[:0]
					p.pending = p.pending[:0]
					continue
				}
				events = append(events, decodeCSIInput(p.revision, p.pending)...)
				p.reset()
			}
		case frontendParserCSIDiscard:
			if isFrontendSequenceFinal(b) {
				p.reset()
			}
		case frontendParserUTF8:
			p.pending = append(p.pending, b)
			if !utf8.FullRune(p.pending) {
				if len(p.pending) >= utf8.UTFMax {
					p.reset()
				}
				continue
			}
			r, size := utf8.DecodeRune(p.pending)
			if r != utf8.RuneError || size > 1 {
				events = append(events, keyInputEvent(p.revision, frontendKeyEvent{Code: frontendKeyRune, Rune: r, Action: frontendKeyPress, Raw: append([]byte(nil), p.pending...)}))
			}
			p.reset()
		}
	}
	return events
}

func (p *frontendInputParser) startEscape(layoutRevision protocol.ClientLayoutRevision) {
	p.state = frontendParserEscape
	p.revision = layoutRevision
	p.pending = append(p.pending[:0], 0x1b)
	p.paste = p.paste[:0]
	p.pasteOverflow = false
}

func isFrontendSequenceFinal(b byte) bool {
	return b >= 0x40 && b <= 0x7e
}

func (p *frontendInputParser) reset() {
	p.state = frontendParserGround
	p.pending = p.pending[:0]
	p.paste = p.paste[:0]
	p.pasteOverflow = false
}

func (p *frontendInputParser) hasLoneEscape() bool {
	return p.state == frontendParserEscape && len(p.pending) == 1
}

func (p *frontendInputParser) flushLoneEscape() (frontendInputEvent, bool) {
	if !p.hasLoneEscape() {
		return frontendInputEvent{}, false
	}
	event := keyInputEvent(p.revision, frontendKeyEvent{Code: frontendKeyEscape, Action: frontendKeyPress, Raw: []byte{0x1b}})
	p.reset()
	return event, true
}

func keyInputEvent(revision protocol.ClientLayoutRevision, key frontendKeyEvent) frontendInputEvent {
	return frontendInputEvent{Kind: frontendEventKey, LayoutRevision: revision, Key: key}
}

func decodeGroundByte(b byte) frontendKeyEvent {
	key := frontendKeyEvent{Code: frontendKeyRune, Rune: rune(b), Action: frontendKeyPress, Raw: []byte{b}}
	switch b {
	case 0x1b:
		key.Code, key.Rune = frontendKeyEscape, 0
	case '\r', '\n':
		key.Code, key.Rune = frontendKeyEnter, 0
	case '\t':
		key.Code, key.Rune = frontendKeyTab, 0
	case 0x7f:
		key.Code, key.Rune = frontendKeyBackspace, 0
	default:
		if b >= 1 && b <= 26 {
			key.Rune = rune('a' + b - 1)
			key.Modifiers = frontendModifierControl
		}
	}
	return key
}

func decodeLegacySequence(raw []byte) frontendKeyEvent {
	key := frontendKeyEvent{Action: frontendKeyPress, Raw: append([]byte(nil), raw...)}
	if len(raw) == 2 && raw[0] == 0x1b {
		key = decodeGroundByte(raw[1])
		key.Modifiers |= frontendModifierAlt
		key.Raw = append([]byte(nil), raw...)
		return key
	}
	if len(raw) == 3 && raw[0] == 0x1b && raw[1] == 'O' {
		switch raw[2] {
		case 'A':
			key.Code = frontendKeyUp
		case 'B':
			key.Code = frontendKeyDown
		case 'C':
			key.Code = frontendKeyRight
		case 'D':
			key.Code = frontendKeyLeft
		case 'H':
			key.Code = frontendKeyHome
		case 'F':
			key.Code = frontendKeyEnd
		case 'P':
			key.Code = frontendKeyF1
		case 'Q':
			key.Code = frontendKeyF2
		case 'R':
			key.Code = frontendKeyF3
		case 'S':
			key.Code = frontendKeyF4
		}
	}
	return key
}

func decodeCSIInput(revision protocol.ClientLayoutRevision, raw []byte) []frontendInputEvent {
	if len(raw) < 3 {
		return nil
	}
	body := string(raw[2 : len(raw)-1])
	final := raw[len(raw)-1]
	if body == "" && (final == 'I' || final == 'O') {
		return []frontendInputEvent{{Kind: frontendEventFocus, LayoutRevision: revision, Focused: final == 'I'}}
	}
	if strings.HasPrefix(body, "<") && (final == 'M' || final == 'm') {
		if pointer, ok := decodeSGRMouse(body[1:], final); ok {
			return []frontendInputEvent{{Kind: frontendEventPointer, LayoutRevision: revision, Pointer: pointer}}
		}
		return nil
	}
	if final == 'u' {
		if key, ok := decodeKittyKey(body); ok {
			return []frontendInputEvent{keyInputEvent(revision, key)}
		}
		return nil
	}
	key := decodeLegacyCSI(body, final, raw)
	if key.Code == 0 {
		return nil
	}
	return []frontendInputEvent{keyInputEvent(revision, key)}
}

func decodeLegacyCSI(body string, final byte, raw []byte) frontendKeyEvent {
	key := frontendKeyEvent{Action: frontendKeyPress, Raw: append([]byte(nil), raw...)}
	switch final {
	case 'A':
		key.Code = frontendKeyUp
	case 'B':
		key.Code = frontendKeyDown
	case 'C':
		key.Code = frontendKeyRight
	case 'D':
		key.Code = frontendKeyLeft
	case 'H':
		key.Code = frontendKeyHome
	case 'F':
		key.Code = frontendKeyEnd
	case '~':
		first, _, _ := strings.Cut(body, ";")
		switch first {
		case "2":
			key.Code = frontendKeyInsert
		case "3":
			key.Code = frontendKeyDelete
		case "5":
			key.Code = frontendKeyPageUp
		case "6":
			key.Code = frontendKeyPageDown
		case "1", "7":
			key.Code = frontendKeyHome
		case "4", "8":
			key.Code = frontendKeyEnd
		case "15":
			key.Code = frontendKeyF5
		case "17":
			key.Code = frontendKeyF6
		case "18":
			key.Code = frontendKeyF7
		case "19":
			key.Code = frontendKeyF8
		case "20":
			key.Code = frontendKeyF9
		case "21":
			key.Code = frontendKeyF10
		case "23":
			key.Code = frontendKeyF11
		case "24":
			key.Code = frontendKeyF12
		}
	}
	parts := strings.Split(body, ";")
	if len(parts) > 1 {
		modifierParts := strings.Split(parts[len(parts)-1], ":")
		if encoded, err := strconv.Atoi(modifierParts[0]); err == nil {
			key.Modifiers = decodeXTermModifiers(encoded)
		}
		if len(modifierParts) == 2 {
			switch modifierParts[1] {
			case "1":
				key.Action = frontendKeyPress
				key.HasEventType = true
			case "2":
				key.Action = frontendKeyRepeat
				key.HasEventType = true
			case "3":
				key.Action = frontendKeyRelease
				key.HasEventType = true
			}
		}
	}
	if key.HasEventType {
		// Kitty functional keys retain their traditional CSI final byte and
		// put the event type after the modifier. Route the decoded key rather
		// than leaking the outer terminal's Kitty packet into a legacy pane.
		key.Raw = nil
	}
	return key
}

func decodeKittyKey(body string) (frontendKeyEvent, bool) {
	fields := strings.Split(body, ";")
	if len(fields) == 0 {
		return frontendKeyEvent{}, false
	}
	codeText := strings.Split(fields[0], ":")[0]
	code, err := strconv.Atoi(codeText)
	if err != nil || code < 0 || code > utf8.MaxRune+60000 {
		return frontendKeyEvent{}, false
	}
	key := frontendKeyEvent{Code: frontendKeyRune, Rune: rune(code), Action: frontendKeyPress, HasEventType: true}
	if len(fields) > 1 {
		modifierParts := strings.Split(fields[1], ":")
		if encoded, err := strconv.Atoi(modifierParts[0]); err == nil {
			key.Modifiers = decodeXTermModifiers(encoded)
		}
		if len(modifierParts) > 1 {
			key.HasEventType = true
			switch modifierParts[1] {
			case "2":
				key.Action = frontendKeyRepeat
			case "3":
				key.Action = frontendKeyRelease
			}
		}
	}
	key.Code, key.Rune = kittyFunctionalKey(code)
	return key, true
}

func kittyFunctionalKey(code int) (frontendKeyCode, rune) {
	switch code {
	case 27:
		return frontendKeyEscape, 0
	case 13:
		return frontendKeyEnter, 0
	case 9:
		return frontendKeyTab, 0
	case 127:
		return frontendKeyBackspace, 0
	case 57344:
		return frontendKeyEscape, 0
	case 57345:
		return frontendKeyEnter, 0
	case 57346:
		return frontendKeyTab, 0
	case 57347:
		return frontendKeyBackspace, 0
	case 57348:
		return frontendKeyInsert, 0
	case 57349:
		return frontendKeyDelete, 0
	case 57350:
		return frontendKeyLeft, 0
	case 57351:
		return frontendKeyRight, 0
	case 57352:
		return frontendKeyUp, 0
	case 57353:
		return frontendKeyDown, 0
	case 57354:
		return frontendKeyPageUp, 0
	case 57355:
		return frontendKeyPageDown, 0
	case 57356:
		return frontendKeyHome, 0
	case 57357:
		return frontendKeyEnd, 0
	case 57364:
		return frontendKeyF1, 0
	case 57365:
		return frontendKeyF2, 0
	case 57366:
		return frontendKeyF3, 0
	case 57367:
		return frontendKeyF4, 0
	case 57368:
		return frontendKeyF5, 0
	case 57369:
		return frontendKeyF6, 0
	case 57370:
		return frontendKeyF7, 0
	case 57371:
		return frontendKeyF8, 0
	case 57372:
		return frontendKeyF9, 0
	case 57373:
		return frontendKeyF10, 0
	case 57374:
		return frontendKeyF11, 0
	case 57375:
		return frontendKeyF12, 0
	default:
		if code >= 57344 {
			return 0, 0
		}
		return frontendKeyRune, rune(code)
	}
}

func decodeXTermModifiers(encoded int) frontendModifiers {
	bits := max(encoded-1, 0)
	var modifiers frontendModifiers
	if bits&1 != 0 {
		modifiers |= frontendModifierShift
	}
	if bits&2 != 0 {
		modifiers |= frontendModifierAlt
	}
	if bits&4 != 0 {
		modifiers |= frontendModifierControl
	}
	return modifiers
}

func decodeSGRMouse(body string, final byte) (frontendPointerEvent, bool) {
	parts := strings.Split(body, ";")
	if len(parts) != 3 {
		return frontendPointerEvent{}, false
	}
	values := [3]int{}
	for index := range parts {
		value, err := strconv.Atoi(parts[index])
		if err != nil {
			return frontendPointerEvent{}, false
		}
		values[index] = value
	}
	buttonCode := values[0]
	pointer := frontendPointerEvent{Button: uint8(buttonCode & 3), X: values[1] - 1, Y: values[2] - 1}
	if buttonCode&4 != 0 {
		pointer.Modifiers |= frontendModifierShift
	}
	if buttonCode&8 != 0 {
		pointer.Modifiers |= frontendModifierAlt
	}
	if buttonCode&16 != 0 {
		pointer.Modifiers |= frontendModifierControl
	}
	switch {
	case buttonCode&64 != 0:
		switch buttonCode & 3 {
		case 0:
			pointer.Action = frontendPointerWheelUp
		case 1:
			pointer.Action = frontendPointerWheelDown
		case 2:
			pointer.Action = frontendPointerWheelLeft
		case 3:
			pointer.Action = frontendPointerWheelRight
		}
	case final == 'm' || buttonCode&3 == 3:
		pointer.Action = frontendPointerRelease
	case buttonCode&32 != 0:
		pointer.Action = frontendPointerMove
	default:
		pointer.Action = frontendPointerPress
	}
	return pointer, pointer.X >= 0 && pointer.Y >= 0
}
