package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/garindra/meja/internal/control"
	"github.com/garindra/meja/internal/protocol"
)

var errSessionChangedDuringSnapshot = errors.New("session changed during process snapshot")

const (
	snapshotVersion         = 1
	sessionAutosaveInterval = 5 * time.Second
	sessionAutosaveTimeout  = 10 * time.Second
)

func restoreModeForOperation(operation string) (restoreCommandMode, bool) {
	switch operation {
	case "restore-session-prepare":
		return restoreCommandsPrepare, true
	case "restore-session-skip":
		return restoreCommandsSkip, true
	case "restore-session-run":
		return restoreCommandsRun, true
	default:
		return "", false
	}
}

type restoreCommandMode string

const (
	restoreCommandsPrepare restoreCommandMode = "prepare"
	restoreCommandsSkip    restoreCommandMode = "skip"
	restoreCommandsRun     restoreCommandMode = "run"
)

// SessionSnapshot is the internal, observation-heavy capture. It is projected
// into PersistedSession before anything is written for users to edit.
type SessionSnapshot struct {
	CapturedAt     time.Time
	SessionID      uint64
	SessionName    string
	ActiveWindowID uint64
	Windows        []WindowSnapshot
	Panes          []PaneSnapshot
}

type PaneSnapshot struct {
	PaneID     uint64
	Launch     PaneLaunch
	CurrentCwd string
	Process    ProcessObservation
}

type WindowSnapshot struct {
	WindowID      uint64
	Name          string
	AutomaticName bool
	ActivePaneID  uint64
	Layout        PersistedLayout
}

// PersistedSession is deliberately small, stable, and user-editable.
type PersistedSession struct {
	Version      int               `json:"version"`
	Name         string            `json:"name"`
	SavedAt      time.Time         `json:"savedAt"`
	ActiveWindow int               `json:"activeWindow"`
	Windows      []PersistedWindow `json:"windows"`
}

type PersistedWindow struct {
	Name          string          `json:"name,omitempty"`
	AutomaticName bool            `json:"automaticName,omitempty"`
	ActivePane    uint64          `json:"activePane"`
	Layout        PersistedLayout `json:"layout"`
	Panes         []PersistedPane `json:"panes"`
}

type PersistedPane struct {
	ID      uint64 `json:"id"`
	Cwd     string `json:"cwd"`
	Shell   string `json:"shell,omitempty"`
	Command string `json:"command,omitempty"`
}

type PersistedLayout struct {
	Pane     *uint64           `json:"pane,omitempty"`
	Split    string            `json:"split,omitempty"`
	Ratio    float64           `json:"ratio,omitempty"`
	Children []PersistedLayout `json:"children,omitempty"`
}

type paneSnapshotInput struct {
	pane   *Pane
	launch PaneLaunch
	anchor Anchor
}

type windowSnapshotInput struct {
	window         *Window
	windowID       uint64
	displayIndex   int
	layoutRevision uint64
	name           string
	automaticName  bool
	activePaneID   uint64
	layout         PersistedLayout
}

func (s *Session) captureSnapshot(ctx context.Context, observer ProcessObserver) (SessionSnapshot, error) {
	if observer == nil {
		observer = NewProcessObserver()
	}
	if err := ctx.Err(); err != nil {
		return SessionSnapshot{}, err
	}

	var inputs []paneSnapshotInput
	var windowInputs []windowSnapshotInput
	var paneCount int
	var sessionName string
	var activeWindowID uint64
	if err := s.coordinate(func() error {
		paneCount = len(s.Panes)
		sessionName = s.Name
		if client := s.Clients[clientID0]; client != nil {
			activeWindowID = client.ActiveWindowID
		}
		inputs = make([]paneSnapshotInput, 0, len(s.Panes))
		for _, pane := range s.Panes {
			if pane == nil || pane.Root.PID <= 0 {
				continue
			}
			launch := clonePaneLaunch(pane.Launch)
			inputs = append(inputs, paneSnapshotInput{
				pane:   pane,
				launch: launch,
				anchor: Anchor{
					Key: PaneKey{
						SessionID: s.ID,
						PaneID:    pane.ID,
					},
					Root:        pane.Root,
					PTY:         pane.PTY,
					RootIsShell: len(launch.RequestedArgv) == 0,
				},
			})
		}
		windowInputs = make([]windowSnapshotInput, 0, len(s.Windows))
		for _, window := range s.Windows {
			layout, err := persistedLayout(window.Layout)
			if err != nil {
				return err
			}
			windowInputs = append(windowInputs, windowSnapshotInput{
				window:         window,
				windowID:       window.ID,
				displayIndex:   window.DisplayIndex,
				layoutRevision: window.LayoutRevision,
				name:           window.Name,
				automaticName:  window.AutomaticName,
				activePaneID:   windowActivePaneID(window),
				layout:         layout,
			})
		}
		return nil
	}); err != nil {
		return SessionSnapshot{}, err
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].anchor.Key.PaneID < inputs[j].anchor.Key.PaneID })
	sort.Slice(windowInputs, func(i, j int) bool { return windowInputs[i].displayIndex < windowInputs[j].displayIndex })

	anchors := make([]Anchor, len(inputs))
	for index := range inputs {
		anchors[index] = inputs[index].anchor
	}
	observations := observer.Observe(ctx, anchors)
	if err := ctx.Err(); err != nil {
		return SessionSnapshot{}, err
	}

	valid := false
	if err := s.coordinate(func() error {
		valid = len(s.Panes) == paneCount && len(s.Windows) == len(windowInputs) && s.Name == sessionName
		for _, input := range inputs {
			current := s.Panes[input.anchor.Key.PaneID]
			if current != input.pane || current.Root != input.anchor.Root {
				valid = false
				break
			}
		}
		for _, input := range windowInputs {
			current := s.Windows[input.windowID]
			if current != input.window || current.LayoutRevision != input.layoutRevision || current.Name != input.name ||
				current.AutomaticName != input.automaticName || windowActivePaneID(current) != input.activePaneID {
				valid = false
				break
			}
		}
		return nil
	}); err != nil {
		return SessionSnapshot{}, err
	}
	if !valid {
		return SessionSnapshot{}, errSessionChangedDuringSnapshot
	}

	snapshot := SessionSnapshot{
		CapturedAt:     time.Now().UTC(),
		SessionID:      s.ID,
		SessionName:    sessionName,
		ActiveWindowID: activeWindowID,
		Windows:        make([]WindowSnapshot, 0, len(windowInputs)),
		Panes:          make([]PaneSnapshot, 0, len(inputs)),
	}
	for _, input := range windowInputs {
		snapshot.Windows = append(snapshot.Windows, WindowSnapshot{
			WindowID:      input.windowID,
			Name:          input.name,
			AutomaticName: input.automaticName,
			ActivePaneID:  input.activePaneID,
			Layout:        input.layout,
		})
	}
	for _, input := range inputs {
		observation, ok := observations[input.anchor.Key]
		if !ok {
			observation = ProcessObservation{
				Key:    input.anchor.Key,
				Status: StatusUnavailable,
				Issues: []string{"observer returned no result for pane"},
			}
		}
		cwd := input.launch.Cwd
		if observation.Root != nil && observation.Root.Cwd != "" {
			cwd = observation.Root.Cwd
		}
		snapshot.Panes = append(snapshot.Panes, PaneSnapshot{
			PaneID:     input.anchor.Key.PaneID,
			Launch:     clonePaneLaunch(input.launch),
			CurrentCwd: cwd,
			Process:    observation,
		})
	}
	return snapshot, nil
}

func clonePaneLaunch(launch PaneLaunch) PaneLaunch {
	launch.RequestedArgv = append([]string(nil), launch.RequestedArgv...)
	return launch
}

func persistedLayout(layout LayoutNode) (PersistedLayout, error) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node == nil {
			return PersistedLayout{}, errors.New("snapshot layout contains an invalid pane")
		}
		return PersistedLayout{Pane: paneIDRef(node.PaneID)}, nil
	case *SplitLayout:
		if node == nil {
			return PersistedLayout{}, errors.New("snapshot layout contains a nil split")
		}
		first, err := persistedLayout(node.First)
		if err != nil {
			return PersistedLayout{}, err
		}
		second, err := persistedLayout(node.Second)
		if err != nil {
			return PersistedLayout{}, err
		}
		direction := "vertical"
		if node.Direction == SplitHorizontal {
			direction = "horizontal"
		} else if node.Direction != SplitVertical {
			return PersistedLayout{}, errors.New("snapshot layout has an invalid split direction")
		}
		ratio := node.Ratio
		if ratio == 0 || ratio >= 1000 {
			ratio = 500
		}
		return PersistedLayout{Split: direction, Ratio: float64(ratio) / 1000, Children: []PersistedLayout{first, second}}, nil
	default:
		return PersistedLayout{}, errors.New("snapshot layout has an unknown node")
	}
}

func persistedSession(snapshot SessionSnapshot) (PersistedSession, error) {
	persisted := PersistedSession{
		Version: snapshotVersion,
		Name:    snapshot.SessionName,
		SavedAt: snapshot.CapturedAt,
		Windows: make([]PersistedWindow, 0, len(snapshot.Windows)),
	}
	panes := make(map[uint64]PaneSnapshot, len(snapshot.Panes))
	for _, pane := range snapshot.Panes {
		panes[pane.PaneID] = pane
	}
	for index, window := range snapshot.Windows {
		if window.WindowID == snapshot.ActiveWindowID {
			persisted.ActiveWindow = index + 1
		}
		output := PersistedWindow{
			Name:          window.Name,
			AutomaticName: window.AutomaticName,
			ActivePane:    window.ActivePaneID,
			Layout:        window.Layout,
		}
		for _, paneID := range persistedLayoutPaneIDs(window.Layout) {
			pane, ok := panes[paneID]
			if !ok {
				return PersistedSession{}, fmt.Errorf("window references pane %d without a process snapshot", paneID)
			}
			output.Panes = append(output.Panes, PersistedPane{
				ID:      pane.PaneID,
				Cwd:     pane.CurrentCwd,
				Shell:   pane.Launch.Shell,
				Command: persistedPaneCommand(pane),
			})
		}
		persisted.Windows = append(persisted.Windows, output)
	}
	if persisted.ActiveWindow == 0 && len(persisted.Windows) > 0 {
		persisted.ActiveWindow = 1
	}
	if err := validatePersistedSession(persisted); err != nil {
		return PersistedSession{}, err
	}
	return persisted, nil
}

func persistedPaneCommand(pane PaneSnapshot) string {
	if len(pane.Launch.RequestedArgv) > 0 {
		return shellJoin(pane.Launch.RequestedArgv)
	}
	if pane.Process.Status != StatusDetected || pane.Process.Candidate == nil {
		return ""
	}
	if len(pane.Process.Candidate.Argv) > 0 {
		return shellJoin(pane.Process.Candidate.Argv)
	}
	return pane.Process.Candidate.Name
}

func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for index, arg := range argv {
		if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') &&
				!strings.ContainsRune("_@%+=:,./-", r)
		}) == -1 {
			quoted[index] = arg
		} else {
			quoted[index] = control.ShellQuote(arg)
		}
	}
	return strings.Join(quoted, " ")
}

func paneIDRef(paneID uint64) *uint64 {
	return &paneID
}

func persistedLayoutPaneIDs(layout PersistedLayout) []uint64 {
	if layout.Pane != nil {
		return []uint64{*layout.Pane}
	}
	var ids []uint64
	for _, child := range layout.Children {
		ids = append(ids, persistedLayoutPaneIDs(child)...)
	}
	return ids
}

func validatePersistedSession(snapshot PersistedSession) error {
	if snapshot.Version != snapshotVersion {
		return fmt.Errorf("unsupported snapshot version %d", snapshot.Version)
	}
	if err := control.ValidateSessionName(snapshot.Name); err != nil {
		return fmt.Errorf("snapshot session name: %w", err)
	}
	if len(snapshot.Windows) == 0 {
		return errors.New("snapshot contains no windows")
	}
	if snapshot.ActiveWindow < 1 || snapshot.ActiveWindow > len(snapshot.Windows) {
		return errors.New("snapshot activeWindow is out of range")
	}
	seenPanes := make(map[uint64]struct{})
	for windowIndex, window := range snapshot.Windows {
		if len(window.Panes) == 0 || len(window.Panes) > int(protocol.MaxRenderSlots) {
			return fmt.Errorf("snapshot window %d has an invalid pane count", windowIndex+1)
		}
		windowPanes := make(map[uint64]struct{}, len(window.Panes))
		for _, pane := range window.Panes {
			if pane.ID == ^uint64(0) {
				return fmt.Errorf("snapshot pane ID %d cannot be restored", pane.ID)
			}
			if _, exists := seenPanes[pane.ID]; exists {
				return fmt.Errorf("snapshot pane ID %d is duplicated", pane.ID)
			}
			seenPanes[pane.ID] = struct{}{}
			windowPanes[pane.ID] = struct{}{}
			if pane.Cwd != "" && pane.Cwd != "~" && !strings.HasPrefix(pane.Cwd, "~/") && !filepath.IsAbs(pane.Cwd) {
				return fmt.Errorf("snapshot pane %d cwd must be absolute or start with ~/", pane.ID)
			}
			if strings.IndexFunc(pane.Command, unicode.IsControl) >= 0 {
				return fmt.Errorf("snapshot pane %d command must not contain control characters", pane.ID)
			}
			if strings.IndexFunc(pane.Shell, unicode.IsControl) >= 0 {
				return fmt.Errorf("snapshot pane %d shell must not contain control characters", pane.ID)
			}
		}
		layoutPanes, err := validatePersistedLayout(window.Layout)
		if err != nil {
			return fmt.Errorf("snapshot window %d layout: %w", windowIndex+1, err)
		}
		if len(layoutPanes) != len(windowPanes) {
			return fmt.Errorf("snapshot window %d layout does not contain each pane exactly once", windowIndex+1)
		}
		for _, paneID := range layoutPanes {
			if _, exists := windowPanes[paneID]; !exists {
				return fmt.Errorf("snapshot window %d layout references unknown pane %d", windowIndex+1, paneID)
			}
		}
		if _, exists := windowPanes[window.ActivePane]; !exists {
			return fmt.Errorf("snapshot window %d activePane is unknown", windowIndex+1)
		}
	}
	return nil
}

func validatePersistedLayout(layout PersistedLayout) ([]uint64, error) {
	if layout.Pane != nil {
		if layout.Split != "" || layout.Ratio != 0 || len(layout.Children) != 0 {
			return nil, errors.New("pane layout cannot also be a split")
		}
		return []uint64{*layout.Pane}, nil
	}
	if layout.Split != "vertical" && layout.Split != "horizontal" {
		return nil, errors.New("split must be vertical or horizontal")
	}
	if layout.Ratio <= 0 || layout.Ratio >= 1 || len(layout.Children) != 2 {
		return nil, errors.New("split requires ratio between 0 and 1 and two children")
	}
	first, err := validatePersistedLayout(layout.Children[0])
	if err != nil {
		return nil, err
	}
	second, err := validatePersistedLayout(layout.Children[1])
	if err != nil {
		return nil, err
	}
	ids := append(first, second...)
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("pane %d appears more than once", id)
		}
		seen[id] = struct{}{}
	}
	return ids, nil
}

func readPersistedSession(path, expectedName string) (PersistedSession, error) {
	file, err := os.Open(path)
	if err != nil {
		return PersistedSession{}, fmt.Errorf("open session snapshot: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	var snapshot PersistedSession
	if err := decoder.Decode(&snapshot); err != nil {
		return PersistedSession{}, fmt.Errorf("decode session snapshot: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return PersistedSession{}, errors.New("session snapshot contains trailing data")
	}
	if snapshot.Name != expectedName {
		return PersistedSession{}, fmt.Errorf("snapshot name %q does not match requested name %q", snapshot.Name, expectedName)
	}
	if err := validatePersistedSession(snapshot); err != nil {
		return PersistedSession{}, err
	}
	return snapshot, nil
}

func restoreLayout(layout PersistedLayout) LayoutNode {
	if layout.Pane != nil {
		return &PaneLayout{PaneID: *layout.Pane}
	}
	direction := SplitVertical
	if layout.Split == "horizontal" {
		direction = SplitHorizontal
	}
	return &SplitLayout{
		Direction: direction,
		Ratio:     uint16(layout.Ratio*1000 + 0.5),
		First:     restoreLayout(layout.Children[0]),
		Second:    restoreLayout(layout.Children[1]),
	}
}

func (s *Session) restoreSnapshot(snapshot PersistedSession, mode restoreCommandMode) error {
	if err := validatePersistedSession(snapshot); err != nil {
		return err
	}
	if mode != restoreCommandsPrepare && mode != restoreCommandsSkip && mode != restoreCommandsRun {
		return fmt.Errorf("invalid restore command mode %q", mode)
	}
	const defaultRestoreCols, defaultRestoreRows = 80, 24
	type restoredPane struct {
		pane    *Pane
		command string
	}
	panes := make([]restoredPane, 0)
	cleanup := func() {
		for _, restored := range panes {
			if restored.pane.Process != nil && restored.pane.Process.Process != nil {
				_ = restored.pane.Process.Process.Kill()
				_ = restored.pane.Process.Wait()
			}
			if restored.pane.PTY != nil {
				_ = restored.pane.PTY.Close()
			}
		}
	}
	var maxPaneID uint64
	for _, window := range snapshot.Windows {
		for _, persistedPane := range window.Panes {
			shell := persistedPane.Shell
			if shell == "" {
				shell = defaultShell()
			}
			pane, err := StartPane(persistedPane.ID, paneRequest{
				Cwd:   persistedPane.Cwd,
				Cols:  defaultRestoreCols,
				Rows:  defaultRestoreRows,
				Shell: shell,
			})
			if err != nil {
				cleanup()
				return fmt.Errorf("restore pane %d: %w", persistedPane.ID, err)
			}
			panes = append(panes, restoredPane{pane: pane, command: persistedPane.Command})
			if persistedPane.ID > maxPaneID {
				maxPaneID = persistedPane.ID
			}
		}
	}

	err := s.coordinate(func() error {
		s.Name = snapshot.Name
		s.NextPaneID = maxPaneID + 1
		s.NextWindowID = uint64(len(snapshot.Windows) + 1)
		for _, restored := range panes {
			s.Panes[restored.pane.ID] = restored.pane
		}
		for index, persistedWindow := range snapshot.Windows {
			windowID := uint64(index + 1)
			s.NextLayoutRevision++
			s.Windows[windowID] = &Window{
				ID:             windowID,
				DisplayIndex:   index,
				Name:           persistedWindow.Name,
				AutomaticName:  persistedWindow.AutomaticName,
				ActivePaneID:   persistedWindow.ActivePane,
				Layout:         restoreLayout(persistedWindow.Layout),
				LayoutRevision: s.NextLayoutRevision,
			}
		}
		client := s.ensureClientLocked(clientID0)
		client.ActiveWindowID = uint64(snapshot.ActiveWindow)
		activeWindow := s.Windows[client.ActiveWindowID]
		client.setFocusedPane(activeWindow.ActivePaneID)
		s.rebuildBindingsLocked(client, activeWindow)
		if len(snapshot.Windows[0].Panes) > 0 {
			s.defaultCwd = snapshot.Windows[0].Panes[0].Cwd
		}
		return nil
	})
	if err != nil {
		cleanup()
		return err
	}
	for _, restored := range panes {
		input := restoredCommandInput(restored.command, mode)
		restored.pane.startupInput = input
		s.startPane(restored.pane)
	}
	return nil
}

func restoredCommandInput(command string, mode restoreCommandMode) []byte {
	if command == "" || mode == restoreCommandsSkip {
		return nil
	}
	input := []byte(command)
	if mode == restoreCommandsRun {
		input = append(input, '\n')
	}
	return input
}

func (s *Session) startAutosave(snapshotDir string) {
	if snapshotDir == "" {
		return
	}
	s.autosave.Do(func() {
		go s.runAutosave(context.Background(), snapshotDir, sessionAutosaveInterval)
	})
}

func (s *Session) runAutosave(ctx context.Context, snapshotDir string, interval time.Duration) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	save := func() {
		saveCtx, cancel := context.WithTimeout(ctx, sessionAutosaveTimeout)
		_, err := s.autosaveSnapshot(saveCtx, snapshotDir, s.processNames)
		cancel()
		if err != nil && s.daemon != nil {
			s.daemon.logf("meja server: autosave session %d: %v\n", s.ID, err)
		}
		resetTimer(timer, interval)
	}
	for {
		select {
		case <-timer.C:
			save()
		case <-s.autosaveNow:
			save()
		case <-s.operationsDone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func (s *Session) autosaveSnapshot(ctx context.Context, snapshotDir string, observer ProcessObserver) (string, error) {
	var named bool
	if err := s.coordinate(func() error {
		named = s.Name != ""
		return nil
	}); err != nil || !named {
		return "", err
	}
	snapshot, err := s.captureSnapshot(ctx, observer)
	if err != nil {
		return "", err
	}
	if snapshot.SessionName == "" {
		return "", nil
	}
	persisted, err := persistedSession(snapshot)
	if err != nil {
		return "", err
	}
	return writeSessionSnapshot(snapshotDir, persisted)
}

func writeSessionSnapshot(snapshotDir string, snapshot PersistedSession) (string, error) {
	if err := validatePersistedSession(snapshot); err != nil {
		return "", err
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode session snapshot: %w", err)
	}
	data = append(data, '\n')
	target := filepath.Join(snapshotDir, snapshot.Name+".json")
	if existingSnapshotMatches(target, snapshot) {
		return target, nil
	}
	temporary, err := os.CreateTemp(snapshotDir, "."+snapshot.Name+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("secure temporary snapshot: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary snapshot: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync temporary snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary snapshot: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return "", fmt.Errorf("replace session snapshot: %w", err)
	}
	return target, nil
}

func existingSnapshotMatches(path string, snapshot PersistedSession) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var existing PersistedSession
	if err := json.Unmarshal(data, &existing); err != nil {
		return false
	}
	existing.SavedAt = time.Time{}
	snapshot.SavedAt = time.Time{}
	return reflect.DeepEqual(existing, snapshot)
}
