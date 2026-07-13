package server

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"tali/internal/server/terminal"
)

func TestPaneWriterSerializesNetworkInputAndDeviceReply(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	pane := &Pane{ID: 1, PTY: writer, terminal: terminal.New(8, 3)}
	pane.initializeRuntime()
	state := &sessionState{session: NewSession(0)}
	go state.runPane(pane)
	writeFailed := make(chan error, 1)
	go runPTYWriter(pane, func(err error) { writeFailed <- err })

	if err := pane.sendInput([]byte("user")); err != nil {
		t.Fatal(err)
	}
	query := ptyReadBuffers.Get().([]byte)
	n := copy(query, "\x1b[?1h\x1b[6n")
	pane.ptyOutput <- query[:n]

	want := []byte("user\x1b[1;1R")
	got := make([]byte, len(want))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(reader, got)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-writeFailed:
		t.Fatalf("PTY writer failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pane input")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("PTY input = %q, want %q", got, want)
	}
	if !pane.UsesApplicationCursorKeys() {
		t.Fatal("pane main loop did not publish application cursor mode")
	}

	close(pane.ptyOutput)
	<-pane.mainDone
	pane.stop()
	<-pane.writerDone
}

func TestPaneResizeRunsOnPaneMainLoop(t *testing.T) {
	pane := &Pane{ID: 1, terminal: terminal.New(8, 3)}
	pane.initializeRuntime()
	state := &sessionState{session: NewSession(0)}
	go state.runPane(pane)
	defer func() {
		close(pane.ptyOutput)
		<-pane.mainDone
	}()

	if err := pane.resize(12, 5); err != nil {
		t.Fatal(err)
	}
	if cols, rows := pane.TerminalSize(); cols != 12 || rows != 5 {
		t.Fatalf("terminal size = %dx%d, want 12x5", cols, rows)
	}
}
