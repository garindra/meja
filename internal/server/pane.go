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
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"

	"tali/internal/server/terminal"
)

type Pane struct {
	ID      uint64
	PTY     *os.File
	Process *exec.Cmd
	User    *user.User
	Title   string

	terminal   *terminal.TerminalState
	metadata   atomic.Pointer[paneTerminalMetadata]
	ptyOutput  chan []byte
	ptyInput   chan []byte
	commands   chan paneCommand
	mainDone   chan struct{}
	writerDone chan struct{}
	done       chan struct{}
	stopping   atomic.Bool

	// Owned exclusively by the pane main goroutine. Production attaches
	// the actual QUIC stream; tests can provide the narrower write capability.
	outputStream io.Writer
}

type paneCommand struct {
	attach  io.Writer
	detach  io.Writer
	release *paneOutputRelease
	refresh func(*renderOutput) error
	apply   func(*renderOutput) error
	resize  *paneResize
	history chan<- *HistorySnapshot
	done    chan error
}

type paneResize struct {
	cols uint16
	rows uint16
}

type paneTerminalMetadata struct {
	cols                  int
	rows                  int
	applicationCursorKeys bool
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
	if metadata := p.metadata.Load(); metadata != nil {
		return metadata.cols, metadata.rows
	}
	if p.terminal != nil {
		return p.terminal.Cols, p.terminal.Rows
	}
	return 0, 0
}

func (p *Pane) UsesApplicationCursorKeys() bool {
	metadata := p.metadata.Load()
	return metadata != nil && metadata.applicationCursorKeys
}

func (p *Pane) initializeRuntime() {
	if p.commands != nil {
		return
	}
	p.ptyOutput = make(chan []byte, 16)
	p.ptyInput = make(chan []byte, 64)
	p.commands = make(chan paneCommand, 8)
	p.mainDone = make(chan struct{})
	p.writerDone = make(chan struct{})
	p.done = make(chan struct{})
	p.publishTerminalMetadata()
}

func (p *Pane) publishTerminalMetadata() {
	if p.terminal == nil {
		return
	}
	next := paneTerminalMetadata{
		cols:                  p.terminal.Cols,
		rows:                  p.terminal.Rows,
		applicationCursorKeys: p.terminal.ApplicationCursorKeys,
	}
	if current := p.metadata.Load(); current != nil && *current == next {
		return
	}
	p.metadata.Store(&next)
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
		terminal: terminal.New(int(request.Cols), int(request.Rows)),
		Title:    paneTitle(shell, request.Command),
	}, nil
}

func (p *Pane) resize(cols, rows uint16) error {
	if p.commands == nil {
		var err error
		if p.PTY != nil {
			err = pty.Setsize(p.PTY, &pty.Winsize{Cols: cols, Rows: rows})
		}
		p.terminal.Resize(int(cols), int(rows))
		p.publishTerminalMetadata()
		return err
	}
	return p.sendRenderCommand(paneCommand{resize: &paneResize{cols: cols, rows: rows}})
}

func (p *Pane) sendInput(data []byte) error {
	return p.sendOwnedInput(append([]byte(nil), data...))
}

func (p *Pane) sendOwnedInput(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if p.ptyInput == nil {
		return writeAll(p.PTY, data)
	}
	select {
	case p.ptyInput <- data:
		return nil
	case <-p.writerDone:
		return io.ErrClosedPipe
	case <-p.done:
		return io.ErrClosedPipe
	}
}

func (p *Pane) stop() {
	if !p.stopping.CompareAndSwap(false, true) {
		return
	}
	if p.done != nil {
		close(p.done)
	}
	if p.PTY != nil {
		_ = p.PTY.Close()
	}
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
