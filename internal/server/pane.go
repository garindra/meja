package server

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"

	"tali/internal/server/terminal"
)

type Pane struct {
	ID         uint64
	PTY        *os.File
	Process    *exec.Cmd
	User       *user.User
	Terminal   *terminal.TerminalState
	Generation uint64
	Title      string

	writeMu sync.Mutex
}

func StartPane(unixUser *user.User, paneID uint64, request paneRequest) (*Pane, error) {
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

	uid, err := strconv.ParseUint(unixUser.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse uid: %w", err)
	}
	gid, err := strconv.ParseUint(unixUser.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse gid: %w", err)
	}
	groupIDs, err := unixUser.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("lookup supplementary groups: %w", err)
	}
	groups := make([]uint32, 0, len(groupIDs))
	for _, raw := range groupIDs {
		groupID, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse group id %q: %w", raw, err)
		}
		groups = append(groups, uint32(groupID))
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uint32(uid),
			Gid:    uint32(gid),
			Groups: groups,
		},
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
