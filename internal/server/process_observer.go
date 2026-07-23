package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type PaneKey struct {
	// Pane IDs are allocated by the daemon and are never recycled. A shared
	// pane therefore has one process-monitor identity regardless of how many
	// sessions currently link to its window.
	PaneID uint64 `json:"paneId"`
}

// Identity distinguishes a live process from a later process that reused its
// PID. BirthToken is an opaque, platform-specific process creation value.
type Identity struct {
	PID        int    `json:"pid"`
	BirthToken uint64 `json:"birthToken"`
}

type Anchor struct {
	Key         PaneKey
	Root        Identity
	PTY         *os.File
	RootIsShell bool
}

type ProcessStatus uint8

const (
	StatusDetected ProcessStatus = iota + 1
	StatusShellOwned
	StatusAmbiguous
	StatusUnstable
	StatusUnavailable
)

func (s ProcessStatus) String() string {
	switch s {
	case StatusDetected:
		return "detected"
	case StatusShellOwned:
		return "shell-owned"
	case StatusAmbiguous:
		return "ambiguous"
	case StatusUnstable:
		return "unstable"
	case StatusUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

type ObservedProcess struct {
	Identity     Identity `json:"identity"`
	Name         string   `json:"name"`
	PPID         int      `json:"ppid"`
	PGID         int      `json:"pgid"`
	SessionState int      `json:"session"`
	TTY          int64    `json:"tty"`
	State        byte     `json:"state"`

	Argv          []string `json:"argv,omitempty"`
	ArgvAvailable bool     `json:"argvAvailable"`
	Exe           string   `json:"exe,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
}

type ProcessObservation struct {
	Key            PaneKey           `json:"key"`
	ForegroundPGID int               `json:"foregroundPgid"`
	Status         ProcessStatus     `json:"status"`
	Root           *ObservedProcess  `json:"root,omitempty"`
	Processes      []ObservedProcess `json:"processes"`
	Candidate      *ObservedProcess  `json:"candidate,omitempty"`
	Issues         []string          `json:"issues,omitempty"`
}

func cloneProcessObservation(observation ProcessObservation) ProcessObservation {
	clone := observation
	clone.Root = cloneObservedProcess(observation.Root)
	clone.Candidate = cloneObservedProcess(observation.Candidate)
	clone.Processes = append([]ObservedProcess(nil), observation.Processes...)
	for index := range clone.Processes {
		clone.Processes[index].Argv = append([]string(nil), observation.Processes[index].Argv...)
	}
	clone.Issues = append([]string(nil), observation.Issues...)
	return clone
}

type ProcessObserver interface {
	Observe(context.Context, []Anchor) map[PaneKey]ProcessObservation
}

func NewProcessObserver() ProcessObserver {
	return systemObserver{}
}

const (
	maxCmdlineBytes    = 1 << 20
	observationRetries = 3
)

var errObservationChanged = errors.New("foreground process group changed during observation")

type systemObserver struct{}

func identifyProcess(pid int) (Identity, error) {
	if runtime.GOOS == "linux" {
		return identifyProc(pid)
	}
	return identifyPS(pid)
}

func (systemObserver) Observe(ctx context.Context, anchors []Anchor) map[PaneKey]ProcessObservation {
	if runtime.GOOS == "linux" {
		return observeProc(ctx, anchors)
	}
	return observePS(ctx, anchors)
}

type procStat struct {
	Identity     Identity
	Name         string
	PPID         int
	PGID         int
	SessionState int
	TTY          int64
	TPGID        int
	State        byte
}

func identifyProc(pid int) (Identity, error) {
	stat, err := readProcStat(pid)
	if err != nil {
		return Identity{}, err
	}
	return stat.Identity, nil
}

func observeProc(ctx context.Context, anchors []Anchor) map[PaneKey]ProcessObservation {
	observations := make(map[PaneKey]ProcessObservation, len(anchors))
	pending := append([]Anchor(nil), anchors...)
	for attempt := 0; attempt < observationRetries && len(pending) > 0; attempt++ {
		attempted, unstable := observeProcBatchAttempt(ctx, pending, scanProcTable)
		for key, observation := range attempted {
			observations[key] = observation
		}
		pending = unstable
	}
	for _, anchor := range pending {
		observations[anchor.Key] = ProcessObservation{
			Key: anchor.Key, Status: StatusUnstable,
			Issues: []string{errObservationChanged.Error()},
		}
	}
	return observations
}

type preparedProcObservation struct {
	anchor         Anchor
	observation    ProcessObservation
	foregroundPGID int
	rootBefore     procStat
	groupBefore    []procStat
}

// observeProcBatchAttempt takes one before/after process-table pair for every
// anchor in the batch. This preserves the old stability check without paying
// for two complete /proc scans independently for each pane.
func observeProcBatchAttempt(ctx context.Context, anchors []Anchor, scan func(context.Context) ([]procStat, error)) (map[PaneKey]ProcessObservation, []Anchor) {
	observations := make(map[PaneKey]ProcessObservation, len(anchors))
	before, err := scan(ctx)
	if err != nil {
		for _, anchor := range anchors {
			observations[anchor.Key] = unavailableProcObservation(anchor.Key, err)
		}
		return observations, nil
	}
	beforeByPID := indexProcStats(before)
	prepared := make([]preparedProcObservation, 0, len(anchors))
	var unstable []Anchor
	for _, anchor := range anchors {
		state, err := prepareProcObservation(anchor, before, beforeByPID)
		if err == nil {
			prepared = append(prepared, state)
			continue
		}
		if errors.Is(err, errObservationChanged) {
			unstable = append(unstable, anchor)
			continue
		}
		observations[anchor.Key] = unavailableProcObservation(anchor.Key, err)
	}
	if len(prepared) == 0 {
		return observations, unstable
	}
	after, err := scan(ctx)
	if err != nil {
		for _, state := range prepared {
			observations[state.anchor.Key] = unavailableProcObservation(state.anchor.Key, err)
		}
		return observations, unstable
	}
	afterByPID := indexProcStats(after)
	for _, state := range prepared {
		rootAfter, ok := afterByPID[state.anchor.Root.PID]
		foregroundAfter, foregroundErr := foregroundProcessGroup(state.anchor.PTY)
		groupAfter := filterProcessGroup(after, state.rootBefore.SessionState, state.rootBefore.TTY, state.foregroundPGID)
		if !ok || rootAfter.Identity != state.rootBefore.Identity ||
			rootAfter.SessionState != state.rootBefore.SessionState || rootAfter.TTY != state.rootBefore.TTY ||
			rootAfter.TPGID != state.foregroundPGID || foregroundErr != nil || foregroundAfter != state.foregroundPGID ||
			!sameProcessGroup(state.groupBefore, groupAfter) {
			unstable = append(unstable, state.anchor)
			continue
		}
		observations[state.anchor.Key] = classifyPreparedProcObservation(state)
	}
	return observations, unstable
}

func unavailableProcObservation(key PaneKey, err error) ProcessObservation {
	return ProcessObservation{Key: key, Status: StatusUnavailable, Issues: []string{err.Error()}}
}

func prepareProcObservation(anchor Anchor, table []procStat, byPID map[int]procStat) (preparedProcObservation, error) {
	state := preparedProcObservation{anchor: anchor}
	state.observation = ProcessObservation{Key: anchor.Key, Status: StatusUnavailable}
	if anchor.PTY == nil {
		return state, errors.New("pane PTY is unavailable")
	}
	if anchor.Root.PID <= 0 || anchor.Root.BirthToken == 0 {
		return state, errors.New("pane root process identity is unavailable")
	}
	foregroundPGID, err := foregroundProcessGroup(anchor.PTY)
	if err != nil {
		return state, fmt.Errorf("read foreground process group: %w", err)
	}
	rootBefore, ok := byPID[anchor.Root.PID]
	if !ok {
		return state, errors.New("read pane root: process is unavailable")
	}
	if rootBefore.Identity != anchor.Root {
		return state, errors.New("pane root process identity changed")
	}
	if rootBefore.TPGID != foregroundPGID {
		return state, errObservationChanged
	}
	state.foregroundPGID = foregroundPGID
	state.rootBefore = rootBefore
	state.groupBefore = filterProcessGroup(table, rootBefore.SessionState, rootBefore.TTY, foregroundPGID)
	state.observation.ForegroundPGID = foregroundPGID
	state.observation.Processes = make([]ObservedProcess, 0, len(state.groupBefore))
	for _, stat := range state.groupBefore {
		process, issues, err := readProcess(stat)
		if err != nil {
			return state, errObservationChanged
		}
		state.observation.Processes = append(state.observation.Processes, process)
		state.observation.Issues = append(state.observation.Issues, issues...)
	}
	rootProcess, issues, err := readProcess(rootBefore)
	if err != nil {
		return state, errObservationChanged
	}
	state.observation.Root = &rootProcess
	state.observation.Issues = append(state.observation.Issues, issues...)
	return state, nil
}

func classifyPreparedProcObservation(state preparedProcObservation) ProcessObservation {
	observation := state.observation
	if !state.anchor.RootIsShell {
		observation.Status = StatusDetected
		observation.Candidate = cloneObservedProcess(observation.Root)
		return observation
	}
	directChildren := make([]*ObservedProcess, 0, 2)
	for index := range observation.Processes {
		if observation.Processes[index].PPID == state.anchor.Root.PID {
			directChildren = append(directChildren, &observation.Processes[index])
		}
	}
	switch len(directChildren) {
	case 0:
		if state.foregroundPGID == state.rootBefore.PGID {
			observation.Status = StatusShellOwned
		} else {
			observation.Status = StatusAmbiguous
			observation.Issues = append(observation.Issues, "foreground group has no direct child of the pane shell")
		}
	case 1:
		observation.Status = StatusDetected
		observation.Candidate = cloneObservedProcess(directChildren[0])
	default:
		observation.Status = StatusAmbiguous
	}
	return observation
}

func foregroundProcessGroup(file *os.File) (int, error) {
	if file == nil {
		return 0, errors.New("pane PTY is unavailable")
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pgid int
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		pgid, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}); err != nil {
		return 0, err
	}
	if ioctlErr != nil {
		return 0, ioctlErr
	}
	if pgid <= 0 {
		return 0, fmt.Errorf("invalid foreground process group %d", pgid)
	}
	return pgid, nil
}

func scanProcTable(ctx context.Context) ([]procStat, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("scan /proc: %w", err)
	}
	stats := make([]procStat, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := readProcStat(pid)
		if err != nil {
			continue
		}
		stats = append(stats, stat)
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Identity.PID < stats[j].Identity.PID })
	return stats, nil
}

func filterProcessGroup(table []procStat, session int, tty int64, pgid int) []procStat {
	stats := make([]procStat, 0, 4)
	for _, stat := range table {
		if stat.SessionState == session && stat.TTY == tty && stat.PGID == pgid && stat.State != 'Z' && stat.State != 'X' && stat.State != 'x' {
			stats = append(stats, stat)
		}
	}
	return stats
}

func indexProcStats(stats []procStat) map[int]procStat {
	indexed := make(map[int]procStat, len(stats))
	for _, stat := range stats {
		indexed[stat.Identity.PID] = stat
	}
	return indexed
}

func sameProcessGroup(first, second []procStat) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index].Identity != second[index].Identity ||
			first[index].PPID != second[index].PPID || first[index].PGID != second[index].PGID {
			return false
		}
	}
	return true
}

func readProcess(stat procStat) (ObservedProcess, []string, error) {
	process := ObservedProcess{
		Identity:     stat.Identity,
		Name:         stat.Name,
		PPID:         stat.PPID,
		PGID:         stat.PGID,
		SessionState: stat.SessionState,
		TTY:          stat.TTY,
		State:        stat.State,
	}
	base := filepath.Join("/proc", strconv.Itoa(stat.Identity.PID))
	beforeExe, beforeExeErr := os.Readlink(filepath.Join(base, "exe"))

	var issues []string
	cmdline, err := readLimitedFile(filepath.Join(base, "cmdline"), maxCmdlineBytes)
	if err != nil {
		issues = append(issues, fmt.Sprintf("pid %d cmdline: %v", stat.Identity.PID, err))
	} else {
		process.Argv = parseCmdline(cmdline)
		process.ArgvAvailable = true
	}
	if beforeExeErr != nil {
		issues = append(issues, fmt.Sprintf("pid %d exe: %v", stat.Identity.PID, beforeExeErr))
	} else {
		process.Exe = beforeExe
	}
	if cwd, err := os.Readlink(filepath.Join(base, "cwd")); err != nil {
		issues = append(issues, fmt.Sprintf("pid %d cwd: %v", stat.Identity.PID, err))
	} else {
		process.Cwd = cwd
	}

	after, err := readProcStat(stat.Identity.PID)
	if err != nil || after.Identity != stat.Identity || after.PPID != stat.PPID ||
		after.PGID != stat.PGID || after.SessionState != stat.SessionState || after.TTY != stat.TTY {
		return ObservedProcess{}, nil, errObservationChanged
	}
	if beforeExeErr == nil {
		afterExe, err := os.Readlink(filepath.Join(base, "exe"))
		if err != nil || afterExe != beforeExe {
			return ObservedProcess{}, nil, errObservationChanged
		}
	}
	return process, issues, nil
}

func readLimitedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("content exceeds %d bytes", maximum)
	}
	return data, nil
}

func parseCmdline(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}
	parts := bytes.Split(data, []byte{0})
	argv := make([]string, len(parts))
	for index, part := range parts {
		argv[index] = string(part)
	}
	return argv
}

func readProcStat(pid int) (procStat, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	return parseProcStat(data)
}

func parseProcStat(data []byte) (procStat, error) {
	line := strings.TrimSpace(string(data))
	open := strings.Index(line, " (")
	close := strings.LastIndex(line, ") ")
	if open <= 0 || close <= open+1 {
		return procStat{}, errors.New("malformed /proc stat header")
	}
	pid, err := strconv.Atoi(line[:open])
	if err != nil || pid <= 0 {
		return procStat{}, errors.New("malformed /proc stat PID")
	}
	fields := strings.Fields(line[close+2:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, errors.New("truncated /proc stat record")
	}
	parseInt := func(index int, name string) (int, error) {
		value, err := strconv.Atoi(fields[index])
		if err != nil {
			return 0, fmt.Errorf("parse /proc stat %s: %w", name, err)
		}
		return value, nil
	}
	ppid, err := parseInt(1, "ppid")
	if err != nil {
		return procStat{}, err
	}
	pgid, err := parseInt(2, "pgrp")
	if err != nil {
		return procStat{}, err
	}
	session, err := parseInt(3, "session")
	if err != nil {
		return procStat{}, err
	}
	tty, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("parse /proc stat tty: %w", err)
	}
	tpgid, err := parseInt(5, "tpgid")
	if err != nil {
		return procStat{}, err
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("parse /proc stat start time: %w", err)
	}
	return procStat{
		Identity:     Identity{PID: pid, BirthToken: startTime},
		Name:         line[open+2 : close],
		PPID:         ppid,
		PGID:         pgid,
		SessionState: session,
		TTY:          tty,
		TPGID:        tpgid,
		State:        fields[0][0],
	}, nil
}

// The ps implementation is the portable fallback for systems without procfs.
// It follows tmux-resurrect's immediate-child model while parsing numeric
// columns exactly.
func identifyPS(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, fmt.Errorf("invalid process ID %d", pid)
	}
	processes, err := scanPS(context.Background(), "-p", strconv.Itoa(pid))
	if err != nil {
		return Identity{}, err
	}
	for _, process := range processes {
		if process.Identity.PID == pid {
			return process.Identity, nil
		}
	}
	return Identity{}, fmt.Errorf("process %d not found", pid)
}

func observePS(ctx context.Context, anchors []Anchor) map[PaneKey]ProcessObservation {
	observations := make(map[PaneKey]ProcessObservation, len(anchors))
	if len(anchors) == 0 {
		return observations
	}

	foregroundBefore := sampleForegroundProcessGroups(anchors)
	before, err := scanPS(ctx, "-ax")
	if err != nil {
		return unavailablePSObservations(anchors, err)
	}
	after, err := scanPS(ctx, "-ax")
	if err != nil {
		return unavailablePSObservations(anchors, err)
	}
	beforeByPID := indexPSProcesses(before)
	afterByPID := indexPSProcesses(after)
	foregroundAfter := sampleForegroundProcessGroups(anchors)

	for _, anchor := range anchors {
		observation := ProcessObservation{Key: anchor.Key, Status: StatusUnavailable}
		if err := ctx.Err(); err != nil {
			observation.Issues = []string{err.Error()}
			observations[anchor.Key] = observation
			continue
		}
		beforeForeground := foregroundBefore[anchor.Key]
		afterForeground := foregroundAfter[anchor.Key]
		if beforeForeground.err == nil && afterForeground.err == nil {
			if beforeForeground.pgid != afterForeground.pgid {
				observation.Status = StatusUnstable
				observation.Issues = []string{"PTY foreground process group changed during observation"}
				observations[anchor.Key] = observation
				continue
			}
			observation.ForegroundPGID = beforeForeground.pgid
		}
		rootBefore := beforeByPID[anchor.Root.PID]
		rootAfter := afterByPID[anchor.Root.PID]
		if rootBefore == nil || rootAfter == nil || anchor.Root.PID <= 0 || anchor.Root.BirthToken == 0 {
			observation.Issues = []string{"pane root process is unavailable"}
			observations[anchor.Key] = observation
			continue
		}
		if rootBefore.Identity != anchor.Root || rootAfter.Identity != anchor.Root || !samePSProcess(rootBefore, rootAfter) {
			observation.Status = StatusUnstable
			observation.Issues = []string{"pane root process changed during observation"}
			observations[anchor.Key] = observation
			continue
		}

		childrenBefore := directPSChildren(before, anchor.Root.PID)
		childrenAfter := directPSChildren(after, anchor.Root.PID)
		if !samePSProcesses(childrenBefore, childrenAfter) {
			observation.Status = StatusUnstable
			observation.Issues = []string{"pane child processes changed during observation"}
			observations[anchor.Key] = observation
			continue
		}

		root := *rootBefore
		observation.Root = &root
		observation.Processes = childrenBefore
		observation.Issues = []string{"ps fallback does not provide structured argv, executable, or cwd data"}
		if !anchor.RootIsShell {
			observation.Status = StatusDetected
			observation.Candidate = cloneObservedProcess(&root)
			observations[anchor.Key] = observation
			continue
		}
		switch len(childrenBefore) {
		case 0:
			observation.Status = StatusShellOwned
		case 1:
			observation.Status = StatusDetected
			observation.Candidate = cloneObservedProcess(&childrenBefore[0])
		default:
			observation.Status = StatusAmbiguous
			observation.Issues = append(observation.Issues, "pane shell has multiple immediate child processes")
		}
		observations[anchor.Key] = observation
	}
	return observations
}

type foregroundProcessGroupSample struct {
	pgid int
	err  error
}

func sampleForegroundProcessGroups(anchors []Anchor) map[PaneKey]foregroundProcessGroupSample {
	samples := make(map[PaneKey]foregroundProcessGroupSample, len(anchors))
	for _, anchor := range anchors {
		pgid, err := foregroundProcessGroup(anchor.PTY)
		samples[anchor.Key] = foregroundProcessGroupSample{pgid: pgid, err: err}
	}
	return samples
}

func unavailablePSObservations(anchors []Anchor, err error) map[PaneKey]ProcessObservation {
	observations := make(map[PaneKey]ProcessObservation, len(anchors))
	for _, anchor := range anchors {
		observations[anchor.Key] = ProcessObservation{
			Key:    anchor.Key,
			Status: StatusUnavailable,
			Issues: []string{err.Error()},
		}
	}
	return observations
}

func scanPS(ctx context.Context, selection ...string) ([]ObservedProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args := append([]string(nil), selection...)
	args = append(args, "-o", "pid=,ppid=,pgid=,tty=,stat=,lstart=,comm=")
	output, err := exec.CommandContext(ctx, "ps", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("scan process table with ps: %w", err)
	}

	processes := make([]ObservedProcess, 0, 64)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		process, err := parsePSProcess(scanner.Text())
		if err != nil {
			continue
		}
		processes = append(processes, process)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read ps output: %w", err)
	}
	if len(processes) == 0 && len(strings.TrimSpace(string(output))) != 0 {
		return nil, errors.New("ps returned no parseable process rows")
	}
	sort.Slice(processes, func(i, j int) bool {
		return processes[i].Identity.PID < processes[j].Identity.PID
	})
	return processes, nil
}

func parsePSProcess(line string) (ObservedProcess, error) {
	fields := strings.Fields(line)
	// pid, ppid, pgid, tty, stat, five lstart fields, then comm.
	if len(fields) < 11 {
		return ObservedProcess{}, errors.New("incomplete ps process row")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return ObservedProcess{}, errors.New("invalid ps pid")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return ObservedProcess{}, errors.New("invalid ps ppid")
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return ObservedProcess{}, errors.New("invalid ps pgid")
	}
	start := strings.Join(fields[5:10], " ")
	name := filepath.Base(strings.Join(fields[10:], " "))
	name = strings.TrimPrefix(name, "-")
	if name == "" || name == "." {
		return ObservedProcess{}, errors.New("invalid ps command name")
	}
	state := byte(0)
	if fields[4] != "" {
		state = fields[4][0]
	}
	return ObservedProcess{
		Identity: Identity{PID: pid, BirthToken: hashPSValue(start)},
		Name:     name,
		PPID:     ppid,
		PGID:     pgid,
		TTY:      int64(hashPSValue(fields[3])),
		State:    state,
	}, nil
}

func hashPSValue(value string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	result := hash.Sum64()
	if result == 0 {
		return 1
	}
	return result
}

func indexPSProcesses(processes []ObservedProcess) map[int]*ObservedProcess {
	indexed := make(map[int]*ObservedProcess, len(processes))
	for index := range processes {
		indexed[processes[index].Identity.PID] = &processes[index]
	}
	return indexed
}

func directPSChildren(processes []ObservedProcess, parentPID int) []ObservedProcess {
	children := make([]ObservedProcess, 0, 2)
	for _, process := range processes {
		if process.PPID == parentPID && process.State != 'Z' {
			children = append(children, process)
		}
	}
	return children
}

func samePSProcess(first, second *ObservedProcess) bool {
	return first != nil && second != nil && first.Identity == second.Identity &&
		first.PPID == second.PPID && first.PGID == second.PGID && first.TTY == second.TTY
}

func samePSProcesses(first, second []ObservedProcess) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if !samePSProcess(&first[index], &second[index]) {
			return false
		}
	}
	return true
}

func cloneObservedProcess(process *ObservedProcess) *ObservedProcess {
	if process == nil {
		return nil
	}
	clone := *process
	clone.Argv = append([]string(nil), process.Argv...)
	return &clone
}
