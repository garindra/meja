package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/garindra/meja/internal/protocol"
	kdl "github.com/sblinch/kdl-go"
	"github.com/sblinch/kdl-go/document"
)

var errSessionChangedDuringCapture = errors.New("session changed while it was being saved")

const (
	mejaFormatVersion         = 1
	persistenceSchemaVersion  = 2
	sessionPersistenceTimeout = 10 * time.Second
)

type restoreCommandMode string

const (
	restoreCommandsPrepare restoreCommandMode = "prepare"
	restoreCommandsSkip    restoreCommandMode = "skip"
	restoreCommandsRun     restoreCommandMode = "run"
)

// SessionCapture is the internal, observation-heavy capture used by an
// explicit save. It is projected into a portable SessionPlan.
type SessionCapture struct {
	CapturedAt     time.Time
	SessionID      uint64
	SessionName    string
	SessionRoot    string
	ActiveWindowID uint64
	Windows        []WindowCapture
	Panes          []PaneCapture
}

type PaneCapture struct {
	PaneID     uint64
	Launch     PaneLaunch
	CurrentCwd string
	Process    ProcessObservation
}

type WindowCapture struct {
	WindowID      uint64
	Name          string
	AutomaticName bool
	ActivePaneID  uint64
	Layout        PlanLayout
}

// SessionPlan is the reconstructable, identity-free portion of a session.
type SessionPlan struct {
	Version int
	// Name is restore context, not part of the user-owned .meja document.
	// Readers derive it from the filename and callers may override it with -s.
	// Private SessionPersistence validates and stores its own required name.
	Name         string
	Root         string
	ActiveWindow int
	Windows      []PlanWindow
}

// SessionPersistence is Meja's private recovery record for one named live
// session. Plan paths and Root are absolute machine-local paths in memory.
type SessionPersistence struct {
	Version          int
	Schema           int
	SessionID        uint64
	GroupID          uint64
	Name             string
	SavedAt          time.Time
	Profile          string
	Root             string
	ActiveWindowID   uint64
	PreviousWindowID uint64
	WindowViews      []SessionViewPersistence
	Plan             SessionPlan
}

// SessionViewPersistence is durable session-local view state. It never
// contains leases, clients, transports, or pane runtime state.
type SessionViewPersistence struct {
	WindowID      uint64
	DisplayIndex  int
	FocusedPaneID uint64
	ZoomedPaneID  uint64
}

type PlanWindow struct {
	ID            uint64
	Cwd           string
	Name          string
	AutomaticName bool
	ActivePane    uint64
	NamedLayout   string
	Layout        PlanLayout
	Panes         []PlanPane
}

type PlanPane struct {
	ID      uint64
	Cwd     string
	Shell   string
	Command string
	Tile    MejaTile
}

type PlanLayout struct {
	Pane     *uint64
	Split    string
	Ratio    float64
	Children []PlanLayout
}

type paneCaptureInput struct {
	pane   *Pane
	launch PaneLaunch
	anchor Anchor
}

type windowCaptureInput struct {
	window         *Window
	windowID       uint64
	displayIndex   int
	layoutRevision uint64
	name           string
	automaticName  bool
	activePaneID   uint64
	layout         PlanLayout
}

func (d *Daemon) captureSession(s *SessionState, ctx context.Context, observer ProcessObserver) (SessionCapture, error) {
	if observer == nil {
		observer = NewProcessObserver()
	}
	if err := ctx.Err(); err != nil {
		return SessionCapture{}, err
	}

	var inputs []paneCaptureInput
	var windowInputs []windowCaptureInput
	var paneCount int
	var sessionName string
	var sessionRoot string
	var activeWindowID uint64
	paneCount = len(s.Panes)
	sessionName = s.Name
	sessionRoot = s.rootDir
	activeWindowID = s.ActiveWindowID
	inputs = make([]paneCaptureInput, 0, len(s.Panes))
	for _, pane := range s.Panes {
		if pane == nil || pane.Root.PID <= 0 {
			continue
		}
		launch := clonePaneLaunch(pane.Launch)
		inputs = append(inputs, paneCaptureInput{
			pane:   pane,
			launch: launch,
			anchor: Anchor{
				Key:         PaneKey{PaneID: pane.ID},
				Root:        pane.Root,
				PTY:         pane.PTY,
				RootIsShell: len(launch.RequestedArgv) == 0,
			},
		})
	}
	windowInputs = make([]windowCaptureInput, 0, len(s.Windows))
	for _, window := range s.Windows {
		layout, err := planLayout(window.Layout)
		if err != nil {
			return SessionCapture{}, err
		}
		windowInputs = append(windowInputs, windowCaptureInput{
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
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].anchor.Key.PaneID < inputs[j].anchor.Key.PaneID })
	sort.Slice(windowInputs, func(i, j int) bool { return windowInputs[i].displayIndex < windowInputs[j].displayIndex })

	anchors := make([]Anchor, len(inputs))
	for index := range inputs {
		anchors[index] = inputs[index].anchor
	}
	observations := observer.Observe(ctx, anchors)
	if err := ctx.Err(); err != nil {
		return SessionCapture{}, err
	}

	valid := false
	valid = len(s.Panes) == paneCount && len(s.Windows) == len(windowInputs) && s.Name == sessionName && s.rootDir == sessionRoot
	currentActiveWindowID := s.ActiveWindowID
	if currentActiveWindowID != activeWindowID {
		valid = false
	}
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
	if !valid {
		return SessionCapture{}, errSessionChangedDuringCapture
	}

	capture := SessionCapture{
		CapturedAt:     time.Now().UTC(),
		SessionID:      s.ID,
		SessionName:    sessionName,
		SessionRoot:    sessionRoot,
		ActiveWindowID: activeWindowID,
		Windows:        make([]WindowCapture, 0, len(windowInputs)),
		Panes:          make([]PaneCapture, 0, len(inputs)),
	}
	for _, input := range windowInputs {
		capture.Windows = append(capture.Windows, WindowCapture{
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
		capture.Panes = append(capture.Panes, PaneCapture{
			PaneID:     input.anchor.Key.PaneID,
			Launch:     clonePaneLaunch(input.launch),
			CurrentCwd: cwd,
			Process:    observation,
		})
	}
	return capture, nil
}

func clonePaneLaunch(launch PaneLaunch) PaneLaunch {
	launch.RequestedArgv = append([]string(nil), launch.RequestedArgv...)
	return launch
}

func planLayout(layout LayoutNode) (PlanLayout, error) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node == nil {
			return PlanLayout{}, errors.New("session plan layout contains an invalid pane")
		}
		return PlanLayout{Pane: paneIDRef(node.PaneID)}, nil
	case *SplitLayout:
		if node == nil {
			return PlanLayout{}, errors.New("session plan layout contains a nil split")
		}
		first, err := planLayout(node.First)
		if err != nil {
			return PlanLayout{}, err
		}
		second, err := planLayout(node.Second)
		if err != nil {
			return PlanLayout{}, err
		}
		direction := "vertical"
		if node.Direction == SplitHorizontal {
			direction = "horizontal"
		} else if node.Direction != SplitVertical {
			return PlanLayout{}, errors.New("session plan layout has an invalid split direction")
		}
		ratio := node.Ratio
		if ratio == 0 || ratio >= 1000 {
			ratio = 500
		}
		return PlanLayout{Split: direction, Ratio: float64(ratio) / 1000, Children: []PlanLayout{first, second}}, nil
	default:
		return PlanLayout{}, errors.New("session plan layout has an unknown node")
	}
}

func sessionPlanFromCapture(capture SessionCapture) (SessionPlan, error) {
	plan := SessionPlan{
		Version: mejaFormatVersion,
		Name:    capture.SessionName,
		Root:    capture.SessionRoot,
		Windows: make([]PlanWindow, 0, len(capture.Windows)),
	}
	panes := make(map[uint64]PaneCapture, len(capture.Panes))
	for _, pane := range capture.Panes {
		panes[pane.PaneID] = pane
	}
	for index, window := range capture.Windows {
		if window.WindowID == capture.ActiveWindowID {
			plan.ActiveWindow = index + 1
		}
		output := PlanWindow{
			ID:            window.WindowID,
			Cwd:           capture.SessionRoot,
			Name:          window.Name,
			AutomaticName: window.AutomaticName,
			ActivePane:    window.ActivePaneID,
			Layout:        window.Layout,
		}
		for _, paneID := range planLayoutPaneIDs(window.Layout) {
			pane, ok := panes[paneID]
			if !ok {
				return SessionPlan{}, fmt.Errorf("window references pane %d without a process capture", paneID)
			}
			output.Panes = append(output.Panes, PlanPane{
				ID:      pane.PaneID,
				Cwd:     pane.CurrentCwd,
				Shell:   pane.Launch.Shell,
				Command: plannedPaneCommand(pane),
			})
		}
		plan.Windows = append(plan.Windows, output)
	}
	if plan.ActiveWindow == 0 && len(plan.Windows) > 0 {
		plan.ActiveWindow = 1
	}
	if err := validateSessionPlan(plan); err != nil {
		return SessionPlan{}, err
	}
	return plan, nil
}

func plannedPaneCommand(pane PaneCapture) string {
	if len(pane.Launch.RequestedArgv) > 0 {
		return shellJoin(pane.Launch.RequestedArgv)
	}
	if pane.Process.Status != StatusDetected || pane.Process.Candidate == nil {
		return ""
	}
	if isTransientObservedCommand(pane.Process.Candidate) {
		return ""
	}
	return observedProcessCommand(pane.Process.Candidate)
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
			quoted[index] = shellQuote(arg)
		}
	}
	return strings.Join(quoted, " ")
}

func paneIDRef(paneID uint64) *uint64 {
	return &paneID
}

func planLayoutPaneIDs(layout PlanLayout) []uint64 {
	if layout.Pane != nil {
		return []uint64{*layout.Pane}
	}
	var ids []uint64
	for _, child := range layout.Children {
		ids = append(ids, planLayoutPaneIDs(child)...)
	}
	return ids
}

func validateSessionPlan(plan SessionPlan) error {
	if plan.Version != mejaFormatVersion {
		return fmt.Errorf("unsupported .meja version %d", plan.Version)
	}
	if !filepath.IsAbs(plan.Root) {
		return errors.New(".meja plan root must resolve to an absolute path")
	}
	if len(plan.Windows) == 0 {
		return errors.New(".meja session contains no windows")
	}
	if plan.ActiveWindow < 1 || plan.ActiveWindow > len(plan.Windows) {
		return errors.New(".meja active window is out of range")
	}
	for windowIndex, window := range plan.Windows {
		if !filepath.IsAbs(window.Cwd) {
			return fmt.Errorf(".meja window %d cwd must resolve to an absolute path", windowIndex+1)
		}
		if len(window.Panes) == 0 || len(window.Panes) > int(protocol.MaxRenderSlots) {
			return fmt.Errorf(".meja window %d has an invalid pane count", windowIndex+1)
		}
		windowPanes := make(map[uint64]struct{}, len(window.Panes))
		for _, pane := range window.Panes {
			if pane.ID == ^uint64(0) {
				return fmt.Errorf(".meja pane ID %d cannot be restored", pane.ID)
			}
			if _, exists := windowPanes[pane.ID]; exists {
				return fmt.Errorf(".meja pane ID %d is duplicated", pane.ID)
			}
			windowPanes[pane.ID] = struct{}{}
			if !filepath.IsAbs(pane.Cwd) {
				return fmt.Errorf(".meja pane %d cwd must resolve to an absolute path", pane.ID)
			}
			if strings.IndexFunc(pane.Command, unicode.IsControl) >= 0 {
				return fmt.Errorf(".meja pane %d command must not contain control characters", pane.ID)
			}
			if strings.IndexFunc(pane.Shell, unicode.IsControl) >= 0 {
				return fmt.Errorf(".meja pane %d shell must not contain control characters", pane.ID)
			}
		}
		layoutPanes, err := validatePlanLayout(window.Layout)
		if err != nil {
			return fmt.Errorf(".meja window %d layout: %w", windowIndex+1, err)
		}
		if len(layoutPanes) != len(windowPanes) {
			return fmt.Errorf(".meja window %d layout does not contain each pane exactly once", windowIndex+1)
		}
		for _, paneID := range layoutPanes {
			if _, exists := windowPanes[paneID]; !exists {
				return fmt.Errorf(".meja window %d layout references unknown pane %d", windowIndex+1, paneID)
			}
		}
		if _, exists := windowPanes[window.ActivePane]; !exists {
			return fmt.Errorf(".meja window %d active pane is unknown", windowIndex+1)
		}
	}
	return nil
}

func validateSessionPersistence(persistence SessionPersistence) error {
	if persistence.Version != mejaFormatVersion {
		return fmt.Errorf("unsupported .meja version %d", persistence.Version)
	}
	if persistence.Schema == 0 {
		persistence.Schema = 1
	}
	if persistence.Schema != 1 && persistence.Schema != persistenceSchemaVersion {
		return fmt.Errorf("unsupported session persistence schema %d", persistence.Schema)
	}
	if err := validateSessionName(persistence.Name); err != nil {
		return fmt.Errorf("session persistence name: %w", err)
	}
	if persistence.SavedAt.IsZero() {
		return errors.New("session persistence requires saved-at")
	}
	if !filepath.IsAbs(persistence.Root) {
		return errors.New("session persistence root must be absolute")
	}
	if persistence.Plan.Name != persistence.Name {
		return errors.New("session persistence plan name does not match session name")
	}
	if filepath.Clean(persistence.Plan.Root) != filepath.Clean(persistence.Root) {
		return errors.New("session persistence plan root does not match session root")
	}
	return validateSessionPlan(persistence.Plan)
}

// Persistence state is projected from daemon-owned graph state. The lowest-ID
// session in a group is the recovery-file owner so shared panes and processes
// are projected once; other session views do not create duplicate execution
// graph records.
type persistenceSnapshot struct {
	persistence   *SessionPersistence
	obsoleteNames []string
}

func (s *SessionState) persistenceRecord() *SessionPersistence {
	if s == nil || s.daemon == nil {
		return nil
	}
	return s.daemon.sessionPersistions[s.ID]
}

func (s *SessionState) setPersistenceRecord(persistence *SessionPersistence) {
	if s == nil || s.daemon == nil {
		return
	}
	if s.daemon.sessionPersistions == nil {
		s.daemon.sessionPersistions = make(map[uint64]*SessionPersistence)
	}
	if persistence == nil {
		delete(s.daemon.sessionPersistions, s.ID)
		return
	}
	s.daemon.sessionPersistions[s.ID] = persistence
}

func (s *SessionState) obsoletePersistenceSet() map[string]struct{} {
	if s == nil || s.daemon == nil {
		return nil
	}
	if s.daemon.obsoletePersistenceNames == nil {
		s.daemon.obsoletePersistenceNames = make(map[uint64]map[string]struct{})
	}
	set := s.daemon.obsoletePersistenceNames[s.ID]
	if set == nil {
		set = make(map[string]struct{})
		s.daemon.obsoletePersistenceNames[s.ID] = set
	}
	return set
}

func (s *SessionState) persistSessionForPersistence() {
	previousProcesses := map[uint64]processSaveProjection{}
	previousName := ""
	if persisted := s.persistenceRecord(); persisted != nil {
		previousProcesses = plannedProcessLeaves(persisted.Plan.Windows)
		previousName = persisted.Name
	}
	plan, err := s.projectSessionPlan(previousProcesses)
	if err != nil {
		s.logPersistenceProjectionError(err)
		return
	}
	if plan == nil {
		return
	}
	if previousName != "" && previousName != s.Name {
		s.obsoletePersistenceSet()[previousName] = struct{}{}
	}
	delete(s.obsoletePersistenceSet(), s.Name)
	record := s.newPersistenceRecord(*plan)
	s.setPersistenceRecord(&record)
	s.queuePersistenceWrite()
}

// Window-local mutations replace only that window's persisted leaf.
func (s *SessionState) persistWindowForPersistence(windowID uint64) {
	persisted := s.ensureSessionPersistence()
	window := s.Windows[windowID]
	if persisted == nil || window == nil {
		return
	}
	projected, err := s.projectPlanWindow(window, plannedProcessLeaves(persisted.Plan.Windows))
	if err != nil {
		s.logPersistenceProjectionError(err)
		return
	}
	for index := range persisted.Plan.Windows {
		if persisted.Plan.Windows[index].ID == windowID {
			persisted.Plan.Windows[index] = projected
			s.queuePersistenceWrite()
			return
		}
	}
	// A new or removed window is a session-level structural mutation.
	previousProcesses := map[uint64]processSaveProjection{}
	previousName := ""
	if current := s.persistenceRecord(); current != nil {
		previousProcesses = plannedProcessLeaves(current.Plan.Windows)
		previousName = current.Name
	}
	plan, err := s.projectSessionPlan(previousProcesses)
	if err != nil || plan == nil {
		return
	}
	if previousName != "" && previousName != s.Name {
		s.obsoletePersistenceSet()[previousName] = struct{}{}
	}
	delete(s.obsoletePersistenceSet(), s.Name)
	record := s.newPersistenceRecord(*plan)
	s.setPersistenceRecord(&record)
	s.queuePersistenceWrite()
}

// initializeRestoredPersistence seeds the actor-owned model from the file
// that created the live session, preserving commands that are prepared at the
// shell prompt and are therefore not part of Pane.Launch.
func (s *SessionState) initializeRestoredPersistence(restoredPlan SessionPlan) {
	plan := cloneSessionPlan(restoredPlan)
	plan.Version = mejaFormatVersion
	plan.Name = s.Name
	plan.Root = s.rootDir
	for windowIndex := range plan.Windows {
		plan.Windows[windowIndex].ID = uint64(windowIndex + 1)
		// Window cwd is plan-only inheritance syntax, not live runtime state.
		// Pane paths have already been resolved, so canonical persistence uses
		// the current session root as each window's implicit parent.
		plan.Windows[windowIndex].Cwd = s.rootDir
	}
	record := s.newPersistenceRecord(plan)
	s.setPersistenceRecord(&record)
	s.queuePersistenceWrite()
}

func (s *SessionState) persistObservedPaneForPersistence(paneID uint64, projection processSaveProjection) {
	persisted := s.ensureSessionPersistence()
	if persisted == nil {
		return
	}
	for windowIndex := range persisted.Plan.Windows {
		for paneIndex := range persisted.Plan.Windows[windowIndex].Panes {
			pane := &persisted.Plan.Windows[windowIndex].Panes[paneIndex]
			if pane.ID != paneID {
				continue
			}
			pane.Cwd = projection.Cwd
			pane.Command = projection.Command
			s.queuePersistenceWrite()
			return
		}
	}
}

func (s *SessionState) ensureSessionPersistence() *SessionPersistence {
	if s.persistenceRecord() != nil {
		return s.persistenceRecord()
	}
	plan, err := s.projectSessionPlan(nil)
	if err != nil {
		s.logPersistenceProjectionError(err)
		return nil
	}
	if plan == nil {
		return nil
	}
	s.setPersistenceRecord(&SessionPersistence{
		Version:   mejaFormatVersion,
		SessionID: s.ID,
		Name:      s.Name,
		SavedAt:   time.Now(),
		Root:      s.rootDir,
		Plan:      *plan,
	})
	return s.persistenceRecord()
}

func (s *SessionState) newPersistenceRecord(plan SessionPlan) SessionPersistence {
	record := SessionPersistence{
		Version:          mejaFormatVersion,
		Schema:           persistenceSchemaVersion,
		SessionID:        s.ID,
		GroupID:          s.GroupID,
		Name:             s.Name,
		SavedAt:          time.Now(),
		Root:             s.rootDir,
		ActiveWindowID:   s.ActiveWindowID,
		PreviousWindowID: s.PreviousWindowID,
		Plan:             plan,
	}
	for _, link := range s.Links {
		view := s.WindowViews[link.WindowID]
		record.WindowViews = append(record.WindowViews, SessionViewPersistence{
			WindowID: link.WindowID, DisplayIndex: link.DisplayIndex,
			FocusedPaneID: view.FocusedPaneID, ZoomedPaneID: view.ZoomedPaneID,
		})
	}
	return record
}

func (s *SessionState) projectSessionPlan(processes map[uint64]processSaveProjection) (*SessionPlan, error) {
	if s.Name == "" || len(s.Windows) == 0 {
		return nil, nil
	}
	windows := s.planWindows()
	plan := &SessionPlan{
		Version: mejaFormatVersion,
		Name:    s.Name,
		Root:    s.rootDir,
		Windows: make([]PlanWindow, 0, len(windows)),
	}
	for _, window := range windows {
		projected, err := s.projectPlanWindow(window, processes)
		if err != nil {
			return nil, err
		}
		plan.Windows = append(plan.Windows, projected)
	}
	plan.ActiveWindow = s.plannedActiveWindow()
	return plan, nil
}

func (s *SessionState) projectPlanWindow(window *Window, processes map[uint64]processSaveProjection) (PlanWindow, error) {
	layout, err := planLayout(window.Layout)
	if err != nil {
		return PlanWindow{}, err
	}
	persisted := PlanWindow{
		ID:            window.ID,
		Cwd:           s.rootDir,
		Name:          window.Name,
		AutomaticName: window.AutomaticName,
		ActivePane:    windowActivePaneID(window),
		Layout:        layout,
	}
	for _, paneID := range planLayoutPaneIDs(layout) {
		pane := s.Panes[paneID]
		if pane == nil {
			continue
		}
		output := PlanPane{ID: paneID, Cwd: pane.Launch.Cwd, Shell: pane.Launch.Shell}
		if len(pane.Launch.RequestedArgv) > 0 {
			output.Command = shellJoin(pane.Launch.RequestedArgv)
		}
		if projection, ok := processes[paneID]; ok {
			output.Cwd = projection.Cwd
			output.Command = projection.Command
		}
		persisted.Panes = append(persisted.Panes, output)
	}
	return persisted, nil
}

func (s *SessionState) planWindows() []*Window {
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
	return windows
}

func (s *SessionState) plannedActiveWindow() int {
	activeID := s.ActiveWindowID
	windows := s.planWindows()
	for index, window := range windows {
		if window.ID == activeID {
			return index + 1
		}
	}
	if len(windows) > 0 {
		return 1
	}
	return 0
}

func plannedProcessLeaves(windows []PlanWindow) map[uint64]processSaveProjection {
	processes := make(map[uint64]processSaveProjection)
	for _, window := range windows {
		for _, pane := range window.Panes {
			processes[pane.ID] = processSaveProjection{Cwd: pane.Cwd, Command: pane.Command}
		}
	}
	return processes
}

func (s *SessionState) queuePersistenceWrite() {
	if s.persistenceRecord() == nil {
		return
	}
	persisted := s.persistenceRecord()
	persisted.SavedAt = time.Now()
	persisted.SessionID = s.ID
	clone := cloneSessionPersistence(*persisted)
	update := persistenceSnapshot{persistence: &clone}
	for name := range s.obsoletePersistenceSet() {
		if update.persistence.Name != name {
			update.obsoleteNames = append(update.obsoleteNames, name)
		}
	}
	if s.daemon != nil && s.daemon.persistenceUpdates != nil {
		select {
		case <-s.daemon.persistenceUpdates:
		default:
		}
		select {
		case s.daemon.persistenceUpdates <- update:
		default:
		}
	}
	select {
	case s.daemon.persistenceNow <- struct{}{}:
	default:
	}
}

func (s *SessionState) logPersistenceProjectionError(err error) {
	if err != nil && s.daemon != nil {
		s.daemon.logf("meja server: project session persistence %d: %v\n", s.ID, err)
	}
}

func shellQuote(raw string) string {
	return "'" + strings.ReplaceAll(raw, "'", "'\\''") + "'"
}

func validatePlanLayout(layout PlanLayout) ([]uint64, error) {
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
	first, err := validatePlanLayout(layout.Children[0])
	if err != nil {
		return nil, err
	}
	second, err := validatePlanLayout(layout.Children[1])
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

func restoreLayout(layout PlanLayout) LayoutNode {
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

func (d *Daemon) restoreSessionPlan(s *SessionState, plan SessionPlan, persisted *SessionPersistence, mode restoreCommandMode) error {
	if err := validateSessionPlan(plan); err != nil {
		return err
	}
	if mode != restoreCommandsPrepare && mode != restoreCommandsSkip && mode != restoreCommandsRun {
		return fmt.Errorf("invalid restore command mode %q", mode)
	}
	plan = cloneSessionPlan(plan)
	if err := s.allocateRestoredPaneIDs(&plan); err != nil {
		return err
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
	for _, window := range plan.Windows {
		for _, persistedPane := range window.Panes {
			shell := persistedPane.Shell
			if shell == "" {
				shell = defaultShell()
			}
			pane, err := startPaneProcess(persistedPane.ID, s.contextualPaneRequest(paneRequest{
				Cwd:   persistedPane.Cwd,
				Cols:  defaultRestoreCols,
				Rows:  defaultRestoreRows,
				Shell: shell,
			}))
			if err != nil {
				cleanup()
				return fmt.Errorf("restore pane %d: %w", persistedPane.ID, err)
			}
			panes = append(panes, restoredPane{pane: pane, command: persistedPane.Command})
		}
	}

	if s == nil {
		cleanup()
		return errSessionUnavailable
	}
	s.Name = plan.Name
	for _, restored := range panes {
		s.Panes[restored.pane.ID] = restored.pane
		if s.daemon != nil {
			if s.daemon.panes == nil {
				s.daemon.panes = make(map[uint64]*Pane)
			}
			s.daemon.panes[restored.pane.ID] = restored.pane
			s.daemon.paneIndex.Store(restored.pane.ID, restored.pane)
		}
	}
	windowIDs := make(map[uint64]uint64, len(plan.Windows))
	for index, persistedWindow := range plan.Windows {
		windowID := uint64(index + 1)
		windowIDs[persistedWindow.ID] = windowID
		layoutCycleIndex := layoutPresetCustom
		if preset, ok := namedLayoutPreset(persistedWindow.NamedLayout); ok {
			layoutCycleIndex = preset
		}
		s.NextLayoutRevision++
		window := &Window{
			ID:               windowID,
			GroupID:          s.GroupID,
			DisplayIndex:     index,
			Name:             persistedWindow.Name,
			AutomaticName:    persistedWindow.AutomaticName,
			ActivePaneID:     persistedWindow.ActivePane,
			Layout:           restoreLayout(persistedWindow.Layout),
			LayoutRevision:   s.NextLayoutRevision,
			layoutCycleIndex: layoutCycleIndex,
		}
		s.Windows[windowID] = window
		if s.daemon != nil {
			s.daemon.windowIndex.Store(windowID, window)
		}
		for _, paneID := range window.Layout.PaneIDs() {
			if pane := s.Panes[paneID]; pane != nil {
				pane.WindowID = windowID
			}
		}
		s.Links = append(s.Links, WindowLink{WindowID: windowID, DisplayIndex: index})
		s.WindowViews[windowID] = SessionWindowView{FocusedPaneID: persistedWindow.ActivePane}
		if s.group != nil {
			s.group.Windows[windowID] = window
		}
		if s.daemon != nil && s.daemon.nextWindowID <= windowID {
			s.daemon.nextWindowID = windowID + 1
		}
	}
	if s.group != nil {
		for paneID, pane := range s.Panes {
			s.group.Panes[paneID] = pane
		}
	}
	s.ActiveWindowID = uint64(plan.ActiveWindow)
	if persisted != nil {
		if active := windowIDs[persisted.ActiveWindowID]; active != 0 {
			s.ActiveWindowID = active
		}
		if previous := windowIDs[persisted.PreviousWindowID]; previous != 0 {
			s.PreviousWindowID = previous
		}
		for _, savedView := range persisted.WindowViews {
			windowID := windowIDs[savedView.WindowID]
			if windowID == 0 {
				continue
			}
			s.WindowViews[windowID] = SessionWindowView{FocusedPaneID: savedView.FocusedPaneID, ZoomedPaneID: savedView.ZoomedPaneID}
		}
	}
	s.rootDir = plan.Root
	s.initializeRestoredPersistence(plan)
	for _, restored := range panes {
		input := restoredCommandInput(restored.command, mode)
		restored.pane.startupInput = input
		d.startPane(s, restored.pane)
	}
	return nil
}

// restoreSessionView attaches a persisted mirror view to an already restored
// execution graph. It deliberately starts no panes and no goroutines.
func (d *Daemon) restoreSessionView(s *SessionState, persisted SessionPersistence) error {
	if d == nil || s == nil || persisted.GroupID == 0 {
		return errSessionUnavailable
	}
	group := d.persistenceGroups[persisted.GroupID]
	if group == nil {
		return errSessionUnavailable
	}
	group.addSession(s)
	s.Name = persisted.Name
	s.rootDir = persisted.Root
	byDisplay := make(map[int]*Window, len(group.Windows))
	for _, window := range group.Windows {
		byDisplay[window.DisplayIndex] = window
	}
	for _, savedView := range persisted.WindowViews {
		window := byDisplay[savedView.DisplayIndex]
		if window == nil {
			continue
		}
		s.WindowViews[window.ID] = SessionWindowView{FocusedPaneID: savedView.FocusedPaneID, ZoomedPaneID: savedView.ZoomedPaneID}
	}
	if persisted.ActiveWindowID != 0 {
		for _, window := range group.Windows {
			if window.ID == persisted.ActiveWindowID {
				s.ActiveWindowID = window.ID
				break
			}
		}
	}
	if s.ActiveWindowID == 0 {
		ids := s.orderedWindowIDs()
		if len(ids) > 0 {
			s.ActiveWindowID = ids[0]
		}
	}
	if persisted.PreviousWindowID != 0 {
		s.PreviousWindowID = persisted.PreviousWindowID
	}
	if d.sessions[s.ID] != s {
		return errSessionUnavailable
	}
	s.grouped.Store(len(group.SessionIDs) > 1)
	for memberID := range group.SessionIDs {
		if member := d.sessions[memberID]; member != nil {
			member.grouped.Store(len(group.SessionIDs) > 1)
		}
	}
	return nil
}

// allocateRestoredPaneIDs converts layout-local project references into fresh
// daemon-wide live IDs. Restoration is initiated on the daemon actor, so the
// direct allocator call avoids re-entering that actor.
func (s *SessionState) allocateRestoredPaneIDs(plan *SessionPlan) error {
	for windowIndex := range plan.Windows {
		window := &plan.Windows[windowIndex]
		mapping := make(map[uint64]uint64, len(window.Panes))
		for paneIndex := range window.Panes {
			oldID := window.Panes[paneIndex].ID
			var newID uint64
			var err error
			if s.daemon == nil {
				return errors.New("restore pane IDs require daemon ownership")
			}
			newID, err = s.daemon.allocatePaneIDNow()
			if err != nil {
				return err
			}
			mapping[oldID] = newID
			window.Panes[paneIndex].ID = newID
		}
		window.ActivePane = mapping[window.ActivePane]
		remapLayoutPaneIDs(&window.Layout, mapping)
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

func flushPersistenceSnapshot(ctx context.Context, sessionPersistenceDir string, update persistenceSnapshot) (string, error) {
	persisted := update.persistence
	obsoleteNames := update.obsoleteNames
	if persisted == nil {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := writeSessionPersistence(sessionPersistenceDir, *persisted)
	if err != nil {
		return "", err
	}
	for _, name := range obsoleteNames {
		if err := os.Remove(filepath.Join(sessionPersistenceDir, name+".session.meja")); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove obsolete session persistence for %q: %w", name, err)
		}
	}
	return path, nil
}

const maxMejaFileSize = 4 << 20

type MejaTile struct {
	X int
	Y int
	W int
	H int
}

type PlanPortability struct {
	AbsolutePanePaths int
}

func node(name string) *document.Node {
	n := document.NewNode()
	n.SetName(name)
	return n
}

func addStringArgument(n *document.Node, value string) {
	v := n.AddArgument(value, "")
	v.Flag = document.FlagQuoted
}

func newMejaDocument() *document.Document {
	return document.New()
}

func planDocumentNodes(plan SessionPlan, private bool) []*document.Node {
	nodes := make([]*document.Node, 0, len(plan.Windows)+2)
	root := node("root")
	addStringArgument(root, plan.Root)
	nodes = append(nodes, root)
	if private {
		activeWindow := node("active-window")
		activeWindow.AddArgument(plan.ActiveWindow-1, "")
		nodes = append(nodes, activeWindow)
	}
	for _, persistedWindow := range plan.Windows {
		window := node("window")
		if persistedWindow.Name != "" && !(persistedWindow.AutomaticName && persistedWindow.Name == "bash") {
			window.AddProperty("name", persistedWindow.Name, "").Flag = document.FlagQuoted
		}
		if private {
			window.AddProperty("active-pane", persistedWindow.ActivePane, "")
		}
		if persistedWindow.Cwd != "" {
			cwd := node("cwd")
			addStringArgument(cwd, persistedWindow.Cwd)
			window.AddNode(cwd)
		}
		if len(persistedWindow.Panes) > 1 && persistedWindow.NamedLayout != "" {
			layout := node("layout")
			addStringArgument(layout, persistedWindow.NamedLayout)
			window.AddNode(layout)
		}
		for _, persistedPane := range persistedWindow.Panes {
			pane := node("pane")
			if persistedPane.Cwd != "" {
				cwd := node("cwd")
				addStringArgument(cwd, persistedPane.Cwd)
				pane.AddNode(cwd)
			}
			if persistedPane.Shell != "" && (private || persistedPane.Shell != defaultShell()) {
				shell := node("shell")
				addStringArgument(shell, persistedPane.Shell)
				pane.AddNode(shell)
			}
			if persistedPane.Command != "" {
				command := node("cmd")
				addStringArgument(command, persistedPane.Command)
				pane.AddNode(command)
			}
			if len(persistedWindow.Panes) > 1 && persistedWindow.NamedLayout == "" {
				tile := node("tile")
				tile.AddProperty("x", persistedPane.Tile.X, "")
				tile.AddProperty("y", persistedPane.Tile.Y, "")
				tile.AddProperty("w", persistedPane.Tile.W, "")
				tile.AddProperty("h", persistedPane.Tile.H, "")
				pane.AddNode(tile)
			}
			window.AddNode(pane)
		}
		nodes = append(nodes, window)
	}
	return nodes
}

func encodeUserSessionPlan(plan SessionPlan, outputPath string) ([]byte, PlanPortability, error) {
	if err := validateSessionPlan(plan); err != nil {
		return nil, PlanPortability{}, err
	}
	plan = cloneSessionPlan(plan)
	preparePlanTiles(&plan)
	report, err := normalizePlanPaths(&plan, filepath.Dir(outputPath), true)
	if err != nil {
		return nil, PlanPortability{}, err
	}
	doc := newMejaDocument()
	for _, planNode := range planDocumentNodes(plan, false) {
		doc.AddNode(planNode)
	}
	data, err := encodeKDLDocument(doc)
	return data, report, err
}

func encodeSessionPersistence(persistence SessionPersistence) ([]byte, error) {
	if err := validateSessionPersistence(persistence); err != nil {
		return nil, err
	}
	plan := cloneSessionPlan(persistence.Plan)
	preparePlanTiles(&plan)
	if _, err := normalizePlanPaths(&plan, persistence.Root, false); err != nil {
		return nil, err
	}
	doc := newMejaDocument()
	session := node("session")
	session.AddProperty("name", persistence.Name, "").Flag = document.FlagQuoted
	session.AddProperty("id", persistence.SessionID, "")
	session.AddProperty("schema", persistenceSchemaVersion, "")
	if persistence.GroupID != 0 {
		session.AddProperty("group-id", persistence.GroupID, "")
	}
	session.AddProperty("saved-at", persistence.SavedAt.Format(time.RFC3339), "").Flag = document.FlagQuoted
	if persistence.ActiveWindowID != 0 {
		session.AddProperty("active-window-id", persistence.ActiveWindowID, "")
	}
	if persistence.PreviousWindowID != 0 {
		session.AddProperty("previous-window-id", persistence.PreviousWindowID, "")
	}
	if persistence.Profile != "" {
		session.AddProperty("profile", persistence.Profile, "").Flag = document.FlagQuoted
	}
	for _, view := range persistence.WindowViews {
		n := node("view")
		n.AddProperty("window-id", view.WindowID, "")
		n.AddProperty("display-index", view.DisplayIndex, "")
		n.AddProperty("focused-pane-id", view.FocusedPaneID, "")
		if view.ZoomedPaneID != 0 {
			n.AddProperty("zoomed-pane-id", view.ZoomedPaneID, "")
		}
		session.AddNode(n)
	}
	for _, planNode := range planDocumentNodes(plan, true) {
		session.AddNode(planNode)
	}
	doc.AddNode(session)
	return encodeKDLDocument(doc)
}

func encodeKDLDocument(doc *document.Document) ([]byte, error) {
	var output bytes.Buffer
	if err := kdl.GenerateWithOptions(deterministicKDLDocument(doc), &output, kdl.GenerateOptions{Indent: "    "}); err != nil {
		return nil, fmt.Errorf("encode .meja file: %w", err)
	}
	return output.Bytes(), nil
}

// kdl-go's production document.Properties uses a map. Convert properties to
// preformatted argument tokens before generation so user-owned plans are
// byte-for-byte deterministic without requiring a special build tag.
func deterministicKDLDocument(doc *document.Document) *document.Document {
	output := document.New()
	for _, source := range doc.Nodes {
		output.AddNode(deterministicKDLNode(source))
	}
	return output
}

func deterministicKDLNode(source *document.Node) *document.Node {
	output := source.ShallowCopy()
	output.Arguments = append([]*document.Value(nil), source.Arguments...)
	output.Children = nil
	if source.Properties.Len() > 0 {
		properties := source.Properties.Unordered()
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			return kdlPropertyRank(keys[i]) < kdlPropertyRank(keys[j])
		})
		for _, key := range keys {
			value := &document.Value{
				Value: key + "=" + properties[key].FormattedString(),
				Flag:  document.FlagBareSuffixed,
			}
			output.Arguments = append(output.Arguments, value)
		}
		var noProperties document.Properties
		output.Properties = noProperties
	}
	for _, child := range source.Children {
		output.AddNode(deterministicKDLNode(child))
	}
	return output
}

func kdlPropertyRank(key string) string {
	ranks := map[string]string{
		"name":          "00",
		"id":            "01",
		"saved-at":      "02",
		"profile":       "03",
		"root":          "04",
		"active-window": "05",
		"active-pane":   "06",
		"x":             "07",
		"y":             "08",
		"w":             "09",
		"h":             "10",
	}
	if rank, ok := ranks[key]; ok {
		return rank
	}
	return "99" + key
}

func preparePlanTiles(plan *SessionPlan) {
	for index := range plan.Windows {
		window := &plan.Windows[index]
		window.ID = uint64(index)
		if len(window.Panes) > 1 && window.NamedLayout == "" {
			window.NamedLayout = supportedNamedLayout(window.Layout, window.Panes, window.ActivePane)
		}
		tiles := layoutTiles(window.Layout, MejaTile{W: 100, H: 100})
		mapping := make(map[uint64]uint64, len(window.Panes))
		for paneIndex := range window.Panes {
			oldID := window.Panes[paneIndex].ID
			if tile, ok := tiles[oldID]; ok {
				window.Panes[paneIndex].Tile = tile
			}
			mapping[oldID] = uint64(paneIndex)
			window.Panes[paneIndex].ID = uint64(paneIndex)
		}
		window.ActivePane = mapping[window.ActivePane]
	}
}

var namedLayoutPresets = []struct {
	name   string
	preset int
}{
	{name: "even-horizontal", preset: layoutPresetEvenHorizontal},
	{name: "even-vertical", preset: layoutPresetEvenVertical},
	{name: "main-horizontal", preset: layoutPresetMainHorizontal},
	{name: "main-vertical", preset: layoutPresetMainVertical},
	{name: "tiled", preset: layoutPresetTiled},
}

func supportedNamedLayout(layout PlanLayout, panes []PlanPane, activePane uint64) string {
	paneIDs := make([]uint64, len(panes))
	for index, pane := range panes {
		paneIDs[index] = pane.ID
	}
	for _, candidate := range namedLayoutPresets {
		preset, err := planLayout(buildPresetLayout(paneIDs, activePane, candidate.preset))
		if err == nil && equalPlanLayouts(layout, preset) {
			return candidate.name
		}
	}
	return ""
}

func buildNamedPlanLayout(name string, paneIDs []uint64, activePane uint64) (PlanLayout, bool) {
	preset, ok := namedLayoutPreset(name)
	if !ok {
		return PlanLayout{}, false
	}
	layout, err := planLayout(buildPresetLayout(paneIDs, activePane, preset))
	return layout, err == nil
}

func namedLayoutPreset(name string) (int, bool) {
	for _, candidate := range namedLayoutPresets {
		if candidate.name == name {
			return candidate.preset, true
		}
	}
	return layoutPresetCustom, false
}

func equalPlanLayouts(first, second PlanLayout) bool {
	if (first.Pane == nil) != (second.Pane == nil) || first.Split != second.Split ||
		math.Abs(first.Ratio-second.Ratio) > 0.000001 || len(first.Children) != len(second.Children) {
		return false
	}
	if first.Pane != nil && *first.Pane != *second.Pane {
		return false
	}
	for index := range first.Children {
		if !equalPlanLayouts(first.Children[index], second.Children[index]) {
			return false
		}
	}
	return true
}

func layoutTiles(layout PlanLayout, bounds MejaTile) map[uint64]MejaTile {
	result := make(map[uint64]MejaTile)
	var walk func(PlanLayout, MejaTile)
	walk = func(current PlanLayout, area MejaTile) {
		if current.Pane != nil {
			result[*current.Pane] = area
			return
		}
		first := area
		second := area
		if current.Split == "horizontal" {
			_, firstMinimum := layoutGridMinimum(current.Children[0])
			_, secondMinimum := layoutGridMinimum(current.Children[1])
			first.H = splitPercent(area.H, current.Ratio, firstMinimum, secondMinimum)
			second.Y += first.H
			second.H -= first.H
		} else {
			firstMinimum, _ := layoutGridMinimum(current.Children[0])
			secondMinimum, _ := layoutGridMinimum(current.Children[1])
			first.W = splitPercent(area.W, current.Ratio, firstMinimum, secondMinimum)
			second.X += first.W
			second.W -= first.W
		}
		walk(current.Children[0], first)
		walk(current.Children[1], second)
	}
	walk(layout, bounds)
	return result
}

func splitPercent(size int, ratio float64, firstMinimum, secondMinimum int) int {
	value := int(float64(size)*ratio + .5)
	if value < firstMinimum {
		return firstMinimum
	}
	if value > size-secondMinimum {
		return size - secondMinimum
	}
	return value
}

func layoutGridMinimum(layout PlanLayout) (int, int) {
	if layout.Pane != nil {
		return 1, 1
	}
	firstWidth, firstHeight := layoutGridMinimum(layout.Children[0])
	secondWidth, secondHeight := layoutGridMinimum(layout.Children[1])
	if layout.Split == "horizontal" {
		return max(firstWidth, secondWidth), firstHeight + secondHeight
	}
	return firstWidth + secondWidth, max(firstHeight, secondHeight)
}

func normalizePlanPaths(plan *SessionPlan, fileDirectory string, userOwned bool) (PlanPortability, error) {
	var report PlanPortability
	sessionRoot := filepath.Clean(plan.Root)
	if !filepath.IsAbs(sessionRoot) {
		return report, errors.New("session plan root must be absolute before encoding")
	}
	if userOwned {
		relativeRoot, err := filepath.Rel(fileDirectory, sessionRoot)
		if err != nil {
			return report, fmt.Errorf("express plan root relative to output file: %w", err)
		}
		plan.Root = filepath.ToSlash(relativeRoot)
	} else {
		plan.Root = sessionRoot
	}
	for windowIndex := range plan.Windows {
		window := &plan.Windows[windowIndex]
		windowCwd := filepath.Clean(window.Cwd)
		if window.Cwd == "" {
			windowCwd = sessionRoot
		}
		paneParent := windowCwd
		if userOwned && !pathWithinRoot(sessionRoot, windowCwd) {
			// A window retains the directory it was created with even after the
			// session root changes. Do not let that stale machine-local value
			// become the parent for portable pane paths.
			window.Cwd = ""
			paneParent = sessionRoot
		} else {
			window.Cwd = encodedPlanPath(sessionRoot, windowCwd, sessionRoot)
		}
		for paneIndex := range window.Panes {
			pane := &window.Panes[paneIndex]
			paneCwd := filepath.Clean(pane.Cwd)
			if !userOwned {
				pane.Cwd = paneCwd
				continue
			}
			if !pathWithinRoot(sessionRoot, paneCwd) {
				pane.Cwd = paneCwd
				report.AbsolutePanePaths++
				continue
			}
			pane.Cwd = encodedPlanPath(paneParent, paneCwd, sessionRoot)
		}
	}
	return report, nil
}

func encodedPlanPath(parent, path, sessionRoot string) string {
	if filepath.Clean(path) == filepath.Clean(parent) {
		return ""
	}
	if pathWithinRoot(sessionRoot, path) {
		if relative, err := filepath.Rel(parent, path); err == nil {
			return filepath.ToSlash(relative)
		}
	}
	return filepath.Clean(path)
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func cloneSessionPlan(plan SessionPlan) SessionPlan {
	clone := plan
	clone.Windows = make([]PlanWindow, len(plan.Windows))
	for index, window := range plan.Windows {
		clone.Windows[index] = window
		clone.Windows[index].Panes = append([]PlanPane(nil), window.Panes...)
		clone.Windows[index].Layout = clonePlanLayout(window.Layout)
	}
	return clone
}

func cloneSessionPersistence(persistence SessionPersistence) SessionPersistence {
	clone := persistence
	clone.Plan = cloneSessionPlan(persistence.Plan)
	clone.WindowViews = append([]SessionViewPersistence(nil), persistence.WindowViews...)
	return clone
}

func clonePlanLayout(layout PlanLayout) PlanLayout {
	clone := layout
	if layout.Pane != nil {
		pane := *layout.Pane
		clone.Pane = &pane
	}
	clone.Children = make([]PlanLayout, len(layout.Children))
	for index := range layout.Children {
		clone.Children[index] = clonePlanLayout(layout.Children[index])
	}
	return clone
}

func readMejaDocument(path string) (*document.Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open .meja file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMejaFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read .meja file: %w", err)
	}
	if len(data) > maxMejaFileSize {
		return nil, errors.New(".meja file exceeds 4 MiB")
	}
	doc, err := kdl.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse .meja file: %w", err)
	}
	return doc, nil
}

func readUserSessionPlan(path string) (SessionPlan, error) {
	doc, err := readMejaDocument(path)
	if err != nil {
		return SessionPlan{}, err
	}
	nodes := planNodesWithoutLegacyVersion(doc.Nodes)
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	plan, err := parsePlanNodes(nodes, filepath.Dir(path), name, true)
	if err != nil {
		return SessionPlan{}, err
	}
	if err := validateSessionPlan(plan); err != nil {
		return SessionPlan{}, err
	}
	return plan, nil
}

func readSessionPersistence(path, expectedName string) (SessionPersistence, error) {
	doc, err := readMejaDocument(path)
	if err != nil {
		return SessionPersistence{}, err
	}
	var sessionNode *document.Node
	for _, current := range planNodesWithoutLegacyVersion(doc.Nodes) {
		if current.Name.ValueString() != "session" {
			continue
		}
		if sessionNode != nil {
			return SessionPersistence{}, errors.New("session persistence file contains more than one session")
		}
		sessionNode = current
	}
	if sessionNode == nil {
		return SessionPersistence{}, errors.New("session persistence file requires a session node")
	}
	persistence, err := parseSessionPersistenceNode(sessionNode)
	if err != nil {
		return SessionPersistence{}, err
	}
	if expectedName != "" && persistence.Name != expectedName {
		return SessionPersistence{}, fmt.Errorf("session name %q does not match requested name %q", persistence.Name, expectedName)
	}
	return persistence, nil
}

func planNodesWithoutLegacyVersion(nodes []*document.Node) []*document.Node {
	if len(nodes) > 0 && nodes[0].Name.ValueString() == "meja" {
		return nodes[1:]
	}
	return nodes
}

func parseSessionPersistenceNode(n *document.Node) (SessionPersistence, error) {
	var persistence SessionPersistence
	persistence.Version = mejaFormatVersion
	if len(n.Arguments) != 0 {
		return persistence, errors.New("session does not accept positional arguments")
	}
	var err error
	idValue, ok := n.Properties.Get("id")
	if !ok {
		return persistence, errors.New("session requires id")
	}
	persistence.SessionID, err = valueUint(idValue)
	if err != nil {
		return persistence, errors.New("session id must be a non-negative integer")
	}
	nameValue, ok := n.Properties.Get("name")
	if !ok {
		return persistence, errors.New("session requires name")
	}
	persistence.Name, ok = nameValue.Value.(string)
	if !ok {
		return persistence, errors.New("session name must be a string")
	}
	savedAtValue, ok := n.Properties.Get("saved-at")
	if !ok {
		return persistence, errors.New("session requires saved-at")
	}
	savedAt, ok := savedAtValue.Value.(string)
	if !ok {
		return persistence, errors.New("session saved-at must be an RFC3339 string")
	}
	persistence.SavedAt, err = time.Parse(time.RFC3339, savedAt)
	if err != nil {
		return persistence, errors.New("session saved-at must be an RFC3339 string")
	}
	if profileValue, exists := n.Properties.Get("profile"); exists {
		persistence.Profile, ok = profileValue.Value.(string)
		if !ok {
			return persistence, errors.New("session profile must be a string")
		}
	}
	if schemaValue, exists := n.Properties.Get("schema"); exists {
		var schema uint64
		schema, err = valueUint(schemaValue)
		if err != nil {
			return persistence, errors.New("session schema must be a non-negative integer")
		}
		persistence.Schema = int(schema)
	}
	if groupValue, exists := n.Properties.Get("group-id"); exists {
		persistence.GroupID, err = valueUint(groupValue)
		if err != nil {
			return persistence, errors.New("session group-id must be a non-negative integer")
		}
	}
	if activeValue, exists := n.Properties.Get("active-window-id"); exists {
		persistence.ActiveWindowID, err = valueUint(activeValue)
		if err != nil {
			return persistence, errors.New("session active-window-id must be a non-negative integer")
		}
	}
	if previousValue, exists := n.Properties.Get("previous-window-id"); exists {
		persistence.PreviousWindowID, err = valueUint(previousValue)
		if err != nil {
			return persistence, errors.New("session previous-window-id must be a non-negative integer")
		}
	}
	for _, child := range n.Children {
		if child.Name.ValueString() != "view" {
			continue
		}
		var view SessionViewPersistence
		windowID, ok := child.Properties.Get("window-id")
		if !ok {
			return persistence, errors.New("session view requires window-id")
		}
		view.WindowID, err = valueUint(windowID)
		if err != nil {
			return persistence, errors.New("session view window-id must be a non-negative integer")
		}
		if focused, ok := child.Properties.Get("focused-pane-id"); ok {
			view.FocusedPaneID, err = valueUint(focused)
			if err != nil {
				return persistence, errors.New("session view focused-pane-id must be a non-negative integer")
			}
		}
		if zoomed, ok := child.Properties.Get("zoomed-pane-id"); ok {
			view.ZoomedPaneID, err = valueUint(zoomed)
			if err != nil {
				return persistence, errors.New("session view zoomed-pane-id must be a non-negative integer")
			}
		}
		if display, ok := child.Properties.Get("display-index"); ok {
			if value, valueErr := valueUint(display); valueErr == nil {
				view.DisplayIndex = int(value)
			} else {
				return persistence, errors.New("session view display-index must be a non-negative integer")
			}
		}
		persistence.WindowViews = append(persistence.WindowViews, view)
	}
	persistence.Plan, err = parsePlanNodes(n.Children, "", persistence.Name, false)
	if err != nil {
		return persistence, err
	}
	persistence.Root = persistence.Plan.Root
	if err := validateSessionPersistence(persistence); err != nil {
		return persistence, err
	}
	if persistence.ActiveWindowID == 0 {
		persistence.ActiveWindowID = uint64(persistence.Plan.ActiveWindow)
	}
	return persistence, nil
}

func parsePlanNodes(nodes []*document.Node, parent, name string, userOwned bool) (SessionPlan, error) {
	plan := SessionPlan{Version: mejaFormatVersion, Name: name}
	var rootNode *document.Node
	var windowNodes []*document.Node
	activeWindowID := uint64(0)
	seenActiveWindow := false
	for _, current := range nodes {
		switch current.Name.ValueString() {
		case "root":
			if rootNode != nil {
				return plan, errors.New("session plan contains more than one root")
			}
			rootNode = current
		case "active-window":
			if userOwned {
				continue
			}
			if seenActiveWindow {
				return plan, errors.New("session plan contains more than one active-window")
			}
			var err error
			activeWindowID, err = simpleUintNode(current, "active-window")
			if err != nil {
				return plan, err
			}
			seenActiveWindow = true
		case "window":
			windowNodes = append(windowNodes, current)
		default:
			continue
		}
	}
	if rootNode == nil {
		return plan, errors.New("session plan requires root")
	}
	rawRoot, err := simpleStringNode(rootNode, "root")
	if err != nil {
		return plan, err
	}
	if userOwned {
		plan.Root, err = resolveMejaDirectory(rawRoot, parent)
		if err != nil {
			return plan, fmt.Errorf("plan root: %w", err)
		}
	} else {
		if !filepath.IsAbs(rawRoot) {
			return plan, errors.New("session root must be an absolute path")
		}
		plan.Root = filepath.Clean(rawRoot)
	}
	if activeWindowID >= uint64(len(windowNodes)) {
		return plan, fmt.Errorf("active window %d is not defined", activeWindowID)
	}
	for index, windowNode := range windowNodes {
		window, err := parseMejaWindow(windowNode, plan.Root, !userOwned)
		if err != nil {
			return plan, err
		}
		window.ID = uint64(index)
		plan.Windows = append(plan.Windows, window)
	}
	plan.ActiveWindow = int(activeWindowID) + 1
	return plan, nil
}

func simpleUintNode(n *document.Node, name string) (uint64, error) {
	if len(n.Arguments) != 1 {
		return 0, fmt.Errorf("%s requires one non-negative integer", name)
	}
	value, err := valueUint(n.Arguments[0])
	if err != nil {
		return 0, fmt.Errorf("%s requires one non-negative integer", name)
	}
	return value, nil
}

func parseMejaWindow(n *document.Node, parent string, private bool) (PlanWindow, error) {
	window := PlanWindow{AutomaticName: true}
	var err error
	if len(n.Arguments) != 0 {
		return window, errors.New("window does not accept positional arguments")
	}
	if value, ok := n.Properties.Get("name"); ok {
		name, ok := value.Value.(string)
		if !ok {
			return window, errors.New("window name must be a string")
		}
		window.Name = name
		window.AutomaticName = false
	}
	if active, ok := n.Properties.Get("active-pane"); ok && private {
		window.ActivePane, err = valueUint(active)
		if err != nil {
			return window, errors.New("window active-pane must be a non-negative integer")
		}
	}
	windowCwd := parent
	seenCwd := false
	var layoutName string
	for _, child := range n.Children {
		switch child.Name.ValueString() {
		case "cwd":
			if seenCwd {
				return window, errors.New("window contains more than one cwd")
			}
			raw, err := simpleStringNode(child, "cwd")
			if err != nil {
				return window, err
			}
			windowCwd, err = resolveMejaDirectory(raw, parent)
			if err != nil {
				return window, fmt.Errorf("window cwd: %w", err)
			}
			seenCwd = true
		case "layout":
			if layoutName != "" {
				return window, errors.New("window contains more than one layout")
			}
			var err error
			layoutName, err = simpleStringNode(child, "layout")
			if err != nil {
				return window, err
			}
		}
	}
	for _, child := range n.Children {
		switch child.Name.ValueString() {
		case "cwd", "layout":
			continue
		case "pane":
			pane, err := parseMejaPane(child, windowCwd)
			if err != nil {
				return window, fmt.Errorf("window: %w", err)
			}
			pane.ID = uint64(len(window.Panes))
			window.Panes = append(window.Panes, pane)
		default:
			continue
		}
	}
	window.Cwd = windowCwd
	if window.ActivePane >= uint64(len(window.Panes)) {
		return window, fmt.Errorf("active pane %d is not defined", window.ActivePane)
	}
	if len(window.Panes) == 1 {
		window.Layout = PlanLayout{Pane: paneIDRef(0)}
		return window, nil
	}
	paneIDs := make([]uint64, len(window.Panes))
	for index := range window.Panes {
		paneIDs[index] = uint64(index)
	}
	if layoutName != "" {
		if layout, ok := buildNamedPlanLayout(layoutName, paneIDs, window.ActivePane); ok {
			window.NamedLayout = layoutName
			window.Layout = layout
			return window, nil
		}
	}
	layout, err := layoutFromTiles(window.Panes, MejaTile{W: 100, H: 100})
	if err != nil {
		if layoutName != "" {
			return window, fmt.Errorf("unsupported layout %q requires tile fallback", layoutName)
		}
		return window, fmt.Errorf("window tiles: %w", err)
	}
	window.Layout = layout
	return window, nil
}

func parseMejaPane(n *document.Node, parent string) (PlanPane, error) {
	var pane PlanPane
	if len(n.Arguments) != 0 {
		return pane, errors.New("pane does not accept positional arguments")
	}
	var err error
	pane.Cwd = parent
	seen := make(map[string]bool)
	for _, child := range n.Children {
		name := child.Name.ValueString()
		known := name == "cwd" || name == "shell" || name == "cmd" || name == "tile"
		if !known {
			continue
		}
		if seen[name] {
			return pane, fmt.Errorf("duplicate pane child %q", name)
		}
		seen[name] = true
		switch name {
		case "cwd":
			raw, err := simpleStringNode(child, "cwd")
			if err != nil {
				return pane, err
			}
			pane.Cwd, err = resolveMejaDirectory(raw, parent)
			if err != nil {
				return pane, fmt.Errorf("pane %d cwd: %w", pane.ID, err)
			}
		case "shell":
			pane.Shell, err = simpleStringNode(child, "shell")
		case "cmd":
			pane.Command, err = simpleStringNode(child, "cmd")
		case "tile":
			pane.Tile, err = parseTile(child)
		}
		if err != nil {
			return pane, err
		}
	}
	return pane, nil
}

func parseTile(n *document.Node) (MejaTile, error) {
	var tile MejaTile
	if len(n.Arguments) != 0 {
		return tile, errors.New("invalid tile")
	}
	values := []*int{&tile.X, &tile.Y, &tile.W, &tile.H}
	for index, name := range []string{"x", "y", "w", "h"} {
		value, ok := n.Properties.Get(name)
		if !ok {
			return tile, fmt.Errorf("tile requires %s", name)
		}
		parsed, err := valueUint(value)
		if err != nil || parsed > 100 {
			return tile, fmt.Errorf("tile %s must be an integer from 0 through 100", name)
		}
		*values[index] = int(parsed)
	}
	if tile.W == 0 || tile.H == 0 || tile.X+tile.W > 100 || tile.Y+tile.H > 100 {
		return tile, errors.New("tile must have positive dimensions within the 100 x 100 surface")
	}
	return tile, nil
}

func layoutFromTiles(panes []PlanPane, bounds MejaTile) (PlanLayout, error) {
	if len(panes) == 0 {
		return PlanLayout{}, errors.New("no panes")
	}
	area := 0
	for i, pane := range panes {
		area += pane.Tile.W * pane.Tile.H
		for _, other := range panes[i+1:] {
			if tilesOverlap(pane.Tile, other.Tile) {
				return PlanLayout{}, fmt.Errorf("pane %d overlaps pane %d", pane.ID, other.ID)
			}
		}
	}
	if area != bounds.W*bounds.H {
		return PlanLayout{}, errors.New("tiles do not cover the complete window")
	}
	return splitTileLayout(panes, bounds)
}

func splitTileLayout(panes []PlanPane, bounds MejaTile) (PlanLayout, error) {
	if len(panes) == 1 {
		if panes[0].Tile != bounds {
			return PlanLayout{}, errors.New("tile does not fill its layout region")
		}
		return PlanLayout{Pane: paneIDRef(panes[0].ID)}, nil
	}
	for _, vertical := range []bool{true, false} {
		var cuts []int
		for _, pane := range panes {
			if vertical {
				cuts = append(cuts, pane.Tile.X, pane.Tile.X+pane.Tile.W)
			} else {
				cuts = append(cuts, pane.Tile.Y, pane.Tile.Y+pane.Tile.H)
			}
		}
		sort.Ints(cuts)
		for _, cut := range cuts {
			start, end := bounds.X, bounds.X+bounds.W
			if !vertical {
				start, end = bounds.Y, bounds.Y+bounds.H
			}
			if cut <= start || cut >= end {
				continue
			}
			var first, second []PlanPane
			valid := true
			for _, pane := range panes {
				paneStart, paneEnd := pane.Tile.X, pane.Tile.X+pane.Tile.W
				if !vertical {
					paneStart, paneEnd = pane.Tile.Y, pane.Tile.Y+pane.Tile.H
				}
				if paneEnd <= cut {
					first = append(first, pane)
				} else if paneStart >= cut {
					second = append(second, pane)
				} else {
					valid = false
					break
				}
			}
			if !valid || len(first) == 0 || len(second) == 0 {
				continue
			}
			firstBounds, secondBounds := bounds, bounds
			direction := "vertical"
			ratio := float64(cut-start) / float64(end-start)
			if vertical {
				firstBounds.W = cut - bounds.X
				secondBounds.X = cut
				secondBounds.W = bounds.X + bounds.W - cut
			} else {
				direction = "horizontal"
				firstBounds.H = cut - bounds.Y
				secondBounds.Y = cut
				secondBounds.H = bounds.Y + bounds.H - cut
			}
			left, leftErr := splitTileLayout(first, firstBounds)
			right, rightErr := splitTileLayout(second, secondBounds)
			if leftErr == nil && rightErr == nil {
				return PlanLayout{Split: direction, Ratio: ratio, Children: []PlanLayout{left, right}}, nil
			}
		}
	}
	return PlanLayout{}, errors.New("tiles are not a restorable pane tiling")
}

func tilesOverlap(a, b MejaTile) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

func remapLayoutPaneIDs(layout *PlanLayout, mapping map[uint64]uint64) {
	if layout.Pane != nil {
		value := mapping[*layout.Pane]
		layout.Pane = &value
		return
	}
	for index := range layout.Children {
		remapLayoutPaneIDs(&layout.Children[index], mapping)
	}
}

func resolveMejaDirectory(raw, parent string) (string, error) {
	if raw == "" {
		return "", errors.New("directory must not be empty")
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if raw == "~" {
			return home, nil
		}
		return filepath.Clean(filepath.Join(home, strings.TrimPrefix(raw, "~/"))), nil
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), nil
	}
	if parent == "" {
		return "", fmt.Errorf("relative directory %q has no parent directory", raw)
	}
	return filepath.Clean(filepath.Join(parent, raw)), nil
}

func simpleStringNode(n *document.Node, kind string) (string, error) {
	if len(n.Arguments) != 1 {
		return "", fmt.Errorf("%s requires one string", kind)
	}
	value, ok := n.Arguments[0].Value.(string)
	if !ok || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s must be a string without control characters", kind)
	}
	return value, nil
}

func valueUint(value *document.Value) (uint64, error) {
	switch number := value.Value.(type) {
	case uint:
		return uint64(number), nil
	case uint64:
		return number, nil
	case int:
		if number >= 0 {
			return uint64(number), nil
		}
	case int64:
		if number >= 0 {
			return uint64(number), nil
		}
	case *big.Int:
		if number.Sign() >= 0 && number.IsUint64() {
			return number.Uint64(), nil
		}
	}
	return 0, errors.New("not a non-negative integer")
}

func writeSessionPersistence(directory string, persistence SessionPersistence) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create session persistence directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", fmt.Errorf("protect session persistence directory: %w", err)
	}
	target := filepath.Join(directory, persistence.Name+".session.meja")
	data, err := encodeSessionPersistence(persistence)
	if err != nil {
		return "", err
	}
	return target, atomicWriteMeja(target, data, 0o600)
}

func writeUserMejaFile(path string, plan SessionPlan, force bool) (PlanPortability, error) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return PlanPortability{}, fmt.Errorf("output file %q already exists; use -f to overwrite", path)
		} else if !os.IsNotExist(err) {
			return PlanPortability{}, err
		}
	}
	data, report, err := encodeUserSessionPlan(plan, path)
	if err != nil {
		return PlanPortability{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return PlanPortability{}, fmt.Errorf("create output directory: %w", err)
	}
	return report, atomicWriteMeja(path, data, 0o644)
}

func atomicWriteMeja(target string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(target)
	temporary, err := os.CreateTemp(directory, ".meja-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary .meja file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("replace .meja file: %w", err)
	}
	return nil
}
