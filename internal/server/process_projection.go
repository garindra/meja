package server

import (
	"path/filepath"
	"strings"
	"unicode"
)

type processSaveProjection struct {
	Cwd     string
	Command string
}

type processSaveCandidate struct {
	Projection processSaveProjection
	Samples    int
}

const processSaveStableSamples = 2

func (d *Daemon) watchPaneProcesses(s *SessionState, pane *Pane) {
	if d == nil || pane == nil || s == nil || d.processMonitor == nil {
		return
	}
	anchor := Anchor{
		Key:         PaneKey{PaneID: pane.ID},
		Root:        pane.Root,
		PTY:         pane.PTY,
		RootIsShell: len(pane.Launch.RequestedArgv) == 0,
	}
	pane.processMonitor = d.processMonitor
	pane.processKey = anchor.Key
	d.processMonitor.Watch(s.ID, d.processObservationDelivery(s.ID), anchor)
}

func (d *Daemon) unwatchPaneProcesses(paneID uint64) {
	if d != nil && d.processMonitor != nil {
		d.processMonitor.Unwatch(PaneKey{PaneID: paneID})
	}
	if d != nil {
		delete(d.processObservations, paneID)
		delete(d.processSaveCandidates, paneID)
	}
}

// applyMonitoredProcessObservations runs in a daemon transaction. Monitor
// results are advisory and may race pane removal, so every anchor is
// revalidated against authoritative pane state before it can affect names or
// persistence.
func (d *Daemon) applyMonitoredProcessObservations(s *SessionState, batch monitoredProcessBatch) error {
	if d == nil || s == nil {
		return errSessionUnavailable
	}
	if d.processSaveCandidates == nil {
		d.processSaveCandidates = make(map[uint64]processSaveCandidate)
	}
	if d.processObservations == nil {
		d.processObservations = make(map[uint64]ProcessObservation)
	}
	latest := map[uint64]processSaveProjection{}
	if persisted := s.persistenceRecord(); persisted != nil {
		latest = plannedProcessLeaves(persisted.Plan.Windows)
	}
	nameChanged := false
	for _, update := range batch {
		paneID := update.anchor.Key.PaneID
		pane := s.Panes[paneID]
		if pane == nil || pane.Root != update.anchor.Root || pane.PTY != update.anchor.PTY {
			continue
		}
		d.processObservations[paneID] = cloneProcessObservation(update.observation)
		if update.observation.Root != nil && update.observation.Root.Cwd != "" {
			pane.KnownCwd = update.observation.Root.Cwd
		}
		projection, valid := observedProcessSaveProjection(pane, update.observation, latest[paneID])
		if valid {
			candidate := d.processSaveCandidates[paneID]
			if candidate.Projection == projection {
				candidate.Samples++
			} else {
				candidate = processSaveCandidate{Projection: projection, Samples: 1}
			}
			d.processSaveCandidates[paneID] = candidate
			requiredSamples := processSaveStableSamples
			if update.observation.Status == StatusShellOwned {
				requiredSamples = 1
			}
			if candidate.Samples >= requiredSamples && latest[paneID] != projection {
				s.persistObservedPaneForPersistence(paneID, projection)
				latest[paneID] = projection
			}
		}
		for _, window := range s.Windows {
			if window == nil || !window.AutomaticName || windowActivePaneID(window) != paneID {
				continue
			}
			name := observationWindowName(update.observation)
			if name != "" && name != window.Name {
				window.Name = name
				s.markWindowChangedForPersistence(window.ID)
				nameChanged = true
			}
			break
		}
	}
	_ = nameChanged // status delivery is posted by Daemon after this transaction.
	return nil
}

func observedProcessSaveProjection(pane *Pane, observation ProcessObservation, previous processSaveProjection) (processSaveProjection, bool) {
	if pane == nil {
		return processSaveProjection{}, false
	}
	projection := processSaveProjection{Cwd: pane.Launch.Cwd}
	if previous.Cwd != "" {
		projection.Cwd = previous.Cwd
	}
	if pane.KnownCwd != "" {
		projection.Cwd = pane.KnownCwd
	}
	if observation.Root != nil && observation.Root.Cwd != "" {
		projection.Cwd = observation.Root.Cwd
	}
	if len(pane.Launch.RequestedArgv) > 0 {
		projection.Command = shellJoin(pane.Launch.RequestedArgv)
		return projection, true
	}
	switch observation.Status {
	case StatusShellOwned:
		projection.Command = previous.Command
		return projection, true
	case StatusDetected:
		if observation.Candidate == nil {
			return processSaveProjection{}, false
		}
		if isTransientObservedCommand(observation.Candidate) {
			projection.Command = previous.Command
			return projection, true
		}
		projection.Command = observedProcessCommand(observation.Candidate)
		return projection, projection.Command != ""
	default:
		return processSaveProjection{}, false
	}
}

func observedProcessCommand(process *ObservedProcess) string {
	if process == nil {
		return ""
	}
	if command, ok := rewrittenCommandProcessTitle(process); ok {
		return command
	}
	if len(process.Argv) > 0 {
		return shellJoin(process.Argv)
	}
	return process.Name
}

// Some programs replace their argv storage with a display title. On Linux,
// /proc/<pid>/cmdline then exposes that whole title as argv[0], followed by
// NUL padding for the overwritten argv bytes. Those padding bytes are not real
// empty arguments.
//
// The rewrite irreversibly loses the original argument boundaries. Require
// the task name to corroborate the title, then tokenize the display text and
// quote every token instead of interpreting shell syntax from an untrusted
// process title.
func rewrittenCommandProcessTitle(process *ObservedProcess) (string, bool) {
	if process == nil || len(process.Argv) < 2 {
		return "", false
	}
	title := process.Argv[0]
	for _, arg := range process.Argv[1:] {
		if arg != "" {
			return "", false
		}
	}
	nameMatchesTitle := process.Name == title ||
		(len(process.Name) == 15 && strings.HasPrefix(title, process.Name)) ||
		strings.HasPrefix(title, process.Name+" ")
	if !nameMatchesTitle {
		return "", false
	}
	argv := strings.Fields(title)
	if len(argv) < 2 {
		return "", false
	}
	return shellJoin(argv), true
}

func isTransientObservedCommand(process *ObservedProcess) bool {
	if process == nil {
		return false
	}
	name := process.Name

	switch name {
	case "ls", "clear", "meja":
		return true
	default:
		return false
	}
}

func observationWindowName(observation ProcessObservation) string {
	var observed *ObservedProcess
	switch observation.Status {
	case StatusDetected:
		observed = observation.Candidate
		if isTransientObservedCommand(observed) {
			return ""
		}
	case StatusShellOwned:
		observed = observation.Root
	default:
		return ""
	}
	if observed == nil {
		return ""
	}
	name := observed.Name
	if name == "" && observed.Exe != "" {
		name = filepath.Base(strings.TrimSuffix(observed.Exe, " (deleted)"))
	}
	if name == "" && len(observed.Argv) > 0 {
		name = filepath.Base(observed.Argv[0])
	}
	return cleanProcessName(name)
}

func cleanProcessName(name string) string {
	runes := make([]rune, 0, len(name))
	for _, current := range strings.ToValidUTF8(name, "�") {
		if unicode.IsControl(current) {
			continue
		}
		runes = append(runes, current)
		if len(runes) == 64 {
			break
		}
	}
	return strings.TrimSpace(string(runes))
}
