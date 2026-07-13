package server

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"

	"tali/internal/server/terminal"
)

type Pane struct {
	ID       uint64
	PTY      *os.File
	Process  *exec.Cmd
	User     *user.User
	Terminal *terminal.TerminalState
	Title    string

	writeMu        sync.Mutex
	terminalMu     sync.Mutex
	renderCommands chan paneRenderCommand
	rendererDone   chan struct{}

	// Owned exclusively by the pane renderer goroutine. Production attaches
	// the actual QUIC stream; tests can provide the narrower write capability.
	outputStream io.Writer
}

type paneRenderCommand struct {
	attach  io.Writer
	detach  io.Writer
	release *paneOutputRelease
	refresh func(*renderOutput) error
	apply   func(*renderOutput) error
	done    chan error
}

type paneOutputRelease struct {
	slot  int
	done  chan<- int
	acked chan struct{}
	once  sync.Once
}

func (r *paneOutputRelease) acknowledge() {
	r.once.Do(func() {
		r.done <- r.slot
		close(r.acked)
	})
}

func (p *Pane) TerminalSize() (int, int) {
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	return p.Terminal.Cols, p.Terminal.Rows
}

func (p *Pane) UsesApplicationCursorKeys() bool {
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	return p.Terminal.ApplicationCursorKeys
}

func StartPane(paneID uint64, request paneRequest) (*Pane, error) {
	unixUser, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve daemon user: %w", err)
	}
	shell := request.Shell
	if shell == "" {
		shell = loginShellForUser(unixUser)
	}

	cmdPath, argv := resolveCommand(shell, request.Command)
	cmd := exec.Command(cmdPath, argv...)
	cmd.Dir = request.Cwd
	if cmd.Dir == "" {
		cmd.Dir = unixUser.HomeDir
	}

	cmd.Env = buildEnv(unixUser, shell)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}

	ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{
		Cols: request.Cols,
		Rows: request.Rows,
	}, cmd.SysProcAttr)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}

	return &Pane{
		ID:       paneID,
		PTY:      ptmx,
		Process:  cmd,
		User:     unixUser,
		Terminal: terminal.New(int(request.Cols), int(request.Rows)),
		Title:    paneTitle(shell, request.Command),
	}, nil
}

func (p *Pane) Resize(cols, rows uint16) error {
	return pty.Setsize(p.PTY, &pty.Winsize{Cols: cols, Rows: rows})
}

func (p *Pane) WriteInput(data []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.PTY.Write(data)
}

func resolveCommand(shell string, argv []string) (string, []string) {
	if len(argv) > 0 {
		return argv[0], argv[1:]
	}
	return shell, []string{"-l"}
}

func buildEnv(unixUser *user.User, shell string) []string {
	return []string{
		"HOME=" + unixUser.HomeDir,
		"USER=" + unixUser.Username,
		"LOGNAME=" + unixUser.Username,
		"SHELL=" + shell,
		"TERM=xterm-256color",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

func loginShellForUser(unixUser *user.User) string {
	if unixUser == nil || unixUser.Username == "" {
		return "/bin/sh"
	}

	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "/bin/sh"
	}

	prefix := unixUser.Username + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) >= 7 && filepath.IsAbs(fields[6]) {
			return fields[6]
		}
		break
	}

	return "/bin/sh"
}

func paneTitle(shell string, argv []string) string {
	if len(argv) > 0 && argv[0] != "" {
		return filepath.Base(argv[0])
	}
	if shell != "" {
		return filepath.Base(shell)
	}
	return "shell"
}
