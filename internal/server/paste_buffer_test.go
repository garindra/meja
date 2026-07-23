package server

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestPasteBufferStoreAutomaticLimitAndNewestSelection(t *testing.T) {
	var store pasteBufferStore
	store.limit = 2
	first, err := store.addAutomatic([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.addAutomatic([]byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.addAutomatic([]byte("third"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second == third {
		t.Fatalf("automatic names = %q, %q, %q", first, second, third)
	}
	if _, _, exists := store.get(first); exists {
		t.Fatalf("oldest automatic buffer %q was not evicted", first)
	}
	data, name, exists := store.get("")
	if !exists || name != third || string(data) != "third" {
		t.Fatalf("newest automatic buffer = %q %q %v", name, data, exists)
	}
}

func TestPasteBufferCommandsRoundTripAndDelete(t *testing.T) {
	d := newCommandTestDaemon(t)
	set := d.executeCommand(protocol.CommandRequest{Args: []string{"set-buffer", "hello\nworld"}})
	if set.exitCode != 0 {
		t.Fatalf("set-buffer = %#v", set)
	}
	show := d.executeCommand(protocol.CommandRequest{Args: []string{"show-buffer"}})
	if show.exitCode != 0 || string(show.stdout) != "hello\nworld" {
		t.Fatalf("show-buffer = %#v", show)
	}
	list := d.executeCommand(protocol.CommandRequest{Args: []string{"list-buffers"}})
	if list.exitCode != 0 || !strings.Contains(string(list.stdout), "buffer0001: 11 bytes") {
		t.Fatalf("list-buffers = %#v", list)
	}

	setNamed := d.executeCommand(protocol.CommandRequest{Args: []string{"set-buffer", "-b", "named", "value"}})
	if setNamed.exitCode != 0 {
		t.Fatalf("set-buffer named = %#v", setNamed)
	}
	delete := d.executeCommand(protocol.CommandRequest{Args: []string{"delete-buffer", "-b", "named"}})
	if delete.exitCode != 0 {
		t.Fatalf("delete-buffer = %#v", delete)
	}
	missing := d.executeCommand(protocol.CommandRequest{Args: []string{"show-buffer", "-b", "named"}})
	if missing.exitCode == 0 {
		t.Fatalf("deleted buffer still exists: %#v", missing)
	}

	input := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(input, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := d.executeCommand(protocol.CommandRequest{Args: []string{"load-buffer", "-b", "file", input}})
	if loaded.exitCode != 0 {
		t.Fatalf("load-buffer = %#v", loaded)
	}
	output := filepath.Join(t.TempDir(), "output.txt")
	saved := d.executeCommand(protocol.CommandRequest{Args: []string{"save-buffer", "-b", "file", output}})
	if saved.exitCode != 0 {
		t.Fatalf("save-buffer = %#v", saved)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "from file" {
		t.Fatalf("saved buffer = %q, err=%v", data, err)
	}
}

func TestPasteBufferCommandUsesTmuxSeparatorsAndDeletes(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(1)
	t.Cleanup(func() { stopState(s) })
	s.daemon = d
	s.setSessionName("work")
	client := newStandaloneClient(s)
	client.TerminalCols, client.TerminalRows = 8, 1
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	pane := &Pane{ID: testAddPaneID(s), PTY: writer, terminal: newTerminal(8, 1)}
	pane.metadata.Store(&paneTerminalMetadata{bracketedPaste: true})
	createTestWindow(s, pane)
	d.sessions[s.ID] = s
	d.names[s.Name] = s

	set := d.executeCommand(protocol.CommandRequest{Args: []string{"set-buffer", "-b", "named", "one\ntwo"}})
	if set.exitCode != 0 {
		t.Fatalf("set buffer = %#v", set)
	}
	pasted := d.executeCommand(protocol.CommandRequest{Args: []string{"paste-buffer", "-t", "work", "-b", "named", "-p", "-d"}})
	if pasted.exitCode != 0 {
		t.Fatalf("paste buffer = %#v", pasted)
	}
	got := make([]byte, len("\x1b[200~one\rtwo\x1b[201~"))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read pasted data: %v", err)
	}
	if want := []byte("\x1b[200~one\rtwo\x1b[201~"); !bytes.Equal(got, want) {
		t.Fatalf("pasted bytes = %q, want %q", got, want)
	}
	missing := d.executeCommand(protocol.CommandRequest{Args: []string{"show-buffer", "-b", "named"}})
	if missing.exitCode == 0 {
		t.Fatalf("-d did not delete buffer: %#v", missing)
	}
}
