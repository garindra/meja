package client

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

type clientPromptDraft struct {
	descriptor protocol.ClientStatusPromptState
	text       []rune
	cursor     int
	textBytes  int
	resolved   bool
	decoder    promptInputDecoder
}

func newClientPromptDraft(descriptor protocol.ClientStatusPromptState) *clientPromptDraft {
	text := []rune(descriptor.Initial)
	return &clientPromptDraft{
		descriptor: descriptor,
		text:       text,
		cursor:     len(text),
		textBytes:  len(descriptor.Initial),
	}
}

type promptInputOutcome struct {
	handled bool
	changed bool
	result  *protocol.FrontendPromptResult
}

func (p *clientPromptDraft) consume(data []byte, sourceIdle bool) promptInputOutcome {
	if p == nil {
		return promptInputOutcome{}
	}
	outcome := promptInputOutcome{handled: true}
	if p.resolved {
		return outcome
	}
	for _, event := range p.decoder.Feed(data, sourceIdle) {
		if p.descriptor.Mode == protocol.ClientStatusPromptConfirm {
			switch {
			case event.kind == promptKeyRune && (event.rune == 'y' || event.rune == 'Y'):
				outcome.result = p.resolve(true, "y")
			case event.kind == promptKeyRune && (event.rune == 'n' || event.rune == 'N'):
				outcome.result = p.resolve(false, "")
			case event.kind == promptKeyEnter || event.kind == promptKeyEscape ||
				(event.kind == promptKeyRune && event.control && event.rune == 'c'):
				outcome.result = p.resolve(false, "")
			}
			if outcome.result != nil {
				outcome.changed = true
				return outcome
			}
			continue
		}

		switch event.kind {
		case promptKeyRune:
			switch {
			case event.control && event.rune == 'c':
				outcome.result = p.resolve(false, "")
			case event.control && event.rune == 'u':
				if len(p.text) > 0 {
					p.text = p.text[:0]
					p.cursor = 0
					p.textBytes = 0
					outcome.changed = true
				}
			case !event.control && event.rune >= 0x20 && event.rune != 0x7f:
				if p.insertRune(event.rune) {
					outcome.changed = true
				}
			}
		case promptKeyPaste:
			for _, r := range event.text {
				switch r {
				case '\r', '\n', '\t':
					r = ' '
				}
				if r >= 0x20 && r != 0x7f && p.insertRune(r) {
					outcome.changed = true
				}
			}
		case promptKeyBackspace:
			if p.deletePreviousRune() {
				outcome.changed = true
			}
		case promptKeyDelete:
			if p.deleteCurrentRune() {
				outcome.changed = true
			}
		case promptKeyLeft:
			if p.cursor > 0 {
				p.cursor--
				outcome.changed = true
			}
		case promptKeyRight:
			if p.cursor < len(p.text) {
				p.cursor++
				outcome.changed = true
			}
		case promptKeyHome:
			if p.cursor != 0 {
				p.cursor = 0
				outcome.changed = true
			}
		case promptKeyEnd:
			if p.cursor != len(p.text) {
				p.cursor = len(p.text)
				outcome.changed = true
			}
		case promptKeyEnter:
			outcome.result = p.resolve(true, string(p.text))
		case promptKeyEscape:
			outcome.result = p.resolve(false, "")
		}
		if outcome.result != nil {
			outcome.changed = true
			return outcome
		}
	}
	return outcome
}

func (p *clientPromptDraft) resolve(submitted bool, text string) *protocol.FrontendPromptResult {
	p.resolved = true
	return &protocol.FrontendPromptResult{
		PromptID:  p.descriptor.PromptID,
		Submitted: submitted,
		Text:      text,
	}
}

func (p *clientPromptDraft) insertRune(r rune) bool {
	size := utf8.RuneLen(r)
	if size < 0 || uint64(p.textBytes+size) > protocol.MaxStringLen {
		return false
	}
	p.text = append(p.text, 0)
	copy(p.text[p.cursor+1:], p.text[p.cursor:])
	p.text[p.cursor] = r
	p.cursor++
	p.textBytes += size
	return true
}

func (p *clientPromptDraft) deletePreviousRune() bool {
	if p.cursor <= 0 || len(p.text) == 0 {
		return false
	}
	p.cursor--
	p.textBytes -= utf8.RuneLen(p.text[p.cursor])
	copy(p.text[p.cursor:], p.text[p.cursor+1:])
	p.text = p.text[:len(p.text)-1]
	return true
}

func (p *clientPromptDraft) deleteCurrentRune() bool {
	if p.cursor < 0 || p.cursor >= len(p.text) {
		return false
	}
	p.textBytes -= utf8.RuneLen(p.text[p.cursor])
	copy(p.text[p.cursor:], p.text[p.cursor+1:])
	p.text = p.text[:len(p.text)-1]
	return true
}

type promptKeyKind uint8

const (
	promptKeyRune promptKeyKind = iota + 1
	promptKeyEscape
	promptKeyEnter
	promptKeyBackspace
	promptKeyDelete
	promptKeyLeft
	promptKeyRight
	promptKeyHome
	promptKeyEnd
	promptKeyPaste
)

type promptInputEvent struct {
	kind    promptKeyKind
	rune    rune
	control bool
	text    string
}

type promptDecodeState uint8

const (
	promptDecodeGround promptDecodeState = iota
	promptDecodeEscape
	promptDecodeCSI
	promptDecodeCSIDiscard
	promptDecodeSS3
	promptDecodeUTF8
	promptDecodePaste
)

const maxPromptSequenceBytes = 512

var promptPasteEnd = []byte("\x1b[201~")

type promptInputDecoder struct {
	state         promptDecodeState
	pending       []byte
	paste         []byte
	pasteOverflow bool
}

func (d *promptInputDecoder) Feed(data []byte, sourceIdle bool) []promptInputEvent {
	events := make([]promptInputEvent, 0, min(len(data), 32))
	for _, b := range data {
		if b == 0x1b && d.state != promptDecodePaste {
			d.startEscape()
			continue
		}
		switch d.state {
		case promptDecodePaste:
			d.paste = append(d.paste, b)
			if !d.pasteOverflow && uint64(len(d.paste)) > protocol.MaxStringLen+uint64(len(promptPasteEnd)) {
				d.pasteOverflow = true
				d.paste = append([]byte(nil), d.paste[len(d.paste)-len(promptPasteEnd):]...)
			} else if d.pasteOverflow && len(d.paste) > len(promptPasteEnd) {
				d.paste = append(d.paste[:0], d.paste[len(d.paste)-len(promptPasteEnd):]...)
			}
			if bytes.HasSuffix(d.paste, promptPasteEnd) {
				if !d.pasteOverflow {
					payload := d.paste[:len(d.paste)-len(promptPasteEnd)]
					if utf8.Valid(payload) {
						events = append(events, promptInputEvent{kind: promptKeyPaste, text: string(payload)})
					}
				}
				d.reset()
			}
		case promptDecodeGround:
			switch {
			case b < utf8.RuneSelf:
				if event, ok := decodePromptGroundByte(b); ok {
					events = append(events, event)
				}
			default:
				d.state = promptDecodeUTF8
				d.pending = append(d.pending[:0], b)
			}
		case promptDecodeEscape:
			d.pending = append(d.pending, b)
			switch b {
			case '[':
				d.state = promptDecodeCSI
			case 'O':
				d.state = promptDecodeSS3
			default:
				events = append(events, promptInputEvent{kind: promptKeyEscape})
				d.reset()
			}
		case promptDecodeSS3:
			if event, ok := decodePromptSS3(b); ok {
				events = append(events, event)
			}
			d.reset()
		case promptDecodeCSI:
			d.pending = append(d.pending, b)
			if len(d.pending) > maxPromptSequenceBytes {
				d.pending = d.pending[:0]
				if isPromptSequenceFinal(b) {
					d.reset()
				} else {
					d.state = promptDecodeCSIDiscard
				}
				continue
			}
			if !isPromptSequenceFinal(b) {
				continue
			}
			if bytes.Equal(d.pending, []byte("\x1b[200~")) {
				d.state = promptDecodePaste
				d.pending = d.pending[:0]
				d.paste = d.paste[:0]
				d.pasteOverflow = false
				continue
			}
			if event, ok := decodePromptCSI(d.pending); ok {
				events = append(events, event)
			}
			d.reset()
		case promptDecodeCSIDiscard:
			if isPromptSequenceFinal(b) {
				d.reset()
			}
		case promptDecodeUTF8:
			d.pending = append(d.pending, b)
			if !utf8.FullRune(d.pending) && len(d.pending) < utf8.UTFMax {
				continue
			}
			r, size := utf8.DecodeRune(d.pending)
			if r != utf8.RuneError || size > 1 {
				events = append(events, promptInputEvent{kind: promptKeyRune, rune: r})
			}
			d.reset()
		}
	}
	if sourceIdle && d.state == promptDecodeEscape && len(d.pending) == 1 {
		events = append(events, promptInputEvent{kind: promptKeyEscape})
		d.reset()
	}
	return events
}

func (d *promptInputDecoder) startEscape() {
	d.state = promptDecodeEscape
	d.pending = append(d.pending[:0], 0x1b)
	d.paste = d.paste[:0]
	d.pasteOverflow = false
}

func (d *promptInputDecoder) reset() {
	d.state = promptDecodeGround
	d.pending = d.pending[:0]
	d.paste = d.paste[:0]
	d.pasteOverflow = false
}

func isPromptSequenceFinal(b byte) bool {
	return b >= 0x40 && b <= 0x7e
}

func decodePromptGroundByte(b byte) (promptInputEvent, bool) {
	switch b {
	case '\r', '\n':
		return promptInputEvent{kind: promptKeyEnter}, true
	case 0x08, 0x7f:
		return promptInputEvent{kind: promptKeyBackspace}, true
	case 0x03:
		return promptInputEvent{kind: promptKeyRune, rune: 'c', control: true}, true
	case 0x15:
		return promptInputEvent{kind: promptKeyRune, rune: 'u', control: true}, true
	default:
		if b >= 0x20 {
			return promptInputEvent{kind: promptKeyRune, rune: rune(b)}, true
		}
		return promptInputEvent{}, false
	}
}

func decodePromptSS3(final byte) (promptInputEvent, bool) {
	switch final {
	case 'C':
		return promptInputEvent{kind: promptKeyRight}, true
	case 'D':
		return promptInputEvent{kind: promptKeyLeft}, true
	case 'H':
		return promptInputEvent{kind: promptKeyHome}, true
	case 'F':
		return promptInputEvent{kind: promptKeyEnd}, true
	default:
		return promptInputEvent{}, false
	}
}

func decodePromptCSI(raw []byte) (promptInputEvent, bool) {
	if len(raw) < 3 {
		return promptInputEvent{}, false
	}
	body := string(raw[2 : len(raw)-1])
	final := raw[len(raw)-1]
	if body == "" && (final == 'I' || final == 'O') {
		return promptInputEvent{}, false
	}
	if strings.HasPrefix(body, "<") && (final == 'M' || final == 'm') {
		return promptInputEvent{}, false
	}
	if final == 'u' {
		return decodePromptKittyKey(body)
	}
	switch final {
	case 'C':
		return promptInputEvent{kind: promptKeyRight}, true
	case 'D':
		return promptInputEvent{kind: promptKeyLeft}, true
	case 'H':
		return promptInputEvent{kind: promptKeyHome}, true
	case 'F':
		return promptInputEvent{kind: promptKeyEnd}, true
	case '~':
		first, _, _ := strings.Cut(body, ";")
		switch first {
		case "3":
			return promptInputEvent{kind: promptKeyDelete}, true
		case "1", "7":
			return promptInputEvent{kind: promptKeyHome}, true
		case "4", "8":
			return promptInputEvent{kind: promptKeyEnd}, true
		}
	}
	return promptInputEvent{}, false
}

func decodePromptKittyKey(body string) (promptInputEvent, bool) {
	fields := strings.Split(body, ";")
	if len(fields) == 0 {
		return promptInputEvent{}, false
	}
	codeText := strings.Split(fields[0], ":")[0]
	code, err := strconv.Atoi(codeText)
	if err != nil || code < 0 || code > utf8.MaxRune+60000 {
		return promptInputEvent{}, false
	}
	control := false
	if len(fields) > 1 {
		modifierAndEvent := strings.Split(fields[1], ":")
		modifier, err := strconv.Atoi(modifierAndEvent[0])
		if err != nil || modifier < 1 {
			return promptInputEvent{}, false
		}
		control = (modifier-1)&4 != 0
		if len(modifierAndEvent) > 1 && modifierAndEvent[1] == "3" {
			return promptInputEvent{}, false
		}
	}
	switch code {
	case 13, 57345:
		return promptInputEvent{kind: promptKeyEnter}, true
	case 27, 57344:
		return promptInputEvent{kind: promptKeyEscape}, true
	case 127, 57347:
		return promptInputEvent{kind: promptKeyBackspace}, true
	case 57349:
		return promptInputEvent{kind: promptKeyDelete}, true
	case 57350:
		return promptInputEvent{kind: promptKeyLeft}, true
	case 57351:
		return promptInputEvent{kind: promptKeyRight}, true
	case 57356:
		return promptInputEvent{kind: promptKeyHome}, true
	case 57357:
		return promptInputEvent{kind: promptKeyEnd}, true
	}
	if code <= utf8.MaxRune {
		return promptInputEvent{kind: promptKeyRune, rune: rune(code), control: control}, true
	}
	return promptInputEvent{}, false
}
