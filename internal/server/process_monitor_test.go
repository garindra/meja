package server

import (
	"context"
	"testing"
	"time"
)

type monitorRecordingObserver struct {
	calls   [][]Anchor
	pgids   map[PaneKey]int
	rootCwd string
}

func (o *monitorRecordingObserver) Observe(_ context.Context, anchors []Anchor) map[PaneKey]ProcessObservation {
	o.calls = append(o.calls, append([]Anchor(nil), anchors...))
	observations := make(map[PaneKey]ProcessObservation, len(anchors))
	for _, anchor := range anchors {
		root := ObservedProcess{Identity: anchor.Root, Cwd: o.rootCwd}
		observations[anchor.Key] = ProcessObservation{
			Key: anchor.Key, ForegroundPGID: o.pgids[anchor.Key], Status: StatusShellOwned, Root: &root,
		}
	}
	return observations
}

func TestProcessMonitorBatchesDeepObservationsAndUsesCheapActivityProbes(t *testing.T) {
	firstKey := PaneKey{SessionID: 1, PaneID: 1}
	secondKey := PaneKey{SessionID: 2, PaneID: 1}
	observer := &monitorRecordingObserver{
		pgids:   map[PaneKey]int{firstKey: 101, secondKey: 202},
		rootCwd: "/work",
	}
	probes := map[PaneKey]int{}
	monitor := &ProcessMonitor{
		observer: observer,
		foregroundProbe: func(anchor Anchor) (int, error) {
			return probes[anchor.Key], nil
		},
		reconcileInterval: time.Hour,
	}
	firstSession := &Session{processObservationUpdates: make(chan monitoredProcessBatch, 2)}
	secondSession := &Session{processObservationUpdates: make(chan monitoredProcessBatch, 2)}
	watches := map[PaneKey]*processWatch{
		firstKey: {
			anchor:  Anchor{Key: firstKey, Root: Identity{PID: 101, BirthToken: 1001}},
			session: firstSession,
		},
		secondKey: {
			anchor:  Anchor{Key: secondKey, Root: Identity{PID: 202, BirthToken: 2002}},
			session: secondSession,
		},
	}
	now := time.Now()
	monitor.observeDeep(context.Background(), allProcessWatches(watches), now)
	if len(observer.calls) != 1 || len(observer.calls[0]) != 2 {
		t.Fatalf("initial observer calls = %#v, want one two-anchor batch", observer.calls)
	}
	<-firstSession.processObservationUpdates
	<-secondSession.processObservationUpdates
	for key, watch := range watches {
		probes[key] = watch.fingerprint.foregroundPGID
	}

	for _, watch := range watches {
		watch.activityDue = now.Add(time.Second)
	}
	monitor.refreshDue(context.Background(), watches, now.Add(time.Second))
	if len(observer.calls) != 1 {
		t.Fatalf("stable probes caused %d deep observations, want 1 total", len(observer.calls))
	}

	probes[firstKey]++
	watches[firstKey].activityDue = now.Add(2 * time.Second)
	monitor.refreshDue(context.Background(), watches, now.Add(2*time.Second))
	if len(observer.calls) != 2 || len(observer.calls[1]) != 1 || observer.calls[1][0].Key != firstKey {
		t.Fatalf("changed observer call = %#v, want only first watch", observer.calls)
	}
}

func TestPaneActivityIsSourceCoalescedUntilMonitorProbe(t *testing.T) {
	key := PaneKey{SessionID: 1, PaneID: 2}
	monitor := &ProcessMonitor{
		commands: make(chan processMonitorCommand, 4),
		done:     make(chan struct{}),
		foregroundProbe: func(_ Anchor) (int, error) {
			return 10, nil
		},
	}
	pane := &Pane{processMonitor: monitor, processKey: key}
	pane.notifyProcessActivity()
	pane.notifyProcessActivity()
	if got := len(monitor.commands); got != 1 {
		t.Fatalf("coalesced activity messages = %d, want 1", got)
	}
	if !pane.processActivityPending.Load() {
		t.Fatal("source edge cleared before monitor probe")
	}

	watch := &processWatch{
		anchor:         Anchor{Key: key, Root: Identity{PID: 10}},
		fingerprint:    processFingerprint{foregroundPGID: 10},
		hasFingerprint: true,
	}
	watches := map[PaneKey]*processWatch{key: watch}
	monitor.applyCommand(watches, <-monitor.commands)
	monitor.refreshDue(context.Background(), watches, watch.activityDue)
	if pane.processActivityPending.Load() {
		t.Fatal("source edge remained set after monitor probe")
	}
}

func TestForegroundProcessInputHints(t *testing.T) {
	for _, input := range [][]byte{{'\r'}, {'\n'}, {0x03}, {0x04}, {0x1a}, {0x1c}, []byte("echo ok\r")} {
		if !inputMayChangeForegroundProcess(input) {
			t.Fatalf("input %q did not signal a possible foreground transition", input)
		}
	}
	for _, input := range [][]byte{nil, []byte("nvim"), {0x1b, '[', 'A'}} {
		if inputMayChangeForegroundProcess(input) {
			t.Fatalf("ordinary input %q signaled a foreground transition", input)
		}
	}
}

func TestShellReturnUsesCachedRootWithoutDeepObservation(t *testing.T) {
	key := PaneKey{SessionID: 1, PaneID: 1}
	rootIdentity := Identity{PID: 100, BirthToken: 1000}
	observer := &monitorRecordingObserver{pgids: map[PaneKey]int{key: 200}, rootCwd: "/work"}
	monitor := &ProcessMonitor{
		observer: observer,
		foregroundProbe: func(_ Anchor) (int, error) {
			return rootIdentity.PID, nil
		},
	}
	session := &Session{processObservationUpdates: make(chan monitoredProcessBatch, 2)}
	watch := &processWatch{
		anchor:  Anchor{Key: key, Root: rootIdentity, RootIsShell: true},
		session: session,
	}
	monitor.observeDeep(context.Background(), []*processWatch{watch}, time.Now())
	<-session.processObservationUpdates

	now := time.Now()
	watch.activityDue = now
	monitor.refreshDue(context.Background(), map[PaneKey]*processWatch{key: watch}, now)
	if len(observer.calls) != 1 {
		t.Fatalf("shell return caused %d deep observations, want initial observation only", len(observer.calls))
	}
	batch := <-session.processObservationUpdates
	if len(batch) != 1 || batch[0].observation.Status != StatusShellOwned {
		t.Fatalf("shell return batch = %#v, want one shell-owned observation", batch)
	}
}

func TestProcessMonitorUnwatchAndDropSession(t *testing.T) {
	monitor := &ProcessMonitor{}
	watches := map[PaneKey]*processWatch{
		{SessionID: 1, PaneID: 1}: {},
		{SessionID: 1, PaneID: 2}: {},
		{SessionID: 2, PaneID: 1}: {},
	}
	key := PaneKey{SessionID: 1, PaneID: 1}
	monitor.applyCommand(watches, processMonitorCommand{unwatch: &key})
	if _, ok := watches[key]; ok {
		t.Fatal("unwatched pane remains registered")
	}
	monitor.applyCommand(watches, processMonitorCommand{dropSession: 1})
	if len(watches) != 1 {
		t.Fatalf("watches after dropping session = %#v", watches)
	}
	if _, ok := watches[PaneKey{SessionID: 2, PaneID: 1}]; !ok {
		t.Fatal("dropping session removed another session's watch")
	}
}

func TestSessionAppliesCurrentMonitoredObservationAndRejectsStaleAnchor(t *testing.T) {
	session := NewSession(9)
	t.Cleanup(session.stopOperations)
	pane := &Pane{
		ID: 1, Root: Identity{PID: 101, BirthToken: 1001},
		Launch: PaneLaunch{Shell: "/bin/sh", Cwd: "/work"},
	}
	if err := session.coordinate(func() error {
		session.setSessionName("work")
		session.CreateWindow(pane, clientID0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.persistenceNow:
	default:
	}
	root := ObservedProcess{Identity: pane.Root, Name: "sh", Cwd: "/work"}
	candidate := ObservedProcess{
		Identity: Identity{PID: 102, BirthToken: 1002}, Name: "nvim",
		Argv: []string{"nvim", "."}, ArgvAvailable: true, Cwd: "/work",
	}
	update := monitoredProcessObservation{
		anchor: Anchor{Key: PaneKey{SessionID: session.ID, PaneID: pane.ID}, Root: pane.Root, PTY: pane.PTY, RootIsShell: true},
		observation: ProcessObservation{
			Key: PaneKey{SessionID: session.ID, PaneID: pane.ID}, ForegroundPGID: 102,
			Status: StatusDetected, Root: &root, Candidate: &candidate,
		},
	}
	for range processSaveStableSamples {
		if err := session.coordinate(func() error {
			return session.applyMonitoredProcessObservations(monitoredProcessBatch{update})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.coordinate(func() error {
		window, _ := session.ActiveWindow(clientID0)
		if window.Name != "nvim" {
			t.Fatalf("automatic window name = %q", window.Name)
		}
		if got := plannedProcessLeaves(session.sessionPersistence.Plan.Windows)[pane.ID].Command; got != "nvim ." {
			t.Fatalf("persisted command = %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stale := update
	stale.anchor.Root.BirthToken++
	stale.observation.Candidate.Name = "vite"
	if err := session.coordinate(func() error {
		return session.applyMonitoredProcessObservations(monitoredProcessBatch{stale})
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.coordinate(func() error {
		window, _ := session.ActiveWindow(clientID0)
		if window.Name != "nvim" {
			t.Fatalf("stale observation changed window name to %q", window.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	shell := update
	shell.observation.ForegroundPGID = pane.Root.PID
	shell.observation.Status = StatusShellOwned
	shell.observation.Candidate = nil
	shell.observation.Root.Name = "sh"
	shell.observation.Root.Cwd = "/next"
	if err := session.coordinate(func() error {
		return session.applyMonitoredProcessObservations(monitoredProcessBatch{shell})
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.coordinate(func() error {
		window, _ := session.ActiveWindow(clientID0)
		if window.Name != "sh" {
			t.Fatalf("shell return window name = %q", window.Name)
		}
		got := plannedProcessLeaves(session.sessionPersistence.Plan.Windows)[pane.ID]
		if got.Command != "nvim ." || got.Cwd != "/next" {
			t.Fatalf("shell return persistence = %#v, want retained command and updated cwd", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
