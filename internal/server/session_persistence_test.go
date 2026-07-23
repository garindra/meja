package server

import (
	"bytes"
	"context"
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

type changingProcessObserver struct {
	name string
	argv []string
	cwd  string
}

func (o *changingProcessObserver) Observe(_ context.Context, anchors []Anchor) map[PaneKey]ProcessObservation {
	observations := make(map[PaneKey]ProcessObservation, len(anchors))
	for _, anchor := range anchors {
		root := ObservedProcess{Identity: anchor.Root, Name: "sh", Cwd: o.cwd}
		candidate := ObservedProcess{Name: o.name, Argv: append([]string(nil), o.argv...), ArgvAvailable: true}
		observations[anchor.Key] = ProcessObservation{
			Key: anchor.Key, Status: StatusDetected, Root: &root, Candidate: &candidate,
		}
	}
	return observations
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

func TestCaptureSessionBuildsProcessAnchorsOutsideDurableTypes(t *testing.T) {
	session := NewSessionState(7)
	t.Cleanup(func() { stopState(session) })
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
	if err := runStateOperation(session, func() error {
		session.Panes[shellPane.ID] = shellPane
		session.Panes[commandPane.ID] = commandPane
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	observer := &recordingProcessObserver{}
	capture, err := session.daemon.captureSession(session, context.Background(), observer)
	if err != nil {
		t.Fatal(err)
	}
	if capture.SessionID != 7 || len(capture.Panes) != 2 || len(observer.anchors) != 2 {
		t.Fatalf("capture=%#v anchors=%#v", capture, observer.anchors)
	}
	if capture.Panes[0].PaneID != 2 || capture.Panes[1].PaneID != 4 {
		t.Fatalf("panes are not stable by ID: %#v", capture.Panes)
	}
	if observer.anchors[0].RootIsShell || !observer.anchors[1].RootIsShell {
		t.Fatalf("incorrect shell classification: %#v", observer.anchors)
	}
	for _, pane := range capture.Panes {
		if pane.CurrentCwd != "/current" {
			t.Fatalf("pane %d cwd=%q", pane.PaneID, pane.CurrentCwd)
		}
	}

	commandPane.Launch.RequestedArgv[0] = "changed"
	if capture.Panes[0].Launch.RequestedArgv[0] != "nvim" {
		t.Fatal("capture retained mutable pane launch argv")
	}
}

func TestCaptureSessionMarksMissingObserverResultUnavailable(t *testing.T) {
	session := NewSessionState(8)
	t.Cleanup(func() { stopState(session) })
	pane := &Pane{
		ID:     1,
		Root:   Identity{PID: 101, BirthToken: 1001},
		Launch: PaneLaunch{Shell: "/bin/sh", Cwd: "/initial"},
	}
	if err := runStateOperation(session, func() error {
		session.Panes[pane.ID] = pane
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	capture, err := session.daemon.captureSession(session, context.Background(), emptyProcessObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.Panes) != 1 || capture.Panes[0].Process.Status != StatusUnavailable ||
		capture.Panes[0].CurrentCwd != "/initial" {
		t.Fatalf("capture=%#v", capture)
	}
}

type emptyProcessObserver struct{}

func (emptyProcessObserver) Observe(context.Context, []Anchor) map[PaneKey]ProcessObservation {
	return nil
}

func TestSessionPersistenceWritesPrivateRecoveryFile(t *testing.T) {
	session := NewSessionState(9)
	t.Cleanup(func() { stopState(session) })
	session.setSessionName("work")
	addPersistenceTestWindow(session, 0)
	directory := filepath.Join(t.TempDir(), "sessions")

	path, err := flushTestSessionPersistence(context.Background(), session, directory)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(directory, "work.session.meja") {
		t.Fatalf("persistence path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `session name="work" id=9 saved-at=`) ||
		!strings.Contains(string(data), `root "/work"`) || !strings.Contains(string(data), `active-window 0`) ||
		!strings.Contains(string(data), `active-pane=0`) || strings.Contains(string(data), "plan ") ||
		strings.Contains(string(data), "meja 1") {
		t.Fatalf("session persistence envelope is missing:\n%s", data)
	}
	persistence, err := readSessionPersistence(path, "work")
	if err != nil {
		t.Fatal(err)
	}
	if persistence.Version != mejaFormatVersion || persistence.SessionID != 9 || persistence.Name != "work" ||
		len(persistence.Plan.Windows) != 1 || len(persistence.Plan.Windows[0].Panes) != 1 ||
		persistence.Plan.Windows[0].Layout.Pane == nil || *persistence.Plan.Windows[0].Layout.Pane != 0 {
		t.Fatalf("persistence = %#v", persistence)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("persistence permissions = %o", info.Mode().Perm())
	}
	if info, err := os.Stat(directory); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("persistence directory permissions = %o", info.Mode().Perm())
	}

	oldModTime := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(path, oldModTime, oldModTime); err != nil {
		t.Fatal(err)
	}
	if _, err := flushTestSessionPersistence(context.Background(), session, directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(oldModTime) {
		t.Fatal("a persistence flush did not rewrite the target")
	}
}

func TestGroupedPersistenceRoundTripsIndependentSessionViewMetadata(t *testing.T) {
	root := t.TempDir()
	persistence := SessionPersistence{
		Version:          mejaFormatVersion,
		Schema:           persistenceSchemaVersion,
		SessionID:        9,
		GroupID:          42,
		Name:             "mirror",
		SavedAt:          time.Now(),
		Root:             root,
		ActiveWindowID:   8,
		PreviousWindowID: 7,
		WindowViews:      []SessionViewPersistence{{WindowID: 8, DisplayIndex: 1, FocusedPaneID: 99, ZoomedPaneID: 100}},
		Plan: SessionPlan{
			Version: mejaFormatVersion, Name: "mirror", Root: root, ActiveWindow: 1,
			Windows: []PlanWindow{{ID: 8, Cwd: root, ActivePane: 99, Layout: PlanLayout{Pane: paneIDRef(99)}, Panes: []PlanPane{{ID: 99, Cwd: root}}}},
		},
	}
	path, err := writeSessionPersistence(root, persistence)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := readSessionPersistence(path, "mirror")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Schema != persistenceSchemaVersion || parsed.GroupID != persistence.GroupID ||
		parsed.ActiveWindowID != persistence.ActiveWindowID || parsed.PreviousWindowID != persistence.PreviousWindowID ||
		len(parsed.WindowViews) != 1 || parsed.WindowViews[0] != persistence.WindowViews[0] {
		t.Fatalf("grouped persistence metadata = %#v", parsed)
	}
}

func TestSessionPersistenceSkipsUnnamedSession(t *testing.T) {
	session := NewSessionState(10)
	t.Cleanup(func() { stopState(session) })
	directory := filepath.Join(t.TempDir(), "sessions")
	path, err := flushTestSessionPersistence(context.Background(), session, directory)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("persistence path = %q", path)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("unnamed session created persistence directory: %v", err)
	}
}

func TestSessionPersistenceDirectoryIsBesideProfileSocket(t *testing.T) {
	controlPath := filepath.Join("home", "me", ".meja", "dev", "meja.sock")
	want := filepath.Join("home", "me", ".meja", "dev", "sessions")
	if got := sessionPersistenceDirectory(controlPath); got != want {
		t.Fatalf("persistence directory = %q, want %q", got, want)
	}
}

func TestSessionRenameConfirmsBeforeOverwritingPersistence(t *testing.T) {
	session := NewSessionState(12)
	t.Cleanup(func() { stopState(session) })
	session.setSessionName("current")
	directory := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "work.session.meja"), []byte("meja 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		sessions:              map[uint64]*SessionState{session.ID: session},
		names:                 map[string]*SessionState{"current": session},
		sessionPersistenceDir: directory,
	}
	session.daemon = d
	connection := &ClientInstance{Daemon: d}
	setTestClient(session, connection)
	createTestWindow(session, &Pane{ID: testAddPaneID(session), terminal: newTerminal(80, 23)})

	outcome, err := renameSessionAnswer(d, session.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.applyAttachedCommandOutcome(outcome); err != nil {
		t.Fatal(err)
	}
	prompt := clientForState(session).ActivePrompt()
	if prompt == nil || prompt.Mode != PromptModeConfirm ||
		prompt.Label != `persisted session "work" exists; overwrite? (y/N) ` {
		t.Fatalf("confirmation prompt = %#v", prompt)
	}
	if _, err := connection.handleServerInputEvent(connection.ConsumeInputByte('\r')); err != nil {
		t.Fatal(err)
	}
	if session.Name != "current" || clientForState(session).ActivePrompt() != nil {
		t.Fatalf("default-No confirmation changed session: name=%q prompt=%#v", session.Name, clientForState(session).ActivePrompt())
	}

	outcome, err = renameSessionAnswer(d, session.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.applyAttachedCommandOutcome(outcome); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.handleServerInputEvent(connection.ConsumeInputByte('y')); err != nil {
		t.Fatal(err)
	}
	if session.Name != "work" || d.names["work"] != session || d.names["current"] != nil {
		t.Fatalf("confirmed rename failed: name=%q names=%#v", session.Name, d.names)
	}
}

func TestSessionPlanEncodesAsTerseEditableMeja(t *testing.T) {
	process := ObservedProcess{Name: "nvim", Argv: []string{"nvim", "notes with spaces.md"}, ArgvAvailable: true}
	capture := SessionCapture{
		CapturedAt:     time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
		SessionName:    "work",
		SessionRoot:    "/repo",
		ActiveWindowID: 4,
		Windows: []WindowCapture{{
			WindowID: 4, Name: "editor", ActivePaneID: 2,
			Layout: PlanLayout{Split: "vertical", Ratio: 0.6, Children: []PlanLayout{{Pane: paneIDRef(2)}, {Pane: paneIDRef(3)}}},
		}},
		Panes: []PaneCapture{
			{PaneID: 2, CurrentCwd: "/repo", Process: ProcessObservation{Status: StatusDetected, Candidate: &process}},
			{PaneID: 3, CurrentCwd: "/repo", Launch: PaneLaunch{RequestedArgv: []string{"go", "test", "./..."}}},
		},
	}
	persisted, err := sessionPlanFromCapture(capture)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := encodeUserSessionPlan(persisted, "/repo/work.meja")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, unwanted := range []string{"birthToken", "foregroundPgid", "sessionId", "argvAvailable", "issues"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("persisted .meja contains internal field %q:\n%s", unwanted, text)
		}
	}
	if !strings.Contains(text, `cmd "nvim 'notes with spaces.md'"`) ||
		!strings.Contains(text, `cmd "go test ./..."`) || !strings.Contains(text, `layout "main-vertical"`) {
		t.Fatalf("persisted .meja is missing readable restore data:\n%s", text)
	}
	if strings.Contains(text, "cwd ") {
		t.Fatalf("plan repeated inherited session/window root:\n%s", text)
	}
}

func TestSessionPlanRejectsCommandsThatCouldExecuteDuringPrepare(t *testing.T) {
	capture := SessionPlan{
		Version: mejaFormatVersion, Name: "work", Root: "/repo", ActiveWindow: 1,
		Windows: []PlanWindow{{
			Cwd: "/repo", ActivePane: 1, Layout: PlanLayout{Pane: paneIDRef(1)},
			Panes: []PlanPane{{ID: 1, Cwd: "/repo", Command: "echo safe\necho unsafe"}},
		}},
	}
	if err := validateSessionPlan(capture); err == nil {
		t.Fatal("capture with an embedded command newline was accepted")
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

func TestDaemonRestoresPersistenceWindowsLayoutsAndPanes(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	base := t.TempDir()
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "work", Root: base, ActiveWindow: 1,
		Windows: []PlanWindow{{
			Cwd: base, Name: "editor", ActivePane: 1,
			Layout: PlanLayout{Split: "vertical", Ratio: 0.6, Children: []PlanLayout{{Pane: paneIDRef(0)}, {Pane: paneIDRef(1)}}},
			Panes: []PlanPane{
				{ID: 0, Cwd: base, Command: "echo first"},
				{ID: 1, Cwd: base, Command: "echo second"},
			},
		}},
	}
	persistence := SessionPersistence{Version: mejaFormatVersion, SessionID: 17, Name: "work", SavedAt: time.Now(), Root: base, Plan: plan}
	if _, err := writeSessionPersistence(d.sessionPersistenceDir, persistence); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := d.executeSessionOperation("restore-session", commandSessionTarget{name: "work", restoreMode: restoreCommandsSkip})
	if err != nil {
		t.Fatal(err)
	}
	session := bootstrap.session
	var panes []*Pane
	if err := runStateOperation(session, func() error {
		if session.Name != "work" || session.rootDir != base || len(session.Windows) != 1 || len(session.Panes) != 2 {
			return fmt.Errorf("restored session = %#v", session)
		}
		window := session.Windows[1]
		if window == nil || window.Name != "editor" || window.ActivePaneID != 2 || window.Layout.PaneIDs()[0] != 1 {
			return fmt.Errorf("restored window = %#v", window)
		}
		for _, pane := range session.Panes {
			panes = append(panes, pane)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, pane := range panes {
		_ = terminatePane(pane)
	}
	stopState(session)
}

func TestPersistenceLoopWritesOnlyAfterSessionChange(t *testing.T) {
	session := NewSessionState(11)
	session.setSessionName("changed")
	addPersistenceTestWindow(session, 1)
	<-session.daemon.persistenceNow
	directory := filepath.Join(t.TempDir(), "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		session.daemon.runPersistence(ctx, directory)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		stopState(session)
	})

	path := filepath.Join(directory, "changed.session.meja")
	time.Sleep(25 * time.Millisecond)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("persistence ran without a session change: %v", err)
	}
	if err := runStateOperation(session, func() error {
		session.markSessionChangedForPersistence()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("change-driven persistence %q was not written", path)
}

func TestAcceptedSessionRenameRequestsImmediatePersistence(t *testing.T) {
	session := NewSessionState(13)
	t.Cleanup(func() { stopState(session) })
	session.setSessionName("before")
	addPersistenceTestWindow(session, 0)
	<-session.daemon.persistenceNow
	if err := session.daemon.renameSession(session, "after"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.daemon.persistenceNow:
	default:
		t.Fatal("accepted session rename did not request an immediate persistence")
	}
	if err := session.daemon.renameSession(session, "after"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.daemon.persistenceNow:
		t.Fatal("unchanged session name requested a persistence")
	default:
	}
}

func TestSessionActorPublishesOnlyPersistedStructureChanges(t *testing.T) {
	session := NewSessionState(15)
	t.Cleanup(func() { stopState(session) })
	pane := &Pane{ID: 0, Title: "shell", Launch: PaneLaunch{Cwd: "/work", Shell: "/bin/sh"}}
	if err := runStateOperation(session, func() error {
		session.setSessionName("work")
		createTestWindow(session, pane)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.daemon.persistenceNow:
	default:
		t.Fatal("window creation did not publish a persistence change")
	}
	if err := runStateOperation(session, func() error {
		_ = session.Pane(0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.daemon.persistenceNow:
		t.Fatal("read-only actor operation published a persistence change")
	default:
	}
	if err := runStateOperation(session, func() error {
		_, err := session.splitFocusedPaneNow(&Pane{ID: 1, Launch: PaneLaunch{Cwd: "/work", Shell: "/bin/sh"}}, SplitVertical)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	<-session.daemon.persistenceNow
	if err := runStateOperation(session, func() error {
		_, _, err := session.toggleZoomNow()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.daemon.persistenceNow:
		t.Fatal("non-persisted zoom-only change published a persistence change")
	default:
	}
}

func TestMonitoredObservationsPersistStableCommandsAndIgnoreTransientCommands(t *testing.T) {
	session := NewSessionState(16)
	t.Cleanup(func() { stopState(session) })
	pane := &Pane{
		ID: 0, Title: "shell", Root: Identity{PID: 100, BirthToken: 1000},
		Launch: PaneLaunch{Cwd: "/work", Shell: "/bin/sh"},
	}
	if err := runStateOperation(session, func() error {
		session.setSessionName("work")
		createTestWindow(session, pane)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-session.daemon.persistenceNow

	observer := &changingProcessObserver{name: "nvim", argv: []string{"nvim", "."}, cwd: "/work"}
	anchor := Anchor{Key: PaneKey{PaneID: pane.ID}, Root: pane.Root, PTY: pane.PTY, RootIsShell: true}
	applyObservation := func() {
		t.Helper()
		observation := observer.Observe(context.Background(), []Anchor{anchor})[anchor.Key]
		if err := runStateOperation(session, func() error {
			return session.daemon.applyMonitoredProcessObservations(session, monitoredProcessBatch{{anchor: anchor, observation: observation}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range processSaveStableSamples {
		applyObservation()
	}
	select {
	case <-session.daemon.persistenceNow:
	default:
		t.Fatal("stable nvim command did not publish a persistence change")
	}
	assertCachedCommand := func(want string) {
		t.Helper()
		var got string
		if err := runStateOperation(session, func() error {
			got = plannedProcessLeaves(session.persistenceRecord().Plan.Windows)[0].Command
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("cached command = %q, want %q", got, want)
		}
	}
	assertCachedCommand("nvim .")
	var automaticName string
	var persistedName string
	if err := runStateOperation(session, func() error {
		for _, window := range session.Windows {
			automaticName = window.Name
		}
		persistedName = session.persistenceRecord().Plan.Windows[0].Name
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if automaticName != "nvim" {
		t.Fatalf("automatic window name = %q, want process-observer result", automaticName)
	}
	if persistedName != "nvim" {
		t.Fatalf("persisted automatic window name = %q, want process-observer result", persistedName)
	}
	for range processSaveStableSamples {
		applyObservation()
	}
	select {
	case <-session.daemon.persistenceNow:
		t.Fatal("unchanged stable command published another persistence change")
	default:
	}

	observer.name, observer.argv = "ls", []string{"ls", "-la"}
	for range processSaveStableSamples {
		applyObservation()
	}
	select {
	case <-session.daemon.persistenceNow:
		t.Fatal("transient ls command published a persistence change")
	default:
	}
	assertCachedCommand("nvim .")
	automaticName = ""
	if err := runStateOperation(session, func() error {
		for _, window := range session.Windows {
			automaticName = window.Name
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if automaticName != "nvim" {
		t.Fatalf("transient command changed automatic window name to %q", automaticName)
	}

	observer.name, observer.argv = "meja", []string{"meja", "save", "-t", "work", "-o", "dev.meja"}
	for range processSaveStableSamples {
		applyObservation()
	}
	select {
	case <-session.daemon.persistenceNow:
		t.Fatal("transient meja command published a persistence change")
	default:
	}
	assertCachedCommand("nvim .")

	observer.name, observer.argv = "vite", []string{"vite"}
	for range processSaveStableSamples {
		applyObservation()
	}
	select {
	case <-session.daemon.persistenceNow:
	default:
		t.Fatal("stable vite command did not publish a persistence change")
	}
}

func TestShellReturnRetainsLastMeaningfulCommandAndUpdatesCwd(t *testing.T) {
	pane := &Pane{Launch: PaneLaunch{Cwd: "/old"}}
	root := ObservedProcess{Cwd: "/new"}
	got, ok := observedProcessSaveProjection(pane, ProcessObservation{
		Status: StatusShellOwned,
		Root:   &root,
	}, processSaveProjection{Cwd: "/old", Command: "nvim ."})
	if !ok {
		t.Fatal("shell-owned observation was rejected")
	}
	want := processSaveProjection{Cwd: "/new", Command: "nvim ."}
	if got != want {
		t.Fatalf("shell return projection = %#v, want %#v", got, want)
	}
}

func TestSessionRenameImmediatelyWritesPersistence(t *testing.T) {
	session := NewSessionState(14)
	session.setSessionName("before")
	addPersistenceTestWindow(session, 0)
	directory := filepath.Join(t.TempDir(), "sessions")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		session.daemon.runPersistence(ctx, directory)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		stopState(session)
	})

	if err := session.daemon.renameSession(session, "after"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "after.session.meja")
	oldPath := filepath.Join(directory, "before.session.meja")
	deadline := time.Now().Add(time.Second)
	var newErr, oldErr error
	for time.Now().Before(deadline) {
		_, newErr = os.Stat(path)
		_, oldErr = os.Stat(oldPath)
		if newErr == nil && os.IsNotExist(oldErr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("renamed persistence did not converge: new %q: %v; old %q: %v", path, newErr, oldPath, oldErr)
}

func addPersistenceTestWindow(session *SessionState, paneID uint64) {
	if session.daemon == nil {
		session.daemon = testDaemonForState(session)
	}
	session.rootDir = "/work"
	pane := &Pane{
		ID:     paneID,
		Title:  "shell",
		Root:   Identity{PID: 100 + int(paneID), BirthToken: 1000 + paneID},
		Launch: PaneLaunch{Shell: "/bin/sh", Cwd: "/work"},
	}
	createTestWindow(session, pane)
}

func TestMejaFileReferenceFormatResolvesInheritedDirectories(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "dev.meja")
	data := `// Development session for acme-admin.
root "."
window name="editor" {
    cwd "frontend/"
    pane {
        cwd "react/"
        cmd "vite"
        tile x=0 y=0 w=72 h=100
    }
    pane {
        cwd "server/"
        cmd "npm run dev"
        tile x=72 y=0 w=28 h=100
    }
}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	capture, err := readUserSessionPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(root, "frontend")
	if capture.Windows[0].Panes[0].Cwd != filepath.Join(wantRoot, "react") ||
		capture.Windows[0].Panes[1].Cwd != filepath.Join(wantRoot, "server") {
		t.Fatalf("resolved pane directories = %#v", capture.Windows[0].Panes)
	}
	if capture.Windows[0].Layout.Split != "vertical" || capture.Windows[0].Layout.Ratio != .72 {
		t.Fatalf("restored layout = %#v", capture.Windows[0].Layout)
	}
}

func TestMejaEncodingUsesSessionRootAndWindowCwds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "acme-admin")
	zero, one := uint64(0), uint64(1)
	capture := SessionPlan{
		Version: mejaFormatVersion, Name: "dev", Root: root, ActiveWindow: 1,
		Windows: []PlanWindow{{
			ID: 0, Cwd: filepath.Join(root, "frontend"), ActivePane: 0,
			Layout: PlanLayout{Split: "vertical", Ratio: .72, Children: []PlanLayout{{Pane: &zero}, {Pane: &one}}},
			Panes: []PlanPane{
				{ID: 0, Cwd: filepath.Join(root, "frontend", "react"), Command: "vite"},
				{ID: 1, Cwd: filepath.Join(root, "frontend", "server"), Command: "npm run dev"},
			},
		}},
	}
	outputPath := filepath.Join(root, ".meja", "dev.meja")
	data, report, err := encodeUserSessionPlan(capture, outputPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if report.AbsolutePanePaths != 0 {
		t.Fatalf("portability report = %#v", report)
	}
	for _, fragment := range []string{`root ".."`, `window`, `cwd "frontend"`, `cwd "react"`, `cwd "server"`} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("encoded .meja file is missing %q:\n%s", fragment, text)
		}
	}
	if strings.Contains(text, "saved-at") || strings.Contains(text, "session ") || strings.Contains(text, "plan ") {
		t.Fatalf("user-owned file contains recovery metadata:\n%s", text)
	}
}

func TestUserMejaDropsStaleWindowCwdOutsideChangedRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "test-project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "dev", Root: root, ActiveWindow: 1,
		Windows: []PlanWindow{{
			Cwd: home, ActivePane: 0, Layout: PlanLayout{Pane: paneIDRef(0)},
			Panes: []PlanPane{{ID: 0, Cwd: root}},
		}},
	}
	path := filepath.Join(root, "dev.meja")
	encoded, report, err := encodeUserSessionPlan(plan, path)
	if err != nil {
		t.Fatal(err)
	}
	if report.AbsolutePanePaths != 0 || strings.Contains(string(encoded), "cwd ") {
		t.Fatalf("portable plan retained stale cwd:\n%s\n%#v", encoded, report)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readUserSessionPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Windows[0].Cwd != root || parsed.Windows[0].Panes[0].Cwd != root {
		t.Fatalf("restored directories = window %q pane %q, want root %q", parsed.Windows[0].Cwd, parsed.Windows[0].Panes[0].Cwd, root)
	}
}

func TestPrivateMejaDoesNotRetainCreationTimeWindowCwd(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "test-project")
	s := NewSessionState(1)
	s.Name = "dev"
	s.rootDir = root
	pane := &Pane{ID: testAddPaneID(s), Launch: PaneLaunch{Cwd: home, Shell: "/bin/bash"}}
	createTestWindow(s, pane)
	plan, err := s.projectSessionPlan(map[uint64]processSaveProjection{
		pane.ID: {Cwd: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	persistence := SessionPersistence{
		Version: mejaFormatVersion, SessionID: 1, Name: "dev", SavedAt: time.Now(), Root: root, Plan: *plan,
	}
	encoded, err := encodeSessionPersistence(persistence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `cwd "`+home+`"`) || !strings.Contains(string(encoded), `cwd "`+root+`"`) {
		t.Fatalf("private persistence retained a creation-time window cwd:\n%s", encoded)
	}
}

func TestUserMejaRootIsRelativeToOutputFile(t *testing.T) {
	project := filepath.Join(t.TempDir(), "acme")
	for _, directory := range []string{project, filepath.Join(project, ".meja"), filepath.Join(project, "apps", "admin")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name       string
		root       string
		outputPath string
		want       string
	}{
		{name: "beside project", root: project, outputPath: filepath.Join(project, "dev.meja"), want: "."},
		{name: "metadata directory", root: project, outputPath: filepath.Join(project, ".meja", "dev.meja"), want: ".."},
		{name: "session subdirectory", root: filepath.Join(project, "apps", "admin"), outputPath: filepath.Join(project, "dev.meja"), want: "apps/admin"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan := SessionPlan{
				Version: mejaFormatVersion, Name: "dev", Root: test.root, ActiveWindow: 1,
				Windows: []PlanWindow{{Cwd: test.root, ActivePane: 0, Layout: PlanLayout{Pane: paneIDRef(0)}, Panes: []PlanPane{{ID: 0, Cwd: test.root}}}},
			}
			encoded, _, err := encodeUserSessionPlan(plan, test.outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), `root "`+test.want+`"`) || strings.Contains(string(encoded), `root "`+test.root+`"`) {
				t.Fatalf("encoded root =\n%s", encoded)
			}
		})
	}
}

func TestMejaEncodingIsDeterministic(t *testing.T) {
	base := t.TempDir()
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "dev", Root: base, ActiveWindow: 1,
		Windows: []PlanWindow{{
			ID: 0, Cwd: base, Name: "editor", ActivePane: 0, Layout: PlanLayout{Pane: paneIDRef(0)},
			Panes: []PlanPane{{ID: 0, Cwd: base}},
		}},
	}
	var first []byte
	for attempt := 0; attempt < 100; attempt++ {
		encoded, _, err := encodeUserSessionPlan(plan, filepath.Join(base, "dev.meja"))
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			first = encoded
			continue
		}
		if !bytes.Equal(encoded, first) {
			t.Fatalf("encoding changed between attempts:\nfirst:\n%s\nlater:\n%s", first, encoded)
		}
	}
}

func TestMejaNamedLayoutsRoundTripWithoutTiles(t *testing.T) {
	base := t.TempDir()
	paneIDs := []uint64{0, 1, 2, 3}
	for _, candidate := range namedLayoutPresets {
		t.Run(candidate.name, func(t *testing.T) {
			layout, err := planLayout(buildPresetLayout(paneIDs, 0, candidate.preset))
			if err != nil {
				t.Fatal(err)
			}
			plan := SessionPlan{Version: mejaFormatVersion, Name: "dev", Root: base, ActiveWindow: 1}
			window := PlanWindow{Cwd: base, ActivePane: 0, Layout: layout}
			for _, paneID := range paneIDs {
				window.Panes = append(window.Panes, PlanPane{ID: paneID, Cwd: base})
			}
			plan.Windows = []PlanWindow{window}
			path := filepath.Join(base, candidate.name+".meja")
			encoded, _, err := encodeUserSessionPlan(plan, path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(encoded)
			if !strings.Contains(text, `layout "`+candidate.name+`"`) || strings.Contains(text, "tile ") {
				t.Fatalf("named layout was not encoded declaratively:\n%s", text)
			}
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			parsed, err := readUserSessionPlan(path)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Windows[0].NamedLayout != candidate.name {
				t.Fatalf("parsed named layout = %q, want %q", parsed.Windows[0].NamedLayout, candidate.name)
			}
		})
	}
}

func TestMejaFlatFormatUsesDocumentOrderAndImplicitDefaultFocus(t *testing.T) {
	base := t.TempDir()
	firstPane, secondPane := uint64(3), uint64(7)
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "ignored", Root: base, ActiveWindow: 1,
		Windows: []PlanWindow{
			{ID: 10, Cwd: base, Name: "frontend", ActivePane: firstPane, Layout: PlanLayout{Pane: &firstPane}, Panes: []PlanPane{{ID: firstPane, Cwd: base, Command: "vite"}}},
			{ID: 20, Cwd: base, Name: "server", ActivePane: secondPane, Layout: PlanLayout{Pane: &secondPane}, Panes: []PlanPane{{ID: secondPane, Cwd: base, Command: "npm run dev"}}},
		},
	}
	path := filepath.Join(base, "dev.meja")
	encoded, _, err := encodeUserSessionPlan(plan, path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "plan ") || strings.Contains(text, "session ") ||
		strings.Contains(text, "active-window") || strings.Contains(text, "active-pane") {
		t.Fatalf("flat plan contains a wrapper or redundant default focus:\n%s", text)
	}
	if strings.Count(text, "pane {") != 2 || strings.Contains(text, "pane 0") || strings.Contains(text, "window 0") {
		t.Fatalf("numeric window or pane IDs were serialized:\n%s", text)
	}
	if strings.Contains(text, "layout ") || strings.Contains(text, "tile ") {
		t.Fatalf("single-pane windows contain redundant layout data:\n%s", text)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readUserSessionPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "dev" || parsed.ActiveWindow != 1 || parsed.Windows[0].ActivePane != 0 || parsed.Windows[1].ActivePane != 0 {
		t.Fatalf("parsed defaults/name = %#v", parsed)
	}
}

func TestUserMejaEncodingOmitsAllActiveState(t *testing.T) {
	base := t.TempDir()
	zero, one, two := uint64(0), uint64(1), uint64(2)
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "dev", Root: base, ActiveWindow: 2,
		Windows: []PlanWindow{
			{Cwd: base, ActivePane: zero, Layout: PlanLayout{Pane: &zero}, Panes: []PlanPane{{ID: zero, Cwd: base}}},
			{Cwd: base, ActivePane: two, Layout: PlanLayout{Split: "vertical", Ratio: .5, Children: []PlanLayout{{Pane: &one}, {Pane: &two}}}, Panes: []PlanPane{{ID: one, Cwd: base}, {ID: two, Cwd: base}}},
		},
	}
	encoded, _, err := encodeUserSessionPlan(plan, filepath.Join(base, "dev.meja"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "active-window") || strings.Contains(text, "active-pane") {
		t.Fatalf("user plan contains private active state:\n%s", text)
	}
}

func TestUserMejaParsingIgnoresPrivateActiveState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.meja")
	data := `root "."
active-window 1
window active-pane=0 {
    pane
}
window active-pane=1 {
    layout "even-horizontal"
    pane
    pane
}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := readUserSessionPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ActiveWindow != 1 || plan.Windows[0].ActivePane != 0 || plan.Windows[1].ActivePane != 0 {
		t.Fatalf("user plan retained private active state: %#v", plan)
	}
}

func TestPrivateMejaRetainsPositionalActiveState(t *testing.T) {
	base := t.TempDir()
	zero, one, two := uint64(0), uint64(1), uint64(2)
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "dev", Root: base, ActiveWindow: 2,
		Windows: []PlanWindow{
			{Cwd: base, ActivePane: zero, Layout: PlanLayout{Pane: &zero}, Panes: []PlanPane{{ID: zero, Cwd: base, Shell: "/bin/zsh"}}},
			{Cwd: base, ActivePane: two, Layout: PlanLayout{Split: "vertical", Ratio: .5, Children: []PlanLayout{{Pane: &one}, {Pane: &two}}}, Panes: []PlanPane{{ID: one, Cwd: filepath.Join(base, "web"), Shell: "/bin/zsh"}, {ID: two, Cwd: filepath.Join(base, "api"), Shell: "/bin/zsh"}}},
		},
	}
	persistence := SessionPersistence{Version: mejaFormatVersion, SessionID: 17, Name: "dev", SavedAt: time.Now(), Root: base, Plan: plan}
	encoded, err := encodeSessionPersistence(persistence)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, "active-window 1") || !strings.Contains(text, "active-pane=1") ||
		!strings.Contains(text, `cwd "`+filepath.Join(base, "web")+`"`) || !strings.Contains(text, `shell "/bin/zsh"`) {
		t.Fatalf("private recovery state is incomplete:\n%s", text)
	}
	path := filepath.Join(base, "dev.session.meja")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readSessionPersistence(path, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Plan.ActiveWindow != 2 || parsed.Plan.Windows[1].ActivePane != 1 {
		t.Fatalf("private active state did not round trip: %#v", parsed.Plan)
	}
}

func TestMejaEncodingOmitsAutomaticBashWindowNameOnly(t *testing.T) {
	base := t.TempDir()
	encode := func(automatic bool) string {
		t.Helper()
		plan := SessionPlan{
			Version: mejaFormatVersion, Name: "dev", Root: base, ActiveWindow: 1,
			Windows: []PlanWindow{{
				Cwd: base, Name: "bash", AutomaticName: automatic, ActivePane: 0,
				Layout: PlanLayout{Pane: paneIDRef(0)}, Panes: []PlanPane{{ID: 0, Cwd: base}},
			}},
		}
		encoded, _, err := encodeUserSessionPlan(plan, filepath.Join(base, "dev.meja"))
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	if text := encode(true); strings.Contains(text, `name="bash"`) {
		t.Fatalf("automatic bash name was persisted:\n%s", text)
	}
	if text := encode(false); !strings.Contains(text, `name="bash"`) {
		t.Fatalf("explicit bash name was omitted:\n%s", text)
	}
}

func TestMejaEncodingKeepsPaneOutsideSessionRootAbsolute(t *testing.T) {
	base := "/srv/acme"
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "dev", Root: base, ActiveWindow: 1,
		Windows: []PlanWindow{{
			ID: 0, Cwd: base, ActivePane: 0, Layout: PlanLayout{Pane: paneIDRef(0)},
			Panes: []PlanPane{{ID: 0, Cwd: "/srv/shared/logs"}},
		}},
	}
	data, report, err := encodeUserSessionPlan(plan, "/srv/acme/dev.meja")
	if err != nil {
		t.Fatal(err)
	}
	if report.AbsolutePanePaths != 1 || !strings.Contains(string(data), `cwd "/srv/shared/logs"`) {
		t.Fatalf("encoded plan/report =\n%s\n%#v", data, report)
	}
}

func TestUnknownNamedLayoutFallsBackToExplicitTiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.meja")
	data := `root "."
window {
    layout "future-layout"
    pane {
        tile x=0 y=0 w=50 h=100
    }
    pane {
        tile x=50 y=0 w=50 h=100
    }
}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := readUserSessionPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Windows[0].NamedLayout != "" || plan.Windows[0].Layout.Split != "vertical" {
		t.Fatalf("unknown layout did not use tile fallback: %#v", plan.Windows[0])
	}
}

func TestMejaFileToleratesLegacyVersionAndFutureExtensions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.meja")
	data := `meja 99
future-top-level enabled=true
root "." future=true
window name="editor" future=true {
    future-window "value"
    pane future=true {
        future-pane "value"
    }
}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUserSessionPlan(path); err != nil {
		t.Fatalf("compatible extensions were rejected: %v\n%s", err, data)
	}
}

func TestMejaFileRejectsOverlappingCustomTiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.meja")
	data := "root \".\"\nwindow { pane { tile x=0 y=0 w=100 h=100 } pane { tile x=0 y=0 w=100 h=100 } }\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUserSessionPlan(path); err == nil {
		t.Fatalf("invalid .meja file was accepted:\n%s", data)
	}
}

func TestUserPlanAndSessionPersistenceEnvelopesAreNotInterchangeable(t *testing.T) {
	base := t.TempDir()
	plan := SessionPlan{
		Version: mejaFormatVersion, Name: "dev", Root: base, ActiveWindow: 1,
		Windows: []PlanWindow{{
			ID: 0, Cwd: base, ActivePane: 0, Layout: PlanLayout{Pane: paneIDRef(0)},
			Panes: []PlanPane{{ID: 0, Cwd: base}},
		}},
	}
	userPath := filepath.Join(base, "dev.meja")
	if _, err := writeUserMejaFile(userPath, plan, false); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionPersistence(userPath, "dev"); err == nil {
		t.Fatal("user-owned plan was accepted as private session persistence")
	}
	persistenceDir := filepath.Join(t.TempDir(), "sessions")
	persistence := SessionPersistence{
		Version: mejaFormatVersion, SessionID: 17, Name: "dev", SavedAt: time.Now(), Root: base, Plan: plan,
	}
	persistencePath, err := writeSessionPersistence(persistenceDir, persistence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readUserSessionPlan(persistencePath); err == nil {
		t.Fatal("private session persistence was accepted as a user-owned plan")
	}
}

func TestSessionPersistenceRejectsRelativeMachineRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.session.meja")
	data := `session name="dev" id=17 saved-at="2026-07-18T12:00:00+07:00" {
    root "."
    active-window 0
    window active-pane=0 {
        pane
    }
}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionPersistence(path, "dev"); err == nil {
		t.Fatal("relative machine-local recovery root was accepted")
	}
}
