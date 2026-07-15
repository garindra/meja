package server

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/garindra/meja/internal/protocol"
	"github.com/garindra/meja/internal/server/terminal"
)

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
	s := NewSession(0)
	s.NewClient(0)
	if event := s.ConsumeInputByte(0, 'a'); event.Command != serverCommandLiteral || event.Byte != 'a' {
		t.Fatalf("normal input event = %#v", event)
	}
	if event := s.ConsumeInputByte(0, 0x02); event.Command != serverCommandNone {
		t.Fatalf("prefix start event = %#v", event)
	}
	if event := s.ConsumeInputByte(0, '['); event.Command != serverCommandEnterHistory {
		t.Fatalf("history prefix event = %#v", event)
	}
	s.ConsumeInputByte(0, 0x02)
	if event := s.ConsumeInputByte(0, 0x02); event.Command != serverCommandLiteral || event.Byte != 0x02 {
		t.Fatalf("literal prefix event = %#v", event)
	}
}

func TestServerRecognizesRenameSessionPrefix(t *testing.T) {
	s := NewSession(1)
	s.NewClient(0)
	s.ConsumeInputByte(0, 0x02)
	if event := s.ConsumeInputByte(0, '$'); event.Command != serverCommandBeginSessionPrompt {
		t.Fatalf("rename-session event = %#v", event)
	}
}

func TestServerRecognizesSwapPanePrefixes(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	for key, want := range map[byte]serverInputCommand{
		'{': serverCommandSwapPanePrevious,
		'}': serverCommandSwapPaneNext,
	} {
		s.ConsumeInputByte(0, 0x02)
		if event := s.ConsumeInputByte(0, key); event.Command != want {
			t.Fatalf("prefix %q event = %#v, want command %d", key, event, want)
		}
	}
}

func TestRepeatedDetachInputExitsOnFirstAttempt(t *testing.T) {
	s := NewSession(1)
	s.NewClient(0)
	s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash", terminal: terminal.New(80, 24)}, 0)
	var input bytes.Buffer
	payload, err := protocol.EncodeInputBytes(nil, protocol.InputBytes{Data: []byte{0x02, 'd', 0x02, 'd'}})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgInputBytes, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	handler := &Connection{Session: s}
	done := make(chan error, 1)
	handler.Session.readInputFrames(handler, protocol.NewDecoder(bytes.NewReader(input.Bytes()), protocol.DefaultMaxFrameSize), done)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerParsesPrefixArrowAndWindowIndex(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	for _, b := range []byte{0x02, 0x1b, '[', 'A'} {
		event := s.ConsumeInputByte(0, b)
		if b == 'A' && (event.Command != serverCommandFocusDirection || event.Direction != 'A') {
			t.Fatalf("prefix arrow event = %#v", event)
		}
	}
	s.ConsumeInputByte(0, 0x02)
	if event := s.ConsumeInputByte(0, '3'); event.Command != serverCommandSelectIndex || event.Index != 3 {
		t.Fatalf("numeric window event = %#v", event)
	}
}

func TestServerPromptEditsAndCancelsAuthoritatively(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	pane := &Pane{ID: s.AddPaneID(), Title: "bash"}
	window, _ := s.CreateWindow(pane, 0)

	s.ConsumeInputByte(0, 0x02)
	if event := s.ConsumeInputByte(0, ','); event.Command != serverCommandBeginWindowPrompt {
		t.Fatalf("rename prompt event = %#v", event)
	}
	if _, err := s.BeginRenameWindowPrompt(0); err != nil {
		t.Fatal(err)
	}
	if event := s.ConsumeInputByte(0, 'x'); event.Command != serverCommandPrompt || event.PromptAction != PromptActionChanged {
		t.Fatalf("prompt text event = %#v", event)
	}
	if got := string(s.ActivePrompt(0).Text); got != "bashx" {
		t.Fatalf("prompt text after typing = %q", got)
	}
	if event := s.ConsumeInputByte(0, 0x7f); event.Command != serverCommandPrompt || event.PromptAction != PromptActionChanged {
		t.Fatalf("backspace event = %#v", event)
	}
	if got := string(s.ActivePrompt(0).Text); got != "bash" {
		t.Fatalf("prompt text after backspace = %q", got)
	}
	for _, b := range []byte("xy") {
		s.ConsumeInputByte(0, b)
	}
	consumed, events, terminated := s.ConsumePromptInput(0, []byte("\x1b[3~"))
	if consumed != 4 || len(events) != 1 || events[0].PromptAction != PromptActionChanged || terminated {
		t.Fatalf("delete sequence consumed=%d events=%#v terminated=%v", consumed, events, terminated)
	}
	if got := string(s.ActivePrompt(0).Text); got != "bashx" {
		t.Fatalf("prompt text after delete = %q", got)
	}
	if event := s.ConsumeInputByte(0, 0x1b); event.Command != serverCommandNone {
		t.Fatalf("escape prefix event = %#v", event)
	}
	if event := s.ConsumeInputByte(0, 'x'); event.Command != serverCommandPrompt || event.PromptAction != PromptActionCancel {
		t.Fatalf("bare escape cancel event = %#v", event)
	}
	if s.ActivePrompt(0) != nil {
		t.Fatal("prompt remained active after escape")
	}
	if s.Windows[window.ID].Name != "bash" {
		t.Fatalf("cancel changed window name to %q", s.Windows[window.ID].Name)
	}
}

func TestPromptDeleteSequenceSurvivesEveryPayloadBoundary(t *testing.T) {
	sequence := []byte{0x1b, '[', '3', '~'}
	for boundary := 1; boundary < len(sequence); boundary++ {
		s := NewSession(0)
		s.NewClient(0)
		s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash"}, 0)
		if _, err := s.BeginRenameWindowPrompt(0); err != nil {
			t.Fatal(err)
		}
		for _, b := range []byte("x") {
			s.ConsumeInputByte(0, b)
		}

		consumed, events, terminated := s.ConsumePromptInput(0, sequence[:boundary])
		if consumed != boundary || len(events) != 0 || terminated {
			t.Fatalf("boundary %d first payload consumed=%d events=%#v terminated=%v", boundary, consumed, events, terminated)
		}
		prompt := s.ActivePrompt(0)
		if prompt == nil || !bytes.Equal(prompt.PendingEscape, sequence[:boundary]) {
			var pending []byte
			if prompt != nil {
				pending = prompt.PendingEscape
			}
			t.Fatalf("boundary %d pending escape=%#v prompt=%#v", boundary, pending, prompt)
		}

		consumed, events, terminated = s.ConsumePromptInput(0, sequence[boundary:])
		if consumed != len(sequence)-boundary || len(events) != 1 || events[0].PromptAction != PromptActionChanged || terminated {
			t.Fatalf("boundary %d second payload consumed=%d events=%#v terminated=%v", boundary, consumed, events, terminated)
		}
		if got := string(s.ActivePrompt(0).Text); got != "bash" {
			t.Fatalf("boundary %d prompt text=%q, want bash", boundary, got)
		}
	}
}

func TestPromptTerminationConsumesRemainderWithoutPTYLeak(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pane := &Pane{ID: s.AddPaneID(), PTY: writer, terminal: terminal.New(80, 23), Title: "bash"}
	window, _ := s.CreateWindow(pane, 0)
	if _, err := s.BeginRenameWindowPrompt(0); err != nil {
		t.Fatal(err)
	}

	var input bytes.Buffer
	payload, err := protocol.EncodeInputBytes(nil, protocol.InputBytes{Data: []byte("x\rLEAK")})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgInputBytes, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	state := s
	handler := &Connection{Session: state, managementOut: make(chan protocol.Frame, 8)}
	state.attachConnection(handler)
	done := make(chan error, 1)
	state.readInputFrames(handler, protocol.NewDecoder(bytes.NewReader(input.Bytes()), protocol.DefaultMaxFrameSize), done)
	if err := <-done; err != nil {
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
		t.Fatalf("prompt input leaked to PTY: %q", got)
	}
	if window.Name != "bashx" || s.ActivePrompt(0) != nil {
		t.Fatalf("prompt termination state window=%q prompt=%#v", window.Name, s.ActivePrompt(0))
	}
}

func TestUTF8InputFrameIsForwardedIntact(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pane := &Pane{ID: s.AddPaneID(), PTY: writer, terminal: terminal.New(80, 23), Title: "bash"}
	s.CreateWindow(pane, 0)

	want := []byte("你好，世界")
	payload, err := protocol.EncodeInputBytes(nil, protocol.InputBytes{Data: want})
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := protocol.NewEncoder(&input).WriteFrame(protocol.Frame{Type: protocol.MsgInputBytes, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	state := s
	handler := &Connection{Session: state, managementOut: make(chan protocol.Frame, 1)}
	state.attachConnection(handler)
	done := make(chan error, 1)
	state.readInputFrames(handler, protocol.NewDecoder(bytes.NewReader(input.Bytes()), protocol.DefaultMaxFrameSize), done)
	if err := <-done; err != nil {
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

func TestPromptBufferIsRuneAware(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash"}, 0)
	if _, err := s.BeginPrompt(0, PromptKindRenameWindow, "prompt ", "猫"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte("é") {
		s.ConsumeInputByte(0, b)
	}
	prompt := s.ActivePrompt(0)
	if got := string(prompt.Text); got != "猫é" || prompt.Cursor != 2 {
		t.Fatalf("rune prompt = %#v, want text 猫é cursor 2", prompt)
	}
	s.ConsumeInputByte(0, 0x7f)
	if got := string(s.ActivePrompt(0).Text); got != "猫" {
		t.Fatalf("rune prompt after backspace = %q", got)
	}
}

func TestServerOwnsLastAndRelativeWindowSelection(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	first := &Pane{ID: s.AddPaneID(), terminal: terminal.New(80, 23)}
	window1, _ := s.CreateWindow(first, 0)
	second := &Pane{ID: s.AddPaneID(), terminal: terminal.New(80, 23)}
	window2, _ := s.CreateWindow(second, 0)
	if got, ok := s.LastWindowID(0); !ok || got != window1.ID {
		t.Fatalf("LastWindowID() = %d, %v; want %d, true", got, ok, window1.ID)
	}
	if got, ok := s.RelativeWindowID(0, 1); !ok || got != window1.ID {
		t.Fatalf("RelativeWindowID(+1) = %d, %v; want %d, true", got, ok, window1.ID)
	}
	if _, _, err := s.SelectWindow(0, window1.ID); err != nil {
		t.Fatalf("SelectWindow() error = %v", err)
	}
	if got, ok := s.LastWindowID(0); !ok || got != window2.ID {
		t.Fatalf("LastWindowID() after selection = %d, %v; want %d, true", got, ok, window2.ID)
	}
}

func TestServerGeometricFocusHandlesPaneZero(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	top := &Pane{ID: s.AddPaneID(), terminal: terminal.New(80, 23)}
	s.CreateWindow(top, 0)
	bottom := &Pane{ID: s.AddPaneID(), terminal: terminal.New(80, 23)}
	if _, _, err := s.SplitFocusedPane(0, bottom, SplitHorizontal); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	_, state, err := s.FocusPaneDirection(0, 'A')
	if err != nil || state.FocusedPaneID != top.ID {
		t.Fatalf("FocusPaneDirection(up) = state %#v err=%v; want pane %d", state, err, top.ID)
	}
}

func TestDirectionalFocusRemembersPositionAcrossUnevenPanes(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	left := &Pane{ID: s.AddPaneID(), terminal: terminal.New(80, 24)}
	s.CreateWindow(left, 0)
	topRight := &Pane{ID: s.AddPaneID(), terminal: terminal.New(80, 24)}
	if _, _, err := s.SplitFocusedPane(0, topRight, SplitVertical); err != nil {
		t.Fatal(err)
	}
	bottomRight := &Pane{ID: s.AddPaneID(), terminal: terminal.New(80, 24)}
	if _, _, err := s.SplitFocusedPane(0, bottomRight, SplitHorizontal); err != nil {
		t.Fatal(err)
	}

	move := func(direction byte, want uint64) {
		t.Helper()
		_, state, err := s.FocusPaneDirection(0, direction)
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
