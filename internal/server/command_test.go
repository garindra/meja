package server

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
	"github.com/garindra/meja/internal/version"
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
	setCommandTestPersistenceDir(t, d)
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
	pane, _ := testActivePane(session)
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

func TestDetachedNewSessionPrintsInitialPaneWithoutBootstrapOrAttachment(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	root := t.TempDir()
	result := d.executeCommand(protocol.CommandRequest{
		Args:             []string{"new-session", "-d", "-P", "-F", "#{session_id}:#{pane_id}", "-s", "worker", "--", "/bin/sleep", "30"},
		WorkingDirectory: root,
		TerminalCols:     80,
		TerminalRows:     23,
	})
	defer d.disconnectActiveClients()
	if result.exitCode != 0 || result.bootstrap != nil || string(result.stdout) != "1:1\n" {
		t.Fatalf("detached new result = %#v", result)
	}
	if len(d.attachGrants) != 0 {
		t.Fatalf("detached creation left attach grants: %#v", d.attachGrants)
	}
	session := d.sessionByName("worker")
	if session == nil || testClientOf(session) != nil || len(session.Panes) != 1 {
		t.Fatalf("detached session state = %#v", session)
	}
}

func TestDetachedNewSessionDoesNotActivateInvokingClient(t *testing.T) {
	d := newCommandTestDaemon(t)
	defer d.disconnectActiveClients()
	setCommandTestPersistenceDir(t, d)
	source := NewSessionState(1)
	source.daemon = d
	source.setSessionName("source")
	createTestWindow(source, &Pane{ID: testAddPaneID(source), terminal: newTerminal(80, 23)})
	setTestClientSize(source, 80, 23)
	d.sessions[source.ID] = source
	d.names[source.Name] = source
	d.nextID = 2

	identity := &ClientIdentity{ResumeToken: "detached-test"}
	instance := newClientInstance(d, identity)
	d.clientInstances[identity] = instance
	setTestClient(source, instance)
	d.clientSessions[identity] = source.ID
	d.attachments[source.ID] = identity

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"new-session", "-d", "-P", "-F", "#{session_id}:#{pane_id}", "-s", "detached"},
		CallerSessionTarget: "source",
		WorkingDirectory:    t.TempDir(),
		TerminalCols:        80,
		TerminalRows:        23,
	})
	if result.exitCode != 0 || result.bootstrap != nil || string(result.stdout) != "2:2\n" {
		t.Fatalf("pane CLI detached new result = %#v", result)
	}
	if testClientOf(source) != instance {
		t.Fatal("source client was moved from the invoking session")
	}
	created := d.sessionByName("detached")
	if created == nil || testClientOf(created) != nil {
		t.Fatalf("detached session state = %#v", created)
	}
	if len(d.attachGrants) != 0 {
		t.Fatalf("detached creation left attach grants: %#v", d.attachGrants)
	}
}

func TestDetachedNewSessionDoesNotRequireAttachedSource(t *testing.T) {
	d := newCommandTestDaemon(t)
	defer d.disconnectActiveClients()
	setCommandTestPersistenceDir(t, d)
	source := NewSessionState(1)
	source.daemon = d
	source.setSessionName("source")
	createTestWindow(source, &Pane{ID: testAddPaneID(source), terminal: newTerminal(80, 23)})
	// The fixture creates windows through a synthetic ClientInstance so the
	// normal projection boundary is exercised. Remove it here because this test
	// specifically models a detached source session.
	setTestClient(source, nil)
	d.sessions[source.ID] = source
	d.names[source.Name] = source
	d.nextID = 2

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"new", "-d", "-P", "-F", "#{session_id}:#{pane_id}", "-s", "detached"},
		CallerSessionTarget: "source",
		WorkingDirectory:    t.TempDir(),
		TerminalCols:        80,
		TerminalRows:        23,
	})
	if result.exitCode != 0 || result.bootstrap != nil || string(result.stdout) != "2:2\n" {
		t.Fatalf("detached new without attached source = %#v", result)
	}
	if testClientOf(source) != nil || source.SessionName() != "source" || len(source.Panes) != 1 {
		t.Fatalf("source session changed = %#v", source)
	}
	created := d.sessionByName("detached")
	if created == nil || testClientOf(created) != nil {
		t.Fatalf("detached session state = %#v", created)
	}
	if len(d.attachGrants) != 0 {
		t.Fatalf("detached creation left attach grants: %#v", d.attachGrants)
	}
}

func TestNewSessionDetachedPreflightStopsAtCommandSeparator(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"-d"}, want: true},
		{args: []string{"-d=true"}, want: true},
		{args: []string{"--d"}, want: true},
		{args: []string{"--", "-d"}, want: false},
		{args: []string{"--", "editor", "-d"}, want: false},
		{args: []string{"-d=false"}, want: false},
	}
	for _, test := range tests {
		if got := newSessionRequestsDetached(test.args); got != test.want {
			t.Errorf("newSessionRequestsDetached(%v) = %v, want %v", test.args, got, test.want)
		}
	}
}

func TestDetachedNewSessionRollbackRemovesFailedSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	result := d.executeCommand(protocol.CommandRequest{
		Args:         []string{"new", "-d", "-s", "broken", "--", "/does/not/exist"},
		TerminalCols: 80,
		TerminalRows: 23,
	})
	if result.exitCode == 0 || d.sessionByName("broken") != nil {
		t.Fatalf("failed detached new result = %#v", result)
	}
}

func TestPaneIDsAreDaemonWideMonotonicAndNotReused(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	defer d.disconnectActiveClients()
	newDetached := func(name string) string {
		result := d.executeCommand(protocol.CommandRequest{
			Args:         []string{"new", "-d", "-P", "-F", "#{pane_id}", "-s", name},
			TerminalCols: 80,
			TerminalRows: 23,
		})
		if result.exitCode != 0 {
			t.Fatalf("create %s = %#v", name, result)
		}
		return strings.TrimSpace(string(result.stdout))
	}
	if got := newDetached("one"); got != "1" {
		t.Fatalf("first pane ID = %q, want 1", got)
	}
	if got := newDetached("two"); got != "2" {
		t.Fatalf("second pane ID = %q, want 2", got)
	}
	killed := d.executeCommand(protocol.CommandRequest{Args: []string{"kill-session", "-t", "one"}})
	if killed.exitCode != 0 {
		t.Fatalf("kill first session = %#v", killed)
	}
	if got := newDetached("three"); got != "3" {
		t.Fatalf("post-exit pane ID = %q, want 3", got)
	}
}

func TestNewWindowAndSplitUseDaemonPaneIDs(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	created := d.executeCommand(protocol.CommandRequest{
		Args:         []string{"new", "-d", "-s", "work"},
		TerminalCols: 80,
		TerminalRows: 23,
	})
	defer d.disconnectActiveClients()
	if created.exitCode != 0 {
		t.Fatalf("initial session = %#v", created)
	}
	for _, args := range [][]string{{"new-window", "-t", "work"}, {"split-window", "-t", "work", "-v"}} {
		result := d.executeCommand(protocol.CommandRequest{Args: args})
		if result.exitCode != 0 {
			t.Fatalf("%s = %#v", args[0], result)
		}
	}
	listing := d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-a", "-F", "#{pane_id}"}})
	if listing.exitCode != 0 || string(listing.stdout) != "1\n2\n3\n" {
		t.Fatalf("new-window/split pane IDs = %#v", listing)
	}
	if len(d.clients) != 0 {
		t.Fatalf("detached commands manufactured live clients: %#v", d.clients)
	}
}

func TestCommandEngineRegistryIsCanonicalAndExcludesClientUX(t *testing.T) {
	d := newCommandTestDaemon(t)
	engine := d.commandEngine()
	if engine == nil || d.commandEngine() != engine {
		t.Fatal("daemon did not retain one command engine")
	}
	alias, ok := engine.lookup("neww")
	if !ok || alias.Name != "new-window" {
		t.Fatalf("neww lookup = %#v, %v", alias, ok)
	}
	for _, localUX := range []string{"command-prompt", "confirm-before"} {
		if command, exists := engine.lookup(localUX); exists {
			t.Fatalf("client-local UX %q registered as command %#v", localUX, command)
		}
	}
}

func TestAttachedAdapterRejectsCommandStdout(t *testing.T) {
	s := NewSessionState(1)
	newTestClient(s)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)})
	client := clientForState(s)
	if _, err := client.executeAttachedCommand([]string{"set-buffer", "payload"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.executeAttachedCommand([]string{"show-buffer"}); err == nil || !strings.Contains(err.Error(), "only available through the CLI") {
		t.Fatalf("attached show-buffer error = %v", err)
	}
}

func TestContextualKillPaneUsesInjectedCallerPaneID(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(1)
	s.daemon = d
	d.sessions[s.ID] = s
	d.ensureSessionGroupInActor(s)
	newTestClient(s)
	first := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)}
	createTestWindow(s, first)
	second := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)}
	if _, _, err := splitTestFocusedPane(s, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)
	setTestClient(s, nil)

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"kill-pane"},
		CallerSessionTarget: strconv.FormatUint(s.ID, 10),
		CallerPaneID:        first.ID,
	})
	if result.exitCode != 0 {
		t.Fatalf("kill pane CLI pane = %#v", result)
	}
	if s.Pane(first.ID) != nil || s.Pane(second.ID) == nil {
		t.Fatalf("panes after contextual kill: first=%#v second=%#v", s.Pane(first.ID), s.Pane(second.ID))
	}
}

func TestPaneIDAllocationIsSerializedForConcurrentRequests(t *testing.T) {
	d := newCommandTestDaemonWithActor(t)
	const count = 24
	ids := make(chan uint64, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			id, err := d.allocatePaneID()
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := make(map[uint64]struct{}, count)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate concurrent pane ID %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("allocated %d unique IDs, want %d", len(seen), count)
	}
}

func TestRestoreRemapsLayoutLocalPaneReferencesToFreshDaemonIDs(t *testing.T) {
	d := newCommandTestDaemon(t)
	base := t.TempDir()
	path := filepath.Join(t.TempDir(), "project.meja")
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "project", Root: base, ActiveWindow: 1,
		Windows: []PlanWindow{
			{Cwd: base, ActivePane: 0, Layout: PlanLayout{Pane: paneIDRef(0)}, Panes: []PlanPane{{ID: 0, Cwd: base}}},
			{Cwd: base, ActivePane: 0, Layout: PlanLayout{Pane: paneIDRef(0)}, Panes: []PlanPane{{ID: 0, Cwd: base}}},
		},
	}
	if _, err := writeUserMejaFile(path, plan, false); err != nil {
		t.Fatal(err)
	}
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"new", "-f", path, "--commands=skip"}})
	defer d.disconnectActiveClients()
	if result.exitCode != 0 {
		t.Fatalf("restore project = %#v", result)
	}
	listing := d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-a", "-F", "#{pane_id}"}})
	if listing.exitCode != 0 || string(listing.stdout) != "1\n2\n" {
		t.Fatalf("restored pane IDs = %#v", listing)
	}
}

func TestNewSessionFileRejectsDetachedFormatFlags(t *testing.T) {
	d := newCommandTestDaemon(t)
	for _, args := range [][]string{
		{"new", "-f", "project.meja", "-d"},
		{"new", "-f", "project.meja", "-P"},
		{"new", "-f", "project.meja", "-F", "#{session_id}:#{pane_id}"},
		{"new", "-f", "project.meja", "-F", "#{session_id}"},
	} {
		result := d.executeCommand(protocol.CommandRequest{Args: args})
		if result.exitCode == 0 || !strings.Contains(string(result.stderr), "does not support -d, -P, or -F") {
			t.Fatalf("new file flags %v = %#v", args, result)
		}
	}
}

func newFormatTestSession(id uint64, name string, paneID uint64) *SessionState {
	session := NewSessionState(id)
	session.daemon = testDaemonForState(session)
	session.setSessionName(name)
	pane := &Pane{ID: paneID, Launch: PaneLaunch{Shell: "/bin/bash", Cwd: "/launch"}, terminal: newTerminal(80, 24)}
	createTestWindow(session, pane)
	session.daemon.processObservations[paneID] = ProcessObservation{
		Status: StatusDetected,
		Root:   &ObservedProcess{Cwd: "/observed"},
		Candidate: &ObservedProcess{
			Name: "editor",
			Argv: []string{"editor", "--wait"},
		},
	}
	return session
}

func TestSharedPaneFormatExpandsRequiredValuesAndStableCreationTime(t *testing.T) {
	d := newCommandTestDaemon(t)
	first := newFormatTestSession(9, "work", 4)
	first.CreatedAt = 1234567890
	first.Windows[1].DisplayIndex = 3
	second := newFormatTestSession(2, "other", 4)
	second.CreatedAt = 1234567891
	d.sessions[first.ID] = first
	d.sessions[second.ID] = second
	d.names[first.Name] = first
	d.names[second.Name] = second
	t.Cleanup(func() {
		stopState(first)
		stopState(second)
	})

	format := "#{session_id}|#{session_name}|#{session_created}|#{window_index}|#{pane_id}|#{pane_dead}|#{pane_current_command}|#{pane_current_path}|#{pane_in_mode}|#{pane_index}|#{pane_width}|#{pane_height}|#{pane_in_copy_mode}|#{unknown}"
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-t", "work", "-F", format}})
	if result.exitCode != 0 {
		t.Fatalf("formatted panes = %#v", result)
	}
	want := "9|work|1234567890|3|4|0|editor|/observed|0|4|80|24|0|#{unknown}\n"
	if string(result.stdout) != want {
		t.Fatalf("formatted pane = %q, want %q", result.stdout, want)
	}
	for i := 0; i < 2; i++ {
		listed := d.executeCommand(protocol.CommandRequest{Args: []string{"list-sessions", "-F", "#{session_id}:#{session_created}"}})
		if listed.exitCode != 0 || string(listed.stdout) != "2:1234567891\n9:1234567890\n" {
			t.Fatalf("stable session listing = %#v", listed)
		}
	}
}

func TestPaneFormatUsesLaunchedCommandAndCwdFallbacks(t *testing.T) {
	d := newCommandTestDaemon(t)
	session := NewSessionState(1)
	newTestClient(session)
	session.setSessionName("fallback")
	pane := &Pane{ID: 7, Launch: PaneLaunch{Shell: "/bin/zsh", RequestedArgv: []string{"/bin/sleep", "5"}, Cwd: "/launch"}, terminal: newTerminal(10, 2)}
	createTestWindow(session, pane)
	d.sessions[session.ID] = session
	d.names[session.Name] = session
	t.Cleanup(func() { stopState(session) })
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-t", "fallback", "-F", "#{pane_current_command}|#{pane_current_path}"}})
	if result.exitCode != 0 || string(result.stdout) != "sleep|/launch\n" {
		t.Fatalf("launch fallback = %#v", result)
	}
	pane.Launch.RequestedArgv = nil
	result = d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-t", "fallback", "-F", "#{pane_current_command}|#{pane_current_path}"}})
	if result.exitCode != 0 || string(result.stdout) != "zsh|/launch\n" {
		t.Fatalf("shell fallback = %#v", result)
	}
	pane.Launch.Shell = ""
	pane.Process = &exec.Cmd{Path: "/opt/tool (deleted)"}
	result = d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-t", "fallback", "-F", "#{pane_current_command}|#{pane_current_path}"}})
	if result.exitCode != 0 || string(result.stdout) != "tool|/launch\n" {
		t.Fatalf("process path fallback = %#v", result)
	}
	pane.Process = nil
	result = d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-t", "fallback", "-F", "#{pane_current_command}|#{pane_current_path}"}})
	if result.exitCode != 0 || string(result.stdout) != "|/launch\n" {
		t.Fatalf("empty command metadata = %#v", result)
	}
}

func TestListSessionsFormatUsesActiveWindowAndPane(t *testing.T) {
	d := newCommandTestDaemon(t)
	session := newFormatTestSession(8, "active", 10)
	session.CreatedAt = 987654321
	secondPane := &Pane{ID: 11, Launch: PaneLaunch{Shell: "/bin/zsh", Cwd: "/second"}, terminal: newTerminal(90, 30)}
	secondWindow, _ := createTestWindow(session, secondPane)
	secondWindow.DisplayIndex = 7
	session.daemon.processObservations[secondPane.ID] = ProcessObservation{
		Status: StatusDetected,
		Root:   &ObservedProcess{Cwd: "/observed-second"},
		Candidate: &ObservedProcess{
			Argv: []string{"editor", "--wait"},
		},
	}
	d.sessions[session.ID] = session
	d.names[session.Name] = session
	t.Cleanup(func() { stopState(session) })

	format := "#{session_id}|#{session_name}|#{session_created}|#{window_index}|#{pane_id}|#{pane_dead}|#{pane_current_command}|#{pane_current_path}|#{pane_in_mode}"
	result := d.executeCommand(protocol.CommandRequest{Args: []string{"list-sessions", "-F", format}})
	want := "8|active|987654321|7|11|0|editor|/observed-second|0\n"
	if result.exitCode != 0 || string(result.stdout) != want {
		t.Fatalf("active session format = %#v, want %q", result, want)
	}
}

func TestListSessionsKeepsTableAndListPanesAllUsesDaemonWideIDs(t *testing.T) {
	d := newCommandTestDaemon(t)
	first := newFormatTestSession(5, "five", 1)
	second := newFormatTestSession(2, "two", 2)
	d.sessions[first.ID] = first
	d.sessions[second.ID] = second
	d.names[first.Name] = first
	d.names[second.Name] = second
	t.Cleanup(func() {
		stopState(first)
		stopState(second)
	})

	table := d.executeCommand(protocol.CommandRequest{Args: []string{"list-sessions"}})
	if table.exitCode != 0 || !strings.Contains(string(table.stdout), "Active Sessions\n") || strings.Contains(string(table.stdout), "#{") {
		t.Fatalf("human session table = %#v", table)
	}
	nonEmpty := d.executeCommand(protocol.CommandRequest{Args: []string{"list-sessions", "-F", "#{session_id}"}})
	if nonEmpty.exitCode != 0 || string(nonEmpty.stdout) != "2\n5\n" {
		t.Fatalf("formatted session list = %#v", nonEmpty)
	}
	empty := d.executeCommand(protocol.CommandRequest{Args: []string{"list-sessions", "-F", ""}})
	if empty.exitCode != 0 || string(empty.stdout) != "\n\n" {
		t.Fatalf("explicit empty session format = %#v", empty)
	}
	emptyEquals := d.executeCommand(protocol.CommandRequest{Args: []string{"list-sessions", "-F="}})
	if emptyEquals.exitCode != 0 || string(emptyEquals.stdout) != "\n\n" {
		t.Fatalf("explicit equals-empty session format = %#v", emptyEquals)
	}
	all := d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-a", "-F", "#{session_id}:#{pane_id}"}})
	if all.exitCode != 0 || string(all.stdout) != "2:2\n5:1\n" {
		t.Fatalf("all panes = %#v", all)
	}
	conflict := d.executeCommand(protocol.CommandRequest{Args: []string{"list-panes", "-a", "-t", "five"}})
	if conflict.exitCode == 0 || !strings.Contains(string(conflict.stderr), "cannot be combined with -t") {
		t.Fatalf("all panes target conflict = %#v", conflict)
	}
}

func TestNewSessionRootFlagsSetRootAndInitialPaneCwd(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
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
	pane, _ := testActivePane(session)
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
	setCommandTestPersistenceDir(t, d)
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
	setCommandTestPersistenceDir(t, d)
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

func TestKillSessionCommandTargetsByNameAndID(t *testing.T) {
	d := newCommandTestDaemon(t)
	first, err := d.executeSessionOperation("create-session", commandSessionTarget{name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.executeSessionOperation("create-session", commandSessionTarget{name: "other"})
	if err != nil {
		t.Fatal(err)
	}

	killedByName := d.executeCommand(protocol.CommandRequest{Args: []string{"kill-session", "-t", "work"}})
	if killedByName.exitCode != 0 || d.sessionByName("work") != nil {
		t.Fatalf("kill-session by name = %#v", killedByName)
	}

	killedByID := d.executeCommand(protocol.CommandRequest{Args: []string{"kill-session", "-t", strconv.FormatUint(second.session.ID, 10)}})
	if killedByID.exitCode != 0 || testDaemonSession(d, second.session.ID) != nil {
		t.Fatalf("kill-session by ID = %#v", killedByID)
	}

	if first.session == second.session {
		t.Fatal("test sessions unexpectedly share state")
	}
	missingTarget := d.executeCommand(protocol.CommandRequest{Args: []string{"kill-session"}})
	if missingTarget.exitCode == 0 || !strings.Contains(string(missingTarget.stderr), "requires -t") {
		t.Fatalf("kill-session missing target = %#v", missingTarget)
	}
}

func TestPaneCLIKillSessionInfersInjectedSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	created, err := d.executeSessionOperation("create-session", commandSessionTarget{name: "work"})
	if err != nil {
		t.Fatal(err)
	}

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"kill-session"},
		CallerSessionTarget: strconv.FormatUint(created.session.ID, 10),
	})
	if result.exitCode != 0 {
		t.Fatalf("contextual kill-session = %#v", result)
	}
	if testDaemonSession(d, created.session.ID) != nil {
		t.Fatal("contextual kill-session did not remove the injected session")
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
	if _, ok := d.commandEngine().lookup("server"); ok {
		t.Fatal("removed server compatibility command remains registered")
	}
	removed := d.executeCommand(protocol.CommandRequest{Args: []string{"server", "stop"}})
	if removed.exitCode == 0 || !strings.Contains(string(removed.stderr), `unknown command "server"`) {
		t.Fatalf("removed server command result = %#v", removed)
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

func TestAttachedOutputCommandsRejectBeforeProducingOutputOrSideEffects(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(71)
	t.Cleanup(func() { stopState(s) })
	s.daemon = d
	s.setSessionName("work")
	s.rootDir = t.TempDir()
	d.sessions[s.ID] = s
	d.names[s.Name] = s
	d.ensureSessionGroupInActor(s)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)})
	client := clientForState(s)

	for _, argv := range [][]string{
		{"help"},
		{"new-session", "--help"},
		{"list-sessions"},
	} {
		if _, err := client.executeAttachedCommand(argv); err == nil || !strings.Contains(err.Error(), "output is only available through the CLI") {
			t.Fatalf("attached %v error = %v", argv, err)
		}
	}

	path := filepath.Join(t.TempDir(), "must-not-exist.meja")
	_, err := client.executeAttachedCommand([]string{"save-session", "-t", "work", "-o", path})
	if err == nil || !strings.Contains(err.Error(), "output is only available through the CLI") {
		t.Fatalf("attached save-session error = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("attached save-session wrote %q before rejecting it: %v", path, statErr)
	}
}

func TestSaveAndNewFileCommandsRoundTripSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
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
	work := d.sessionByName("work")
	if work == nil {
		t.Fatal("created work session is unavailable")
	}
	saved := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"save-session", "-o", path},
		CallerSessionTarget: strconv.FormatUint(work.ID, 10),
		WorkingDirectory:    root,
	})
	if saved.exitCode != 0 {
		t.Fatalf("contextual save = %#v", saved)
	}
	if output := string(saved.stdout); !strings.Contains(output, "Session root: "+root) ||
		!strings.Contains(output, "Written to: "+path) || strings.Contains(output, "Warning: save was run from the current directory") {
		t.Fatalf("same-root save output = %q", output)
	}
	standalonePath := filepath.Join(t.TempDir(), "must-not-exist.meja")
	missingTarget := d.executeCommand(protocol.CommandRequest{Args: []string{"save-session", "-o", standalonePath}})
	if missingTarget.exitCode == 0 || !strings.Contains(string(missingTarget.stderr), "requires -t") {
		t.Fatalf("standalone save without target = %#v", missingTarget)
	}
	if _, err := os.Stat(standalonePath); !os.IsNotExist(err) {
		t.Fatalf("standalone save wrote before rejecting its missing target: %v", err)
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

func TestSaveUnnamedSessionUsesOutputFilenameAsRestoreName(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	created := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new-session", "--", "/bin/sleep", "30"}, WorkingDirectory: t.TempDir(),
		TerminalCols: 80, TerminalRows: 23,
	})
	defer d.disconnectActiveClients()
	if created.exitCode != 0 || created.session == nil || created.session.SessionName() != "" {
		t.Fatalf("unnamed session creation = %#v", created)
	}

	path := filepath.Join(t.TempDir(), "dev7.meja")
	saved := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"save-session", "-o", path},
		CallerSessionTarget: strconv.FormatUint(created.session.ID, 10),
	})
	if saved.exitCode != 0 {
		t.Fatalf("unnamed session save = %#v", saved)
	}
	plan, err := readUserSessionPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Name != "dev7" {
		t.Fatalf("restored plan name = %q, want filename-derived dev7", plan.Name)
	}
	restored := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new-session", "-f", path, "--commands=skip"}, WorkingDirectory: t.TempDir(),
		TerminalCols: 80, TerminalRows: 23,
	})
	if restored.exitCode != 0 || d.sessionByName("dev7") == nil {
		t.Fatalf("restore of unnamed-session save = %#v", restored)
	}
}

func TestPaneCLISelectWindowUsesInjectedSessionWithWindowOnlyTarget(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(17)
	t.Cleanup(func() { stopState(s) })
	s.daemon = d
	s.setSessionName("work")
	client := clientForState(s)
	first, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)})
	second, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)})
	d.sessions[s.ID] = s
	d.names[s.Name] = s
	d.windowLeases[second.ID] = &WindowViewLease{
		WindowID: second.ID, SessionID: s.ID, AttachmentID: client.AttachmentID, Generation: 1,
	}
	s.ActiveWindowID = second.ID

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"select-window", "-t", strconv.Itoa(first.DisplayIndex)},
		CallerSessionTarget: strconv.FormatUint(s.ID, 10),
	})
	if result.exitCode != 0 {
		t.Fatalf("contextual select-window = %#v", result)
	}
	if s.ActiveWindowID != first.ID {
		t.Fatalf("active window = %d, want %d", s.ActiveWindowID, first.ID)
	}
}

func TestPaneCLIRenameWindowUsesInjectedSessionWithWindowOnlyTarget(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(17)
	t.Cleanup(func() { stopState(s) })
	s.daemon = d
	s.setSessionName("work")
	clientForState(s)
	window, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)})
	d.sessions[s.ID] = s
	d.names[s.Name] = s

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"rename-window", "-t", strconv.Itoa(window.DisplayIndex), "renamed"},
		CallerSessionTarget: strconv.FormatUint(s.ID, 10),
	})
	if result.exitCode != 0 {
		t.Fatalf("contextual rename-window = %#v", result)
	}
	if window.Name != "renamed" {
		t.Fatalf("window name = %q, want renamed", window.Name)
	}
}

func TestSaveRelativeOutputUsesTargetSessionRoot(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
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
	output := string(saved.stdout)
	for _, want := range []string{
		"Saved session.",
		"Session root: " + base,
		"Written to: " + wantPath,
		"Warning: save was run from the current directory:\n  " + invokerCwd,
		"which differs from the current session root:\n  " + base,
		"If the current directory is the intended project root",
		"run `meja set-root .` here and save again",
		"reconstructed pane paths relative to that project root",
		"project directory is mirrored on another machine",
		"Reminder: review captured pane commands and scrub any sensitive values",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("save output omitted %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Saved dev.meja.") {
		t.Fatalf("save output = %q", saved.stdout)
	}
}

func TestRestoreRejectsMalformedPersistenceWithoutCreatingSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
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
	setCommandTestPersistenceDir(t, d)
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
	setCommandTestPersistenceDir(t, d)
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
	setCommandTestPersistenceDir(t, d)
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

func TestCommandPromptIsClientLocalAndSubmitsToCommandEngine(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	pane := &Pane{ID: testAddPaneID(s), Title: "bash", terminal: newTerminal(80, 23)}
	window, _ := createTestWindow(s, pane)
	clientForState(s).ConsumeInputByte(0x02)
	event := clientForState(s).ConsumeInputByte(':')
	if event.Command != serverCommandOpenCommandPrompt {
		t.Fatalf("command prompt binding = %#v", event)
	}
	if detach, err := clientForState(s).handleServerInputEvent(event); err != nil || detach {
		t.Fatalf("open command prompt: detach=%v err=%v", detach, err)
	}
	prompt := clientForState(s).ActivePrompt()
	if prompt == nil || prompt.Mode != PromptModeText || prompt.Label != ":" {
		t.Fatalf("command prompt = %#v", prompt)
	}
	for _, b := range []byte(`rename-window "build output"`) {
		if detach, err := clientForState(s).handleServerInputEvent(clientForState(s).ConsumeInputByte(b)); err != nil || detach {
			t.Fatalf("type command prompt: detach=%v err=%v", detach, err)
		}
	}
	if detach, err := clientForState(s).handleServerInputEvent(clientForState(s).ConsumeInputByte('\r')); err != nil || detach {
		t.Fatalf("submit command prompt: detach=%v err=%v", detach, err)
	}
	if got := s.Windows[window.ID].Name; got != "build output" {
		t.Fatalf("window name = %q, want build output", got)
	}

	instance := clientForState(s)
	if _, err := instance.executeAttachedCommand([]string{"rename-window", "direct"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Windows[window.ID].Name; got != "direct" {
		t.Fatalf("direct command window name = %q", got)
	}
}

func TestCommandPromptReportsCommandErrorsWithoutClosingInput(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 23)})
	if _, err := clientForState(s).BeginCommandPrompt(); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte("not-a-command\r") {
		detach, err := clientForState(s).handleServerInputEvent(clientForState(s).ConsumeInputByte(b))
		if err != nil || detach {
			t.Fatalf("prompt error escaped command engine: detach=%v err=%v", detach, err)
		}
	}
	state := snapshotTestClient(s)
	message, _ := clientForState(s).statusMessage.Load().(string)
	if state.Prompt != nil || message != `unknown command "not-a-command"` {
		t.Fatalf("client after command error = %#v", state)
	}
}

func TestServerVersionCommandReportsDaemonCompatibility(t *testing.T) {
	previous := version.Value
	version.Value = "v1.2.3"
	t.Cleanup(func() { version.Value = previous })

	outcome, err := runServerVersionCommand(nil, CommandContext{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "server:           meja 1.2.3\ncommand protocol: 1\nQUIC profile:     meja-quic/12\n"
	if got := string(outcome.Stdout); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestSwitchSessionCommandAppliesPreparedTransition(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSessionState(1)
	target := NewSessionState(2)
	t.Cleanup(func() { stopState(source) })
	t.Cleanup(func() { stopState(target) })
	source.setSessionName("source")
	target.setSessionName("target")
	d.sessions[source.ID] = source
	d.sessions[target.ID] = target
	d.names[source.Name] = source
	d.names[target.Name] = target
	source.daemon = d
	target.daemon = d
	d.ensureSessionGroupInActor(source)
	d.ensureSessionGroupInActor(target)
	createTestWindow(source, &Pane{ID: testAddPaneID(source), terminal: newTerminal(101, 37)})
	createTestWindow(target, &Pane{ID: testAddPaneID(target), terminal: newTerminal(101, 37)})
	fixtureClient := newTestClient(source)
	fixtureClient.setTestTerminalSize(101, 37)
	client := clientForState(source)
	identity := &ClientIdentity{ResumeToken: "switch-command", lastAllocatedClientLayoutRevision: client.currentLayout.LayoutRevision}
	client.identity = identity
	d.clientInstances[identity] = client
	d.clientSessions[identity] = source.ID
	d.attachments[source.ID] = identity
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{WindowID: source.ActiveWindowID, SessionID: source.ID, AttachmentID: client.AttachmentID, Generation: 1}
	if _, err := client.executeAttachedCommand([]string{"switch-session", "-t", "target"}); err != nil {
		t.Fatal(err)
	}
	if client.sessionState() != target || testClientOf(source) != nil || testClientOf(target) != client {
		t.Fatalf("switch did not install target: source=%#v target=%#v client-session=%#v", testClientOf(source), testClientOf(target), client.sessionState())
	}

	if _, err := client.executeAttachedCommand([]string{"switch-session", "target"}); err == nil || err.Error() != "switch-session requires -t <session-target>" {
		t.Fatalf("missing target flag error = %v", err)
	}
}

func TestSwitchSessionHandlerPreparesButDoesNotApplyClientView(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSessionState(51)
	target := NewSessionState(52)
	t.Cleanup(func() { stopState(source) })
	t.Cleanup(func() { stopState(target) })
	for _, state := range []*SessionState{source, target} {
		state.daemon = d
		d.sessions[state.ID] = state
		d.ensureSessionGroupInActor(state)
		createTestWindow(state, &Pane{ID: testAddPaneID(state), terminal: newTerminal(80, 23)})
	}
	source.setSessionName("source")
	target.setSessionName("target")
	d.names[source.Name] = source
	d.names[target.Name] = target

	client := clientForState(source)
	client.terminalCols.Store(80)
	client.terminalRows.Store(23)
	identity := &ClientIdentity{ResumeToken: "prepare-only", lastAllocatedClientLayoutRevision: client.currentLayout.LayoutRevision}
	client.identity = identity
	d.clientInstances[identity] = client
	d.clientSessions[identity] = source.ID
	d.attachments[source.ID] = identity
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{
		WindowID: source.ActiveWindowID, SessionID: source.ID,
		AttachmentID: client.AttachmentID, Generation: 1,
	}

	outcome, err := d.commandEngine().run(client.commandContext(), []string{"switch-session", "-t", "target"})
	if err != nil {
		t.Fatal(err)
	}
	action, ok := outcome.Action.(applyViewTransitionAction)
	if !ok {
		t.Fatalf("switch-session action = %T, want applyViewTransitionAction", outcome.Action)
	}
	if action.Transition.Projection.SessionID != target.ID {
		t.Fatalf("prepared target session = %d, want %d", action.Transition.Projection.SessionID, target.ID)
	}
	if d.clients[target.ID] != client || d.clients[source.ID] != nil {
		t.Fatalf("daemon assignment was not committed during preparation: source=%p target=%p", d.clients[source.ID], d.clients[target.ID])
	}
	if client.sessionID != source.ID {
		t.Fatalf("handler applied client state: session=%d, want still %d", client.sessionID, source.ID)
	}

	// Applying a prepared transition must also support an empty client-local
	// starting point. This is the same application path used by initial
	// activation; daemon authorization in the prepared plan is sufficient.
	client.sessionID = 0
	if _, err := client.applyAttachedCommandOutcome(outcome); err != nil {
		t.Fatal(err)
	}
	if client.sessionID != target.ID || client.sessionState() != target {
		t.Fatalf("applied session = %d/%p, want %d/%p", client.sessionID, client.sessionState(), target.ID, target)
	}
}

func TestAttachedRestoreCreatesSessionAndAppliesPreparedTransition(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
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

	source := NewSessionState(41)
	t.Cleanup(func() { stopState(source) })
	source.daemon = d
	source.setSessionName("source")
	source.rootDir = project
	state := newTestClient(source)
	state.setTestTerminalSize(101, 37)
	createTestWindow(source, &Pane{
		ID: testAddPaneID(source), Launch: PaneLaunch{Cwd: project}, terminal: newTerminal(101, 37),
	})
	clientForState(source).terminalCols.Store(101)
	clientForState(source).terminalRows.Store(37)

	d.sessions[source.ID] = source
	d.names[source.Name] = source
	client := clientForState(source)
	identity := &ClientIdentity{ResumeToken: "restore-command", lastAllocatedClientLayoutRevision: client.currentLayout.LayoutRevision}
	client.identity = identity
	d.clientInstances[identity] = client
	d.clientSessions[identity] = source.ID
	d.attachments[source.ID] = identity
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{WindowID: source.ActiveWindowID, SessionID: source.ID, AttachmentID: client.AttachmentID, Generation: 1}

	_, err := client.executeAttachedCommand([]string{
		"restore", "-t", "persisted", "-s", "mynewsession", "--commands=skip",
	})
	if err != nil {
		t.Fatal(err)
	}
	restored := d.sessionByName("mynewsession")
	if restored == nil {
		t.Fatal("attached restore did not create mynewsession")
	}
	if client.sessionState() != restored || testClientOf(restored) != client {
		t.Fatalf("restore did not install restored session: client=%#v restored-client=%#v", client.sessionState(), testClientOf(restored))
	}
	if restored.rootDir != project {
		t.Fatalf("restored root = %q, want %q", restored.rootDir, project)
	}

	for _, pane := range restored.PanesSnapshot() {
		_ = terminatePane(pane)
	}
	stopState(restored)
}

func TestPaneCLITargetUsesExistingNumericTargetResolver(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(17)
	t.Cleanup(func() { stopState(s) })
	s.daemon = d
	s.setSessionName("renamed-session")
	s.rootDir = t.TempDir()
	s.daemon.processObserver = emptyProcessObserver{}
	project := t.TempDir()
	newTestClient(s)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Launch: PaneLaunch{Cwd: project}})
	d.sessions[s.ID] = s
	d.names[s.Name] = s

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"set-root", "."},
		WorkingDirectory:    project,
		CallerSessionTarget: "17",
	})
	if result.exitCode != 0 {
		t.Fatalf("pane CLI set-root = %#v", result)
	}
	if s.rootDir != project {
		t.Fatalf("pane CLI set-root changed root to %q, want %q", s.rootDir, project)
	}
}

func TestPaneCLIGroupTargetResolvesToThePaneWindowLeaseSession(t *testing.T) {
	d := groupedTestDaemon()
	base := groupedTestSession(d, 1, "base")
	clientForState(base)
	pane := &Pane{ID: testAddPaneID(base), terminal: newTerminal(80, 23)}
	window, _ := createTestWindow(base, pane)
	mirror := groupedTestSession(d, 2, "mirror")
	if err := d.groupSession(base, mirror); err != nil {
		t.Fatal(err)
	}
	d.windowLeases[window.ID] = &WindowViewLease{
		WindowID: window.ID, SessionID: mirror.ID, AttachmentID: 22, Generation: 1,
	}

	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"rename-session", "renamed-mirror"},
		CallerSessionTarget: "@" + strconv.FormatUint(base.GroupID, 10),
		CallerPaneID:        pane.ID,
	})
	if result.exitCode != 0 {
		t.Fatalf("group-contextual rename-session = %#v", result)
	}
	if base.SessionName() != "base" || mirror.SessionName() != "renamed-mirror" {
		t.Fatalf("renamed sessions: base=%q mirror=%q", base.SessionName(), mirror.SessionName())
	}
}

func TestCLIAndAttachedInvocationShareOperationalCommandHandler(t *testing.T) {
	d := newCommandTestDaemon(t)
	s := NewSessionState(41)
	t.Cleanup(func() { stopState(s) })
	s.setSessionName("work")
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	left := &Pane{ID: testAddPaneID(s), Title: "left", terminal: newTerminal(80, 23)}
	window, _ := createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s), Title: "right", terminal: newTerminal(80, 23)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
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

	clientInstance := clientForState(s)
	if _, err := clientInstance.executeAttachedCommand([]string{"rename-window", "from-prompt"}); err != nil {
		t.Fatal(err)
	}
	if s.Windows[window.ID].Name != "from-prompt" {
		t.Fatalf("attached rename = %q", s.Windows[window.ID].Name)
	}
	if _, err := clientInstance.executeAttachedCommand([]string{"resize-pane", "-L", "1"}); err != nil {
		t.Fatal(err)
	}
	placements = s.Windows[window.ID].Layout.Compute(Rect{Width: 80, Height: 23})
	if placements[0].Rect.Width != initialLeftWidth+1 {
		t.Fatalf("width after attached resize = %d, want %d", placements[0].Rect.Width, initialLeftWidth+1)
	}
}

func TestSetRootUsesObservedPaneCwdAndDoesNotMoveExistingPane(t *testing.T) {
	s := NewSessionState(42)
	t.Cleanup(func() { stopState(s) })
	oldRoot := t.TempDir()
	observedCwd := t.TempDir()
	relativeRoot := filepath.Join(observedCwd, "project")
	if err := os.Mkdir(relativeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	s.rootDir = oldRoot
	s.setSessionName("work")
	s.daemon = testDaemonForState(s)
	s.daemon.processObserver = &changingProcessObserver{name: "bash", cwd: observedCwd}
	pane := &Pane{ID: 0, Root: Identity{PID: 100, BirthToken: 1000}, Launch: PaneLaunch{Cwd: oldRoot, Shell: "/bin/sh"}}
	createTestWindow(s, pane)
	client := clientForState(s)
	if _, err := client.executeAttachedCommand([]string{"set-root"}); err != nil {
		t.Fatal(err)
	}
	if s.rootDir != observedCwd || pane.Launch.Cwd != oldRoot || s.persistenceRecord().Root != observedCwd {
		t.Fatalf("set-root without path: root=%q pane=%q persistence=%#v", s.rootDir, pane.Launch.Cwd, s.persistenceRecord())
	}
	if _, err := client.executeAttachedCommand([]string{"set-root", "project"}); err != nil {
		t.Fatal(err)
	}
	if s.rootDir != relativeRoot || pane.Launch.Cwd != oldRoot {
		t.Fatalf("relative set-root: root=%q pane=%q", s.rootDir, pane.Launch.Cwd)
	}
}

func TestSetRootControlsFutureWindowsPanesAndSaveLocation(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
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
	recoveryPath := filepath.Join(d.sessionPersistenceDir, "work.session.meja")
	deadline := time.Now().Add(time.Second)
	var recovery []byte
	var recoveryErr error
	for time.Now().Before(deadline) {
		recovery, recoveryErr = os.ReadFile(recoveryPath)
		if recoveryErr == nil && strings.Contains(string(recovery), `root "`+newRoot+`"`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if recoveryErr != nil || !strings.Contains(string(recovery), `root "`+newRoot+`"`) {
		t.Fatalf("recovery file did not converge to root %q: %v\n%s", newRoot, recoveryErr, recovery)
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
