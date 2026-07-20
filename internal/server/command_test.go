package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func shortUnixSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "meja-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestNewSessionCommandCreatesInitialPaneBeforeAttach(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = t.TempDir()
	callerCwd := t.TempDir()
	result := d.executeCommand(protocol.CommandRequest{
		Args:             []string{"new", "-s", "work", "--", "/bin/sleep", "30"},
		WorkingDirectory: callerCwd,
		TerminalCols:     91,
		TerminalRows:     27,
	})
	defer d.disconnectActiveClients()
	if result.exitCode != 0 || result.bootstrap == nil {
		t.Fatalf("new result = %#v", result)
	}
	session := d.sessionByName("work")
	if session == nil || !session.HasWindows() || len(session.PanesSnapshot()) != 1 {
		t.Fatalf("new session state = %#v", session)
	}
	pane, _ := session.ActivePane(clientID0)
	if pane == nil {
		t.Fatal("new session has no focused pane")
	}
	if session.rootDir != callerCwd || pane.Launch.Cwd != callerCwd {
		t.Fatalf("default root/pane cwd = %q/%q, want %q", session.rootDir, pane.Launch.Cwd, callerCwd)
	}
	cols, rows := pane.TerminalSize()
	if cols != 91 || rows != 27 {
		t.Fatalf("initial pane size = %dx%d, want 91x27", cols, rows)
	}
	if got := pane.Launch.RequestedArgv; !reflect.DeepEqual(got, []string{"/bin/sleep", "30"}) {
		t.Fatalf("initial pane argv = %v", got)
	}
}

func TestNewSessionRootFlagsSetRootAndInitialPaneCwd(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = t.TempDir()
	callerCwd := t.TempDir()
	root := t.TempDir()
	result := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new", "-s", "work", "--root", root, "--", "/bin/sleep", "30"}, WorkingDirectory: callerCwd,
		TerminalCols: 80, TerminalRows: 23,
	})
	defer d.disconnectActiveClients()
	if result.exitCode != 0 {
		t.Fatalf("new --root result = %#v", result)
	}
	session := d.sessionByName("work")
	pane, _ := session.ActivePane(clientID0)
	if session.rootDir != root || pane == nil || pane.Launch.Cwd != root {
		t.Fatalf("created root/pane = %q %#v", session.rootDir, pane)
	}
	legacy := d.executeCommand(protocol.CommandRequest{Args: []string{"new", "-s", "legacy", "-c", root}})
	if legacy.exitCode == 0 {
		t.Fatalf("removed -c flag was accepted: %#v", legacy)
	}
}

func TestNewSessionShortRootFlagResolvesRelativeToCaller(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = t.TempDir()
	callerCwd := t.TempDir()
	root := filepath.Join(callerCwd, "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	result := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new", "-s", "work", "-r", "project", "--", "/bin/sleep", "30"}, WorkingDirectory: callerCwd,
		TerminalCols: 80, TerminalRows: 23,
	})
	defer d.disconnectActiveClients()
	session := d.sessionByName("work")
	if result.exitCode != 0 || session == nil || session.rootDir != root {
		t.Fatalf("new -r result = %#v session=%#v", result, session)
	}
}

func TestCommandAliasesAreResolvedByDaemon(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = t.TempDir()
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
	if attached.exitCode != 0 || attached.bootstrap == nil || attached.bootstrap.AttachToken == created.bootstrap.AttachToken {
		t.Fatalf("attach alias result = %#v", attached)
	}
	renamed := d.executeCommand(protocol.CommandRequest{Args: []string{"rename", "-t", "work", "renamed"}})
	if renamed.exitCode != 0 || d.sessionByName("renamed") == nil {
		t.Fatalf("rename alias result = %#v", renamed)
	}
}

func TestHelpIsGeneratedFromRegisteredCommands(t *testing.T) {
	d := newCommandTestDaemon(t)
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"--help"}})
	if result.exitCode != 0 {
		t.Fatalf("help failed: %s", result.stderr)
	}
	output := string(result.stdout)
	for _, want := range []string{
		"transport options",
		"start-server",
		"new-session (new)",
		"resize-pane (resizep)",
		"meja help <command>",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output omitted %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "server (stop)") {
		t.Fatalf("help exposed legacy server command:\n%s", output)
	}
}

func TestCommandSpecificHelpSupportsBothFormsWithoutExecuting(t *testing.T) {
	d := newCommandTestDaemon(t)
	for _, args := range [][]string{
		{"help", "new"},
		{"new", "-s", "must-not-exist", "--help"},
	} {
		result := d.executeCommand(protocol.CommandRequest{Args: args})
		if result.exitCode != 0 || !strings.Contains(string(result.stdout), "usage: meja [transport-options] new-session") {
			t.Fatalf("help %v = %#v", args, result)
		}
		if d.sessionByName("must-not-exist") != nil {
			t.Fatal("command-specific help executed new-session")
		}
	}
}

func TestHelpRejectsUnknownCommand(t *testing.T) {
	d := newCommandTestDaemon(t)
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"help", "no-such-command"}})
	if result.exitCode == 0 || !strings.Contains(string(result.stderr), `unknown command "no-such-command"`) {
		t.Fatalf("unknown help result = %#v", result)
	}
}

func TestSaveAndNewFileCommandsRoundTripSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = filepath.Join(t.TempDir(), "sessions")
	root := t.TempDir()
	created := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new", "-s", "work", "--", "/bin/sleep", "30"}, WorkingDirectory: root,
		TerminalCols: 80, TerminalRows: 23,
	})
	defer d.disconnectActiveClients()
	if created.exitCode != 0 {
		t.Fatalf("new session = %#v", created)
	}
	path := filepath.Join(t.TempDir(), "dev.meja")
	saved := d.executeCommand(protocol.CommandRequest{Args: []string{"save-session", "-t", "work", "-o", path}})
	if saved.exitCode != 0 {
		t.Fatalf("save = %#v", saved)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "saved-at") || !strings.Contains(string(data), `root "`) ||
		!strings.Contains(string(data), "window") || strings.Contains(string(data), "meja 1") {
		t.Fatalf("saved file =\n%s", data)
	}
	refused := d.executeCommand(protocol.CommandRequest{Args: []string{"save", "-t", "work", "-o", path}})
	if refused.exitCode == 0 || !strings.Contains(string(refused.stderr), "use -f") {
		t.Fatalf("save without force = %#v", refused)
	}
	forced := d.executeCommand(protocol.CommandRequest{Args: []string{"save", "-t", "work", "-o", path, "-f"}})
	if forced.exitCode != 0 {
		t.Fatalf("forced save = %#v", forced)
	}
	restored := d.executeCommand(protocol.CommandRequest{Args: []string{"new", "-f", path, "-s", "recovered", "--commands=skip"}})
	if restored.exitCode != 0 || restored.bootstrap == nil {
		t.Fatalf("restore file = %#v", restored)
	}
	if session := d.sessionByName("recovered"); session == nil || session.ID == d.sessionByName("work").ID {
		t.Fatalf("restored session = %#v", session)
	}
}

func TestSaveRelativeOutputUsesTargetSessionRoot(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = filepath.Join(t.TempDir(), "sessions")
	base := t.TempDir()
	created := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new", "-s", "work", "--", "/bin/sleep", "30"}, WorkingDirectory: base,
		TerminalCols: 80, TerminalRows: 23,
	})
	defer d.disconnectActiveClients()
	if created.exitCode != 0 {
		t.Fatalf("new session = %#v", created)
	}
	invokerCwd := t.TempDir()
	saved := d.executeCommand(protocol.CommandRequest{
		Args: []string{"save", "-t", "work", "-o", ".meja/dev.meja"}, WorkingDirectory: invokerCwd,
	})
	if saved.exitCode != 0 {
		t.Fatalf("save = %#v", saved)
	}
	wantPath := filepath.Join(base, ".meja", "dev.meja")
	plan, err := readUserSessionPlan(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Root != base {
		t.Fatalf("resolved plan root = %q, want %q", plan.Root, base)
	}
	if _, err := os.Stat(filepath.Join(invokerCwd, ".meja", "dev.meja")); !os.IsNotExist(err) {
		t.Fatalf("save used invoking cwd: %v", err)
	}
	if !strings.Contains(string(saved.stdout), "Saved dev.meja.") ||
		!strings.Contains(string(saved.stdout), "Reminder: scrub sensitive values") {
		t.Fatalf("save output = %q", saved.stdout)
	}
}

func TestRestoreRejectsMalformedPersistenceWithoutCreatingSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(d.sessionPersistenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d.sessionPersistenceDir, "broken.session.meja")
	if err := os.WriteFile(path, []byte("meja 1\nsession \"broken\" active-window=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nextID := d.nextID
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"restore", "-t", "broken", "--commands=skip"}})
	if result.exitCode == 0 || result.bootstrap != nil || !strings.Contains(string(result.stderr), "parse .meja file") {
		t.Fatalf("malformed persistence restore = %#v", result)
	}
	if d.sessionByName("broken") != nil || d.nextID != nextID {
		t.Fatalf("malformed persistence created a session: sessions=%#v nextID=%d", d.sessions, d.nextID)
	}
}

func TestRestoreDoesNotReadLegacyPersistenceFilename(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(d.sessionPersistenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.sessionPersistenceDir, "work.meja"), []byte("meja 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"restore", "-t", "work", "--commands=skip"}})
	if result.exitCode == 0 || !strings.Contains(string(result.stderr), "work.session.meja") {
		t.Fatalf("legacy persistence filename was used: %#v", result)
	}
}

func TestNewFileRejectsMalformedUserMejaWithoutCreatingSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = filepath.Join(t.TempDir(), "sessions")
	path := filepath.Join(t.TempDir(), "broken.meja")
	if err := os.WriteFile(path, []byte("meja 1\nsession \"broken\" active-window=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nextID := d.nextID
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"new", "-f", path, "-s", "recovered", "--commands=skip"}})
	if result.exitCode == 0 || result.bootstrap != nil || !strings.Contains(string(result.stderr), "parse .meja file") {
		t.Fatalf("malformed user file restore = %#v", result)
	}
	if d.sessionByName("recovered") != nil || d.nextID != nextID {
		t.Fatalf("malformed user file created a session: sessions=%#v nextID=%d", d.sessions, d.nextID)
	}
}

func TestRestoreRequiresTargetAndRejectsFilesAndPositionalNames(t *testing.T) {
	d := newCommandTestDaemon(t)
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"restore"}, want: "requires -t <session-name>"},
		{args: []string{"restore", "work"}, want: "requires -t <session-name>"},
		{args: []string{"restore", "-f", filepath.Join(t.TempDir(), "dev.meja")}, want: "flag provided but not defined: -f"},
	} {
		result := d.executeCommand(protocol.CommandRequest{Args: test.args})
		if result.exitCode == 0 || !strings.Contains(string(result.stderr), test.want) {
			t.Fatalf("restore source validation for %v = %#v", test.args, result)
		}
	}
}

func TestNewFileRejectsRootAndInitialCommand(t *testing.T) {
	d := newCommandTestDaemon(t)
	for _, args := range [][]string{
		{"new", "-f", "dev.meja", "-r", "/tmp"},
		{"new", "-f", "dev.meja", "--", "echo", "no"},
	} {
		result := d.executeCommand(protocol.CommandRequest{Args: args})
		if result.exitCode == 0 || !strings.Contains(string(result.stderr), "cannot be combined with a root or initial command") {
			t.Fatalf("new file combination validation for %v = %#v", args, result)
		}
	}
}

func TestNewSessionCommandRollsBackWhenInitialPaneFails(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = t.TempDir()
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
	dir := filepath.Join(shortUnixSocketDir(t), "profile")
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
	dir := filepath.Join(shortUnixSocketDir(t), "profile")
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
	connection := &ClientInstance{}

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
	connection := &ClientInstance{}

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

func TestSwitchSessionCommandReturnsLiveHandoffRequest(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSession(1)
	target := NewSession(2)
	t.Cleanup(source.stopOperations)
	t.Cleanup(target.stopOperations)
	source.setSessionName("source")
	target.setSessionName("target")
	d.sessions[source.ID] = source
	d.sessions[target.ID] = target
	d.names[source.Name] = source
	d.names[target.Name] = target
	clientState := source.NewClient(clientID0)
	clientState.TerminalCols, clientState.TerminalRows = 101, 37
	client := &ClientInstance{Daemon: d}

	_, err := source.executeSessionCommand(client, []string{"switch-session", "-t", "target"})
	var request *sessionSwitchRequest
	if !errors.As(err, &request) {
		t.Fatalf("switch command error = %v, want handoff request", err)
	}
	if request.rawTarget != "target" || request.cols != 101 || request.rows != 37 {
		t.Fatalf("switch request = %#v", request)
	}

	if _, err := source.executeSessionCommand(client, []string{"switch-session", "target"}); err == nil || err.Error() != "switch-session requires -t <session-target>" {
		t.Fatalf("missing target flag error = %v", err)
	}
}

func TestAttachedRestoreCreatesSessionAndReturnsHandoffRequest(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = filepath.Join(t.TempDir(), "sessions")
	project := t.TempDir()
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "persisted", Root: project, ActiveWindow: 1,
		Windows: []PlanWindow{{
			ID: 1, Cwd: project, ActivePane: 1, Layout: PlanLayout{Pane: paneIDRef(1)},
			Panes: []PlanPane{{ID: 1, Cwd: project}},
		}},
	}
	if _, err := writeSessionPersistence(d.sessionPersistenceDir, SessionPersistence{
		Version: mejaFormatVersion, SessionID: 6, Name: "persisted", SavedAt: time.Now(), Root: project, Plan: plan,
	}); err != nil {
		t.Fatal(err)
	}

	source := NewSession(41)
	t.Cleanup(source.stopOperations)
	source.daemon = d
	source.setSessionName("source")
	source.rootDir = project
	state := source.NewClient(clientID0)
	state.TerminalCols, state.TerminalRows = 101, 37
	source.CreateWindow(&Pane{
		ID: source.AddPaneID(), Launch: PaneLaunch{Cwd: project}, terminal: newTerminal(101, 37),
	}, clientID0)
	d.sessions[source.ID] = source
	d.names[source.Name] = source

	client := &ClientInstance{Daemon: d}
	_, err := source.executeSessionCommand(client, []string{
		"restore", "-t", "persisted", "-s", "mynewsession", "--commands=skip",
	})
	var request *sessionSwitchRequest
	if !errors.As(err, &request) {
		t.Fatalf("attached restore error = %v, want handoff request", err)
	}
	restored := d.sessionByName("mynewsession")
	if restored == nil {
		t.Fatal("attached restore did not create mynewsession")
	}
	if request.rawTarget != strconv.FormatUint(restored.ID, 10) || request.cols != 101 || request.rows != 37 {
		t.Fatalf("restore handoff request = %#v, restored session ID %d", request, restored.ID)
	}
	if restored.rootDir != project {
		t.Fatalf("restored root = %q, want %q", restored.rootDir, project)
	}

	if err := restored.coordinate(func() error {
		restored.daemon = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, pane := range restored.PanesSnapshot() {
		_ = terminatePane(pane)
	}
	select {
	case <-restored.operationsDone:
	case <-time.After(time.Second):
		t.Fatal("restored session did not stop after its panes were terminated")
	}
	if restored.persistenceStarted.Load() {
		select {
		case <-restored.persistenceDone:
		case <-time.After(time.Second):
			t.Fatal("restored session persistence did not stop")
		}
	}
}

func TestContextualCLITargetUsesExistingNumericTargetResolver(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSession(17)
	t.Cleanup(s.stopOperations)
	s.daemon = d
	s.setSessionName("renamed-session")
	s.rootDir = t.TempDir()
	s.processObserver = emptyProcessObserver{}
	project := t.TempDir()
	s.NewClient(clientID0)
	s.CreateWindow(&Pane{ID: s.AddPaneID(), Launch: PaneLaunch{Cwd: project}}, clientID0)
	d.sessions[s.ID] = s
	d.names[s.Name] = s

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"set-root", "."},
		WorkingDirectory:    project,
		CallerSessionTarget: "17",
	})
	if result.exitCode != 0 {
		t.Fatalf("contextual set-root = %#v", result)
	}
	if s.rootDir != project {
		t.Fatalf("contextual set-root changed root to %q, want %q", s.rootDir, project)
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

	connection := &ClientInstance{Daemon: d}
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

func TestSetRootUsesObservedPaneCwdAndDoesNotMoveExistingPane(t *testing.T) {
	s := NewSession(42)
	t.Cleanup(s.stopOperations)
	oldRoot := t.TempDir()
	observedCwd := t.TempDir()
	relativeRoot := filepath.Join(observedCwd, "project")
	if err := os.Mkdir(relativeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	s.rootDir = oldRoot
	s.setSessionName("work")
	s.processObserver = &changingProcessObserver{name: "bash", cwd: observedCwd}
	pane := &Pane{ID: 0, Root: Identity{PID: 100, BirthToken: 1000}, Launch: PaneLaunch{Cwd: oldRoot, Shell: "/bin/sh"}}
	s.CreateWindow(pane, clientID0)
	connection := &ClientInstance{}
	if _, err := s.executeSessionCommand(connection, []string{"set-root"}); err != nil {
		t.Fatal(err)
	}
	if s.rootDir != observedCwd || pane.Launch.Cwd != oldRoot || s.sessionPersistence.Root != observedCwd {
		t.Fatalf("set-root without path: root=%q pane=%q persistence=%#v", s.rootDir, pane.Launch.Cwd, s.sessionPersistence)
	}
	if _, err := s.executeSessionCommand(connection, []string{"set-root", "project"}); err != nil {
		t.Fatal(err)
	}
	if s.rootDir != relativeRoot || pane.Launch.Cwd != oldRoot {
		t.Fatalf("relative set-root: root=%q pane=%q", s.rootDir, pane.Launch.Cwd)
	}
}

func TestSetRootControlsFutureWindowsPanesAndSaveLocation(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.sessionPersistenceDir = filepath.Join(t.TempDir(), "sessions")
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	created := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new", "-s", "work", "--", "/bin/sleep", "30"}, WorkingDirectory: oldRoot,
		TerminalCols: 80, TerminalRows: 23,
	})
	defer d.disconnectActiveClients()
	if created.exitCode != 0 {
		t.Fatalf("new session = %#v", created)
	}
	if result := d.executeCommand(protocol.CommandRequest{Args: []string{"set-root", "-t", "work", newRoot}}); result.exitCode != 0 {
		t.Fatalf("set-root = %#v", result)
	}
	if result := d.executeCommand(protocol.CommandRequest{Args: []string{"new-window", "-t", "work"}}); result.exitCode != 0 {
		t.Fatalf("new-window = %#v", result)
	}
	if result := d.executeCommand(protocol.CommandRequest{Args: []string{"split-window", "-t", "work", "-h"}}); result.exitCode != 0 {
		t.Fatalf("split-window = %#v", result)
	}
	session := d.sessionByName("work")
	oldCount, newCount := 0, 0
	for _, pane := range session.PanesSnapshot() {
		switch pane.Launch.Cwd {
		case oldRoot:
			oldCount++
		case newRoot:
			newCount++
		}
	}
	if session.rootDir != newRoot || oldCount != 1 || newCount != 2 {
		t.Fatalf("future pane roots: session=%q old=%d new=%d", session.rootDir, oldCount, newCount)
	}
	recoveryPath, err := session.flushPersistence(context.Background(), d.sessionPersistenceDir)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := os.ReadFile(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recovery), `root "`+newRoot+`"`) {
		t.Fatalf("recovery file retained old root:\n%s", recovery)
	}
	saved := d.executeCommand(protocol.CommandRequest{Args: []string{"save", "-t", "work", "-o", "dev.meja"}})
	if saved.exitCode != 0 {
		t.Fatalf("save after set-root = %#v", saved)
	}
	plan, err := readUserSessionPlan(filepath.Join(newRoot, "dev.meja"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Root != newRoot {
		t.Fatalf("saved root = %q, want %q", plan.Root, newRoot)
	}
}

func TestCLIOperationalCommandRequiresExplicitSessionTarget(t *testing.T) {
	d := newCommandTestDaemon(t)
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"resize-pane", "-L"}})
	if result.exitCode != 1 || !strings.Contains(string(result.stderr), "requires -t") {
		t.Fatalf("missing target result = %#v", result)
	}
}
