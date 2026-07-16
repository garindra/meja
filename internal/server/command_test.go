package server

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/garindra/meja/internal/protocol"
)

func TestNewSessionCommandCreatesInitialPaneBeforeAttach(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.snapshotDir = t.TempDir()
	result := d.executeCommand(protocol.CommandRequest{
		Args:             []string{"new", "-s", "work", "--", "/bin/sleep", "30"},
		WorkingDirectory: t.TempDir(),
		TerminalCols:     91,
		TerminalRows:     27,
	})
	defer d.disconnectActiveClients()
	if result.exitCode != 0 || result.bootstrap == nil {
		t.Fatalf("new result = %#v", result)
	}
	session := d.session(result.bootstrap.SessionID)
	if session == nil || !session.HasWindows() || len(session.PanesSnapshot()) != 1 {
		t.Fatalf("new session state = %#v", session)
	}
	pane, _ := session.ActivePane(clientID0)
	if pane == nil {
		t.Fatal("new session has no focused pane")
	}
	cols, rows := pane.TerminalSize()
	if cols != 91 || rows != 27 {
		t.Fatalf("initial pane size = %dx%d, want 91x27", cols, rows)
	}
	if got := pane.Launch.RequestedArgv; !reflect.DeepEqual(got, []string{"/bin/sleep", "30"}) {
		t.Fatalf("initial pane argv = %v", got)
	}
}

func TestCommandAliasesAreResolvedByDaemon(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.snapshotDir = t.TempDir()
	created := d.executeCommand(protocol.CommandRequest{Args: []string{"new", "-s", "work"}, TerminalCols: 80, TerminalRows: 23})
	defer d.disconnectActiveClients()
	if created.bootstrap == nil {
		t.Fatalf("new alias failed: %#v", created)
	}
	listed := d.executeCommand(protocol.CommandRequest{Args: []string{"ls"}})
	if listed.exitCode != 0 || !strings.Contains(string(listed.stdout), "work") {
		t.Fatalf("ls alias result = %#v", listed)
	}
	attached := d.executeCommand(protocol.CommandRequest{Args: []string{"a", "-t", "work"}})
	if attached.exitCode != 0 || attached.bootstrap == nil || attached.bootstrap.SessionID != created.bootstrap.SessionID {
		t.Fatalf("attach alias result = %#v", attached)
	}
}

func TestNewSessionCommandRollsBackWhenInitialPaneFails(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.snapshotDir = t.TempDir()
	result := d.executeCommand(protocol.CommandRequest{
		Args:         []string{"new-session", "-s", "broken", "--", "/does/not/exist"},
		TerminalCols: 80,
		TerminalRows: 23,
	})
	if result.exitCode == 0 {
		t.Fatalf("invalid initial command succeeded: %#v", result)
	}
	if session := d.sessionByName("broken"); session != nil {
		t.Fatalf("failed new-session remained registered: %#v", session)
	}
}

func TestCommandSocketStreamsFramedResult(t *testing.T) {
	d := &Daemon{}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		serveCommandConnection(serverConn, d)
		close(done)
	}()
	if err := protocol.WriteCommandRequest(clientConn, protocol.CommandRequest{Args: []string{"ls"}}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for {
		frame, err := protocol.ReadCommandFrame(clientConn)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == protocol.CommandFrameStdout {
			output.Write(frame.Data)
		}
		if frame.Type == protocol.CommandFrameExit {
			if frame.ExitCode != 0 {
				t.Fatalf("exit frame = %#v", frame)
			}
			break
		}
	}
	_ = clientConn.Close()
	<-done
	if !strings.Contains(output.String(), "Active Sessions") {
		t.Fatalf("command output = %q", output.String())
	}
}

func TestCommandOutputCanExceedLegacy64KiBLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 128<<10)
	var wire bytes.Buffer
	if err := protocol.WriteCommandOutput(&wire, protocol.CommandFrameStdout, data); err != nil {
		t.Fatal(err)
	}
	var decoded []byte
	for wire.Len() > 0 {
		frame, err := protocol.ReadCommandFrame(&wire)
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, frame.Data...)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("decoded %d bytes, want %d", len(decoded), len(data))
	}
}

func TestCommandSocketDirectoryMustAlreadyBePrivate(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureCommandSocketDir(filepath.Join(parent, "meja.sock")); err == nil {
		t.Fatal("shared command socket parent was accepted")
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("parent mode changed to %o", info.Mode().Perm())
	}
}

func TestCommandServerLockIsExclusivePerSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "profile", "meja.sock")
	first, err := acquireCommandServerLock(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := acquireCommandServerLock(socket); err == nil {
		t.Fatal("second command server lock was acquired")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireCommandServerLock(socket)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = second.Close()
}

func TestStaleCommandSocketCleanupPreservesActiveSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "meja.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := removeStaleCommandSocket(socket); err == nil {
		t.Fatal("active command socket was treated as stale")
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("active command socket was removed: %v", err)
	}
}

func TestStaleCommandSocketCleanupDeletesUnboundSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "meja.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleCommandSocket(socket); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale command socket still exists: %v", err)
	}
}

func TestParseCommandLinePreservesQuotedAndEscapedArguments(t *testing.T) {
	got, err := parseCommandLine(`rename-window "build and test" empty\ value ''`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rename-window", "build and test", "empty value", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCommandLine() = %#v, want %#v", got, want)
	}
	for _, input := range []string{`rename-window "unfinished`, `rename-window trailing\`} {
		if _, err := parseCommandLine(input); err == nil {
			t.Fatalf("parseCommandLine(%q) succeeded", input)
		}
	}
}

func TestCommandPromptAndPrefixUseTheSameSessionCommandEngine(t *testing.T) {
	s := NewSession(1)
	client := s.NewClient(clientID0)
	client.TerminalCols, client.TerminalRows = 80, 23
	pane := &Pane{ID: s.AddPaneID(), Title: "bash", terminal: newTerminal(80, 23)}
	window, _ := s.CreateWindow(pane, clientID0)
	connection := &Connection{Session: s}

	s.ConsumeInputByte(clientID0, 0x02)
	event := s.ConsumeInputByte(clientID0, ':')
	if !isCommandInput(event, "command-prompt") {
		t.Fatalf("command prompt binding = %#v", event)
	}
	if detach, err := s.handleServerInputEvent(connection, event); err != nil || detach {
		t.Fatalf("open command prompt: detach=%v err=%v", detach, err)
	}
	prompt := s.ActivePrompt(clientID0)
	if prompt == nil || prompt.Kind != PromptKindCommand || prompt.Label != ":" {
		t.Fatalf("command prompt = %#v", prompt)
	}
	for _, b := range []byte(`rename-window "build output"`) {
		if detach, err := s.handleServerInputEvent(connection, s.ConsumeInputByte(clientID0, b)); err != nil || detach {
			t.Fatalf("type command prompt: detach=%v err=%v", detach, err)
		}
	}
	if detach, err := s.handleServerInputEvent(connection, s.ConsumeInputByte(clientID0, '\r')); err != nil || detach {
		t.Fatalf("submit command prompt: detach=%v err=%v", detach, err)
	}
	if got := s.Windows[window.ID].Name; got != "build output" {
		t.Fatalf("window name = %q, want build output", got)
	}

	if _, err := s.executeSessionCommand(connection, []string{"rename-window", "direct"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Windows[window.ID].Name; got != "direct" {
		t.Fatalf("direct command window name = %q", got)
	}
}

func TestCommandPromptReportsCommandErrorsWithoutClosingInput(t *testing.T) {
	s := NewSession(1)
	client := s.NewClient(clientID0)
	client.TerminalCols, client.TerminalRows = 80, 23
	s.CreateWindow(&Pane{ID: s.AddPaneID(), terminal: newTerminal(80, 23)}, clientID0)
	connection := &Connection{Session: s}

	if _, err := s.executeSessionCommand(connection, []string{"command-prompt"}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte("not-a-command\r") {
		detach, err := s.handleServerInputEvent(connection, s.ConsumeInputByte(clientID0, b))
		if err != nil || detach {
			t.Fatalf("prompt error escaped command engine: detach=%v err=%v", detach, err)
		}
	}
	state := s.SnapshotClient(clientID0)
	if state.Prompt != nil || state.StatusMessage != `unknown command "not-a-command"` {
		t.Fatalf("client after command error = %#v", state)
	}
}

func TestCLIAndAttachedInvocationShareOperationalCommandHandler(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSession(41)
	t.Cleanup(s.stopOperations)
	s.setSessionName("work")
	client := s.NewClient(clientID0)
	client.TerminalCols, client.TerminalRows = 80, 23
	left := &Pane{ID: s.AddPaneID(), Title: "left", terminal: newTerminal(80, 23)}
	window, _ := s.CreateWindow(left, clientID0)
	right := &Pane{ID: s.AddPaneID(), Title: "right", terminal: newTerminal(80, 23)}
	if _, _, err := s.SplitFocusedPane(clientID0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	d.sessions[s.ID] = s
	d.names[s.Name] = s
	initialPlacements := s.Windows[window.ID].Layout.Compute(Rect{Width: 80, Height: 23})
	initialLeftWidth := initialPlacements[0].Rect.Width

	cliRename := d.executeCommand(protocol.CommandRequest{Args: []string{"rename-window", "-t", "work:0", "from-cli"}})
	if cliRename.exitCode != 0 || s.Windows[window.ID].Name != "from-cli" {
		t.Fatalf("CLI rename result=%#v window=%#v", cliRename, s.Windows[window.ID])
	}
	cliResize := d.executeCommand(protocol.CommandRequest{Args: []string{"resize-pane", "-t", "work", "-R", "2"}})
	if cliResize.exitCode != 0 {
		t.Fatalf("CLI resize result=%#v", cliResize)
	}
	placements := s.Windows[window.ID].Layout.Compute(Rect{Width: 80, Height: 23})
	if placements[0].Rect.Width != initialLeftWidth+2 {
		t.Fatalf("width after CLI resize = %d, want %d", placements[0].Rect.Width, initialLeftWidth+2)
	}

	connection := &Connection{Session: s, Daemon: d}
	if _, err := s.executeSessionCommand(connection, []string{"rename-window", "from-prompt"}); err != nil {
		t.Fatal(err)
	}
	if s.Windows[window.ID].Name != "from-prompt" {
		t.Fatalf("attached rename = %q", s.Windows[window.ID].Name)
	}
	if _, err := s.executeSessionCommand(connection, []string{"resize-pane", "-L", "1"}); err != nil {
		t.Fatal(err)
	}
	placements = s.Windows[window.ID].Layout.Compute(Rect{Width: 80, Height: 23})
	if placements[0].Rect.Width != initialLeftWidth+1 {
		t.Fatalf("width after attached resize = %d, want %d", placements[0].Rect.Width, initialLeftWidth+1)
	}
}

func TestCLIOperationalCommandRequiresExplicitSessionTarget(t *testing.T) {
	d := newCommandTestDaemon(t)
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"resize-pane", "-L"}})
	if result.exitCode != 1 || !strings.Contains(string(result.stderr), "requires -t") {
		t.Fatalf("missing target result = %#v", result)
	}
}
