package server

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func feedBytewise(parser *frontendInputParser, revision uint64, data []byte) []frontendInputEvent {
	var events []frontendInputEvent
	for _, b := range data {
		events = append(events, parser.Feed(revision, []byte{b})...)
	}
	return events
}

func TestSessionSwitchResetsTransportInputStateAndKeepsLatestLayout(t *testing.T) {
	client := newClientInstance(nil, nil)
	client.rememberLayout(protocol.WindowLayout{LayoutRevision: 4})
	client.rememberLayout(protocol.WindowLayout{LayoutRevision: 5})
	client.highestLayoutRevision.Store(5)
	client.frontendInput.Feed(5, []byte{0x1b})
	client.heldKeys[frontendHeldKey{Code: frontendKeyRune, Rune: 'x'}] = 1
	client.pointerCapture = frontendPaneCapture{active: true}
	client.pasteCapture = frontendPaneCapture{active: true}

	client.resetInputForSessionSwitch()

	if client.frontendInput.state != frontendParserGround || len(client.heldKeys) != 0 || client.pointerCapture.active || client.pasteCapture.active {
		t.Fatalf("frontend input state was not reset: %#v", client)
	}
	if len(client.layouts) != 1 || client.layouts[5].LayoutRevision != 5 {
		t.Fatalf("retained layouts = %#v", client.layouts)
	}
}

func TestFrontendParserKittyKeyAcrossEverySplit(t *testing.T) {
	sequence := []byte("\x1b[98;5:2u")
	for split := 0; split <= len(sequence); split++ {
		var parser frontendInputParser
		events := append(parser.Feed(7, sequence[:split]), parser.Feed(7, sequence[split:])...)
		if len(events) != 1 {
			t.Fatalf("split %d produced %d events", split, len(events))
		}
		key := events[0].Key
		if key.Code != frontendKeyRune || key.Rune != 'b' || key.Modifiers != frontendModifierControl || key.Action != frontendKeyRepeat || !key.HasEventType {
			t.Fatalf("split %d key = %#v", split, key)
		}
	}
}

func TestFrontendParserKittyFunctionalKeyAcrossEverySplit(t *testing.T) {
	for _, test := range []struct {
		sequence []byte
		action   frontendKeyAction
	}{
		{sequence: []byte("\x1b[1;1:1A"), action: frontendKeyPress},
		{sequence: []byte("\x1b[1;1:2A"), action: frontendKeyRepeat},
		{sequence: []byte("\x1b[1;1:3A"), action: frontendKeyRelease},
	} {
		for split := 0; split <= len(test.sequence); split++ {
			var parser frontendInputParser
			events := append(parser.Feed(7, test.sequence[:split]), parser.Feed(7, test.sequence[split:])...)
			if len(events) != 1 {
				t.Fatalf("sequence %q split %d produced %d events", test.sequence, split, len(events))
			}
			key := events[0].Key
			if key.Code != frontendKeyUp || key.Modifiers != 0 || key.Action != test.action || !key.HasEventType || len(key.Raw) != 0 {
				t.Fatalf("sequence %q split %d key = %#v", test.sequence, split, key)
			}
		}
	}
}

func TestFrontendParserMousePasteFocusAndUTF8Bytewise(t *testing.T) {
	var parser frontendInputParser
	events := feedBytewise(&parser, 11, []byte("\x1b[<64;20;10M"))
	if len(events) != 1 || events[0].Kind != frontendEventPointer {
		t.Fatalf("mouse events = %#v", events)
	}
	pointer := events[0].Pointer
	if pointer.Action != frontendPointerWheelUp || pointer.X != 19 || pointer.Y != 9 || events[0].LayoutRevision != 11 {
		t.Fatalf("mouse = %#v", events[0])
	}
	events = feedBytewise(&parser, 11, []byte("\x1b[<66;20;10M\x1b[<67;20;10M"))
	if len(events) != 2 || events[0].Pointer.Action != frontendPointerWheelLeft || events[1].Pointer.Action != frontendPointerWheelRight {
		t.Fatalf("horizontal wheel events = %#v", events)
	}

	events = feedBytewise(&parser, 12, []byte("\x1b[200~hello\x02world\x1b[201~"))
	if len(events) != 2 || events[0].Kind != frontendEventPasteStart || events[1].Kind != frontendEventPaste || string(events[1].Paste) != "hello\x02world" {
		t.Fatalf("paste events = %#v", events)
	}

	events = feedBytewise(&parser, 13, []byte("\x1b[I\x1b[O界"))
	if len(events) != 3 || events[0].Kind != frontendEventFocus || !events[0].Focused || events[1].Focused || events[2].Key.Rune != '界' {
		t.Fatalf("focus/text events = %#v", events)
	}
}

func TestFrontendParserFlushesLoneEscape(t *testing.T) {
	var parser frontendInputParser
	if events := parser.Feed(3, []byte{0x1b}); len(events) != 0 || !parser.hasLoneEscape() {
		t.Fatalf("pending escape events=%#v parser=%#v", events, parser)
	}
	event, ok := parser.flushLoneEscape()
	if !ok || event.Key.Code != frontendKeyEscape || event.LayoutRevision != 3 {
		t.Fatalf("flushed escape = %#v, %v", event, ok)
	}
}

func TestFrontendParserEscapeResynchronizesIncompleteInput(t *testing.T) {
	for _, damaged := range [][]byte{
		[]byte("\x1b[123"),
		[]byte("\x1bO"),
		{0xc3},
	} {
		var parser frontendInputParser
		if events := parser.Feed(3, damaged); len(events) != 0 {
			t.Fatalf("damaged prefix %q produced events %#v", damaged, events)
		}
		if events := parser.Feed(4, []byte{0x1b}); len(events) != 0 {
			t.Fatalf("replacement Escape after %q produced events %#v", damaged, events)
		}
		event, ok := parser.flushLoneEscape()
		if !ok || event.Key.Code != frontendKeyEscape || event.LayoutRevision != 4 {
			t.Fatalf("replacement Escape after %q = %#v, %v", damaged, event, ok)
		}
	}
}

func TestFrontendParserDiscardsUnknownControlSequenceAndRecovers(t *testing.T) {
	for _, unknown := range [][]byte{
		[]byte("\x1b[s"),
		[]byte("\x1bOz"),
	} {
		var parser frontendInputParser
		events := parser.Feed(7, append(append([]byte(nil), unknown...), 'x'))
		if len(events) != 1 || events[0].Key.Code != frontendKeyRune || events[0].Key.Rune != 'x' {
			t.Fatalf("events after unknown sequence %q = %#v", unknown, events)
		}
	}
}

func TestFrontendParserDiscardsOversizedCSIThroughFinalByte(t *testing.T) {
	var parser frontendInputParser
	input := append([]byte("\x1b["), bytes.Repeat([]byte{'1'}, maxFrontendSequenceBytes+32)...)
	input = append(input, []byte(";35;69M")...)
	input = append(input, 'x')
	events := parser.Feed(9, input)
	if len(events) != 1 || events[0].Key.Code != frontendKeyRune || events[0].Key.Rune != 'x' {
		t.Fatalf("events after oversized CSI = %#v", events)
	}
	if parser.state != frontendParserGround || len(parser.pending) != 0 {
		t.Fatalf("parser retained oversized CSI: %#v", parser)
	}
}

func TestFrontendTransportDelayDoesNotLeakFragmentedSequences(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence string
	}{
		{name: "Kitty key release", sequence: "\x1b[115;1:3u"},
		{name: "SGR mouse motion", sequence: "\x1b[<35;69;42M"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := NewSessionState(1)
			newStandaloneClient(s)
			pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(20, 5)}
			pane.initializeRuntime()
			createTestWindow(s, pane)
			frontend := newFrontendTestClient(s)
			setTestClient(s, frontend)
			layout, err := testWindowLayout(s)
			if err != nil {
				t.Fatal(err)
			}
			frontend.rememberLayout(layout)

			escapePayload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{LayoutRevision: layout.LayoutRevision, Data: []byte{0x1b}})
			if err != nil {
				t.Fatal(err)
			}
			if stopped, err := frontend.handleControlFrame(protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: escapePayload}); err != nil || stopped {
				t.Fatalf("Escape fragment stopped=%v err=%v", stopped, err)
			}
			if !frontend.frontendInput.hasLoneEscape() {
				t.Fatal("Escape fragment was not retained")
			}

			// This is twice the old production timeout. Wall-clock or transport
			// delay must not resolve the parser's pending Escape.
			time.Sleep(50 * time.Millisecond)
			select {
			case got := <-pane.ptyInput:
				t.Fatalf("transport delay leaked input %q", got)
			default:
			}

			suffixPayload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{LayoutRevision: layout.LayoutRevision, Data: []byte(test.sequence[1:])})
			if err != nil {
				t.Fatal(err)
			}
			if stopped, err := frontend.handleControlFrame(protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: suffixPayload}); err != nil || stopped {
				t.Fatalf("sequence suffix stopped=%v err=%v", stopped, err)
			}
			select {
			case got := <-pane.ptyInput:
				t.Fatalf("fragmented %s leaked input %q", test.name, got)
			default:
			}
		})
	}
}

func TestFrontendSourceIdleResolvesStandaloneEscape(t *testing.T) {
	s := NewSessionState(1)
	newStandaloneClient(s)
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(20, 5)}
	pane.initializeRuntime()
	createTestWindow(s, pane)
	frontend := newFrontendTestClient(s)
	setTestClient(s, frontend)

	payload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{SourceIdle: true, Data: []byte{0x1b}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped, err := frontend.handleControlFrame(protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: payload}); err != nil || stopped {
		t.Fatalf("source-idle Escape stopped=%v err=%v", stopped, err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte{0x1b}) {
			t.Fatalf("resolved Escape = %q", got)
		}
	default:
		t.Fatal("source idle did not resolve Escape")
	}
}

func TestPaneInputModesAndKittyQueryAreVirtualized(t *testing.T) {
	term := newTerminal(80, 24)
	update := term.Apply([]byte("\x1b[?1003h\x1b[?1006h\x1b[?1004h\x1b[?2004h\x1b[>3u\x1b[?u"))
	if term.MouseTracking != MouseTrackingMotion || term.MouseEncoding != MouseEncodingSGR || !term.FocusReporting || !term.BracketedPaste || term.KittyFlags != 3 {
		t.Fatalf("terminal input modes = %#v", term)
	}
	if len(update.Replies) != 1 || !bytes.Equal(update.Replies[0], []byte("\x1b[?3u")) {
		t.Fatalf("Kitty query replies = %q", update.Replies)
	}
	term.Apply([]byte("\x1b[<u\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l"))
	if term.KittyFlags != 0 || term.MouseTracking != MouseTrackingNone || term.MouseEncoding != MouseEncodingClassic || term.FocusReporting || term.BracketedPaste {
		t.Fatalf("restored terminal input modes = %#v", term)
	}
}

func TestFrontendAttachmentCleanupDisablesCaptureWithoutXTermModeRestore(t *testing.T) {
	term := newTerminal(80, 24)
	term.Apply([]byte(frontendTerminalSetup))
	if term.MouseTracking != MouseTrackingMotion || term.MouseEncoding != MouseEncodingSGR || !term.FocusReporting || !term.BracketedPaste || term.KittyFlags != 3 {
		t.Fatalf("frontend setup modes = %#v", term)
	}

	term.Apply([]byte(frontendTerminalExitCommand))
	if term.MouseTracking != MouseTrackingNone || term.MouseEncoding != MouseEncodingClassic || term.FocusReporting || term.BracketedPaste || term.KittyFlags != 0 {
		t.Fatalf("frontend cleanup left capture active = %#v", term)
	}
}

func TestFrontendAttachmentCleanupRestoresOuterKittyMode(t *testing.T) {
	term := newTerminal(80, 24)
	term.Apply([]byte("\x1b[>1u"))
	term.Apply([]byte(frontendTerminalSetup))
	term.Apply([]byte(frontendTerminalExitCommand))
	if term.KittyFlags != 1 {
		t.Fatalf("frontend cleanup did not restore outer Kitty flags: got %d, want 1", term.KittyFlags)
	}
}

func TestKittyInputConvertsForLegacyAndKittyPanes(t *testing.T) {
	key := frontendKeyEvent{Code: frontendKeyRune, Rune: 'b', Modifiers: frontendModifierControl, Action: frontendKeyPress, HasEventType: true}
	if got := encodeKeyForPane(key, paneTerminalMetadata{}); !bytes.Equal(got, []byte{0x02}) {
		t.Fatalf("legacy Ctrl+B = %q", got)
	}
	if got := encodeKeyForPane(key, paneTerminalMetadata{kittyFlags: 3}); !bytes.Equal(got, []byte("\x1b[98;5:1u")) {
		t.Fatalf("Kitty Ctrl+B = %q", got)
	}
	release := key
	release.Action = frontendKeyRelease
	if got := encodeKeyForPane(release, paneTerminalMetadata{}); len(got) != 0 {
		t.Fatalf("legacy release = %q", got)
	}
	if got := encodeKeyForPane(release, paneTerminalMetadata{kittyFlags: 3}); !bytes.Equal(got, []byte("\x1b[98;5:3u")) {
		t.Fatalf("Kitty release = %q", got)
	}
	ctrlUp := frontendKeyEvent{Code: frontendKeyUp, Modifiers: frontendModifierControl, Action: frontendKeyPress}
	if got := encodeKeyForPane(ctrlUp, paneTerminalMetadata{}); !bytes.Equal(got, []byte("\x1b[1;5A")) {
		t.Fatalf("legacy Ctrl+Up = %q", got)
	}
}

func TestKittyFunctionalPressAndReleaseConvertForLegacyPane(t *testing.T) {
	var parser frontendInputParser
	press := parser.Feed(1, []byte("\x1b[1;1:1A"))
	release := parser.Feed(1, []byte("\x1b[1;1:3A"))
	if len(press) != 1 || len(release) != 1 {
		t.Fatalf("press=%#v release=%#v", press, release)
	}
	if got := encodeKeyForPane(press[0].Key, paneTerminalMetadata{}); !bytes.Equal(got, []byte("\x1b[A")) {
		t.Fatalf("legacy press = %q", got)
	}
	if got := encodeKeyForPane(release[0].Key, paneTerminalMetadata{}); len(got) != 0 {
		t.Fatalf("legacy release = %q", got)
	}
}

func TestKittyCtrlBUsesServerPrefixAndPasteBypassesIt(t *testing.T) {
	s := NewSessionState(1)
	newStandaloneClient(s)
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(20, 5)}
	pane.initializeRuntime()
	createTestWindow(s, pane)
	frontend := newFrontendTestClient(s)

	if detach, err := frontend.handleInputBytes(1, []byte("\x1b[98;5u")); err != nil || detach {
		t.Fatalf("first prefix detach=%v err=%v", detach, err)
	}
	if clientForState(s).InputIsNormal() {
		t.Fatal("Kitty Ctrl+B did not enter prefix state")
	}
	if detach, err := frontend.handleInputBytes(1, []byte("\x1b[98;5u")); err != nil || detach {
		t.Fatalf("literal prefix detach=%v err=%v", detach, err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte{0x02}) {
			t.Fatalf("literal prefix = %q", got)
		}
	default:
		t.Fatal("literal prefix was not delivered")
	}

	if detach, err := frontend.handleInputBytes(1, []byte("\x1b[200~a\x02b\x1b[201~")); err != nil || detach {
		t.Fatalf("paste detach=%v err=%v", detach, err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte{'a', 0x02, 'b'}) {
			t.Fatalf("paste = %q", got)
		}
	default:
		t.Fatal("paste was not delivered")
	}
}

func TestKittyReleaseStaysWithPressPane(t *testing.T) {
	s := NewSessionState(1)
	newStandaloneClient(s)
	first := &Pane{ID: testAddPaneID(s), terminal: newTerminal(10, 4)}
	second := &Pane{ID: testAddPaneID(s), terminal: newTerminal(10, 4)}
	first.initializeRuntime()
	second.initializeRuntime()
	first.terminal.KittyFlags = 3
	second.terminal.KittyFlags = 3
	first.publishTerminalMetadata()
	second.publishTerminalMetadata()
	createTestWindow(s, first)
	if _, _, err := splitTestFocusedPane(s, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, first.ID); err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	press := frontendKeyEvent{Code: frontendKeyRune, Rune: 'x', Action: frontendKeyPress, HasEventType: true}
	if _, err := frontend.handleFrontendKey(press); err != nil {
		t.Fatal(err)
	}
	<-first.ptyInput
	if _, _, err := focusTestSessionPane(s, second.ID); err != nil {
		t.Fatal(err)
	}
	release := press
	release.Action = frontendKeyRelease
	if _, err := frontend.handleFrontendKey(release); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-first.ptyInput:
		if !bytes.Equal(got, []byte("\x1b[120;1:3u")) {
			t.Fatalf("release = %q", got)
		}
	default:
		t.Fatal("release did not return to the press pane")
	}
	select {
	case got := <-second.ptyInput:
		t.Fatalf("release reached focused pane: %q", got)
	default:
	}
}

func TestMouseRoutingUsesStampedLayoutAndPaneMode(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 20, 6
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(20, 6)}
	pane.initializeRuntime()
	pane.terminal.MouseTracking = MouseTrackingMotion
	pane.terminal.MouseEncoding = MouseEncodingSGR
	pane.publishTerminalMetadata()
	createTestWindow(s, pane)
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	frontend.rememberLayout(layout)
	setTestClient(s, frontend)

	event := frontendPointerEvent{Action: frontendPointerWheelDown, X: 4, Y: 2}
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, event); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte("\x1b[<65;5;3M")) {
			t.Fatalf("pane mouse = %q", got)
		}
	default:
		t.Fatalf("mouse event was not delivered: layout=%#v mode=%#v panes=%d", layout, pane.InputMode(), len(s.Panes))
	}
	for index, pointer := range []frontendPointerEvent{
		{Action: frontendPointerPress, Button: 0, X: 4, Y: 2},
		{Action: frontendPointerMove, Button: 0, X: 5, Y: 2},
		{Action: frontendPointerRelease, Button: 0, X: 5, Y: 2},
	} {
		if err := frontend.handleFrontendPointer(layout.LayoutRevision, pointer); err != nil {
			t.Fatal(err)
		}
		want := []string{"\x1b[<0;5;3M", "\x1b[<32;6;3M", "\x1b[<0;6;3m"}[index]
		select {
		case got := <-pane.ptyInput:
			if string(got) != want {
				t.Fatalf("application mouse event %d = %q, want %q", index, got, want)
			}
		default:
			t.Fatalf("application mouse event %d was not delivered", index)
		}
	}
	if pane.isHistoryMode() {
		t.Fatal("application mouse drag entered Meja history mode")
	}

	pane.terminal.MouseTracking = MouseTrackingNone
	pane.publishTerminalMetadata()
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerWheelUp, X: 4, Y: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte("\x1b[A")) {
			t.Fatalf("wheel fallback = %q", got)
		}
	default:
		t.Fatal("wheel fallback was not delivered")
	}
	pane.terminal.ApplicationCursorKeys = true
	pane.publishTerminalMetadata()
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerWheelDown, X: 4, Y: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte("\x1bOB")) {
			t.Fatalf("application-cursor wheel fallback = %q", got)
		}
	default:
		t.Fatal("application-cursor wheel fallback was not delivered")
	}
	pane.terminal.KittyFlags = 3
	pane.publishTerminalMetadata()
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerWheelUp, X: 4, Y: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte("\x1b[1;1:1A")) {
			t.Fatalf("Kitty wheel fallback = %q", got)
		}
	default:
		t.Fatal("Kitty wheel fallback was not delivered")
	}

	if err := frontend.handleFrontendPointer(layout.LayoutRevision+999, event); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pane.ptyInput:
		t.Fatalf("unknown layout delivered %q", got)
	default:
	}
}

func TestMousePressFocusesClickedPaneAndPublishesProjection(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 20, 6
	first := &Pane{ID: testAddPaneID(s), terminal: newTerminal(10, 6)}
	second := &Pane{ID: testAddPaneID(s), terminal: newTerminal(10, 6)}
	first.initializeRuntime()
	second.initializeRuntime()
	createTestWindow(s, first)
	if _, _, err := splitTestFocusedPane(s, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, first.ID); err != nil {
		t.Fatal(err)
	}
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	secondPlacement, ok := panePlacement(layout, second.ID)
	if !ok {
		t.Fatalf("second pane missing from layout: %#v", layout)
	}

	frontend := newClientInstance(nil, nil)
	frontend.controlOut = make(chan protocol.Frame, 1)
	frontend.rememberLayout(layout)
	setLeasedTestClient(t, s, frontend, 9)
	renderedRevision := frontend.highestLayoutRevision.Load()
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{
		Action: frontendPointerPress,
		Button: 0,
		X:      secondPlacement.Rect.X,
		Y:      secondPlacement.Rect.Y,
	}); err != nil {
		t.Fatal(err)
	}

	frame := <-frontend.controlOut
	published, err := protocol.DecodeWindowLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if published.FocusedPaneID != second.ID {
		t.Fatalf("published focused pane = %d, want %d", published.FocusedPaneID, second.ID)
	}
	if published.LayoutRevision != renderedRevision {
		t.Fatalf("mouse focus-only layout revision = %d, want existing rendered revision %d", published.LayoutRevision, renderedRevision)
	}
	if got := frontend.ensureClientState().FocusedPaneID; got != second.ID {
		t.Fatalf("client focused pane = %d, want %d", got, second.ID)
	}
	if active, _ := testActivePane(s); active != second {
		t.Fatalf("input target = %#v, want pane %d", active, second.ID)
	}
	if detach, err := frontend.handleInputBytes(published.LayoutRevision, []byte("clicked")); err != nil || detach {
		t.Fatalf("input after click detach=%v err=%v", detach, err)
	}
	assertOnlyPaneInput(t, second.ptyInput, "clicked")
}

func TestMouseSelectionCopiesThroughOSC52AndReturnsToLivePane(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 12, 4
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(12, 4)}
	pane.terminal.Apply([]byte("hello"))
	createTestWindow(s, pane)
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	frontend.controlOut = make(chan protocol.Frame, 1)
	frontend.rememberLayout(layout)
	placement := layout.Panes[0]

	for _, pointer := range []frontendPointerEvent{
		{Action: frontendPointerPress, Button: 0, X: placement.Rect.X, Y: placement.Rect.Y},
		{Action: frontendPointerMove, Button: 0, X: placement.Rect.X + 4, Y: placement.Rect.Y},
		{Action: frontendPointerRelease, Button: 0, X: placement.Rect.X + 4, Y: placement.Rect.Y},
	} {
		if err := frontend.handleFrontendPointer(layout.LayoutRevision, pointer); err != nil {
			t.Fatal(err)
		}
	}
	if pane.isHistoryMode() {
		t.Fatal("automatic mouse copy left pane in history mode")
	}
	frame := <-frontend.controlOut
	if frame.Type != protocol.MsgFrontendTerminalWrite {
		t.Fatalf("frame type = %d", frame.Type)
	}
	write, err := protocol.DecodeFrontendTerminalWrite(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\x1b\\"
	if got := string(write.Data); got != want {
		t.Fatalf("terminal write = %q, want %q", got, want)
	}
}

func TestMouseClickWithoutDragDoesNotCopy(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 12, 4
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(12, 4)}
	createTestWindow(s, pane)
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	frontend.controlOut = make(chan protocol.Frame, 1)
	frontend.rememberLayout(layout)
	point := layout.Panes[0].Rect
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerPress, Button: 0, X: point.X, Y: point.Y}); err != nil {
		t.Fatal(err)
	}
	if pane.isHistoryMode() {
		t.Fatal("mouse press entered history mode before a drag")
	}
	if !frontend.pointerCapture.active || !frontend.pointerCapture.mejaSelection || frontend.pointerCapture.selecting {
		t.Fatalf("mouse press capture = %#v", frontend.pointerCapture)
	}
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerRelease, Button: 0, X: point.X, Y: point.Y}); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-frontend.controlOut:
		t.Fatalf("click emitted terminal frame %#v", frame)
	default:
	}
	if pane.isHistoryMode() {
		t.Fatal("click left pane in history mode")
	}
	if frontend.pointerCapture.active {
		t.Fatalf("click left pointer capture active: %#v", frontend.pointerCapture)
	}
}

func TestMousePressWithoutReleaseDoesNotBlockKey(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 12, 4
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(12, 4), ptyInput: make(chan []byte, 1)}
	createTestWindow(s, pane)
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	frontend.rememberLayout(layout)
	point := layout.Panes[0].Rect
	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerPress, Button: 0, X: point.X, Y: point.Y}); err != nil {
		t.Fatal(err)
	}

	if detach, err := frontend.handleFrontendKey(frontendKeyEvent{Code: frontendKeyRune, Rune: 'x', Action: frontendKeyPress}); err != nil || detach {
		t.Fatalf("key after truncated click detach=%v err=%v", detach, err)
	}
	if pane.isHistoryMode() || frontend.pointerCapture.active {
		t.Fatalf("key left pending click state: pane history=%v capture=%#v", pane.isHistoryMode(), frontend.pointerCapture)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte("x")) {
			t.Fatalf("forwarded key = %q", got)
		}
	default:
		t.Fatal("key was not forwarded after a truncated click")
	}
}

func TestMouseMotionWithoutHeldButtonCancelsTruncatedGesture(t *testing.T) {
	for _, test := range []struct {
		name       string
		activeDrag bool
	}{
		{name: "pending press"},
		{name: "active drag", activeDrag: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := NewSessionState(1)
			clientState := newStandaloneClient(s)
			clientState.TerminalCols, clientState.TerminalRows = 12, 4
			pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(12, 4)}
			createTestWindow(s, pane)
			layout, err := testWindowLayout(s)
			if err != nil {
				t.Fatal(err)
			}
			frontend := newFrontendTestClient(s)
			frontend.rememberLayout(layout)
			point := layout.Panes[0].Rect
			if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerPress, Button: 0, X: point.X, Y: point.Y}); err != nil {
				t.Fatal(err)
			}
			if test.activeDrag {
				if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerMove, Button: 0, X: point.X + 1, Y: point.Y}); err != nil {
					t.Fatal(err)
				}
			}
			if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{Action: frontendPointerMove, Button: 3, X: point.X + 2, Y: point.Y}); err != nil {
				t.Fatal(err)
			}
			if pane.isHistoryMode() || frontend.pointerCapture.active {
				t.Fatalf("buttonless motion left gesture active: pane history=%v capture=%#v", pane.isHistoryMode(), frontend.pointerCapture)
			}
		})
	}
}

func TestMouseSelectionWithoutReleaseCancelsAndForwardsKey(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 12, 4
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(12, 4), ptyInput: make(chan []byte, 1)}
	pane.terminal.Apply([]byte("hello"))
	createTestWindow(s, pane)
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	frontend.rememberLayout(layout)
	point := layout.Panes[0].Rect

	for _, pointer := range []frontendPointerEvent{
		{Action: frontendPointerPress, Button: 0, X: point.X, Y: point.Y},
		{Action: frontendPointerMove, Button: 0, X: point.X + 2, Y: point.Y},
	} {
		if err := frontend.handleFrontendPointer(layout.LayoutRevision, pointer); err != nil {
			t.Fatal(err)
		}
	}
	if !pane.isHistoryMode() || !frontend.pointerCapture.selecting {
		t.Fatalf("drag did not enter selection: pane history=%v capture=%#v", pane.isHistoryMode(), frontend.pointerCapture)
	}

	if detach, err := frontend.handleFrontendKey(frontendKeyEvent{Code: frontendKeyRune, Rune: 'x', Action: frontendKeyPress}); err != nil || detach {
		t.Fatalf("key after truncated drag detach=%v err=%v", detach, err)
	}
	if pane.isHistoryMode() || frontend.pointerCapture.active {
		t.Fatalf("key did not cancel automatic selection: pane history=%v capture=%#v", pane.isHistoryMode(), frontend.pointerCapture)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, []byte("x")) {
			t.Fatalf("forwarded key = %q", got)
		}
	default:
		t.Fatal("key was not forwarded after cancelling automatic selection")
	}
}

func TestMouseSelectionWithoutReleaseCancelsOnFocusLossAndRelayout(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(*SessionState, *ClientInstance) error
	}{
		{name: "focus loss", cancel: func(s *SessionState, frontend *ClientInstance) error {
			_, err := frontend.handleFrontendInputEvent(frontendInputEvent{Kind: frontendEventFocus, Focused: false})
			return err
		}},
		{name: "split relayout", cancel: func(s *SessionState, frontend *ClientInstance) error {
			second := &Pane{ID: testAddPaneID(s), terminal: newTerminal(12, 4)}
			_, clientState, err := splitTestFocusedPane(s, second, SplitVertical)
			if err != nil {
				return err
			}
			if err := resizeTestSessionActiveWindow(s, clientState.TerminalCols, clientState.TerminalRows); err != nil {
				return err
			}
			return clientForState(s).applyCurrentTestViewWithHandoff(nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := NewSessionState(1)
			clientState := newStandaloneClient(s)
			clientState.TerminalCols, clientState.TerminalRows = 12, 4
			pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(12, 4)}
			createTestWindow(s, pane)
			layout, err := testWindowLayout(s)
			if err != nil {
				t.Fatal(err)
			}
			frontend := newFrontendTestClient(s)
			frontend.rememberLayout(layout)
			setTestClient(s, frontend)
			point := layout.Panes[0].Rect
			for _, pointer := range []frontendPointerEvent{
				{Action: frontendPointerPress, Button: 0, X: point.X, Y: point.Y},
				{Action: frontendPointerMove, Button: 0, X: point.X + 2, Y: point.Y},
			} {
				if err := frontend.handleFrontendPointer(layout.LayoutRevision, pointer); err != nil {
					t.Fatal(err)
				}
			}
			if err := test.cancel(s, frontend); err != nil {
				t.Fatal(err)
			}
			if pane.isHistoryMode() || frontend.pointerCapture.active {
				t.Fatalf("boundary did not cancel selection: pane history=%v capture=%#v", pane.isHistoryMode(), frontend.pointerCapture)
			}
		})
	}
}

func TestOSC52ClipboardWrite(t *testing.T) {
	if got, want := string(osc52ClipboardWrite([]byte("a\nβ"))), "\x1b]52;c;YQrOsg==\x1b\\"; got != want {
		t.Fatalf("OSC 52 = %q, want %q", got, want)
	}
}

func TestFallbackWheelBurstsCoalesceNetDeltaWithoutTouchingNativeMouse(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 20, 6
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(20, 6)}
	pane.initializeRuntime()
	createTestWindow(s, pane)
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	frontend.rememberLayout(layout)

	up := []byte("\x1b[<64;5;3M")
	down := []byte("\x1b[<65;5;3M")
	if _, err := frontend.handleInputBytes(layout.LayoutRevision, bytes.Repeat(up, 3)); err != nil {
		t.Fatal(err)
	}
	assertOnlyPaneInput(t, pane.ptyInput, "\x1b[A")

	left := []byte("\x1b[<66;5;3M")
	jitter := append(append(append(append([]byte(nil), up...), left...), up...), down...)
	if _, err := frontend.handleInputBytes(layout.LayoutRevision, jitter); err != nil {
		t.Fatal(err)
	}
	assertOnlyPaneInput(t, pane.ptyInput, "\x1b[A")

	pane.terminal.MouseTracking = MouseTrackingMotion
	pane.terminal.MouseEncoding = MouseEncodingSGR
	pane.publishTerminalMetadata()
	if _, err := frontend.handleInputBytes(layout.LayoutRevision, bytes.Repeat(up, 3)); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		select {
		case got := <-pane.ptyInput:
			if !bytes.Equal(got, up) {
				t.Fatalf("native mouse event %d = %q", index, got)
			}
		default:
			t.Fatalf("missing native mouse event %d", index)
		}
	}
	if _, err := frontend.handleInputBytes(layout.LayoutRevision, left); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pane.ptyInput:
		if !bytes.Equal(got, left) {
			t.Fatalf("native horizontal mouse event = %q", got)
		}
	default:
		t.Fatal("missing native horizontal mouse event")
	}
}

func assertOnlyPaneInput(t *testing.T, inputs <-chan []byte, want string) {
	t.Helper()
	select {
	case got := <-inputs:
		if string(got) != want {
			t.Fatalf("pane input = %q, want %q", got, want)
		}
	default:
		t.Fatalf("missing pane input %q", want)
	}
	select {
	case got := <-inputs:
		t.Fatalf("unexpected extra pane input %q", got)
	default:
	}
}

func TestMouseWheelScrollsMejaHistoryBeforePaneMouseMode(t *testing.T) {
	s := NewSessionState(1)
	clientState := newStandaloneClient(s)
	clientState.TerminalCols, clientState.TerminalRows = 20, 6
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(20, 6)}
	pane.initializeRuntime()
	pane.terminal.Apply([]byte("one\r\ntwo\r\nthree\r\nfour\r\nfive\r\nsix\r\nseven\r\neight\r\nnine\r\nten\r\neleven\r\ntwelve\r\nthirteen\r\nfourteen\r\nfifteen"))
	pane.terminal.MouseTracking = MouseTrackingMotion
	pane.terminal.MouseEncoding = MouseEncodingSGR
	pane.publishTerminalMetadata()
	createTestWindow(s, pane)
	layout, err := testWindowLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	frontend := newFrontendTestClient(s)
	frontend.rememberLayout(layout)
	request := paneHistoryRequest{Action: paneHistoryEnter}
	if result := pane.handleHistoryRequest(&request); result.Err != nil {
		t.Fatal(result.Err)
	}
	// This unit fixture has no pane owner goroutine. Use the synchronous
	// history-request fallback while retaining the initialized PTY input queue.
	pane.commands = nil
	pane.historyView.CursorRow = pane.historyView.ViewTop
	before := pane.historyView.ViewTop

	if err := frontend.handleFrontendPointer(layout.LayoutRevision, frontendPointerEvent{
		Action: frontendPointerWheelUp,
		X:      4,
		Y:      2,
	}); err != nil {
		t.Fatal(err)
	}
	if pane.historyView.ViewTop != before-1 {
		t.Fatalf("wheel moved history viewport by %d lines, want 1", before-pane.historyView.ViewTop)
	}
	select {
	case got := <-pane.ptyInput:
		t.Fatalf("history wheel leaked to pane PTY: %q", got)
	default:
	}
}

func TestFrontendInputProtocolCarriesLayoutRevision(t *testing.T) {
	payload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{LayoutRevision: 91, Data: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeFrontendInputBytes(payload)
	if err != nil || decoded.LayoutRevision != 91 || string(decoded.Data) != "x" {
		t.Fatalf("decoded = %#v, err = %v", decoded, err)
	}
}
