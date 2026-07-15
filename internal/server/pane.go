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

	"github.com/garindra/meja/internal/server/terminal"
)

// Pane owns a child process, PTY, terminal emulator, and its four enduring
// goroutines: PTY reader, PTY writer, process waiter, and terminal actor.
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

	// Held exclusively by the pane main goroutine. A lease contains the actual
	// QUIC stream and is physically returned before another pane receives it.
	outputLease *OutputLease
}

type paneRequest struct {
	Cwd     string
	Command []string
	Cols    uint16
	Rows    uint16
	Shell   string
}

type paneCommand struct {
	attach  *OutputLease
	live    bool
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
	done  chan<- *OutputLease
	acked chan struct{}
	once  sync.Once
}

func (r *paneOutputRelease) acknowledge() {
	r.once.Do(func() {
		r.done <- nil
		close(r.acked)
	})
}

func (r *paneOutputRelease) returnLease(lease *OutputLease) {
	r.once.Do(func() {
		r.done <- lease
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
		shell = defaultShell()
	}

	cmdPath, argv := resolveCommand(shell, request.Command)
	cmd := exec.Command(cmdPath, argv...)
	cmd.Dir, err = resolveStartingDirectoryForUser(request.Cwd, unixUser)
	if err != nil {
		return nil, err
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

func resolveStartingDirectory(raw string) (string, error) {
	unixUser, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve daemon user: %w", err)
	}
	return resolveStartingDirectoryForUser(raw, unixUser)
}

func resolveStartingDirectoryForUser(raw string, unixUser *user.User) (string, error) {
	path := raw
	if path == "" || path == "~" {
		path = unixUser.HomeDir
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(unixUser.HomeDir, strings.TrimPrefix(path, "~/"))
	} else if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("starting directory %q: only ~ and ~/... home expansion are supported", raw)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("starting directory %q must be absolute or start with ~/", raw)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("starting directory %q: %w", raw, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("starting directory %q is not a directory", raw)
	}
	return path, nil
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
	env := []string{
		"HOME=" + unixUser.HomeDir,
		"USER=" + unixUser.Username,
		"LOGNAME=" + unixUser.Username,
		"SHELL=" + shell,
		"TERM=xterm-256color",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func defaultShell() string {
	if shell := os.Getenv("SHELL"); filepath.IsAbs(shell) {
		return shell
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
