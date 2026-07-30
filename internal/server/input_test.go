package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func isCommandInput(event serverInputEvent, args ...string) bool {
	return event.Command == serverCommandExecute && slices.Equal(event.CommandArgs, args)
}

func handleTestControlFrames(s *SessionState, client *ClientInstance, decoder *protocol.Decoder) error {
	for {
		frame, err := decoder.ReadFrame()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		stopped, err := client.handleControlFrame(frame)
		if err != nil || stopped {
			return err
		}
	}
}

func resizeDirectionFlag(direction PaneResizeDirection) string {
	switch direction {
	case ResizePaneUp:
		return "-U"
	case ResizePaneDown:
		return "-D"
	case ResizePaneRight:
		return "-R"
	default:
		return "-L"
	}
}

func TestTranslateApplicationCursor(t *testing.T) {
	got, consumed, ok := translateApplicationCursor([]byte("\x1b[Drest"), true)
	if !ok || consumed != 3 || !bytes.Equal(got, []byte("\x1bOD")) {
		t.Fatalf("translation=%q consumed=%d ok=%v", got, consumed, ok)
	}
	if _, _, ok := translateApplicationCursor([]byte("\x1b[D"), false); ok {
		t.Fatal("translated while mode disabled")
	}
}

func TestServerConsumesPrefixCommandsAndForwardsLiteralBytes(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	if event := clientForState(s).ConsumeInputByte('a'); event.Command != serverCommandLiteral || event.Byte != 'a' {
		t.Fatalf("normal input event = %#v", event)
	}
	if event := clientForState(s).ConsumeInputByte(0x02); event.Command != serverCommandNone {
		t.Fatalf("prefix start event = %#v", event)
	}
	if event := clientForState(s).ConsumeInputByte('['); !isCommandInput(event, "copy-mode") {
		t.Fatalf("history prefix event = %#v", event)
	}
	clientForState(s).ConsumeInputByte(0x02)
	if event := clientForState(s).ConsumeInputByte(0x02); event.Command != serverCommandLiteral || event.Byte != 0x02 {
		t.Fatalf("literal prefix event = %#v", event)
	}
}

func TestEveryPrefixBindingExpandsToCanonicalCommandArgs(t *testing.T) {
	bindings := []struct {
		key  byte
		argv []string
	}{
		{'c', []string{"new-window"}},
		{' ', []string{"next-layout"}},
		{'%', []string{"split-window", "-h"}},
		{'"', []string{"split-window", "-v"}},
		{'d', []string{"detach-client"}},
		{'n', []string{"next-window"}},
		{'p', []string{"previous-window"}},
		{'l', []string{"last-window"}},
		{'x', []string{"kill-pane"}},
		{'z', []string{"resize-pane", "-Z"}},
		{'[', []string{"copy-mode"}},
		{']', []string{"paste-buffer"}},
		{'{', []string{"swap-pane", "-U"}},
		{'}', []string{"swap-pane", "-D"}},
		{',', []string{"rename-window"}},
		{'$', []string{"rename-session"}},
	}
	for _, binding := range bindings {
		s := NewSessionState(1)
		newTestClient(s)
		clientForState(s).ConsumeInputByte(0x02)
		if event := clientForState(s).ConsumeInputByte(binding.key); !isCommandInput(event, binding.argv...) {
			t.Errorf("prefix %q = %#v, want %v", binding.key, event, binding.argv)
		}
	}
	s := NewSessionState(1)
	newTestClient(s)
	clientForState(s).ConsumeInputByte(0x02)
	if event := clientForState(s).ConsumeInputByte(':'); event.Command != serverCommandOpenCommandPrompt {
		t.Errorf("prefix ':' = %#v, want local command prompt", event)
	}
}

func TestServerRecognizesRenameSessionPrefix(t *testing.T) {
	s := NewSessionState(1)
	newTestClient(s)
	clientForState(s).ConsumeInputByte(0x02)
	if event := clientForState(s).ConsumeInputByte('$'); !isCommandInput(event, "rename-session") {
		t.Fatalf("rename-session event = %#v", event)
	}
}

func TestServerRecognizesSwapPanePrefixes(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	for key, want := range map[byte][]string{
		'{': {"swap-pane", "-U"},
		'}': {"swap-pane", "-D"},
	} {
		clientForState(s).ConsumeInputByte(0x02)
		if event := clientForState(s).ConsumeInputByte(key); !isCommandInput(event, want...) {
			t.Fatalf("prefix %q event = %#v, want command %v", key, event, want)
		}
	}
}

func TestServerRecognizesToggleZoomPrefix(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	clientForState(s).ConsumeInputByte(0x02)
	if event := clientForState(s).ConsumeInputByte('z'); !isCommandInput(event, "resize-pane", "-Z") {
		t.Fatalf("toggle zoom event = %#v", event)
	}
}

func TestClosePanePromptsBeforeKilling(t *testing.T) {
	s := NewSessionState(1)
	newTestClient(s)
	first := &Pane{ID: testAddPaneID(s), Title: "first"}
	createTestWindow(s, first)
	second := &Pane{ID: testAddPaneID(s), Title: "second"}
	if _, _, err := splitTestFocusedPane(s, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)
	clientForState(s).ConsumeInputByte(0x02)
	event := clientForState(s).ConsumeInputByte('x')
	if !isCommandInput(event, "kill-pane") {
		t.Fatalf("close-pane event = %#v", event)
	}
	if _, err := clientForState(s).handleServerInputEvent(event); err != nil {
		t.Fatal(err)
	}
	prompt := clientForState(s).ActivePrompt()
	if prompt == nil || prompt.Mode != PromptModeConfirm || prompt.Label != "kill-pane? (y/N) " {
		t.Fatalf("close-pane confirmation prompt = %#v", prompt)
	}
	if s.Pane(second.ID) == nil {
		t.Fatal("pane was killed before confirmation")
	}

	if _, err := resolveTestPrompt(clientForState(s), false, ""); err != nil {
		t.Fatal(err)
	}
	if clientForState(s).ActivePrompt() != nil || s.Pane(second.ID) == nil {
		t.Fatalf("default-No confirmation changed pane state: prompt=%#v pane=%#v", clientForState(s).ActivePrompt(), s.Pane(second.ID))
	}

	clientForState(s).ConsumeInputByte(0x02)
	event = clientForState(s).ConsumeInputByte('x')
	if _, err := clientForState(s).handleServerInputEvent(event); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTestPrompt(clientForState(s), true, "y"); err != nil {
		t.Fatal(err)
	}
	if clientForState(s).ActivePrompt() != nil || s.Pane(second.ID) != nil {
		t.Fatalf("confirmed pane close did not complete: prompt=%#v pane=%#v", clientForState(s).ActivePrompt(), s.Pane(second.ID))
	}
	got, _ := testActivePane(s)
	if got != first {
		t.Fatalf("active pane after close = %#v, want %#v", got, first)
	}
}

func TestStalePromptResultDoesNotResolveActivePrompt(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	window, _ := createTestWindow(s, &Pane{
		ID: testAddPaneID(s), Title: "before", terminal: newTerminal(80, 24),
	})
	if _, err := executeTestClientCommand(client, []string{"rename-window"}); err != nil {
		t.Fatal(err)
	}
	prompt := client.ActivePrompt()
	if prompt == nil {
		t.Fatal("rename-window did not open a prompt")
	}
	if _, err := client.resolvePrompt(protocol.FrontendPromptResult{
		PromptID: prompt.ID + 1, Submitted: true, Text: "stale",
	}); err != nil {
		t.Fatal(err)
	}
	if active := client.ActivePrompt(); active == nil || active.ID != prompt.ID {
		t.Fatalf("stale result changed active prompt: %#v", active)
	}
	if window.Name != "before" {
		t.Fatalf("stale result renamed window to %q", window.Name)
	}
	if _, err := client.resolvePrompt(protocol.FrontendPromptResult{
		PromptID: prompt.ID, Submitted: true, Text: "after",
	}); err != nil {
		t.Fatal(err)
	}
	if client.ActivePrompt() != nil || window.Name != "after" {
		t.Fatalf("matching result left prompt=%#v window=%q", client.ActivePrompt(), window.Name)
	}
}

func TestRepeatedDetachInputExitsOnFirstAttempt(t *testing.T) {
	s := NewSessionState(1)
	newTestClient(s)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash", terminal: newTerminal(80, 24)})
	var input bytes.Buffer
	payload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{Data: []byte{0x02, 'd', 0x02, 'd'}})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	handler := newClientInstance(nil, nil)
	setTestClient(s, handler)
	if err := handleTestControlFrames(s, handler, protocol.NewDecoder(bytes.NewReader(input.Bytes()), protocol.DefaultMaxFrameSize)); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchSessionPromptAppliesPreparedTransition(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSessionState(1)
	target := NewSessionState(2)
	t.Cleanup(func() { stopState(source) })
	t.Cleanup(func() { stopState(target) })
	target.setSessionName("logs")
	d.sessions[source.ID] = source
	d.sessions[target.ID] = target
	d.names[target.Name] = target
	source.daemon = d
	target.daemon = d
	d.ensureSessionGroupInActor(source)
	d.ensureSessionGroupInActor(target)
	fixtureClient := newTestClient(source)
	fixtureClient.setTestTerminalSize(90, 28)
	createTestWindow(source, &Pane{ID: testAddPaneID(source), terminal: newTerminal(90, 28)})
	createTestWindow(target, &Pane{ID: testAddPaneID(target), terminal: newTerminal(90, 28)})
	client := clientForState(source)
	identity := testClientIdentity(client)
	identity.ResumeToken = "input-switch"
	identity.lastAllocatedClientLayoutRevision = client.currentView.Layout.LayoutRevision
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{WindowID: source.ActiveWindowID, SessionID: source.ID, ClientID: client.clientID, Generation: 1}

	payload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{Data: []byte{0x02, ':'}})
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := handleTestControlFrames(source, client, protocol.NewDecoder(&input, protocol.DefaultMaxFrameSize)); err != nil {
		t.Fatal(err)
	}
	prompt := client.ActivePrompt()
	resultPayload, err := protocol.EncodeFrontendPromptResult(nil, protocol.FrontendPromptResult{
		PromptID: prompt.ID, Submitted: true, Text: "switch-session -t logs",
	})
	if err != nil {
		t.Fatal(err)
	}
	input.Reset()
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgFrontendPromptResult, Payload: resultPayload}); err != nil {
		t.Fatal(err)
	}
	if err := handleTestControlFrames(source, client, protocol.NewDecoder(&input, protocol.DefaultMaxFrameSize)); err != nil {
		t.Fatal(err)
	}
	if testClientSession(client) != target {
		t.Fatalf("command prompt left client in session %#v, want %#v", testClientSession(client), target)
	}
}

func TestServerParsesPrefixArrowAndWindowIndex(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	for _, b := range []byte{0x02, 0x1b, '[', 'A'} {
		event := clientForState(s).ConsumeInputByte(b)
		if b == 'A' && !isCommandInput(event, "select-pane", "-U") {
			t.Fatalf("prefix arrow event = %#v", event)
		}
	}
	clientForState(s).ConsumeInputByte(0x02)
	if event := clientForState(s).ConsumeInputByte('3'); !isCommandInput(event, "select-window", "-t", ":3") {
		t.Fatalf("numeric window event = %#v", event)
	}
}

func TestServerParsesModifiedPrefixArrowsForResize(t *testing.T) {
	tests := []struct {
		name      string
		sequence  []byte
		direction PaneResizeDirection
		amount    int
	}{
		{name: "control up", sequence: []byte("\x1b[1;5A"), direction: ResizePaneUp, amount: 1},
		{name: "control down", sequence: []byte("\x1b[1;5B"), direction: ResizePaneDown, amount: 1},
		{name: "control right", sequence: []byte("\x1b[1;5C"), direction: ResizePaneRight, amount: 1},
		{name: "control left", sequence: []byte("\x1b[1;5D"), direction: ResizePaneLeft, amount: 1},
		{name: "parameterized meta", sequence: []byte("\x1b[1;3D"), direction: ResizePaneLeft, amount: 5},
		{name: "escape-prefixed meta", sequence: []byte("\x1b\x1b[A"), direction: ResizePaneUp, amount: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := NewSessionState(0)
			newTestClient(s)
			clientForState(s).ConsumeInputByte(0x02)
			var event serverInputEvent
			for _, b := range test.sequence {
				event = clientForState(s).ConsumeInputByte(b)
			}
			if !isCommandInput(event, "resize-pane", resizeDirectionFlag(test.direction), strconv.Itoa(test.amount)) {
				t.Fatalf("resize event = %#v", event)
			}
			client := snapshotTestClient(s)
			if client.InputState != serverInputNormal || len(client.PrefixEscape) != 0 {
				t.Fatalf("parser did not reset after resize: %#v", client)
			}
		})
	}
}

func TestServerResetsOverlongPrefixCSI(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	clientForState(s).ConsumeInputByte(0x02)
	for _, b := range append([]byte("\x1b["), []byte("11111111111111111111111111111111")...) {
		clientForState(s).ConsumeInputByte(b)
	}
	client := snapshotTestClient(s)
	if client.InputState != serverInputNormal || len(client.PrefixEscape) != 0 {
		t.Fatalf("overlong CSI left parser active: %#v", client)
	}
}

func TestPaneResizeBindingRepeatsWithoutPrefix(t *testing.T) {
	client := &clientInputState{}
	now := time.Unix(100, 0)
	var event serverInputEvent
	for _, b := range append([]byte{0x02}, []byte("\x1b[1;5C")...) {
		event = consumeInputByteAt(client, b, now)
	}
	if !isCommandInput(event, "resize-pane", "-R", "1") {
		t.Fatalf("initial resize event = %#v", event)
	}
	if want := now.Add(paneResizeRepeatWindow); !client.ResizeRepeatUntil.Equal(want) {
		t.Fatalf("repeat deadline = %v, want %v", client.ResizeRepeatUntil, want)
	}

	repeatedAt := now.Add(100 * time.Millisecond)
	for _, b := range []byte("\x1b[1;5C") {
		event = consumeInputByteAt(client, b, repeatedAt)
	}
	if !isCommandInput(event, "resize-pane", "-R", "1") {
		t.Fatalf("repeated resize event = %#v", event)
	}
	if want := repeatedAt.Add(paneResizeRepeatWindow); !client.ResizeRepeatUntil.Equal(want) {
		t.Fatalf("rearmed deadline = %v, want %v", client.ResizeRepeatUntil, want)
	}
}

func TestPaneResizeRepeatCancellationPreservesInput(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		wantData []byte
	}{
		{name: "ordinary byte", input: []byte{'x'}, wantData: []byte{'x'}},
		{name: "plain arrow", input: []byte("\x1b[A"), wantData: []byte("\x1b[A")},
		{name: "unknown escape", input: []byte("\x1bx"), wantData: []byte("\x1bx")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &clientInputState{InputState: serverInputNormal, ResizeRepeatUntil: time.Unix(200, 0)}
			now := time.Unix(199, 750_000_000)
			var event serverInputEvent
			for _, b := range test.input {
				event = consumeInputByteAt(client, b, now)
			}
			if event.Command != serverCommandLiteral {
				t.Fatalf("cancellation event = %#v", event)
			}
			got := event.Data
			if len(got) == 0 {
				got = []byte{event.Byte}
			}
			if !bytes.Equal(got, test.wantData) {
				t.Fatalf("preserved input = %q, want %q", got, test.wantData)
			}
			if !client.ResizeRepeatUntil.IsZero() || client.InputState != serverInputNormal || len(client.PrefixEscape) != 0 {
				t.Fatalf("repeat state not cleared: %#v", client)
			}
		})
	}
}

func TestPaneResizeRepeatExpires(t *testing.T) {
	deadline := time.Unix(300, 0)
	client := &clientInputState{InputState: serverInputNormal, ResizeRepeatUntil: deadline}
	event := consumeInputByteAt(client, 0x1b, deadline)
	if event.Command != serverCommandLiteral || event.Byte != 0x1b {
		t.Fatalf("expired repeat event = %#v", event)
	}
	if !client.ResizeRepeatUntil.IsZero() {
		t.Fatalf("expired repeat deadline was not cleared: %v", client.ResizeRepeatUntil)
	}
}

func TestHandleInputBytesAppliesRepeatedPaneResize(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	left := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	input := append([]byte{0x02}, []byte("\x1b[1;5C\x1b[1;5C")...)
	if detach, err := clientForState(s).handleInputBytes(0, input); err != nil || detach {
		t.Fatalf("handleInputBytes() detach=%v err=%v", detach, err)
	}
	placements := s.Windows[client.testLayout().WindowID].Layout.Compute(Rect{Width: 80, Height: 24})
	if placements[0].Rect.Width != 41 || placements[1].Rect.Width != 38 {
		t.Fatalf("placements after repeated resize = %#v", placements)
	}
}

func TestPromptBlocksOrdinaryInputAndAcceptsStructuredResult(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pane := &Pane{ID: testAddPaneID(s), PTY: writer, terminal: newTerminal(80, 23), Title: "bash"}
	window, _ := createTestWindow(s, pane)
	handler := &ClientInstance{controlOut: make(chan protocol.Frame, 8)}
	setTestClient(s, handler)
	if _, err := executeTestClientCommand(clientForState(s), []string{"rename-window"}); err != nil {
		t.Fatal(err)
	}

	var input bytes.Buffer
	payload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{Data: []byte("LEAK")})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	prompt := clientForState(s).ActivePrompt()
	resultPayload, err := protocol.EncodeFrontendPromptResult(nil, protocol.FrontendPromptResult{
		PromptID: prompt.ID, Submitted: true, Text: "bashx",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgFrontendPromptResult, Payload: resultPayload}); err != nil {
		t.Fatal(err)
	}
	state := s
	if err := handleTestControlFrames(state, handler, protocol.NewDecoder(bytes.NewReader(input.Bytes()), protocol.DefaultMaxFrameSize)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ordinary input while prompt active leaked to PTY: %q", got)
	}
	if window.Name != "bashx" || clientForState(s).ActivePrompt() != nil {
		t.Fatalf("prompt termination state window=%q prompt=%#v", window.Name, clientForState(s).ActivePrompt())
	}
}

func TestUTF8InputFrameIsForwardedIntact(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pane := &Pane{ID: testAddPaneID(s), PTY: writer, terminal: newTerminal(80, 23), Title: "bash"}
	createTestWindow(s, pane)

	want := []byte("你好，世界")
	payload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{Data: want})
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	state := s
	handler := &ClientInstance{controlOut: make(chan protocol.Frame, 1)}
	setTestClient(state, handler)
	if err := handleTestControlFrames(state, handler, protocol.NewDecoder(bytes.NewReader(input.Bytes()), protocol.DefaultMaxFrameSize)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("PTY input = %q, want %q", got, want)
	}
}

func TestStaleTransportInputIsIgnoredAfterReconnect(t *testing.T) {
	s := NewSessionState(0)
	fixtureClient := newTestClient(s)
	fixtureClient.setTestTerminalSize(80, 24)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)})
	client := newClientInstance(nil, nil)
	client.Daemon = s.daemon
	client.sessionID = s.ID
	currentClient := newClientInstance(nil, nil)
	currentClient.terminalCols = 80
	currentClient.terminalRows = 24
	setTestClient(s, currentClient)
	client.connection.revoke()

	payload, err := protocol.EncodeFrontendResize(nil, protocol.FrontendResize{Cols: 40, Rows: 12})
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgFrontendResize, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := handleTestControlFrames(s, client, protocol.NewDecoder(&input, protocol.DefaultMaxFrameSize)); err != nil {
		t.Fatal(err)
	}
	current := clientForState(s)
	if cols, rows := current.terminalCols, current.terminalRows; cols != 80 || rows != 24 {
		t.Fatalf("stale resize changed client size to %dx%d", cols, rows)
	}
}

func TestStandaloneWindowNavigationBookkeeping(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	first := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)}
	window1, _ := createTestWindow(s, first)
	second := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)}
	window2, _ := createTestWindow(s, second)
	if got, ok := s.daemon.windowSelectionTarget(s.ID, 0, true); !ok || got != window1.ID {
		t.Fatalf("LastWindowID() = %d, %v; want %d, true", got, ok, window1.ID)
	}
	if got, ok := s.daemon.windowSelectionTarget(s.ID, 1, false); !ok || got != window1.ID {
		t.Fatalf("RelativeWindowID(+1) = %d, %v; want %d, true", got, ok, window1.ID)
	}
	if _, _, err := selectTestSessionWindow(s, window1.ID); err != nil {
		t.Fatalf("SelectWindow() error = %v", err)
	}
	if got, ok := s.daemon.windowSelectionTarget(s.ID, 0, true); !ok || got != window2.ID {
		t.Fatalf("LastWindowID() after selection = %d, %v; want %d, true", got, ok, window2.ID)
	}
}

func TestDirectionalFocusGeometryHandlesPaneZero(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	top := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)}
	createTestWindow(s, top)
	bottom := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)}
	if _, _, err := splitTestFocusedPane(s, bottom, SplitHorizontal); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	_, state, err := clientForState(s).FocusPaneDirection('A')
	if err != nil || state.FocusedPaneID != top.ID {
		t.Fatalf("FocusPaneDirection(up) = state %#v err=%v; want pane %d", state, err, top.ID)
	}
}

func TestDirectionalFocusRemembersPositionAcrossUnevenPanes(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	left := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	createTestWindow(s, left)
	topRight := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	if _, _, err := splitTestFocusedPane(s, topRight, SplitVertical); err != nil {
		t.Fatal(err)
	}
	bottomRight := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	if _, _, err := splitTestFocusedPane(s, bottomRight, SplitHorizontal); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)

	move := func(direction byte, want uint64) {
		t.Helper()
		_, state, err := clientForState(s).FocusPaneDirection(direction)
		if err != nil {
			t.Fatal(err)
		}
		if state.FocusedPaneID != want {
			t.Fatalf("direction %q focused pane %d, want %d", direction, state.FocusedPaneID, want)
		}
	}

	move('A', topRight.ID)
	move('D', left.ID)
	move('C', topRight.ID)
	move('B', bottomRight.ID)
	move('D', left.ID)
	move('C', bottomRight.ID)
}
