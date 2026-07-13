package server

import (
	"strings"
	"testing"

	"tali/internal/protocol"
)

func TestRenameWindowPromptRendersEditsSubmitAndCancel(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	window, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash"}, 0)
	frames := make(chan protocol.Frame, 32)
	state := &sessionState{session: s}
	ctrl := &controller{
		state:      state,
		mgmtFrames: frames,
	}
	state.attachConnection(frames, nil)

	s.ConsumeInputByte(0, 0x02)
	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, ',')); err != nil {
		t.Fatal(err)
	}
	status := readStatusBar(t, frames)
	assertStatusText(t, status, "(rename-window) bash")
	if got := status.Styles[0].Style; got.FG != (protocol.Color{Mode: "indexed", Index: 0}) || got.BG != (protocol.Color{Mode: "indexed", Index: 3}) {
		t.Fatalf("prompt style = %#v", got)
	}
	for i, cell := range status.Cells {
		if cell.StyleID != 0 || cell.Width != 1 {
			t.Fatalf("status cell %d = %#v, want style 0 width 1", i, cell)
		}
	}

	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, 'x')); err != nil {
		t.Fatal(err)
	}
	readStatusBar(t, frames)
	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, 0x7f)); err != nil {
		t.Fatal(err)
	}
	readStatusBar(t, frames)

	for _, b := range []byte("xy") {
		if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, b)); err != nil {
			t.Fatal(err)
		}
		readStatusBar(t, frames)
	}
	consumed, events, terminated := s.ConsumePromptInput(0, []byte("\x1b[3~"))
	if consumed != 4 || len(events) != 1 || terminated {
		t.Fatalf("delete sequence consumed=%d events=%#v", consumed, events)
	}
	if err := runStatusEvent(t, ctrl, events[0]); err != nil {
		t.Fatal(err)
	}
	readStatusBar(t, frames)

	for i := 0; i < len("bashx"); i++ {
		if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, 0x7f)); err != nil {
			t.Fatal(err)
		}
		readStatusBar(t, frames)
	}
	for _, b := range []byte("zsh") {
		if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, b)); err != nil {
			t.Fatal(err)
		}
		readStatusBar(t, frames)
	}
	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, '\r')); err != nil {
		t.Fatal(err)
	}
	status = readStatusBar(t, frames)
	assertStatusText(t, status, "[0] 0:zsh* ")
	if window.Name != "zsh" || s.ActivePrompt(0) != nil {
		t.Fatalf("submitted window = %#v prompt=%#v", window, s.ActivePrompt(0))
	}

	s.ConsumeInputByte(0, 0x02)
	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, ',')); err != nil {
		t.Fatal(err)
	}
	readStatusBar(t, frames)
	s.ConsumeInputByte(0, 0x1b)
	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, 'x')); err != nil {
		t.Fatal(err)
	}
	status = readStatusBar(t, frames)
	assertStatusText(t, status, "[0] 0:zsh* ")
	if window.Name != "zsh" {
		t.Fatalf("cancel changed window name to %q", window.Name)
	}

	s.ConsumeInputByte(0, 0x02)
	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, ',')); err != nil {
		t.Fatal(err)
	}
	readStatusBar(t, frames)
	if err := runStatusEvent(t, ctrl, s.ConsumeInputByte(0, 0x03)); err != nil {
		t.Fatal(err)
	}
	status = readStatusBar(t, frames)
	assertStatusText(t, status, "[0] 0:zsh* ")
}

func runStatusEvent(t *testing.T, ctrl *controller, event serverInputEvent) error {
	t.Helper()
	_, err := ctrl.handleServerInputEvent(event)
	return err
}

func readStatusBar(t *testing.T, frames <-chan protocol.Frame) protocol.StatusBar {
	t.Helper()
	frame := <-frames
	if frame.Type != protocol.MsgStatusBar {
		t.Fatalf("frame type = %d, want STATUS_BAR", frame.Type)
	}
	status, err := protocol.DecodeStatusBar(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func assertStatusText(t *testing.T, status protocol.StatusBar, want string) {
	t.Helper()
	text := make([]rune, 0, len(status.Cells))
	for _, cell := range status.Cells {
		text = append(text, cell.Rune)
	}
	if got := strings.TrimRight(string(text), " "); strings.TrimRight(want, " ") != got {
		t.Fatalf("status text = %q, want %q", got, strings.TrimRight(want, " "))
	}
}
