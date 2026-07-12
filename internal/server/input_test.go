package server

import (
	"bytes"
	"testing"

	"tali/internal/server/terminal"
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

func TestServerOwnsLastAndRelativeWindowSelection(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	first := &Pane{ID: s.AddPaneID(), Terminal: terminal.New(80, 23)}
	window1, _ := s.CreateWindow(first, 0)
	second := &Pane{ID: s.AddPaneID(), Terminal: terminal.New(80, 23)}
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
	top := &Pane{ID: s.AddPaneID(), Terminal: terminal.New(80, 23)}
	s.CreateWindow(top, 0)
	bottom := &Pane{ID: s.AddPaneID(), Terminal: terminal.New(80, 23)}
	if _, _, err := s.SplitFocusedPane(0, bottom, SplitHorizontal); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	_, state, err := s.FocusPaneDirection(0, 'A')
	if err != nil || state.FocusedPaneID != top.ID {
		t.Fatalf("FocusPaneDirection(up) = state %#v err=%v; want pane %d", state, err, top.ID)
	}
}
