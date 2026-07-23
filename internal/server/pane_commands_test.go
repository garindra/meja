package server

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestEncodeSendKeysUsesPaneKeyEncoding(t *testing.T) {
	got, err := encodeSendKeys([]string{"C-a", "M-x", "Enter", "Up", "hello"}, false, paneTerminalMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 0x1b, 'x', '\r', 0x1b, '[', 'A'}
	want = append(want, []byte("hello")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded keys = %q, want %q", got, want)
	}
}

func TestEncodeSendKeysLiteralPreservesArguments(t *testing.T) {
	got, err := encodeSendKeys([]string{"hello world", "\x00"}, true, paneTerminalMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte("hello world\x00"); !bytes.Equal(got, want) {
		t.Fatalf("literal keys = %q, want %q", got, want)
	}
}

func TestCaptureTerminalViewportTrimsOrPreservesTrailingSpaces(t *testing.T) {
	terminal := newTerminal(8, 3)
	terminal.Apply([]byte("abc"))

	trimmed, err := captureTerminalViewport(terminal, capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "abc\n\n\n"; string(trimmed) != want {
		t.Fatalf("trimmed capture = %q, want %q", trimmed, want)
	}

	preserved, err := captureTerminalViewport(terminal, capturePaneOptions{preserveTrailing: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "abc     \n" + strings.Repeat(" ", 8) + "\n" + strings.Repeat(" ", 8) + "\n"
	if string(preserved) != want {
		t.Fatalf("preserved capture = %q, want %q", preserved, want)
	}
}

func TestCaptureTerminalViewportHandlesWideClustersAndWrappedLines(t *testing.T) {
	terminal := newTerminal(4, 2)
	terminal.Apply([]byte("🙂abc"))

	plain, err := captureTerminalViewport(terminal, capturePaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "🙂ab\nc\n"; string(plain) != want {
		t.Fatalf("plain capture = %q, want %q", plain, want)
	}

	joined, err := captureTerminalViewport(terminal, capturePaneOptions{joinWrapped: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := "🙂abc\n"; string(joined) != want {
		t.Fatalf("joined capture = %q, want %q", joined, want)
	}
}

func TestPaneCommandsWorkThroughTheDaemonCommandEngine(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(1)
	t.Cleanup(func() { stopState(s) })
	s.daemon = d
	s.setSessionName("work")
	client := newTestClient(s)
	client.setTestTerminalSize(8, 1)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	pane := &Pane{ID: testAddPaneID(s), PTY: writer, terminal: newTerminal(8, 1)}
	pane.terminal.Apply([]byte("screen"))
	createTestWindow(s, pane)
	d.sessions[s.ID] = s
	d.names[s.Name] = s

	sent := d.executeCommand(protocol.CommandRequest{Args: []string{"send-keys", "-t", "work", "C-a", "M-x", "Enter"}})
	if sent.exitCode != 0 {
		t.Fatalf("send-keys result = %#v", sent)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read sent keys: %v", err)
	}
	if want := []byte{1, 0x1b, 'x', '\r'}; !bytes.Equal(got, want) {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}

	captured := d.executeCommand(protocol.CommandRequest{Args: []string{"capture-pane", "-t", "work", "-p"}})
	if captured.exitCode != 0 {
		t.Fatalf("capture-pane result = %#v", captured)
	}
	if want := "screen\n"; string(captured.stdout) != want {
		t.Fatalf("captured text = %q, want %q", captured.stdout, want)
	}
	stored := d.executeCommand(protocol.CommandRequest{Args: []string{"capture-pane", "-t", "work"}})
	if stored.exitCode != 0 {
		t.Fatalf("stored capture = %#v", stored)
	}
	buffer := d.executeCommand(protocol.CommandRequest{Args: []string{"show-buffer"}})
	if buffer.exitCode != 0 || string(buffer.stdout) != "screen\n" {
		t.Fatalf("captured buffer = %#v", buffer)
	}
}

func TestCapturePaneSupportsHistoryRangesAndEscapes(t *testing.T) {
	oldest, err := parseCapturePaneArgs([]string{"-p", "-S", "-"})
	if err != nil {
		t.Fatal(err)
	}
	if !oldest.startHistory || !oldest.print {
		t.Fatalf("oldest capture options = %#v", oldest)
	}
	lastLines, err := parseCapturePaneArgs([]string{"-p", "-e", "-S", "-100"})
	if err != nil {
		t.Fatal(err)
	}
	if !lastLines.escape || lastLines.startLine != -100 {
		t.Fatalf("history capture options = %#v", lastLines)
	}

	terminal := newTerminal(4, 2)
	terminal.Apply([]byte("1111\r\n2222\r\n3333\r\n"))

	all, err := captureTerminalViewport(terminal, capturePaneOptions{startSet: true, startHistory: true, endSet: true, endVisible: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(all), "1111\n") || !strings.Contains(string(all), "3333\n") {
		t.Fatalf("history capture = %q", all)
	}

	styled := newTerminal(8, 1)
	styled.Apply([]byte("\x1b[31mred"))
	withEscapes, err := captureTerminalViewport(styled, capturePaneOptions{escape: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withEscapes), "\x1b[") || !strings.Contains(string(withEscapes), "31") || !strings.HasSuffix(string(withEscapes), "\x1b[0m") {
		t.Fatalf("escaped capture = %q", withEscapes)
	}
}

func TestSendKeysCopyModeCommandsAndPaneInModeFormat(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(1)
	t.Cleanup(func() { stopState(s) })
	s.daemon = d
	s.setSessionName("work")
	client := newTestClient(s)
	client.setTestTerminalSize(8, 2)
	pane := &Pane{ID: testAddPaneID(s), terminal: newTerminal(8, 2)}
	pane.terminal.Apply([]byte("history\r\nline"))
	createTestWindow(s, pane)
	d.sessions[s.ID] = s
	d.names[s.Name] = s

	entered := d.executeCommand(protocol.CommandRequest{Args: []string{"copy-mode", "-t", "work"}})
	if entered.exitCode != 0 || !pane.isHistoryMode() {
		t.Fatalf("copy-mode = %#v mode=%v", entered, pane.isHistoryMode())
	}
	scrolled := d.executeCommand(protocol.CommandRequest{Args: []string{"send-keys", "-t", "work", "-X", "scroll-up"}})
	if scrolled.exitCode != 0 {
		t.Fatalf("scroll-up = %#v", scrolled)
	}
	if err := pane.beginHistorySelection(0, 0, false); err != nil {
		t.Fatalf("begin selection = %v", err)
	}
	if err := pane.updateHistorySelection(0, 1); err != nil {
		t.Fatalf("update selection = %v", err)
	}
	copied := d.executeCommand(protocol.CommandRequest{Args: []string{"send-keys", "-t", "work", "-X", "copy-selection"}})
	if copied.exitCode != 0 {
		t.Fatalf("copy-selection = %#v", copied)
	}
	buffer := d.executeCommand(protocol.CommandRequest{Args: []string{"show-buffer"}})
	if buffer.exitCode != 0 || len(buffer.stdout) == 0 {
		t.Fatalf("copy-selection buffer = %#v", buffer)
	}
	mode := d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-t", "work", "-F", "#{pane_in_mode}"}})
	if mode.exitCode != 0 || string(mode.stdout) != "1\n" {
		t.Fatalf("pane_in_mode = %#v", mode)
	}
	cancelled := d.executeCommand(protocol.CommandRequest{Args: []string{"send-keys", "-t", "work", "-X", "cancel"}})
	if cancelled.exitCode != 0 || pane.isHistoryMode() {
		t.Fatalf("cancel = %#v mode=%v", cancelled, pane.isHistoryMode())
	}
}
