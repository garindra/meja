package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestParseProcStatHandlesClosingParenthesisInCommand(t *testing.T) {
	data := []byte("123 (worker ) name) S 10 20 30 40 50 0 0 0 0 0 0 0 0 0 0 1 0 0 999 0 0")
	stat, err := parseProcStat(data)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Identity != (Identity{PID: 123, BirthToken: 999}) || stat.Name != "worker ) name" || stat.PPID != 10 ||
		stat.PGID != 20 || stat.SessionState != 30 || stat.TTY != 40 || stat.TPGID != 50 || stat.State != 'S' {
		t.Fatalf("unexpected stat: %#v", stat)
	}
}

func TestParseProcStatRejectsMalformedRecords(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(""),
		[]byte("12 no-parentheses S 1 2 3"),
		[]byte("12 (short) S 1 2 3"),
		[]byte("x (bad pid) S 1 2 3 4 5 0 0 0 0 0 0 0 0 0 0 1 0 9"),
	} {
		if _, err := parseProcStat(data); err == nil {
			t.Fatalf("parseProcStat(%q) unexpectedly succeeded", data)
		}
	}
}

func TestParseCmdlinePreservesArgumentBoundaries(t *testing.T) {
	got := parseCmdline([]byte("command\x00argument with spaces\x00\x00last\x00"))
	want := []string{"command", "argument with spaces", "", "last"}
	if len(got) != len(want) {
		t.Fatalf("argv=%q want %q", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("argv=%q want %q", got, want)
		}
	}
}

func TestObserverClassifiesRealPTYJobs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("foreground process-group classification uses the Linux procfs observer")
	}
	tests := []struct {
		name  string
		input string
		check func(*testing.T, ProcessObservation) bool
	}{
		{
			name: "idle shell",
			check: func(t *testing.T, observation ProcessObservation) bool {
				return observation.Status == StatusShellOwned && observation.Candidate == nil
			},
		},
		{
			name:  "single foreground child",
			input: "sleep 30\n",
			check: func(t *testing.T, observation ProcessObservation) bool {
				return observation.Status == StatusDetected && commandBase(observation.Candidate) == "sleep" &&
					observation.Candidate.PPID == observation.Root.Identity.PID
			},
		},
		{
			name:  "background child excluded",
			input: "sleep 30 &\n",
			check: func(t *testing.T, observation ProcessObservation) bool {
				if observation.Status != StatusShellOwned || observation.Candidate != nil {
					return false
				}
				for _, process := range observation.Processes {
					if commandBase(&process) == "sleep" {
						t.Fatalf("background sleep appeared in foreground group: %#v", observation)
					}
				}
				return true
			},
		},
		{
			name:  "wrapper preferred over descendant",
			input: "sh -c 'sleep 30 & wait'\n",
			check: func(t *testing.T, observation ProcessObservation) bool {
				if observation.Status != StatusDetected || commandBase(observation.Candidate) != "sh" {
					return false
				}
				hasDescendant := false
				for _, process := range observation.Processes {
					if commandBase(&process) == "sleep" && process.PPID == observation.Candidate.Identity.PID {
						hasDescendant = true
					}
				}
				return hasDescendant
			},
		},
		{
			name:  "pipeline remains ambiguous",
			input: "sleep 30 | cat\n",
			check: func(t *testing.T, observation ProcessObservation) bool {
				if observation.Status != StatusAmbiguous || observation.Candidate != nil {
					return false
				}
				directChildren := 0
				for _, process := range observation.Processes {
					if process.PPID == observation.Root.Identity.PID {
						directChildren++
					}
				}
				return directChildren >= 2
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			shell := startTestShell(t)
			if test.input != "" {
				if _, err := shell.ptmx.Write([]byte(test.input)); err != nil {
					t.Fatal(err)
				}
			}
			observation := waitForObservation(t, shell.anchor, test.check)
			if !test.check(t, observation) {
				t.Fatalf("unexpected observation: status=%s pgid=%d candidate=%#v processes=%#v issues=%v",
					observation.Status, observation.ForegroundPGID, observation.Candidate, observation.Processes, observation.Issues)
			}
		})
	}
}

func TestProcObserverSharesSnapshotPairAcrossAnchors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs batching is Linux-specific")
	}
	shell := startTestShell(t)
	second := shell.anchor
	second.Key.PaneID++
	scans := 0
	observations, unstable := observeProcBatchAttempt(context.Background(), []Anchor{shell.anchor, second}, func(ctx context.Context) ([]procStat, error) {
		scans++
		return scanProcTable(ctx)
	})
	if len(unstable) != 0 {
		t.Fatalf("stable idle shell reported unstable anchors: %#v", unstable)
	}
	if scans != 2 {
		t.Fatalf("process table scans = %d, want one before/after pair", scans)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestPSObserverPublishesStableForegroundProcessGroup(t *testing.T) {
	shell := startTestShell(t)
	anchor := shell.anchor
	identity, err := identifyPS(shell.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	anchor.Root = identity
	observations := observePS(context.Background(), []Anchor{anchor})
	observation := observations[anchor.Key]
	want, err := foregroundProcessGroup(anchor.PTY)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ForegroundPGID != want {
		t.Fatalf("foreground pgid = %d, want %d; observation=%#v", observation.ForegroundPGID, want, observation)
	}
}

type testShell struct {
	ptmx   *os.File
	cmd    *exec.Cmd
	anchor Anchor
}

func startTestShell(t *testing.T) *testShell {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required for PTY process observation tests")
	}
	cmd := exec.Command(bash, "--noprofile", "--norc")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: 80, Rows: 24}, cmd.SysProcAttr)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identifyProcess(cmd.Process.Pid)
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	shell := &testShell{
		ptmx: ptmx,
		cmd:  cmd,
		anchor: Anchor{
			Key:         PaneKey{PaneID: 1},
			Root:        identity,
			PTY:         ptmx,
			RootIsShell: true,
		},
	}
	t.Cleanup(func() {
		_ = shell.ptmx.Close()
		_ = shell.cmd.Process.Signal(syscall.SIGHUP)
		done := make(chan struct{})
		go func() {
			_ = shell.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = shell.cmd.Process.Kill()
			<-done
		}
	})
	return shell
}

func waitForObservation(t *testing.T, anchor Anchor, accept func(*testing.T, ProcessObservation) bool) ProcessObservation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last ProcessObservation
	for time.Now().Before(deadline) {
		observations := NewProcessObserver().Observe(context.Background(), []Anchor{anchor})
		last = observations[anchor.Key]
		if accept(t, last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

func commandBase(process *ObservedProcess) string {
	if process == nil {
		return ""
	}
	if len(process.Argv) > 0 {
		return filepath.Base(process.Argv[0])
	}
	if process.Exe != "" {
		return filepath.Base(strings.TrimSuffix(process.Exe, " (deleted)"))
	}
	return ""
}

func TestParsePSProcess(t *testing.T) {
	process, err := parsePSProcess("123 7 123 ttys001 S+ Thu Jul 16 10:41:22 2026 /opt/homebrew/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	if process.Identity.PID != 123 || process.Identity.BirthToken == 0 {
		t.Fatalf("identity = %+v", process.Identity)
	}
	if process.PPID != 7 || process.PGID != 123 || process.Name != "bash" || process.State != 'S' {
		t.Fatalf("process = %+v", process)
	}
}

func TestDirectPSChildrenMatchesNumericParentID(t *testing.T) {
	processes := []ObservedProcess{
		{Identity: Identity{PID: 10, BirthToken: 1}, PPID: 7, Name: "vim"},
		{Identity: Identity{PID: 11, BirthToken: 2}, PPID: 70, Name: "sleep"},
	}
	children := directPSChildren(processes, 7)
	if len(children) != 1 || children[0].Name != "vim" {
		t.Fatalf("children = %+v", children)
	}
}
