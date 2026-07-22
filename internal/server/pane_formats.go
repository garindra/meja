package server

import (
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type formatSessionSnapshot struct {
	ID                uint64
	Name              string
	CreatedAt         int64
	Attached          bool
	ActiveWindowIndex int
	HasActiveWindow   bool
	ActivePane        *formatPaneSnapshot
	Panes             []formatPaneSnapshot
}

type formatPaneSnapshot struct {
	WindowIndex    int
	ID             uint64
	Width          int
	Height         int
	InMode         bool
	CurrentCommand string
	CurrentPath    string
}

type formatContext struct {
	session *formatSessionSnapshot
	pane    *formatPaneSnapshot
}

// formatSnapshot is collected from passive session state. The command layer
// only formats this immutable copy, so listing never reads live terminal or
// process-monitor state without synchronization.
func (s *SessionState) formatSnapshot() formatSessionSnapshot {
	snapshot := formatSessionSnapshot{}
	if s == nil {
		return snapshot
	}
	snapshot.ID = s.ID
	snapshot.Name = s.Name
	snapshot.CreatedAt = s.CreatedAt
	snapshot.Attached = s.attachedClient() != nil
	activeWindowID := s.ActiveWindowID
	activePaneID := uint64(0)
	if view, ok := s.WindowViews[activeWindowID]; ok {
		activePaneID = view.FocusedPaneID
	}
	if activeWindowID == 0 {
		ids := s.orderedWindowIDs()
		if len(ids) > 0 {
			activeWindowID = ids[0]
		}
	}
	if activePaneID == 0 {
		if window := s.Windows[activeWindowID]; window != nil {
			activePaneID = window.ActivePaneID
		}
	}

	windows := make([]*Window, 0, len(s.Windows))
	for _, window := range s.Windows {
		if window != nil {
			windows = append(windows, window)
		}
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].DisplayIndex != windows[j].DisplayIndex {
			return windows[i].DisplayIndex < windows[j].DisplayIndex
		}
		return windows[i].ID < windows[j].ID
	})

	seen := make(map[uint64]struct{}, len(s.Panes))
	for _, window := range windows {
		if window.ID == activeWindowID {
			snapshot.ActiveWindowIndex = window.DisplayIndex
			snapshot.HasActiveWindow = true
		}
		paneIDs := []uint64(nil)
		if window.Layout != nil {
			paneIDs = window.Layout.PaneIDs()
		}
		for _, paneID := range paneIDs {
			pane := s.Panes[paneID]
			if pane == nil {
				continue
			}
			seen[paneID] = struct{}{}
			snapshot.Panes = append(snapshot.Panes, s.formatPaneSnapshot(pane, window.DisplayIndex))
		}
	}

	// A live pane should normally always be present in a window layout. Keep
	// a deterministic fallback for transitional or test states.
	remaining := make([]uint64, 0, len(s.Panes))
	for paneID := range s.Panes {
		if _, ok := seen[paneID]; !ok {
			remaining = append(remaining, paneID)
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i] < remaining[j] })
	for _, paneID := range remaining {
		if pane := s.Panes[paneID]; pane != nil {
			snapshot.Panes = append(snapshot.Panes, s.formatPaneSnapshot(pane, 0))
		}
	}
	activeWindow := s.Windows[activeWindowID]
	if snapshot.HasActiveWindow && activeWindow != nil && windowHasPane(activeWindow, activePaneID) {
		for index := range snapshot.Panes {
			if snapshot.Panes[index].ID == activePaneID {
				active := snapshot.Panes[index]
				snapshot.ActivePane = &active
				break
			}
		}
	}
	return snapshot
}

func (s *SessionState) formatPaneSnapshot(pane *Pane, windowIndex int) formatPaneSnapshot {
	cols, rows := pane.TerminalSize()
	var observation ProcessObservation
	if s.daemon != nil {
		observation = s.daemon.processObservations[pane.ID]
	}
	command := formatPaneCommand(pane, observation)

	path := ""
	if observation.Root != nil {
		path = observation.Root.Cwd
	}
	if path == "" && observation.Candidate != nil {
		path = observation.Candidate.Cwd
	}
	if path == "" {
		path = pane.Launch.Cwd
	}

	return formatPaneSnapshot{
		WindowIndex:    windowIndex,
		ID:             pane.ID,
		Width:          cols,
		Height:         rows,
		InMode:         pane.isHistoryMode(),
		CurrentCommand: command,
		CurrentPath:    path,
	}
}

func formatPaneCommand(pane *Pane, observation ProcessObservation) string {
	if pane == nil {
		return ""
	}
	if command := observedProcessBasename(observation.Candidate); command != "" {
		return command
	}
	if len(pane.Launch.RequestedArgv) > 0 {
		if command := commandBasename(pane.Launch.RequestedArgv[0]); command != "" {
			return command
		}
	}
	if command := commandBasename(pane.Launch.Shell); command != "" {
		return command
	}
	if pane.Process != nil {
		return commandBasename(pane.Process.Path)
	}
	return ""
}

func observedProcessBasename(process *ObservedProcess) string {
	if process == nil {
		return ""
	}
	if len(process.Argv) > 0 {
		if command := commandBasename(process.Argv[0]); command != "" {
			return command
		}
	}
	if command := commandBasename(process.Name); command != "" {
		return command
	}
	return commandBasename(process.Exe)
}

func commandBasename(raw string) string {
	raw = strings.TrimSuffix(raw, " (deleted)")
	if raw == "" {
		return ""
	}
	return filepath.Base(raw)
}

func listPanesCommand() commandHandler {
	return func(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
		fs := commandFlagSet("list-panes")
		all := fs.Bool("a", false, "all sessions")
		target := fs.String("t", "", "session target")
		format := fs.String("F", "#{pane_id}: #{pane_in_mode}", "format")
		if err := fs.Parse(args); err != nil {
			return commandOutcome{}, err
		}
		if len(fs.Args()) != 0 {
			return commandOutcome{}, errors.New("list-panes accepts no positional arguments")
		}
		if *all && *target != "" {
			return commandOutcome{}, errors.New("list-panes -a cannot be combined with -t")
		}

		if *all {
			if d == nil {
				return commandOutcome{}, errors.New("list-panes -a requires the daemon command interface")
			}
			var sessions []*SessionState
			d.call(func() {
				sessions = make([]*SessionState, 0, len(d.sessions))
				for _, session := range d.sessions {
					if session != nil {
						sessions = append(sessions, session)
					}
				}
			})
			sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
			var output strings.Builder
			for _, session := range sessions {
				snapshot := session.formatSnapshot()
				writePaneFormatLines(&output, *format, snapshot)
			}
			data := []byte(output.String())
			return commandOutcome{Stdout: data}, nil
		}

		var session *SessionState
		var err error
		if *target != "" {
			session, err = resolveCommandSessionValue(d, ctx, *target)
		} else {
			session, err = resolveCommandCallerSession(d, ctx)
			if errors.Is(err, errNoImplicitCommandSession) {
				err = errors.New("list-panes requires -t <session-target> or -a")
			}
		}
		if err != nil {
			return commandOutcome{}, err
		}
		snapshot := session.formatSnapshot()
		var output strings.Builder
		writePaneFormatLines(&output, *format, snapshot)
		data := []byte(output.String())
		return commandOutcome{Stdout: data}, nil
	}
}

func writePaneFormatLines(output *strings.Builder, format string, snapshot formatSessionSnapshot) {
	for index := range snapshot.Panes {
		output.WriteString(expandFormat(format, formatContext{session: &snapshot, pane: &snapshot.Panes[index]}))
		output.WriteByte('\n')
	}
}

func expandFormat(format string, context formatContext) string {
	values := make(map[string]string, 16)
	if context.session != nil {
		values["session_id"] = strconv.FormatUint(context.session.ID, 10)
		values["session_name"] = context.session.Name
		values["session_created"] = strconv.FormatInt(context.session.CreatedAt, 10)
		values["window_index"] = ""
		if context.session.HasActiveWindow {
			values["window_index"] = strconv.Itoa(context.session.ActiveWindowIndex)
		}
		values["pane_id"] = ""
		values["pane_dead"] = ""
		values["pane_current_command"] = ""
		values["pane_current_path"] = ""
		values["pane_in_mode"] = ""
		values["pane_index"] = ""
		values["pane_width"] = ""
		values["pane_height"] = ""
		values["pane_in_copy_mode"] = ""
	}
	if context.pane != nil {
		values["window_index"] = strconv.Itoa(context.pane.WindowIndex)
		values["pane_id"] = strconv.FormatUint(context.pane.ID, 10)
		values["pane_dead"] = "0"
		values["pane_current_command"] = context.pane.CurrentCommand
		values["pane_current_path"] = context.pane.CurrentPath
		values["pane_in_mode"] = boolFormat(context.pane.InMode)
		values["pane_index"] = strconv.FormatUint(context.pane.ID, 10)
		values["pane_width"] = strconv.Itoa(context.pane.Width)
		values["pane_height"] = strconv.Itoa(context.pane.Height)
		values["pane_in_copy_mode"] = boolFormat(context.pane.InMode)
	}
	var output strings.Builder
	for len(format) > 0 {
		start := strings.Index(format, "#{")
		if start < 0 {
			output.WriteString(format)
			break
		}
		output.WriteString(format[:start])
		end := strings.IndexByte(format[start+2:], '}')
		if end < 0 {
			output.WriteString(format[start:])
			break
		}
		end += start + 2
		name := format[start+2 : end]
		if value, ok := values[name]; ok {
			output.WriteString(value)
		} else {
			output.WriteString(format[start : end+1])
		}
		format = format[end+1:]
	}
	return output.String()
}

func boolFormat(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
