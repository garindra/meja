package server

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	processReconcileInterval      = 30 * time.Second
	processInitialBatchDelay      = 10 * time.Millisecond
	processActivitySettleDelay    = 250 * time.Millisecond
	processActivityMinInterval    = 500 * time.Millisecond
	processTransitionConfirmDelay = 500 * time.Millisecond
	processObservationTimeout     = time.Second
)

type monitoredProcessObservation struct {
	anchor      Anchor
	observation ProcessObservation
}

type monitoredProcessBatch []monitoredProcessObservation

type processActivity struct {
	key     PaneKey
	pending *atomic.Bool
}

type processMonitorCommand struct {
	watch       *processWatch
	unwatch     *PaneKey
	dropSession uint64
	activity    *processActivity
}

type processWatch struct {
	anchor         Anchor
	session        *Session
	fingerprint    processFingerprint
	hasFingerprint bool
	observation    ProcessObservation
	hasObservation bool

	activityPending *atomic.Bool
	initialDue      time.Time
	activityDue     time.Time
	trailingDue     time.Time
	confirmDue      time.Time
	lastProbe       time.Time
}

type processFingerprint struct {
	foregroundPGID   int
	status           ProcessStatus
	rootCwd          string
	candidate        Identity
	candidateCommand string
	candidateCwd     string
	windowName       string
}

// ProcessMonitor is the daemon-wide authority for live pane process watches.
// PTY producers edge-coalesce activity before it reaches this actor. The actor
// uses foreground-PGID probes for activity and reserves shared process-table
// observations for new jobs, stability confirmation, initialization, and
// reconciliation.
type ProcessMonitor struct {
	commands          chan processMonitorCommand
	done              chan struct{}
	observer          ProcessObserver
	foregroundProbe   func(Anchor) (int, error)
	reconcileInterval time.Duration
}

func NewProcessMonitor(ctx context.Context, observer ProcessObserver) *ProcessMonitor {
	if observer == nil {
		observer = NewProcessObserver()
	}
	monitor := &ProcessMonitor{
		commands:          make(chan processMonitorCommand, 256),
		done:              make(chan struct{}),
		observer:          observer,
		foregroundProbe:   foregroundGroupForAnchor,
		reconcileInterval: processReconcileInterval,
	}
	go monitor.run(ctx)
	return monitor
}

func (m *ProcessMonitor) Watch(session *Session, anchor Anchor) {
	if m == nil || session == nil {
		return
	}
	watch := &processWatch{anchor: anchor, session: session}
	select {
	case m.commands <- processMonitorCommand{watch: watch}:
	case <-m.done:
	}
}

func (m *ProcessMonitor) Unwatch(key PaneKey) {
	if m == nil {
		return
	}
	select {
	case m.commands <- processMonitorCommand{unwatch: &key}:
	case <-m.done:
	}
}

func (m *ProcessMonitor) DropSession(sessionID uint64) {
	if m == nil {
		return
	}
	select {
	case m.commands <- processMonitorCommand{dropSession: sessionID}:
	case <-m.done:
	}
}

// Activity is intentionally nonblocking: pane output must never wait for
// process bookkeeping. The producer clears its edge if the mailbox is full.
func (m *ProcessMonitor) Activity(key PaneKey, pending *atomic.Bool) bool {
	if m == nil {
		return false
	}
	activity := &processActivity{key: key, pending: pending}
	select {
	case m.commands <- processMonitorCommand{activity: activity}:
		return true
	case <-m.done:
		return false
	default:
		return false
	}
}

func (m *ProcessMonitor) run(ctx context.Context) {
	defer close(m.done)
	watches := make(map[PaneKey]*processWatch)

	reconcileInterval := m.reconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = processReconcileInterval
	}
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var timerC <-chan time.Time

	resetTimer := func(now time.Time) {
		next, ok := nextProcessDue(watches)
		if !ok {
			if timerC != nil && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerC = nil
			return
		}
		if timerC != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		delay := next.Sub(now)
		if delay < 0 {
			delay = 0
		}
		timer.Reset(delay)
		timerC = timer.C
	}

	for {
		select {
		case command := <-m.commands:
			m.applyCommand(watches, command)
			resetTimer(time.Now())
		case now := <-timerC:
			m.refreshDue(ctx, watches, now)
			resetTimer(time.Now())
		case now := <-reconcile.C:
			m.observeDeep(ctx, allProcessWatches(watches), now)
			resetTimer(time.Now())
		case <-ctx.Done():
			for _, watch := range watches {
				watch.releaseActivityEdge()
			}
			return
		}
	}
}

func (m *ProcessMonitor) applyCommand(watches map[PaneKey]*processWatch, command processMonitorCommand) {
	now := time.Now()
	if command.watch != nil {
		command.watch.initialDue = now.Add(processInitialBatchDelay)
		watches[command.watch.anchor.Key] = command.watch
	}
	if command.activity != nil {
		watch := watches[command.activity.key]
		if watch == nil {
			if command.activity.pending != nil {
				command.activity.pending.Store(false)
			}
		} else {
			watch.activityPending = command.activity.pending
			due := now.Add(processActivitySettleDelay)
			if minimum := watch.lastProbe.Add(processActivityMinInterval); due.Before(minimum) {
				due = minimum
			}
			if watch.activityDue.IsZero() || due.Before(watch.activityDue) {
				watch.activityDue = due
			}
		}
	}
	if command.unwatch != nil {
		if watch := watches[*command.unwatch]; watch != nil {
			watch.releaseActivityEdge()
		}
		delete(watches, *command.unwatch)
	}
	if command.dropSession != 0 {
		for key, watch := range watches {
			if key.SessionID == command.dropSession {
				watch.releaseActivityEdge()
				delete(watches, key)
			}
		}
	}
}

func (m *ProcessMonitor) refreshDue(ctx context.Context, watches map[PaneKey]*processWatch, now time.Time) {
	deep := make([]*processWatch, 0)
	deliver := make([]*processWatch, 0)
	for _, watch := range watches {
		initial := dueProcessTime(watch.initialDue, now)
		activity := dueProcessTime(watch.activityDue, now)
		trailing := dueProcessTime(watch.trailingDue, now)
		confirm := dueProcessTime(watch.confirmDue, now)
		if !initial && !activity && !trailing && !confirm {
			continue
		}
		if initial {
			watch.initialDue = time.Time{}
		}
		if activity {
			watch.activityDue = time.Time{}
		}
		if trailing {
			watch.trailingDue = time.Time{}
		}
		if confirm {
			watch.confirmDue = time.Time{}
		}

		if initial || !watch.hasFingerprint {
			deep = append(deep, watch)
		} else {
			probe := m.foregroundProbe
			if probe == nil {
				probe = foregroundGroupForAnchor
			}
			foregroundPGID, err := probe(watch.anchor)
			watch.lastProbe = now
			switch {
			case err != nil:
				deep = append(deep, watch)
			case foregroundPGID != watch.fingerprint.foregroundPGID:
				if watch.anchor.RootIsShell && foregroundPGID == watch.anchor.Root.PID && watch.observation.Root != nil {
					watch.observation = shellOwnedObservation(watch.anchor, foregroundPGID, watch.observation.Root)
					watch.fingerprint, watch.hasFingerprint = fingerprintProcessObservation(watch.anchor, watch.observation)
					watch.hasObservation = true
					deliver = append(deliver, watch)
				} else {
					deep = append(deep, watch)
				}
			case confirm:
				// Stability confirmation is another shared observation, not a
				// replay of cached data. This keeps persistence from accepting a
				// command that only briefly occupied the foreground group.
				deep = append(deep, watch)
			}
		}
		if activity {
			watch.releaseActivityEdge()
			watch.trailingDue = now.Add(processTransitionConfirmDelay)
		}
	}

	if len(deep) > 0 {
		m.observeDeep(ctx, deep, now)
	}
	m.deliver(deliver)
}

func foregroundGroupForAnchor(anchor Anchor) (int, error) {
	return foregroundProcessGroup(anchor.PTY)
}

func (m *ProcessMonitor) observeDeep(ctx context.Context, watches []*processWatch, now time.Time) {
	if len(watches) == 0 {
		return
	}
	anchors := make([]Anchor, len(watches))
	for index, watch := range watches {
		anchors[index] = watch.anchor
	}
	scanCtx, cancel := context.WithTimeout(ctx, processObservationTimeout)
	observations := m.observer.Observe(scanCtx, anchors)
	cancel()
	deliver := make([]*processWatch, 0, len(watches))
	for _, watch := range watches {
		observation, ok := observations[watch.anchor.Key]
		if !ok {
			continue
		}
		previousFingerprint := watch.fingerprint
		hadFingerprint := watch.hasFingerprint
		watch.observation = observation
		watch.hasObservation = true
		watch.lastProbe = now
		fingerprintChanged := true
		if fingerprint, ok := fingerprintProcessObservation(watch.anchor, observation); ok {
			watch.fingerprint = fingerprint
			watch.hasFingerprint = true
			fingerprintChanged = !hadFingerprint || fingerprint != previousFingerprint
		} else {
			watch.hasFingerprint = false
		}
		if observation.Status == StatusDetected && fingerprintChanged {
			watch.confirmDue = now.Add(processTransitionConfirmDelay)
		}
		deliver = append(deliver, watch)
	}
	m.deliver(deliver)
}

func (m *ProcessMonitor) deliver(watches []*processWatch) {
	bySession := make(map[*Session]monitoredProcessBatch)
	seen := make(map[*processWatch]struct{}, len(watches))
	for _, watch := range watches {
		if watch == nil || !watch.hasObservation || watch.session == nil {
			continue
		}
		if _, ok := seen[watch]; ok {
			continue
		}
		seen[watch] = struct{}{}
		bySession[watch.session] = append(bySession[watch.session], monitoredProcessObservation{
			anchor: watch.anchor, observation: watch.observation,
		})
	}
	for session, batch := range bySession {
		select {
		case session.processObservationUpdates <- batch:
		default:
		}
	}
}

func (watch *processWatch) releaseActivityEdge() {
	if watch.activityPending != nil {
		watch.activityPending.Store(false)
		watch.activityPending = nil
	}
}

func shellOwnedObservation(anchor Anchor, foregroundPGID int, root *ObservedProcess) ProcessObservation {
	copyRoot := *root
	return ProcessObservation{
		Key: anchor.Key, ForegroundPGID: foregroundPGID, Status: StatusShellOwned,
		Root: &copyRoot, Processes: []ObservedProcess{copyRoot},
	}
}

func dueProcessTime(due, now time.Time) bool {
	return !due.IsZero() && !due.After(now)
}

func nextProcessDue(watches map[PaneKey]*processWatch) (time.Time, bool) {
	var next time.Time
	for _, watch := range watches {
		for _, due := range []time.Time{watch.initialDue, watch.activityDue, watch.trailingDue, watch.confirmDue} {
			if !due.IsZero() && (next.IsZero() || due.Before(next)) {
				next = due
			}
		}
	}
	return next, !next.IsZero()
}

func allProcessWatches(watches map[PaneKey]*processWatch) []*processWatch {
	all := make([]*processWatch, 0, len(watches))
	for _, watch := range watches {
		all = append(all, watch)
	}
	return all
}

func fingerprintProcessObservation(anchor Anchor, observation ProcessObservation) (processFingerprint, bool) {
	if observation.Root == nil || observation.Root.Identity != anchor.Root || observation.ForegroundPGID <= 0 {
		return processFingerprint{}, false
	}
	fingerprint := processFingerprint{
		foregroundPGID: observation.ForegroundPGID,
		status:         observation.Status,
		rootCwd:        observation.Root.Cwd,
		windowName:     observationWindowName(observation),
	}
	if observation.Candidate != nil {
		fingerprint.candidate = observation.Candidate.Identity
		fingerprint.candidateCommand = observedProcessCommand(observation.Candidate)
		fingerprint.candidateCwd = observation.Candidate.Cwd
	}
	return fingerprint, true
}
