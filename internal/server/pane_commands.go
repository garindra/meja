package server

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

const (
	maxSendKeysBytes    = 1 << 20
	maxCapturePaneBytes = 4 << 20
)

type capturePaneOptions struct {
	joinWrapped      bool
	preserveTrailing bool
	print            bool
	escape           bool
	octal            bool
	bufferName       string
	startSet         bool
	startLine        int
	startHistory     bool
	endSet           bool
	endLine          int
	endVisible       bool
}

type paneCaptureRequest struct {
	Options capturePaneOptions
	Result  chan<- paneCaptureResult
}

type paneCaptureResult struct {
	Data []byte
	Err  error
}

func runSendKeysCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	_, client, remaining, err := resolveSessionCommandContextValue(d, ctx, sessionTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if client == nil {
		return commandOutcome{}, errors.New("command requires an attached client")
	}
	return commandOutcome{}, sendKeysToClient(client, remaining)
}

func sendKeysToClient(s *ClientInstance, args []string) error {
	modeArgs, mode, err := parseSendKeysModeArgs(args)
	if err != nil {
		return err
	}
	if mode {
		return sendKeysCopyModeCommand(s, modeArgs)
	}
	literal, keys, err := parseSendKeysArgs(args)
	if err != nil {
		return err
	}
	pane := s.activePane()
	if pane == nil {
		return errors.New("send-keys requires an active pane")
	}
	data, err := encodeSendKeys(keys, literal, pane.InputMode())
	if err != nil {
		return err
	}
	if err := pane.sendInput(data); err != nil {
		return fmt.Errorf("send-keys: %w", err)
	}
	return nil
}

func parseSendKeysModeArgs(args []string) ([]string, bool, error) {
	for index, arg := range args {
		if arg != "-X" && !strings.HasPrefix(arg, "-X=") {
			continue
		}
		if index != 0 {
			return nil, false, errors.New("send-keys -X must be specified before the copy-mode command")
		}
		if strings.HasPrefix(arg, "-X=") {
			command := strings.TrimPrefix(arg, "-X=")
			if command == "" {
				return nil, false, errors.New("send-keys -X requires a copy-mode command")
			}
			return append([]string{command}, args[1:]...), true, nil
		}
		if len(args) < 2 {
			return nil, false, errors.New("send-keys -X requires a copy-mode command")
		}
		return append([]string(nil), args[1:]...), true, nil
	}
	return nil, false, nil
}

func sendKeysCopyModeCommand(s *ClientInstance, args []string) error {
	if len(args) == 0 {
		return errors.New("send-keys -X requires a copy-mode command")
	}
	pane := s.activePane()
	if pane == nil {
		return errors.New("send-keys -X requires an active pane")
	}
	if !pane.isHistoryMode() {
		return errors.New("pane is not in copy mode")
	}
	command := args[0]
	if len(args) > 1 {
		return fmt.Errorf("copy-mode command %q does not accept arguments", command)
	}
	var err error
	switch command {
	case "scroll-up":
		_, err = pane.handleHistoryInput([]byte("\x1b[A"))
	case "scroll-down":
		_, err = pane.handleHistoryInput([]byte("\x1b[B"))
	case "page-up":
		_, err = pane.handleHistoryInput([]byte("\x1b[5~"))
	case "page-down":
		_, err = pane.handleHistoryInput([]byte("\x1b[6~"))
	case "halfpage-up":
		_, err = pane.handleHistoryInput([]byte{0x15})
	case "halfpage-down":
		_, err = pane.handleHistoryInput([]byte{0x04})
	case "history-top":
		_, err = pane.handleHistoryInput([]byte{'g'})
	case "history-bottom":
		_, err = pane.handleHistoryInput([]byte{'G'})
	case "begin-selection":
		err = pane.beginHistorySelectionAtCursor(false)
	case "clear-selection", "stop-selection":
		err = pane.clearHistorySelection()
	case "copy-selection", "copy-selection-and-cancel":
		var data []byte
		data, err = pane.copyHistorySelection(command == "copy-selection-and-cancel")
		if err == nil {
			if len(data) == 0 {
				return errors.New("copy-mode has no selection")
			}
			if s != nil && s.Daemon != nil {
				_, err = s.Daemon.pasteBuffers.addAutomatic(data)
			}
			if err == nil && s != nil {
				err = s.writeFrontendTerminal(osc52ClipboardWrite(data))
			}
		}
	case "cancel":
		_, err = pane.exitHistoryMode()
	default:
		return fmt.Errorf("unknown copy-mode command %q", command)
	}
	if err != nil {
		return fmt.Errorf("send-keys -X %s: %w", command, err)
	}
	return nil
}

func parseSendKeysArgs(args []string) (literal bool, keys []string, err error) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "-l", "--literal":
			if literal {
				return false, nil, errors.New("send-keys accepts -l at most once")
			}
			literal = true
		case "--":
			keys = append(keys, args[index+1:]...)
			index = len(args)
		default:
			keys = append(keys, arg)
		}
	}
	if len(keys) == 0 {
		return false, nil, errors.New("send-keys requires at least one key")
	}
	return literal, keys, nil
}

func encodeSendKeys(keys []string, literal bool, mode paneTerminalMetadata) ([]byte, error) {
	data := make([]byte, 0, min(maxSendKeysBytes, len(keys)*2))
	appendData := func(next []byte) error {
		if len(data)+len(next) > maxSendKeysBytes {
			return fmt.Errorf("send-keys input exceeds %d bytes", maxSendKeysBytes)
		}
		data = append(data, next...)
		return nil
	}
	for _, token := range keys {
		if literal {
			if err := appendData([]byte(token)); err != nil {
				return nil, err
			}
			continue
		}
		events, err := parseSendKeyToken(token)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if err := appendData(encodeKeyForPane(event, mode)); err != nil {
				return nil, err
			}
		}
	}
	if len(data) == 0 {
		return nil, errors.New("send-keys produced no input")
	}
	return data, nil
}

func parseSendKeyToken(token string) ([]frontendKeyEvent, error) {
	if token == "" || !utf8.ValidString(token) {
		return nil, fmt.Errorf("invalid send-keys token %q", token)
	}

	if code, ok := sendKeyName(token); ok {
		runeValue := rune(0)
		if code == frontendKeyRune {
			runeValue = ' '
		}
		return []frontendKeyEvent{{Code: code, Rune: runeValue, Action: frontendKeyPress, HasEventType: true}}, nil
	}
	if r, ok := singleRune(token); ok {
		return []frontendKeyEvent{{Code: frontendKeyRune, Rune: r, Action: frontendKeyPress, HasEventType: true}}, nil
	}

	parts := strings.Split(token, "-")
	if len(parts) > 1 {
		var modifiers frontendModifiers
		index := 0
		for index < len(parts)-1 {
			modifier, ok := sendKeyModifier(parts[index])
			if !ok {
				break
			}
			modifiers |= modifier
			index++
		}
		if modifiers != 0 && index < len(parts) {
			base := strings.Join(parts[index:], "-")
			if code, ok := sendKeyName(base); ok {
				runeValue := rune(0)
				if code == frontendKeyRune {
					runeValue = ' '
				}
				return []frontendKeyEvent{{Code: code, Rune: runeValue, Modifiers: modifiers, Action: frontendKeyPress, HasEventType: true}}, nil
			}
			if r, ok := singleRune(base); ok {
				return []frontendKeyEvent{{Code: frontendKeyRune, Rune: r, Modifiers: modifiers, Action: frontendKeyPress, HasEventType: true}}, nil
			}
			return nil, fmt.Errorf("unknown send-keys key %q", token)
		}
	}

	// tmux accepts an unrecognized UTF-8 token as a sequence of literal keys.
	events := make([]frontendKeyEvent, 0, utf8.RuneCountInString(token))
	for _, r := range token {
		events = append(events, frontendKeyEvent{Code: frontendKeyRune, Rune: r, Action: frontendKeyPress, HasEventType: true})
	}
	return events, nil
}

func sendKeyModifier(raw string) (frontendModifiers, bool) {
	switch strings.ToUpper(raw) {
	case "C", "CTRL", "CONTROL":
		return frontendModifierControl, true
	case "M", "A", "ALT", "META":
		return frontendModifierAlt, true
	case "S", "SHIFT":
		return frontendModifierShift, true
	default:
		return 0, false
	}
}

func singleRune(value string) (rune, bool) {
	r, size := utf8.DecodeRuneInString(value)
	return r, size == len(value) && size > 0
}

func sendKeyName(raw string) (frontendKeyCode, bool) {
	name := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(raw, "_", "-"), " ", ""))
	switch name {
	case "esc", "escape":
		return frontendKeyEscape, true
	case "enter", "return", "cr":
		return frontendKeyEnter, true
	case "tab":
		return frontendKeyTab, true
	case "bs", "bspace", "backspace":
		return frontendKeyBackspace, true
	case "space":
		return frontendKeyRune, true
	case "up":
		return frontendKeyUp, true
	case "down":
		return frontendKeyDown, true
	case "left":
		return frontendKeyLeft, true
	case "right":
		return frontendKeyRight, true
	case "insert", "ins":
		return frontendKeyInsert, true
	case "delete", "del":
		return frontendKeyDelete, true
	case "home":
		return frontendKeyHome, true
	case "end":
		return frontendKeyEnd, true
	case "pageup", "page-up", "pgup":
		return frontendKeyPageUp, true
	case "pagedown", "page-down", "pgdn":
		return frontendKeyPageDown, true
	case "f1":
		return frontendKeyF1, true
	case "f2":
		return frontendKeyF2, true
	case "f3":
		return frontendKeyF3, true
	case "f4":
		return frontendKeyF4, true
	case "f5":
		return frontendKeyF5, true
	case "f6":
		return frontendKeyF6, true
	case "f7":
		return frontendKeyF7, true
	case "f8":
		return frontendKeyF8, true
	case "f9":
		return frontendKeyF9, true
	case "f10":
		return frontendKeyF10, true
	case "f11":
		return frontendKeyF11, true
	case "f12":
		return frontendKeyF12, true
	default:
		return 0, false
	}
}

func capturePaneCommand() commandHandler {
	return func(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
		session, _, normalized, err := resolveSessionCommandContextValue(d, ctx, sessionTarget, args)
		if err != nil {
			return commandOutcome{}, err
		}
		options, err := parseCapturePaneArgs(normalized)
		if err != nil {
			return commandOutcome{}, err
		}
		var data []byte
		pane := session.activePane()
		if pane == nil {
			return commandOutcome{}, errors.New("capture-pane requires an active pane")
		}
		data, err = pane.capturePane(options)
		if err != nil {
			return commandOutcome{}, err
		}
		if options.print {
			return commandOutcome{Stdout: data}, nil
		}
		if d == nil {
			return commandOutcome{}, errors.New("capture-pane requires a running daemon")
		}
		_, err = d.pasteBuffers.set(options.bufferName, options.bufferName != "", data, false, "")
		if err != nil {
			return commandOutcome{}, err
		}
		return commandOutcome{}, nil
	}
}

func parseCapturePaneArgs(args []string) (capturePaneOptions, error) {
	fs := commandFlagSet("capture-pane")
	print := fs.Bool("p", false, "print captured text")
	bufferName := fs.String("b", "", "buffer name")
	start := fs.String("S", "", "start line")
	end := fs.String("E", "", "end line")
	escape := fs.Bool("e", false, "include terminal escape sequences")
	octal := fs.Bool("C", false, "escape non-printable characters")
	join := fs.Bool("J", false, "join wrapped lines")
	preserve := fs.Bool("N", false, "preserve trailing spaces")
	if err := fs.Parse(args); err != nil {
		return capturePaneOptions{}, err
	}
	if len(fs.Args()) != 0 {
		return capturePaneOptions{}, errors.New("capture-pane accepts no positional arguments")
	}
	options := capturePaneOptions{
		joinWrapped:      *join,
		preserveTrailing: *preserve || *join,
		print:            *print,
		escape:           *escape,
		octal:            *octal,
		bufferName:       *bufferName,
	}
	if flagSetProvided(fs, "S") {
		line, allHistory, err := parseCaptureLine(*start)
		if err != nil {
			return capturePaneOptions{}, fmt.Errorf("capture-pane -S: %w", err)
		}
		options.startSet, options.startLine, options.startHistory = true, line, allHistory
	}
	if flagSetProvided(fs, "E") {
		line, endVisible, err := parseCaptureLine(*end)
		if err != nil {
			return capturePaneOptions{}, fmt.Errorf("capture-pane -E: %w", err)
		}
		options.endSet, options.endLine, options.endVisible = true, line, endVisible
	}
	return options, nil
}

func parseCaptureLine(raw string) (line int, special bool, err error) {
	if raw == "-" {
		return 0, true, nil
	}
	line, err = strconv.Atoi(raw)
	if err != nil {
		return 0, false, errors.New("line must be an integer or -")
	}
	return line, false, nil
}

func (p *Pane) capturePane(options capturePaneOptions) ([]byte, error) {
	if p.commands == nil {
		return captureTerminalViewport(p.terminal, options)
	}
	result := make(chan paneCaptureResult, 1)
	request := &paneCaptureRequest{Options: options, Result: result}
	select {
	case p.commands <- paneCommand{capture: request}:
	case <-p.mainDone:
		return nil, nil
	case <-p.done:
		return nil, nil
	}
	select {
	case captured := <-result:
		return captured.Data, captured.Err
	case <-p.mainDone:
		return nil, nil
	case <-p.done:
		return nil, nil
	}
}

func captureTerminalViewport(terminal *TerminalState, options capturePaneOptions) ([]byte, error) {
	if terminal == nil || terminal.Rows <= 0 || terminal.Cols <= 0 {
		return nil, nil
	}
	count := int(terminal.grid.count)
	if count == 0 {
		return nil, nil
	}
	visibleStart := max(0, count-terminal.Rows)
	start, end := visibleStart, count-1
	if options.startSet {
		if options.startHistory {
			start = 0
		} else {
			start = visibleStart + options.startLine
		}
	}
	if options.endSet {
		if options.endVisible {
			end = count - 1
		} else {
			end = visibleStart + options.endLine
		}
	}
	start = min(max(start, 0), count-1)
	end = min(max(end, -1), count-1)
	if start > end {
		return []byte{}, nil
	}
	output := make([]byte, 0, min(maxCapturePaneBytes, (end-start+1)*terminal.Cols+(end-start+1)))
	appendBytes := func(data []byte) error {
		if len(output)+len(data) > maxCapturePaneBytes {
			return fmt.Errorf("capture-pane output exceeds %d bytes", maxCapturePaneBytes)
		}
		output = append(output, data...)
		return nil
	}
	var currentStyle = protocol.CanonicalDefaultStyle()
	styleActive := false
	for logicalRow := start; logicalRow <= end; logicalRow++ {
		cells := terminal.grid.logicalRow(logicalRow, terminal.Cols)
		rowEnd := len(cells)
		if !options.preserveTrailing {
			rowEnd = len(trimTrailingBlankCells(cells))
		}
		for column := 0; column < rowEnd; {
			cell := cells[column]
			if cell.width() == 0 {
				column++
				continue
			}
			style := protocol.CanonicalDefaultStyle()
			if found, ok := terminal.LookupStyle(uint32(cell.styleID())); ok {
				style = found
			}
			if options.escape && (!styleActive || style != currentStyle) {
				if escape := captureStyleEscape(style); len(escape) > 0 {
					if err := appendBytes(escape); err != nil {
						return nil, err
					}
				}
				currentStyle = style
				styleActive = true
			}
			text := cellTextFromStore(cell, &terminal.clusters)
			if text == "" {
				text = " "
			}
			if options.octal {
				text = escapeCaptureText(text)
			}
			if err := appendBytes([]byte(text)); err != nil {
				return nil, err
			}
			column += max(1, int(cell.width()))
		}
		if logicalRow >= end || !options.joinWrapped || !terminal.grid.logicalWrapped(logicalRow) {
			if err := appendBytes([]byte{'\n'}); err != nil {
				return nil, err
			}
		}
	}
	if options.escape && styleActive {
		if err := appendBytes([]byte("\x1b[0m")); err != nil {
			return nil, err
		}
	}
	return output, nil
}

func captureStyleEscape(style protocol.Style) []byte {
	params := []string{"0"}
	if style.Bold {
		params = append(params, "1")
	}
	if style.Dim {
		params = append(params, "2")
	}
	if style.Italic {
		params = append(params, "3")
	}
	if style.Underline {
		params = append(params, "4")
	}
	if style.Blink {
		params = append(params, "5")
	}
	if style.Reverse {
		params = append(params, "7")
	}
	if style.Invisible {
		params = append(params, "8")
	}
	params = append(params, captureColorParams(style.FG, false)...)
	params = append(params, captureColorParams(style.BG, true)...)
	return []byte("\x1b[" + strings.Join(params, ";") + "m")
}

func captureColorParams(color protocol.Color, background bool) []string {
	base := 30
	if background {
		base = 40
	}
	switch color.Mode {
	case "default", "":
		if background {
			return []string{"49"}
		}
		return []string{"39"}
	case "indexed":
		if color.Index < 8 {
			return []string{strconv.Itoa(base + int(color.Index))}
		}
		if color.Index < 16 {
			brightBase := 90
			if background {
				brightBase = 100
			}
			return []string{strconv.Itoa(brightBase + int(color.Index-8))}
		}
		prefix := 38
		if background {
			prefix = 48
		}
		return []string{strconv.Itoa(prefix), "5", strconv.Itoa(int(color.Index))}
	case "rgb":
		prefix := 38
		if background {
			prefix = 48
		}
		return []string{strconv.Itoa(prefix), "2", strconv.Itoa(int(color.R)), strconv.Itoa(int(color.G)), strconv.Itoa(int(color.B))}
	default:
		return nil
	}
}

func escapeCaptureText(text string) string {
	var output bytes.Buffer
	for _, b := range []byte(text) {
		if b < 0x20 || b == 0x7f || b == '\\' {
			fmt.Fprintf(&output, "\\%03o", b)
		} else {
			output.WriteByte(b)
		}
	}
	return output.String()
}
