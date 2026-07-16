package server

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

func serveCommandSocket(ctx context.Context, socket string, daemon *Daemon) error {
	if err := ensureCommandSocketDir(socket); err != nil {
		return err
	}
	if err := removeStaleCommandSocket(socket); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen command socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0o600); err != nil {
		return fmt.Errorf("protect command socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go serveCommandConnection(conn, daemon)
	}
}

func defaultCommandSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".meja", "default", "meja.sock"), nil
}

func ensureCommandSocketDir(socket string) error {
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create command directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUID(info) {
		return fmt.Errorf("command directory %q must be owned by the current user with mode 0700", dir)
	}
	return nil
}

func removeStaleCommandSocket(socket string) error {
	info, err := os.Lstat(socket)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUID(info) {
		return errors.New("command path is not a socket owned by the current user")
	}
	conn, dialErr := net.DialTimeout("unix", socket, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("command socket %s is already accepting connections", socket)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !os.IsNotExist(dialErr) {
		return dialErr
	}
	return os.Remove(socket)
}

func ownedByCurrentUID(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint32(os.Getuid()) == stat.Uid
}

type commandServerLock struct {
	file *os.File
}

func acquireCommandServerLock(socket string) (*commandServerLock, error) {
	if err := ensureCommandSocketDir(socket); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket+".lock", 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("meja server already running for socket %s", socket)
	}
	return &commandServerLock{file: file}, nil
}

func (lock *commandServerLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	err := lock.file.Close()
	lock.file = nil
	return err
}

func serveCommandConnection(conn net.Conn, daemon *Daemon) {
	defer conn.Close()
	request, err := protocol.ReadCommandRequest(conn)
	if err != nil {
		_ = protocol.WriteCommandOutput(conn, protocol.CommandFrameStderr, []byte("invalid command request\n"))
		_ = protocol.WriteCommandFrame(conn, protocol.CommandFrame{Type: protocol.CommandFrameExit, ExitCode: 1})
		return
	}
	result := daemon.executeCommand(request)
	if err := protocol.WriteCommandOutput(conn, protocol.CommandFrameStdout, result.stdout); err != nil {
		return
	}
	if err := protocol.WriteCommandOutput(conn, protocol.CommandFrameStderr, result.stderr); err != nil {
		return
	}
	if result.bootstrap != nil {
		if err := protocol.WriteCommandFrame(conn, protocol.CommandFrame{Type: protocol.CommandFrameAttach, Bootstrap: result.bootstrap}); err != nil {
			return
		}
	}
	if err := protocol.WriteCommandFrame(conn, protocol.CommandFrame{Type: protocol.CommandFrameExit, ExitCode: result.exitCode}); err != nil {
		return
	}
	if result.stopServer {
		go func() {
			daemon.disconnectActiveClients()
			if daemon.stop != nil {
				daemon.stop()
			}
		}()
	}
}

var errSessionUnavailable = errors.New("meja session unavailable")

type commandSessionTarget struct {
	id   uint64
	name string
}

type commandSessionInfo struct {
	id       uint64
	name     string
	attached bool
}

type commandResult struct {
	stdout     []byte
	stderr     []byte
	bootstrap  *protocol.CommandBootstrap
	exitCode   int
	stopServer bool
}

func (d *Daemon) executeCommand(request protocol.CommandRequest) commandResult {
	result, err := d.executeCommandNow(request)
	if err == nil {
		return result
	}
	return commandResult{stderr: []byte(err.Error() + "\n"), exitCode: 1}
}

func (d *Daemon) executeCommandNow(request protocol.CommandRequest) (commandResult, error) {
	if len(request.Args) == 0 {
		return commandResult{}, errors.New("missing command")
	}
	command, ok := resolveRegisteredCommand(request.Args[0])
	if !ok {
		return commandResult{}, fmt.Errorf("unknown command %q", request.Args[0])
	}
	execution, err := command.execute(&commandContext{daemon: d, request: request}, request.Args[1:])
	return execution.result, err
}

type sessionCommandHandler func(*Session, *Connection, []string) (bool, error)
type commandHandler func(*commandContext, []string) (commandExecution, error)

type commandContext struct {
	daemon     *Daemon
	session    *Session
	connection *Connection
	request    protocol.CommandRequest
}

type commandExecution struct {
	result commandResult
	detach bool
}

type commandDefinition struct {
	name    string
	aliases []string
	execute commandHandler
}

func registeredCommands() []commandDefinition {
	return []commandDefinition{
		{name: "new-session", aliases: []string{"new"}, execute: daemonCommand(handleDaemonNewSessionCommand)},
		{name: "attach-session", aliases: []string{"attach", "a"}, execute: daemonCommand(handleDaemonAttachSessionCommand)},
		{name: "restore-session", aliases: []string{"restore"}, execute: daemonCommand(handleDaemonRestoreSessionCommand)},
		{name: "list-sessions", aliases: []string{"ls"}, execute: daemonCommand(handleDaemonListSessionsCommand)},
		{name: "kill-server", execute: daemonCommand(handleDaemonKillServerCommand)},
		{name: "server", execute: daemonCommand(handleLegacyDaemonServerCommand)},
		{name: "new-window", aliases: []string{"neww"}, execute: sessionCommand(sessionTarget, handleNewWindowCommand)},
		{name: "split-window", aliases: []string{"splitw"}, execute: sessionCommand(sessionTarget, handleSplitWindowCommand)},
		{name: "detach-client", aliases: []string{"detach"}, execute: sessionCommand(sessionTarget, handleDetachClientCommand)},
		{name: "next-window", aliases: []string{"next"}, execute: sessionCommand(sessionTarget, handleNextWindowCommand)},
		{name: "previous-window", aliases: []string{"prev"}, execute: sessionCommand(sessionTarget, handlePreviousWindowCommand)},
		{name: "last-window", aliases: []string{"last"}, execute: sessionCommand(sessionTarget, handleLastWindowCommand)},
		{name: "select-window", aliases: []string{"selectw"}, execute: sessionCommand(windowTarget, handleSelectWindowCommand)},
		{name: "kill-pane", aliases: []string{"killp"}, execute: sessionCommand(sessionTarget, handleKillPaneCommand)},
		{name: "copy-mode", execute: sessionCommand(sessionTarget, handleCopyModeCommand)},
		{name: "swap-pane", aliases: []string{"swapp"}, execute: sessionCommand(sessionTarget, handleSwapPaneCommand)},
		{name: "select-pane", aliases: []string{"selectp"}, execute: sessionCommand(sessionTarget, handleSelectPaneCommand)},
		{name: "resize-pane", aliases: []string{"resizep"}, execute: sessionCommand(sessionTarget, handleResizePaneCommand)},
		{name: "rename-window", aliases: []string{"renamew"}, execute: sessionCommand(windowTarget, handleRenameWindowCommand)},
		{name: "rename-session", aliases: []string{"renames"}, execute: sessionCommand(sessionTarget, handleRenameSessionCommand)},
		{name: "confirm-before", execute: attachedCommand(handleConfirmBeforeCommand)},
		{name: "command-prompt", execute: attachedCommand(handleCommandPromptCommand)},
	}
}

func resolveRegisteredCommand(name string) (commandDefinition, bool) {
	for _, command := range registeredCommands() {
		if name == command.name {
			return command, true
		}
		for _, alias := range command.aliases {
			if name == alias {
				return command, true
			}
		}
	}
	return commandDefinition{}, false
}

// executeSessionCommand is the single execution path for attached commands.
// Prefix bindings and the status command prompt both submit argv here.
func (s *Session) executeSessionCommand(c *Connection, argv []string) (bool, error) {
	if len(argv) == 0 {
		return false, errors.New("missing command")
	}
	command, ok := resolveRegisteredCommand(argv[0])
	if !ok {
		return false, fmt.Errorf("unknown command %q", argv[0])
	}
	s.setStatusMessage(clientID0, "")
	execution, err := command.execute(&commandContext{daemon: c.Daemon, session: s, connection: c}, argv[1:])
	return execution.detach, err
}

type commandTargetKind uint8

const (
	sessionTarget commandTargetKind = iota
	windowTarget
)

type daemonCommandFunc func(*Daemon, protocol.CommandRequest, []string) (commandResult, error)

func daemonCommand(run daemonCommandFunc) commandHandler {
	return func(ctx *commandContext, args []string) (commandExecution, error) {
		if ctx.daemon == nil || ctx.session != nil {
			return commandExecution{}, errors.New("command is only available through the daemon CLI")
		}
		result, err := run(ctx.daemon, ctx.request, args)
		return commandExecution{result: result}, err
	}
}

func attachedCommand(run sessionCommandHandler) commandHandler {
	return func(ctx *commandContext, args []string) (commandExecution, error) {
		if ctx.session == nil || ctx.connection == nil {
			return commandExecution{}, errors.New("command requires an attached client")
		}
		detach, err := run(ctx.session, ctx.connection, args)
		return commandExecution{detach: detach}, err
	}
}

func sessionCommand(kind commandTargetKind, run sessionCommandHandler) commandHandler {
	return func(ctx *commandContext, args []string) (commandExecution, error) {
		session, connection, normalized, err := resolveSessionCommandContext(ctx, kind, args)
		if err != nil {
			return commandExecution{}, err
		}
		if ctx.session != session {
			connection = nil
		}
		var detach bool
		execute := func() error {
			if connection == nil {
				connection = session.connection
				if connection == nil {
					connection = &Connection{Session: session, Daemon: ctx.daemon, shell: defaultShell()}
				}
			}
			var executeErr error
			detach, executeErr = run(session, connection, normalized)
			return executeErr
		}
		if ctx.session == session {
			err = execute()
		} else {
			err = session.coordinate(execute)
		}
		if err != nil {
			return commandExecution{}, err
		}
		if ctx.session != session && detach {
			if connection != nil && connection.QUIC != nil {
				_ = connection.QUIC.CloseWithError(0, "detached by command")
			}
			detach = false
		}
		return commandExecution{detach: detach}, nil
	}
}

func resolveSessionCommandContext(ctx *commandContext, kind commandTargetKind, args []string) (*Session, *Connection, []string, error) {
	rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
	if err != nil {
		return nil, nil, nil, err
	}
	if ctx.session == nil && !hasTarget {
		return nil, nil, nil, errors.New("command requires -t <session-target>")
	}

	sessionTargetValue := ""
	normalized := remaining
	if hasTarget {
		switch kind {
		case windowTarget:
			separator := strings.IndexByte(rawTarget, ':')
			if separator >= 0 {
				sessionTargetValue = rawTarget[:separator]
				window := rawTarget[separator+1:]
				if window == "" {
					return nil, nil, nil, errors.New("window target must include an index")
				}
				normalized = append([]string{"-t", ":" + window}, remaining...)
			} else {
				if ctx.session == nil {
					return nil, nil, nil, errors.New("CLI window targets must be session:window")
				}
				normalized = append([]string{"-t", rawTarget}, remaining...)
			}
		case sessionTarget:
			sessionTargetValue = rawTarget
		}
	}

	targetSession := ctx.session
	if sessionTargetValue != "" {
		targetSession, err = resolveCommandSession(ctx, sessionTargetValue)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if targetSession == nil {
		return nil, nil, nil, errors.New("session target is required")
	}
	return targetSession, ctx.connection, normalized, nil
}

func extractCommandTarget(args []string) (string, []string, bool, error) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}
		if arg == "-t" {
			if index+1 >= len(args) || args[index+1] == "" {
				return "", nil, false, errors.New("-t requires a target")
			}
			target := args[index+1]
			remaining := make([]string, 0, len(args)-2)
			remaining = append(remaining, args[:index]...)
			remaining = append(remaining, args[index+2:]...)
			return target, remaining, true, nil
		}
		if strings.HasPrefix(arg, "-t=") {
			target := strings.TrimPrefix(arg, "-t=")
			if target == "" {
				return "", nil, false, errors.New("-t requires a target")
			}
			remaining := make([]string, 0, len(args)-1)
			remaining = append(remaining, args[:index]...)
			remaining = append(remaining, args[index+1:]...)
			return target, remaining, true, nil
		}
	}
	return "", append([]string(nil), args...), false, nil
}

func resolveCommandSession(ctx *commandContext, raw string) (*Session, error) {
	target, err := parseSessionTarget(raw)
	if err != nil {
		return nil, err
	}
	if ctx.session != nil && ((target.id != 0 && ctx.session.ID == target.id) || (target.name != "" && ctx.session.SessionName() == target.name)) {
		return ctx.session, nil
	}
	if ctx.daemon == nil {
		return nil, fmt.Errorf("unknown session %q", raw)
	}
	var session *Session
	ctx.daemon.call(func() {
		if target.id != 0 {
			session = ctx.daemon.sessions[target.id]
		} else {
			session = ctx.daemon.sessionByName(target.name)
		}
	})
	if session == nil {
		return nil, fmt.Errorf("unknown session %q", raw)
	}
	return session, nil
}

func handleDaemonNewSessionCommand(d *Daemon, request protocol.CommandRequest, args []string) (commandResult, error) {
	return d.commandNewSession(request, args)
}

func handleDaemonAttachSessionCommand(d *Daemon, _ protocol.CommandRequest, args []string) (commandResult, error) {
	return d.commandAttachSession(args)
}

func handleDaemonRestoreSessionCommand(d *Daemon, _ protocol.CommandRequest, args []string) (commandResult, error) {
	return d.commandRestoreSession(args)
}

func handleDaemonListSessionsCommand(d *Daemon, _ protocol.CommandRequest, args []string) (commandResult, error) {
	return d.commandListSessions(args)
}

func handleDaemonKillServerCommand(_ *Daemon, _ protocol.CommandRequest, args []string) (commandResult, error) {
	if err := requireNoCommandArgs("kill-server", args); err != nil {
		return commandResult{}, err
	}
	return commandResult{stdout: []byte(fmt.Sprintf("stopped server PID %d\n", os.Getpid())), stopServer: true}, nil
}

func handleLegacyDaemonServerCommand(_ *Daemon, _ protocol.CommandRequest, args []string) (commandResult, error) {
	if len(args) == 1 && args[0] == "stop" {
		return commandResult{stdout: []byte(fmt.Sprintf("stopped server PID %d\n", os.Getpid())), stopServer: true}, nil
	}
	return commandResult{}, errors.New("server accepts only the legacy 'stop' alias")
}

func requireNoCommandArgs(name string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s accepts no arguments", name)
	}
	return nil
}

func handleNewWindowCommand(s *Session, c *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("new-window", args); err != nil {
		return false, err
	}
	return false, s.commandCreateWindow(c)
}

func handleSplitWindowCommand(s *Session, c *Connection, args []string) (bool, error) {
	fs := commandFlagSet("split-window")
	horizontal := fs.Bool("h", false, "split left/right")
	vertical := fs.Bool("v", false, "split top/bottom")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if len(fs.Args()) != 0 || (*horizontal && *vertical) {
		return false, errors.New("split-window accepts one of -h or -v")
	}
	direction := SplitHorizontal
	if *horizontal {
		direction = SplitVertical
	}
	return false, s.commandSplit(c, direction)
}

func handleDetachClientCommand(_ *Session, _ *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("detach-client", args); err != nil {
		return false, err
	}
	return true, nil
}

func handleNextWindowCommand(s *Session, _ *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("next-window", args); err != nil {
		return false, err
	}
	if id, ok := s.RelativeWindowID(clientID0, 1); ok {
		return false, s.commandSelectWindow(id)
	}
	return false, nil
}

func handlePreviousWindowCommand(s *Session, _ *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("previous-window", args); err != nil {
		return false, err
	}
	if id, ok := s.RelativeWindowID(clientID0, -1); ok {
		return false, s.commandSelectWindow(id)
	}
	return false, nil
}

func handleLastWindowCommand(s *Session, _ *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("last-window", args); err != nil {
		return false, err
	}
	if id, ok := s.LastWindowID(clientID0); ok {
		return false, s.commandSelectWindow(id)
	}
	return false, nil
}

func handleSelectWindowCommand(s *Session, _ *Connection, args []string) (bool, error) {
	fs := commandFlagSet("select-window")
	target := fs.String("t", "", "window target")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if *target == "" || len(fs.Args()) != 0 {
		return false, errors.New("select-window requires -t <window-index>")
	}
	index, err := strconv.Atoi(strings.TrimPrefix(*target, ":"))
	if err != nil || index < 0 {
		return false, fmt.Errorf("invalid window target %q", *target)
	}
	id, ok := s.WindowIDByIndex(index)
	if !ok {
		return false, fmt.Errorf("unknown window %d", index)
	}
	return false, s.commandSelectWindow(id)
}

func handleKillPaneCommand(s *Session, _ *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("kill-pane", args); err != nil {
		return false, err
	}
	return false, s.commandClosePaneNow()
}

func handleConfirmBeforeCommand(s *Session, c *Connection, args []string) (bool, error) {
	if len(args) == 0 {
		return false, errors.New("confirm-before requires a command")
	}
	label := args[0] + "? (y/N) "
	_, err := s.beginConfirmationPrompt(clientID0, label, func(result promptResult) error {
		if !result.Accepted {
			return s.publishStatusBar()
		}
		_, executeErr := s.executeSessionCommand(c, append([]string(nil), args...))
		return executeErr
	})
	if err != nil {
		return false, err
	}
	return false, s.publishStatusBar()
}

func handleCopyModeCommand(s *Session, _ *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("copy-mode", args); err != nil {
		return false, err
	}
	return false, s.commandEnterHistory()
}

func directionalCommandFlagSet(name string, args []string) (byte, []string, error) {
	fs := commandFlagSet(name)
	up := fs.Bool("U", false, "up")
	down := fs.Bool("D", false, "down")
	left := fs.Bool("L", false, "left")
	right := fs.Bool("R", false, "right")
	if err := fs.Parse(args); err != nil {
		return 0, nil, err
	}
	direction := byte(0)
	flags := []struct {
		enabled bool
		value   byte
	}{{*up, 'A'}, {*down, 'B'}, {*right, 'C'}, {*left, 'D'}}
	for _, flag := range flags {
		if !flag.enabled {
			continue
		}
		if direction != 0 {
			return 0, nil, fmt.Errorf("%s requires exactly one direction", name)
		}
		direction = flag.value
	}
	if direction == 0 {
		return 0, nil, fmt.Errorf("%s requires one of -U, -D, -L, or -R", name)
	}
	return direction, fs.Args(), nil
}

func handleSwapPaneCommand(s *Session, _ *Connection, args []string) (bool, error) {
	direction, rest, err := directionalCommandFlagSet("swap-pane", args)
	if err != nil {
		return false, err
	}
	if len(rest) != 0 || (direction != 'A' && direction != 'B') {
		return false, errors.New("swap-pane requires exactly one of -U or -D")
	}
	swap := SwapPanePrevious
	if direction == 'B' {
		swap = SwapPaneNext
	}
	return false, s.commandSwapPane(swap)
}

func handleSelectPaneCommand(s *Session, _ *Connection, args []string) (bool, error) {
	direction, rest, err := directionalCommandFlagSet("select-pane", args)
	if err != nil {
		return false, err
	}
	if len(rest) != 0 {
		return false, errors.New("select-pane accepts no positional arguments")
	}
	return false, s.commandFocusPaneDirection(direction)
}

func handleResizePaneCommand(s *Session, _ *Connection, args []string) (bool, error) {
	fs := commandFlagSet("resize-pane")
	up := fs.Bool("U", false, "up")
	down := fs.Bool("D", false, "down")
	left := fs.Bool("L", false, "left")
	right := fs.Bool("R", false, "right")
	zoom := fs.Bool("Z", false, "toggle zoom")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if *zoom {
		if *up || *down || *left || *right || len(fs.Args()) != 0 {
			return false, errors.New("resize-pane -Z cannot be combined with a direction or amount")
		}
		return false, s.commandToggleZoom()
	}
	direction := PaneResizeDirection(0)
	count := 0
	for _, candidate := range []struct {
		enabled bool
		value   PaneResizeDirection
	}{{*up, ResizePaneUp}, {*down, ResizePaneDown}, {*left, ResizePaneLeft}, {*right, ResizePaneRight}} {
		if candidate.enabled {
			direction = candidate.value
			count++
		}
	}
	if count != 1 || len(fs.Args()) > 1 {
		return false, errors.New("resize-pane requires exactly one of -U, -D, -L, or -R and an optional amount")
	}
	amount := 1
	if len(fs.Args()) == 1 {
		parsed, err := strconv.Atoi(fs.Args()[0])
		if err != nil || parsed <= 0 {
			return false, errors.New("resize-pane amount must be a positive integer")
		}
		amount = parsed
	}
	return false, s.commandResizePane(direction, amount)
}

func handleRenameWindowCommand(s *Session, _ *Connection, args []string) (bool, error) {
	fs := commandFlagSet("rename-window")
	target := fs.String("t", "", "window target")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if len(fs.Args()) == 0 && *target == "" {
		return false, s.commandBeginRenameWindowPrompt()
	}
	if len(fs.Args()) != 1 {
		return false, errors.New("rename-window requires one name")
	}
	client := s.SnapshotClient(clientID0)
	if client == nil {
		return false, nil
	}
	windowID := client.ActiveWindowID
	if *target != "" {
		index, err := strconv.Atoi(strings.TrimPrefix(*target, ":"))
		if err != nil || index < 0 {
			return false, fmt.Errorf("invalid window target %q", *target)
		}
		var ok bool
		windowID, ok = s.WindowIDByIndex(index)
		if !ok {
			return false, fmt.Errorf("unknown window %d", index)
		}
	}
	if _, err := s.RenameWindow(windowID, fs.Args()[0]); err != nil {
		return false, err
	}
	return false, s.publishStatusBar()
}

func handleRenameSessionCommand(s *Session, c *Connection, args []string) (bool, error) {
	if len(args) == 0 {
		return false, s.commandBeginRenameSessionPrompt()
	}
	if len(args) != 1 {
		return false, errors.New("rename-session accepts one name")
	}
	if c == nil || c.Daemon == nil {
		return false, s.finishSessionRename(args[0], true)
	}
	c.Daemon.requestSessionRename(s, s.Name, args[0])
	return false, nil
}

func handleCommandPromptCommand(s *Session, _ *Connection, args []string) (bool, error) {
	if err := requireNoCommandArgs("command-prompt", args); err != nil {
		return false, err
	}
	if _, err := s.BeginCommandPrompt(clientID0); err != nil {
		return false, err
	}
	return false, s.publishStatusBar()
}

// parseCommandLine turns the command prompt's text into argv. It intentionally
// performs shell-like quoting only; it never expands variables or executes a
// shell, so prompt input cannot escape the command engine.
func parseCommandLine(line string) ([]string, error) {
	var argv []string
	var word strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if started {
			argv = append(argv, word.String())
			word.Reset()
			started = false
		}
	}
	for _, r := range line {
		if escaped {
			word.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			started = true
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped {
		return nil, errors.New("unfinished escape in command")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command")
	}
	flush()
	return argv, nil
}

func (d *Daemon) commandNewSession(request protocol.CommandRequest, args []string) (commandResult, error) {
	fs := commandFlagSet("new-session")
	name := fs.String("s", "", "session name")
	var cwd string
	fs.StringVar(&cwd, "c", "", "starting directory")
	fs.StringVar(&cwd, "cwd", "", "starting directory")
	if err := fs.Parse(args); err != nil {
		return commandResult{}, err
	}
	if *name != "" {
		if err := validateSessionName(*name); err != nil {
			return commandResult{}, err
		}
	}
	if cwd == "" {
		cwd = request.WorkingDirectory
	}
	cols, rows := request.TerminalCols, request.TerminalRows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 23
	}
	bootstrap, _, err := d.executeSessionOperation("create-session", commandSessionTarget{name: *name})
	if err != nil {
		return commandResult{}, err
	}
	s := d.session(bootstrap.SessionID)
	if s == nil {
		return commandResult{}, errors.New("created session is unavailable")
	}
	if err := s.coordinate(func() error {
		resolved, err := resolveStartingDirectory(cwd)
		if err != nil {
			return err
		}
		s.defaultCwd = resolved
		s.EnsureClient(clientID0)
		s.SetClientSize(clientID0, cols, rows)
		pane, _, _, err := s.createWindow(resolved, fs.Args(), cols, rows, defaultShell())
		if err != nil {
			return err
		}
		s.startPane(pane)
		return nil
	}); err != nil {
		_ = s.shutdown()
		d.sessionExited(s)
		return commandResult{}, err
	}
	return commandResult{bootstrap: &bootstrap}, nil
}

func (d *Daemon) commandAttachSession(args []string) (commandResult, error) {
	fs := commandFlagSet("attach-session")
	target := fs.String("t", "", "session target")
	if err := fs.Parse(args); err != nil {
		return commandResult{}, err
	}
	if *target == "" || len(fs.Args()) != 0 {
		return commandResult{}, errors.New("attach-session requires -t <session-id-or-name>")
	}
	parsed, err := parseSessionTarget(*target)
	if err != nil {
		return commandResult{}, err
	}
	bootstrap, _, err := d.executeSessionOperation("connect-session", parsed)
	if err != nil {
		return commandResult{}, err
	}
	return commandResult{bootstrap: &bootstrap}, nil
}

func (d *Daemon) commandRestoreSession(args []string) (commandResult, error) {
	fs := commandFlagSet("restore-session")
	target := fs.String("t", "", "snapshot session name")
	mode := fs.String("commands", "prepare", "restore command mode")
	if err := fs.Parse(args); err != nil {
		return commandResult{}, err
	}
	if *target == "" || len(fs.Args()) != 0 {
		return commandResult{}, errors.New("restore-session requires -t <session-name>")
	}
	if *mode != "prepare" && *mode != "skip" && *mode != "run" {
		return commandResult{}, errors.New("--commands must be prepare, skip, or run")
	}
	bootstrap, _, err := d.executeSessionOperation("restore-session-"+*mode, commandSessionTarget{name: *target})
	if err != nil {
		return commandResult{}, err
	}
	return commandResult{bootstrap: &bootstrap}, nil
}

func (d *Daemon) commandListSessions(args []string) (commandResult, error) {
	if len(args) != 0 {
		return commandResult{}, errors.New("list-sessions accepts no arguments")
	}
	_, sessions, err := d.executeSessionOperation("list-sessions", commandSessionTarget{})
	if err != nil {
		return commandResult{}, err
	}
	var output bytes.Buffer
	if _, err := fmt.Fprintln(&output, "Active Sessions"); err != nil {
		return commandResult{}, err
	}
	table := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tNAME\tSTATUS"); err != nil {
		return commandResult{}, err
	}
	for _, session := range sessions {
		name := session.name
		if name == "" {
			name = "<unnamed>"
		}
		status := "detached"
		if session.attached {
			status = "attached"
		}
		if _, err := fmt.Fprintf(table, "%d\t%s\t%s\n", session.id, name, status); err != nil {
			return commandResult{}, err
		}
	}
	if err := table.Flush(); err != nil {
		return commandResult{}, err
	}
	return commandResult{stdout: output.Bytes()}, nil
}

func commandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseCommandSessionID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("session ID must be a positive integer")
	}
	return id, nil
}

func parseSessionTarget(raw string) (commandSessionTarget, error) {
	if raw == "" {
		return commandSessionTarget{}, errors.New("session target must not be empty")
	}
	if isDecimal(raw) {
		id, err := parseCommandSessionID(raw)
		if err != nil {
			return commandSessionTarget{}, err
		}
		return commandSessionTarget{id: id}, nil
	}
	if err := validateSessionName(raw); err != nil {
		return commandSessionTarget{}, err
	}
	return commandSessionTarget{name: raw}, nil
}

func validateSessionName(name string) error {
	if name == "" {
		return errors.New("session name must not be empty")
	}
	if len(name) > 128 || !utf8.ValidString(name) {
		return errors.New("session name must be valid UTF-8 and at most 128 bytes")
	}
	if isDecimal(name) {
		return errors.New("session name must not be entirely numeric")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("session name must not contain control characters")
		}
		if r == '/' || r == '\\' {
			return errors.New("session name must not contain path separators")
		}
	}
	return nil
}

func isDecimal(raw string) bool {
	if raw == "" {
		return false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (d *Daemon) executeSessionOperation(operation string, target commandSessionTarget) (protocol.CommandBootstrap, []commandSessionInfo, error) {
	if operation == "list-sessions" {
		type listedSession struct {
			id    uint64
			state *Session
		}
		var states []listedSession
		d.call(func() {
			states = make([]listedSession, 0, len(d.sessions))
			for id, state := range d.sessions {
				states = append(states, listedSession{id: id, state: state})
			}
		})
		sessions := make([]commandSessionInfo, 0, len(states))
		for _, listed := range states {
			if listed.state != nil {
				name, attached := listed.state.info()
				sessions = append(sessions, commandSessionInfo{id: listed.id, name: name, attached: attached})
			}
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].id < sessions[j].id })
		return protocol.CommandBootstrap{}, sessions, nil
	}

	restoreMode, restoring := restoreModeForOperation(operation)
	var restoreSnapshot PersistedSession
	if restoring {
		if target.id != 0 || target.name == "" {
			return protocol.CommandBootstrap{}, nil, errors.New("restore requires a session name")
		}
		if err := validateSessionName(target.name); err != nil {
			return protocol.CommandBootstrap{}, nil, err
		}
		var err error
		restoreSnapshot, err = readPersistedSession(filepath.Join(d.snapshotDir, target.name+".json"), target.name)
		if err != nil {
			return protocol.CommandBootstrap{}, nil, err
		}
	}

	var session *Session
	var operationErr error
	created := false
	var port uint16
	var encodedToken string
	var expires time.Time
	d.call(func() {
		switch operation {
		case "create-session":
			if target.name != "" {
				if err := validateSessionName(target.name); err != nil {
					operationErr = err
					return
				}
				if d.sessionByName(target.name) != nil {
					operationErr = fmt.Errorf("session %q already exists", target.name)
					return
				}
			}
			if d.nextID == 0 {
				operationErr = errors.New("session ID exhausted")
				return
			}
			session = newSession(d.nextID, target.name)
			session.daemon = d
			session.startAutosave(d.snapshotDir)
			port, encodedToken, expires, operationErr = session.startQUIC(d.serverCtx, d.tlsConfig)
			if operationErr != nil {
				_ = session.shutdown()
				return
			}
			d.sessions[d.nextID] = session
			d.reserveSessionName(session, target.name)
			d.nextID++
			created = true
		case "connect-session":
			if target.name != "" {
				session = d.sessionByName(target.name)
			} else {
				session = d.sessions[target.id]
			}
			if session == nil {
				operationErr = errSessionUnavailable
			}
		case "restore-session-prepare", "restore-session-skip", "restore-session-run":
			if d.sessionByName(target.name) != nil {
				operationErr = fmt.Errorf("session %q already exists; attach to it instead", target.name)
				return
			}
			if d.nextID == 0 {
				operationErr = errors.New("session ID exhausted")
				return
			}
			session = newSession(d.nextID, restoreSnapshot.Name)
			session.daemon = d
			session.startAutosave(d.snapshotDir)
			port, encodedToken, expires, operationErr = session.startQUIC(d.serverCtx, d.tlsConfig)
			if operationErr == nil {
				operationErr = session.restoreSnapshot(restoreSnapshot, restoreMode)
			}
			if operationErr != nil {
				_ = session.shutdown()
				return
			}
			d.sessions[d.nextID] = session
			d.reserveSessionName(session, restoreSnapshot.Name)
			d.nextID++
			created = true
		default:
			operationErr = fmt.Errorf("unsupported session operation %q", operation)
		}
	})
	if operationErr != nil {
		return protocol.CommandBootstrap{}, nil, operationErr
	}
	if !created {
		var err error
		port, encodedToken, expires, err = session.issueBootstrap()
		if err != nil {
			return protocol.CommandBootstrap{}, nil, err
		}
	}
	return protocol.CommandBootstrap{
		Version:        protocol.CommandBootstrapVersion,
		SessionID:      session.ID,
		Port:           port,
		AttachToken:    encodedToken,
		ExpiresAt:      expires,
		CertSPKISHA256: d.certHash,
	}, nil, nil
}
