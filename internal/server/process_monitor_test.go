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
	firstKey := PaneKey{PaneID: 1}
	secondKey := PaneKey{PaneID: 2}
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
	firstUpdates := make(chan monitoredProcessBatch, 2)
	secondUpdates := make(chan monitoredProcessBatch, 2)
	watches := map[PaneKey]*processWatch{
		firstKey: {
			anchor:    Anchor{Key: firstKey, Root: Identity{PID: 101, BirthToken: 1001}},
			sessionID: 1,
			deliver:   func(batch monitoredProcessBatch) { firstUpdates <- batch },
		},
		secondKey: {
			anchor:    Anchor{Key: secondKey, Root: Identity{PID: 202, BirthToken: 2002}},
			sessionID: 2,
			deliver:   func(batch monitoredProcessBatch) { secondUpdates <- batch },
		},
	}
	now := time.Now()
	monitor.observeDeep(context.Background(), allProcessWatches(watches), now)
	if len(observer.calls) != 1 || len(observer.calls[0]) != 2 {
		t.Fatalf("initial observer calls = %#v, want one two-anchor batch", observer.calls)
	}
	<-firstUpdates
	<-secondUpdates
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
	key := PaneKey{PaneID: 2}
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
	key := PaneKey{PaneID: 1}
	rootIdentity := Identity{PID: 100, BirthToken: 1000}
	observer := &monitorRecordingObserver{pgids: map[PaneKey]int{key: 200}, rootCwd: "/work"}
	monitor := &ProcessMonitor{
		observer: observer,
		foregroundProbe: func(_ Anchor) (int, error) {
			return rootIdentity.PID, nil
		},
	}
	updates := make(chan monitoredProcessBatch, 2)
	watch := &processWatch{
		anchor:    Anchor{Key: key, Root: rootIdentity, RootIsShell: true},
		sessionID: 1,
		deliver:   func(batch monitoredProcessBatch) { updates <- batch },
	}
	monitor.observeDeep(context.Background(), []*processWatch{watch}, time.Now())
	<-updates

	now := time.Now()
	watch.activityDue = now
	monitor.refreshDue(context.Background(), map[PaneKey]*processWatch{key: watch}, now)
	if len(observer.calls) != 1 {
		t.Fatalf("shell return caused %d deep observations, want initial observation only", len(observer.calls))
	}
	batch := <-updates
	if len(batch) != 1 || batch[0].observation.Status != StatusShellOwned {
		t.Fatalf("shell return batch = %#v, want one shell-owned observation", batch)
	}
}

func TestProcessMonitorUnwatchAndDropSession(t *testing.T) {
	monitor := &ProcessMonitor{}
	watches := map[PaneKey]*processWatch{
		{PaneID: 1}: {sessionID: 1},
		{PaneID: 2}: {sessionID: 1},
		{PaneID: 3}: {sessionID: 2},
	}
	key := PaneKey{PaneID: 1}
	monitor.applyCommand(watches, processMonitorCommand{unwatch: &key})
	if _, ok := watches[key]; ok {
		t.Fatal("unwatched pane remains registered")
	}
	monitor.applyCommand(watches, processMonitorCommand{dropSession: 1})
	if len(watches) != 1 {
		t.Fatalf("watches after dropping session = %#v", watches)
	}
	if _, ok := watches[PaneKey{PaneID: 3}]; !ok {
		t.Fatal("dropping session removed another session's watch")
	}
}

func TestProcessMonitorTransferKeepsSharedPaneWatch(t *testing.T) {
	from, to := uint64(1), uint64(2)
	key := PaneKey{PaneID: 9}
	watches := map[PaneKey]*processWatch{
		key: {anchor: Anchor{Key: key}, sessionID: from, deliver: func(monitoredProcessBatch) {}},
	}
	monitor := &ProcessMonitor{}
	monitor.applyCommand(watches, processMonitorCommand{
		transfer: &processMonitorTransfer{from: from, toID: to, to: func(monitoredProcessBatch) {}},
	})
	watch := watches[key]
	if watch == nil || watch.sessionID != to {
		t.Fatalf("transferred watch = %#v, want same pane watch delivered to session %d", watch, to)
	}
}

func TestSessionAppliesCurrentMonitoredObservationAndRejectsStaleAnchor(t *testing.T) {
	session := NewSessionState(9)
	session.daemon = testDaemonForState(session)
	t.Cleanup(func() { stopState(session) })
	pane := &Pane{
		ID: 1, Root: Identity{PID: 101, BirthToken: 1001},
		Launch: PaneLaunch{Shell: "/bin/sh", Cwd: "/work"},
	}
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
	}
	root := ObservedProcess{Identity: pane.Root, Name: "sh", Cwd: "/work"}
	candidate := ObservedProcess{
		Identity: Identity{PID: 102, BirthToken: 1002}, Name: "nvim",
		Argv: []string{"nvim", "."}, ArgvAvailable: true, Cwd: "/work",
	}
	update := monitoredProcessObservation{
		anchor: Anchor{Key: PaneKey{PaneID: pane.ID}, Root: pane.Root, PTY: pane.PTY, RootIsShell: true},
		observation: ProcessObservation{
			Key: PaneKey{PaneID: pane.ID}, ForegroundPGID: 102,
			Status: StatusDetected, Root: &root, Candidate: &candidate,
		},
	}
	for range processSaveStableSamples {
		if err := runStateOperation(session, func() error {
			return session.daemon.applyMonitoredProcessObservations(session, monitoredProcessBatch{update})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runStateOperation(session, func() error {
		window, _ := testActiveWindow(session)
		if window.Name != "nvim" {
			t.Fatalf("automatic window name = %q", window.Name)
		}
		if got := plannedProcessLeaves(session.persistenceRecord().Plan.Windows)[pane.ID].Command; got != "nvim ." {
			t.Fatalf("persisted command = %q", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stale := update
	stale.anchor.Root.BirthToken++
	stale.observation.Candidate.Name = "vite"
	if err := runStateOperation(session, func() error {
		return session.daemon.applyMonitoredProcessObservations(session, monitoredProcessBatch{stale})
	}); err != nil {
		t.Fatal(err)
	}
	if err := runStateOperation(session, func() error {
		window, _ := testActiveWindow(session)
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
	if err := runStateOperation(session, func() error {
		return session.daemon.applyMonitoredProcessObservations(session, monitoredProcessBatch{shell})
	}); err != nil {
		t.Fatal(err)
	}
	if err := runStateOperation(session, func() error {
		window, _ := testActiveWindow(session)
		if window.Name != "sh" {
			t.Fatalf("shell return window name = %q", window.Name)
		}
		got := plannedProcessLeaves(session.persistenceRecord().Plan.Windows)[pane.ID]
		if got.Command != "nvim ." || got.Cwd != "/next" {
			t.Fatalf("shell return persistence = %#v, want retained command and updated cwd", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
