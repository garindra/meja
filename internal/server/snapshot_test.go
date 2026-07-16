package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingProcessObserver struct {
	anchors []Anchor
}

func (o *recordingProcessObserver) Observe(_ context.Context, anchors []Anchor) map[PaneKey]ProcessObservation {
	o.anchors = append([]Anchor(nil), anchors...)
	observations := make(map[PaneKey]ProcessObservation, len(anchors))
	for _, anchor := range anchors {
		root := ObservedProcess{Identity: anchor.Root, Cwd: "/current"}
		observations[anchor.Key] = ProcessObservation{
			Key:    anchor.Key,
			Status: StatusShellOwned,
			Root:   &root,
		}
	}
	return observations
}

func TestCaptureSnapshotBuildsProcessAnchorsOutsideDurableTypes(t *testing.T) {
	session := NewSession(7)
	t.Cleanup(session.stopOperations)
	shellPane := &Pane{
		ID:   4,
		Root: Identity{PID: 104, BirthToken: 1004},
		Launch: PaneLaunch{
			Shell: "/bin/bash",
			Cwd:   "/initial",
		},
	}
	commandPane := &Pane{
		ID:   2,
		Root: Identity{PID: 102, BirthToken: 1002},
		Launch: PaneLaunch{
			Shell:         "/bin/bash",
			RequestedArgv: []string{"nvim", "notes.txt"},
			Cwd:           "/notes",
		},
	}
	if err := session.coordinate(func() error {
		session.Panes[shellPane.ID] = shellPane
		session.Panes[commandPane.ID] = commandPane
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	observer := &recordingProcessObserver{}
	snapshot, err := session.captureSnapshot(context.Background(), observer)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionID != 7 || len(snapshot.Panes) != 2 || len(observer.anchors) != 2 {
		t.Fatalf("snapshot=%#v anchors=%#v", snapshot, observer.anchors)
	}
	if snapshot.Panes[0].PaneID != 2 || snapshot.Panes[1].PaneID != 4 {
		t.Fatalf("panes are not stable by ID: %#v", snapshot.Panes)
	}
	if observer.anchors[0].RootIsShell || !observer.anchors[1].RootIsShell {
		t.Fatalf("incorrect shell classification: %#v", observer.anchors)
	}
	for _, pane := range snapshot.Panes {
		if pane.CurrentCwd != "/current" {
			t.Fatalf("pane %d cwd=%q", pane.PaneID, pane.CurrentCwd)
		}
	}

	commandPane.Launch.RequestedArgv[0] = "changed"
	if snapshot.Panes[0].Launch.RequestedArgv[0] != "nvim" {
		t.Fatal("snapshot retained mutable pane launch argv")
	}
}

func TestCaptureSnapshotMarksMissingObserverResultUnavailable(t *testing.T) {
	session := NewSession(8)
	t.Cleanup(session.stopOperations)
	pane := &Pane{
		ID:     1,
		Root:   Identity{PID: 101, BirthToken: 1001},
		Launch: PaneLaunch{Shell: "/bin/sh", Cwd: "/initial"},
	}
	if err := session.coordinate(func() error {
		session.Panes[pane.ID] = pane
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := session.captureSnapshot(context.Background(), emptyProcessObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Panes) != 1 || snapshot.Panes[0].Process.Status != StatusUnavailable ||
		snapshot.Panes[0].CurrentCwd != "/initial" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

type emptyProcessObserver struct{}

func (emptyProcessObserver) Observe(context.Context, []Anchor) map[PaneKey]ProcessObservation {
	return nil
}

func TestAutosaveSnapshotWritesNamedSessionJSON(t *testing.T) {
	session := NewSession(9)
	t.Cleanup(session.stopOperations)
	session.setSessionName("work")
	addAutosaveTestWindow(session, 0)
	directory := filepath.Join(t.TempDir(), "snapshots")

	path, err := session.autosaveSnapshot(context.Background(), directory, emptyProcessObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(directory, "work.json") {
		t.Fatalf("snapshot path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot PersistedSession
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != snapshotVersion || snapshot.Name != "work" || snapshot.SavedAt.IsZero() || len(snapshot.Windows) != 1 ||
		len(snapshot.Windows[0].Panes) != 1 || snapshot.Windows[0].Panes[0].ID != 0 ||
		snapshot.Windows[0].Layout.Pane == nil || *snapshot.Windows[0].Layout.Pane != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot permissions = %o", info.Mode().Perm())
	}

	oldModTime := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(path, oldModTime, oldModTime); err != nil {
		t.Fatal(err)
	}
	if _, err := session.autosaveSnapshot(context.Background(), directory, emptyProcessObserver{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(oldModTime) {
		t.Fatalf("unchanged snapshot was rewritten: modification time = %v", info.ModTime())
	}
}

func TestAutosaveSnapshotSkipsUnnamedSession(t *testing.T) {
	session := NewSession(10)
	t.Cleanup(session.stopOperations)
	directory := filepath.Join(t.TempDir(), "snapshots")
	path, err := session.autosaveSnapshot(context.Background(), directory, emptyProcessObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("snapshot path = %q", path)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("unnamed autosave created snapshot directory: %v", err)
	}
}

func TestSnapshotDirectoryIsBesideProfileSocket(t *testing.T) {
	controlPath := filepath.Join("home", "me", ".meja", "dev", "meja.sock")
	want := filepath.Join("home", "me", ".meja", "dev", "snapshots")
	if got := snapshotDirectory(controlPath); got != want {
		t.Fatalf("snapshot directory = %q, want %q", got, want)
	}
}

func TestSessionRenameConfirmsBeforeOverwritingSnapshot(t *testing.T) {
	session := NewSession(12)
	t.Cleanup(session.stopOperations)
	session.NewClient(clientID0)
	session.setSessionName("current")
	directory := filepath.Join(t.TempDir(), "snapshots")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "work.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		sessions:    map[uint64]*Session{session.ID: session},
		names:       map[string]*Session{"current": session},
		snapshotDir: directory,
	}
	connection := &Connection{Session: session, Daemon: d}

	d.requestSessionRename(session, "current", "work")
	prompt := session.ActivePrompt(clientID0)
	if prompt == nil || prompt.Kind != PromptKindConfirm ||
		prompt.Label != `snapshot "work" exists; overwrite? (y/N) ` {
		t.Fatalf("confirmation prompt = %#v", prompt)
	}
	if _, err := session.handleServerInputEvent(connection, session.ConsumeInputByte(clientID0, '\r')); err != nil {
		t.Fatal(err)
	}
	if session.Name != "current" || session.ActivePrompt(clientID0) != nil {
		t.Fatalf("default-No confirmation changed session: name=%q prompt=%#v", session.Name, session.ActivePrompt(clientID0))
	}

	d.requestSessionRename(session, "current", "work")
	if _, err := session.handleServerInputEvent(connection, session.ConsumeInputByte(clientID0, 'y')); err != nil {
		t.Fatal(err)
	}
	if session.Name != "work" || d.names["work"] != session || d.names["current"] != nil {
		t.Fatalf("confirmed rename failed: name=%q names=%#v", session.Name, d.names)
	}
}

func TestPersistedSessionIsTerseAndEditable(t *testing.T) {
	process := ObservedProcess{Name: "nvim", Argv: []string{"nvim", "notes with spaces.md"}, ArgvAvailable: true}
	snapshot := SessionSnapshot{
		CapturedAt:     time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
		SessionName:    "work",
		ActiveWindowID: 4,
		Windows: []WindowSnapshot{{
			WindowID: 4, Name: "editor", ActivePaneID: 2,
			Layout: PersistedLayout{Split: "vertical", Ratio: 0.6, Children: []PersistedLayout{{Pane: paneIDRef(2)}, {Pane: paneIDRef(3)}}},
		}},
		Panes: []PaneSnapshot{
			{PaneID: 2, CurrentCwd: "/repo", Process: ProcessObservation{Status: StatusDetected, Candidate: &process}},
			{PaneID: 3, CurrentCwd: "/repo", Launch: PaneLaunch{RequestedArgv: []string{"go", "test", "./..."}}},
		},
	}
	persisted, err := persistedSession(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, unwanted := range []string{"birthToken", "foregroundPgid", "sessionId", "argvAvailable", "issues"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("persisted JSON contains internal field %q:\n%s", unwanted, text)
		}
	}
	if !strings.Contains(text, `"command": "nvim 'notes with spaces.md'"`) ||
		!strings.Contains(text, `"command": "go test ./..."`) || !strings.Contains(text, `"ratio": 0.6`) {
		t.Fatalf("persisted JSON is missing readable restore data:\n%s", text)
	}
}

func TestPersistedSessionRejectsCommandsThatCouldExecuteDuringPrepare(t *testing.T) {
	snapshot := PersistedSession{
		Version: snapshotVersion, Name: "work", ActiveWindow: 1,
		Windows: []PersistedWindow{{
			ActivePane: 1, Layout: PersistedLayout{Pane: paneIDRef(1)},
			Panes: []PersistedPane{{ID: 1, Cwd: "/repo", Command: "echo safe\necho unsafe"}},
		}},
	}
	if err := validatePersistedSession(snapshot); err == nil {
		t.Fatal("snapshot with an embedded command newline was accepted")
	}
}

func TestRestoreCommandModesPrepareSkipAndRun(t *testing.T) {
	if got := string(restoredCommandInput("nvim .", restoreCommandsPrepare)); got != "nvim ." {
		t.Fatalf("prepare input = %q", got)
	}
	if got := restoredCommandInput("nvim .", restoreCommandsSkip); got != nil {
		t.Fatalf("skip input = %q", got)
	}
	if got := string(restoredCommandInput("nvim .", restoreCommandsRun)); got != "nvim .\n" {
		t.Fatalf("run input = %q", got)
	}
}

func TestDaemonRestoresSnapshotWindowsLayoutsAndPanes(t *testing.T) {
	d := newCommandTestDaemon(t)
	d.snapshotDir = filepath.Join(t.TempDir(), "snapshots")
	snapshot := PersistedSession{
		Version: snapshotVersion, Name: "work", SavedAt: time.Now(), ActiveWindow: 1,
		Windows: []PersistedWindow{{
			Name: "editor", ActivePane: 1,
			Layout: PersistedLayout{Split: "vertical", Ratio: 0.6, Children: []PersistedLayout{{Pane: paneIDRef(0)}, {Pane: paneIDRef(1)}}},
			Panes: []PersistedPane{
				{ID: 0, Cwd: t.TempDir(), Command: "echo first"},
				{ID: 1, Cwd: t.TempDir(), Command: "echo second"},
			},
		}},
	}
	if _, err := writeSessionSnapshot(d.snapshotDir, snapshot); err != nil {
		t.Fatal(err)
	}
	bootstrap, _, err := d.executeSessionOperation("restore-session-skip", commandSessionTarget{name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	session := d.sessions[bootstrap.SessionID]
	var panes []*Pane
	if err := session.coordinate(func() error {
		if session.Name != "work" || len(session.Windows) != 1 || len(session.Panes) != 2 {
			return fmt.Errorf("restored session = %#v", session)
		}
		window := session.Windows[1]
		if window == nil || window.Name != "editor" || window.ActivePaneID != 1 || window.Layout.PaneIDs()[0] != 0 {
			return fmt.Errorf("restored window = %#v", window)
		}
		for _, pane := range session.Panes {
			panes = append(panes, pane)
		}
		session.daemon = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, pane := range panes {
		_ = terminatePane(pane)
	}
	select {
	case <-session.operationsDone:
	case <-time.After(time.Second):
		t.Fatal("restored session did not stop after its panes were terminated")
	}
}

func TestAutosaveLoopWritesNamedSessionPeriodically(t *testing.T) {
	session := NewSession(11)
	session.setSessionName("periodic")
	addAutosaveTestWindow(session, 1)
	session.processNames = emptyProcessObserver{}
	directory := filepath.Join(t.TempDir(), "snapshots")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		session.runAutosave(ctx, directory, 5*time.Millisecond)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		session.stopOperations()
	})

	path := filepath.Join(directory, "periodic.json")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("periodic snapshot %q was not written", path)
}

func TestAcceptedSessionRenameRequestsImmediateAutosave(t *testing.T) {
	session := NewSession(13)
	t.Cleanup(session.stopOperations)
	session.setSessionName("before")
	if err := session.finishSessionRename("after", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.autosaveNow:
	default:
		t.Fatal("accepted session rename did not request an immediate autosave")
	}
	if err := session.finishSessionRename("after", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.autosaveNow:
		t.Fatal("unchanged session name requested an autosave")
	default:
	}
}

func TestSessionRenameImmediatelyWritesSnapshot(t *testing.T) {
	session := NewSession(14)
	session.setSessionName("before")
	addAutosaveTestWindow(session, 0)
	session.processNames = emptyProcessObserver{}
	directory := filepath.Join(t.TempDir(), "snapshots")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		session.runAutosave(ctx, directory, time.Hour)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		session.stopOperations()
	})

	if err := session.coordinate(func() error {
		return session.finishSessionRename("after", true)
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "after.json")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			if _, err := os.Stat(filepath.Join(directory, "before.json")); !os.IsNotExist(err) {
				t.Fatalf("old session name was unexpectedly saved: %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("renamed snapshot %q was not written immediately", path)
}

func addAutosaveTestWindow(session *Session, paneID uint64) {
	pane := &Pane{
		ID:     paneID,
		Title:  "shell",
		Root:   Identity{PID: 100 + int(paneID), BirthToken: 1000 + paneID},
		Launch: PaneLaunch{Shell: "/bin/sh", Cwd: "/work"},
	}
	session.CreateWindow(pane, clientID0)
}
