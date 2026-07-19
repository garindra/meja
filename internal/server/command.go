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
	id          uint64
	name        string
	file        string
	newName     string
	restoreMode restoreCommandMode
}

type commandSessionInfo struct {
	id       uint64
	name     string
	attached bool
}

type sessionOperationResult struct {
	bootstrap protocol.CommandBootstrap
	session   *Session
	sessions  []commandSessionInfo
}

type commandResult struct {
	stdout     []byte
	stderr     []byte
	bootstrap  *protocol.CommandBootstrap
	session    *Session
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
	if commandRequestsHelp(request.Args) {
		return commandHelp(request.Args[0:1])
	}
	command, ok := resolveRegisteredCommand(request.Args[0])
	if !ok {
		return commandResult{}, fmt.Errorf("unknown command %q", request.Args[0])
	}
	ctx := &commandContext{daemon: d, request: request}
	var handoff *contextualCommandHandoff
	if request.CallerSessionTarget != "" && command.sessionResult {
		prepared, err := d.prepareContextualCommandHandoff(ctx)
		if err != nil {
			return commandResult{}, err
		}
		handoff = prepared
	}
	execution, err := command.execute(ctx, request.Args[1:])
	if err != nil {
		return commandResult{}, err
	}
	if handoff != nil && execution.result.session != nil {
		if err := d.handoffContextualCommand(handoff, execution.result.session); err != nil {
			return commandResult{}, err
		}
		execution.result.bootstrap = nil
	}
	return execution.result, nil
}

type contextualCommandHandoff struct {
	source     *Session
	instance   *ClientInstance
	cols, rows uint16
}

func (d *Daemon) prepareContextualCommandHandoff(ctx *commandContext) (*contextualCommandHandoff, error) {
	source, err := resolveCommandSession(ctx, ctx.request.CallerSessionTarget)
	if err != nil {
		return nil, err
	}
	var instance *ClientInstance
	var cols, rows uint16
	if err := source.coordinate(func() error {
		instance = source.clientInstance
		if state := source.Clients[clientID0]; state != nil {
			cols, rows = state.TerminalCols, state.TerminalRows
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, errors.New("the invoking Meja session is no longer attached")
	}
	return &contextualCommandHandoff{source: source, instance: instance, cols: cols, rows: rows}, nil
}

func (d *Daemon) handoffContextualCommand(handoff *contextualCommandHandoff, target *Session) error {
	return handoff.instance.requestSessionSwitch(&sessionSwitchRequest{
		rawTarget: strconv.FormatUint(target.ID, 10),
		cols:      handoff.cols,
		rows:      handoff.rows,
		result:    make(chan error, 1),
	})
}

type sessionCommandHandler func(*Session, *ClientInstance, []string) (bool, error)
type commandHandler func(*commandContext, []string) (commandExecution, error)

type commandContext struct {
	daemon  *Daemon
	session *Session
	client  *ClientInstance
	request protocol.CommandRequest
}

type commandExecution struct {
	result commandResult
	detach bool
}

type commandDefinition struct {
	name          string
	aliases       []string
	usage         string
	description   string
	hidden        bool
	sessionResult bool
	execute       commandHandler
}

func registeredCommands() []commandDefinition {
	return []commandDefinition{
		{name: "help", aliases: []string{"--help"}, usage: "help [command]", description: "Show this reference or help for one server command.", execute: daemonCommand(handleDaemonHelpCommand)},
		{name: "new-session", aliases: []string{"new"}, usage: "new-session [-s name] [-r directory] [-- command args...] | new-session -f file [-s name] [--commands=prepare|skip|run]", description: "Create a session, optionally from a .meja file, and attach.", sessionResult: true, execute: daemonCommand(handleDaemonNewSessionCommand)},
		{name: "attach-session", aliases: []string{"attach", "a"}, usage: "attach-session -t session-id-or-name", description: "Attach to an existing session.", sessionResult: true, execute: daemonCommand(handleDaemonAttachSessionCommand)},
		{name: "restore-session", aliases: []string{"restore"}, usage: "restore-session -t name [-s new-name] [--commands=prepare|skip|run]", description: "Restore a named session's automatic snapshot and attach.", sessionResult: true, execute: restoreCommand()},
		{name: "save-session", aliases: []string{"save"}, usage: "save-session -t session-id-or-name -o file [-f]", description: "Save a live session to a .meja file.", execute: daemonCommand(handleDaemonSaveSessionCommand)},
		{name: "list-sessions", aliases: []string{"ls"}, usage: "list-sessions", description: "List active sessions.", execute: daemonCommand(handleDaemonListSessionsCommand)},
		{name: "kill-server", usage: "kill-server", description: "Stop the selected server.", execute: daemonCommand(handleDaemonKillServerCommand)},
		{name: "server", hidden: true, execute: daemonCommand(handleLegacyDaemonServerCommand)},
		{name: "new-window", aliases: []string{"neww"}, usage: "new-window [-t session]", description: "Create a window in a session.", execute: sessionCommand(sessionTarget, handleNewWindowCommand)},
		{name: "next-layout", usage: "next-layout [-t session]", description: "Cycle the active window to its next layout.", execute: sessionCommand(sessionTarget, handleNextLayoutCommand)},
		{name: "split-window", aliases: []string{"splitw"}, usage: "split-window [-t session] [-h | -v]", description: "Split the active pane left/right or top/bottom.", execute: sessionCommand(sessionTarget, handleSplitWindowCommand)},
		{name: "detach-client", aliases: []string{"detach"}, usage: "detach-client [-t session]", description: "Detach the client from a session.", execute: sessionCommand(sessionTarget, handleDetachClientCommand)},
		{name: "next-window", aliases: []string{"next"}, usage: "next-window [-t session]", description: "Select the next window.", execute: sessionCommand(sessionTarget, handleNextWindowCommand)},
		{name: "previous-window", aliases: []string{"prev"}, usage: "previous-window [-t session]", description: "Select the previous window.", execute: sessionCommand(sessionTarget, handlePreviousWindowCommand)},
		{name: "last-window", aliases: []string{"last"}, usage: "last-window [-t session]", description: "Select the last active window.", execute: sessionCommand(sessionTarget, handleLastWindowCommand)},
		{name: "select-window", aliases: []string{"selectw"}, usage: "select-window -t session:window", description: "Select a window by index.", execute: sessionCommand(windowTarget, handleSelectWindowCommand)},
		{name: "kill-pane", aliases: []string{"killp"}, usage: "kill-pane [-t session]", description: "Close the active pane.", execute: sessionCommand(sessionTarget, handleKillPaneCommand)},
		{name: "copy-mode", usage: "copy-mode [-t session]", description: "Open history mode for the active pane.", execute: sessionCommand(sessionTarget, handleCopyModeCommand)},
		{name: "swap-pane", aliases: []string{"swapp"}, usage: "swap-pane [-t session] (-U | -D)", description: "Swap the active pane with its neighbor.", execute: sessionCommand(sessionTarget, handleSwapPaneCommand)},
		{name: "select-pane", aliases: []string{"selectp"}, usage: "select-pane [-t session] (-U | -D | -L | -R)", description: "Select an adjacent pane.", execute: sessionCommand(sessionTarget, handleSelectPaneCommand)},
		{name: "resize-pane", aliases: []string{"resizep"}, usage: "resize-pane [-t session] ((-U | -D | -L | -R) [amount] | -Z)", description: "Resize the active pane or toggle zoom.", execute: sessionCommand(sessionTarget, handleResizePaneCommand)},
		{name: "rename-window", aliases: []string{"renamew"}, usage: "rename-window [-t session:window] [name]", description: "Rename a window, prompting when no name is supplied.", execute: sessionCommand(windowTarget, handleRenameWindowCommand)},
		{name: "rename-session", aliases: []string{"rename", "renames"}, usage: "rename-session [-t session] [name]", description: "Rename a session, prompting when no name is supplied.", execute: sessionCommand(sessionTarget, handleRenameSessionCommand)},
		{name: "set-root", usage: "set-root [-t session] [directory]", description: "Set the session root directory.", execute: sessionCommand(sessionTarget, handleSetRootCommand)},
		{name: "switch-session", usage: "switch-session -t session-id-or-name", description: "Move the attached client to another session.", execute: attachedCommand(handleSwitchSessionCommand)},
		{name: "confirm-before", usage: "confirm-before command [args...]", description: "Prompt before running an attached command.", execute: attachedCommand(handleConfirmBeforeCommand)},
		{name: "command-prompt", usage: "command-prompt", description: "Open the attached client's command prompt.", execute: attachedCommand(handleCommandPromptCommand)},
	}
}

func commandRequestsHelp(args []string) bool {
	if len(args) < 2 {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			return false
		}
		if arg == "--help" {
			return true
		}
	}
	return false
}

func commandHelp(args []string) (commandResult, error) {
	if len(args) > 1 {
		return commandResult{}, errors.New("help accepts at most one command")
	}
	if len(args) == 1 {
		command, ok := resolveRegisteredCommand(args[0])
		if !ok || command.hidden {
			return commandResult{}, fmt.Errorf("unknown command %q", args[0])
		}
		var output strings.Builder
		fmt.Fprintf(&output, "usage: meja [transport-options] %s\n\n%s\n", command.usage, command.description)
		if len(command.aliases) > 0 {
			fmt.Fprintf(&output, "\naliases: %s\n", strings.Join(command.aliases, ", "))
		}
		return commandResult{stdout: []byte(output.String())}, nil
	}

	var output strings.Builder
	output.WriteString(`usage:
  meja version
  meja [transport-options] [command [command-args...]]

transport options (handled by the client before forwarding):
  -L profile              select a named server socket
  -S socket-path          select an exact server socket
  -h, --host user@host    run the command on an SSH host
  -i identity-file        use an SSH identity file
  --port port             use an SSH port
  --remote-path path      remote meja executable (default: meja)

client commands:
  version                 print the client version
  start-server            run the selected local server in the foreground

server commands:
`)
	table := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
	for _, command := range registeredCommands() {
		if command.hidden {
			continue
		}
		name := command.name
		if len(command.aliases) > 0 {
			name += " (" + strings.Join(command.aliases, ", ") + ")"
		}
		fmt.Fprintf(table, "  %s\t%s\n", name, command.description)
	}
	if err := table.Flush(); err != nil {
		return commandResult{}, err
	}
	output.WriteString("\nRun 'meja help <command>' or 'meja <command> --help' for command usage.\n")
	return commandResult{stdout: []byte(output.String())}, nil
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
func (s *Session) executeSessionCommand(c *ClientInstance, argv []string) (bool, error) {
	if len(argv) == 0 {
		return false, errors.New("missing command")
	}
	command, ok := resolveRegisteredCommand(argv[0])
	if !ok {
		return false, fmt.Errorf("unknown command %q", argv[0])
	}
	execution, err := command.execute(&commandContext{daemon: c.Daemon, session: s, client: c}, argv[1:])
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

// restoreCommand attaches when invoked by the standalone CLI, but hands the
// existing client to the restored session when invoked through `:`.
func restoreCommand() commandHandler {
	return func(ctx *commandContext, args []string) (commandExecution, error) {
		if ctx.daemon == nil {
			return commandExecution{}, errors.New("restore requires a running daemon")
		}
		if ctx.session == nil {
			result, err := ctx.daemon.commandRestoreSession(ctx.request, args)
			return commandExecution{result: result}, err
		}
		if ctx.client == nil {
			return commandExecution{}, errors.New("restore requires an attached client")
		}

		workingDirectory := ctx.session.rootDir
		if pane, _ := ctx.session.ActivePane(clientID0); pane != nil {
			workingDirectory = ctx.session.observedPaneCwd(pane)
		}
		var cols, rows uint16
		if state := ctx.session.SnapshotClient(clientID0); state != nil {
			cols, rows = state.TerminalCols, state.TerminalRows
		}
		result, err := ctx.daemon.commandRestoreSession(protocol.CommandRequest{
			WorkingDirectory: workingDirectory,
			TerminalCols:     cols,
			TerminalRows:     rows,
		}, args)
		if err != nil {
			return commandExecution{}, err
		}
		if result.session == nil {
			return commandExecution{}, errors.New("restored session is unavailable")
		}
		return commandExecution{}, &sessionSwitchRequest{
			rawTarget: strconv.FormatUint(result.session.ID, 10),
			cols:      cols,
			rows:      rows,
		}
	}
}

func attachedCommand(run sessionCommandHandler) commandHandler {
	return func(ctx *commandContext, args []string) (commandExecution, error) {
		if ctx.session == nil || ctx.client == nil {
			return commandExecution{}, errors.New("command requires an attached client")
		}
		detach, err := run(ctx.session, ctx.client, args)
		return commandExecution{detach: detach}, err
	}
}

func sessionCommand(kind commandTargetKind, run sessionCommandHandler) commandHandler {
	return func(ctx *commandContext, args []string) (commandExecution, error) {
		session, client, normalized, err := resolveSessionCommandContext(ctx, kind, args)
		if err != nil {
			return commandExecution{}, err
		}
		if ctx.session != session {
			client = nil
		}
		var detach bool
		execute := func() error {
			if client == nil {
				client = session.clientInstance
				if client == nil {
					client = &ClientInstance{Daemon: ctx.daemon, shell: defaultShell()}
				}
			}
			var executeErr error
			detach, executeErr = run(session, client, normalized)
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
			if client != nil && client.QUIC != nil {
				_ = client.QUIC.CloseWithError(0, "detached by command")
			}
			detach = false
		}
		return commandExecution{detach: detach}, nil
	}
}

func resolveSessionCommandContext(ctx *commandContext, kind commandTargetKind, args []string) (*Session, *ClientInstance, []string, error) {
	rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
	if err != nil {
		return nil, nil, nil, err
	}
	if !hasTarget && ctx.session == nil && ctx.request.CallerSessionTarget != "" {
		rawTarget = ctx.request.CallerSessionTarget
		hasTarget = true
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
	return targetSession, ctx.client, normalized, nil
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

func handleDaemonHelpCommand(_ *Daemon, _ protocol.CommandRequest, args []string) (commandResult, error) {
	return commandHelp(args)
}

func handleDaemonAttachSessionCommand(d *Daemon, _ protocol.CommandRequest, args []string) (commandResult, error) {
	return d.commandAttachSession(args)
}

func handleDaemonRestoreSessionCommand(d *Daemon, request protocol.CommandRequest, args []string) (commandResult, error) {
	return d.commandRestoreSession(request, args)
}

func handleDaemonSaveSessionCommand(d *Daemon, request protocol.CommandRequest, args []string) (commandResult, error) {
	return d.commandSaveSession(request, args)
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

func handleNewWindowCommand(s *Session, c *ClientInstance, args []string) (bool, error) {
	if err := requireNoCommandArgs("new-window", args); err != nil {
		return false, err
	}
	return false, s.commandCreateWindow(c)
}

func handleNextLayoutCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
	if err := requireNoCommandArgs("next-layout", args); err != nil {
		return false, err
	}
	return false, s.commandNextLayout()
}

func handleSplitWindowCommand(s *Session, c *ClientInstance, args []string) (bool, error) {
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

func handleDetachClientCommand(_ *Session, _ *ClientInstance, args []string) (bool, error) {
	if err := requireNoCommandArgs("detach-client", args); err != nil {
		return false, err
	}
	return true, nil
}

type sessionSwitchRequest struct {
	rawTarget  string
	cols, rows uint16
	result     chan error
}

func (r *sessionSwitchRequest) Error() string { return "switch session" }

func handleSwitchSessionCommand(s *Session, c *ClientInstance, args []string) (bool, error) {
	rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
	if err != nil {
		return false, err
	}
	if !hasTarget {
		return false, errors.New("switch-session requires -t <session-target>")
	}
	if len(remaining) != 0 {
		return false, errors.New("switch-session accepts only -t <session-target>")
	}
	var cols, rows uint16
	if state := s.SnapshotClient(clientID0); state != nil {
		cols, rows = state.TerminalCols, state.TerminalRows
	}
	return false, &sessionSwitchRequest{rawTarget: rawTarget, cols: cols, rows: rows}
}

func handleNextWindowCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
	if err := requireNoCommandArgs("next-window", args); err != nil {
		return false, err
	}
	if id, ok := s.RelativeWindowID(clientID0, 1); ok {
		return false, s.commandSelectWindow(id)
	}
	return false, nil
}

func handlePreviousWindowCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
	if err := requireNoCommandArgs("previous-window", args); err != nil {
		return false, err
	}
	if id, ok := s.RelativeWindowID(clientID0, -1); ok {
		return false, s.commandSelectWindow(id)
	}
	return false, nil
}

func handleLastWindowCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
	if err := requireNoCommandArgs("last-window", args); err != nil {
		return false, err
	}
	if id, ok := s.LastWindowID(clientID0); ok {
		return false, s.commandSelectWindow(id)
	}
	return false, nil
}

func handleSelectWindowCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
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

func handleKillPaneCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
	if err := requireNoCommandArgs("kill-pane", args); err != nil {
		return false, err
	}
	return false, s.commandClosePaneNow()
}

func handleConfirmBeforeCommand(s *Session, c *ClientInstance, args []string) (bool, error) {
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

func handleCopyModeCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
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

func handleSwapPaneCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
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

func handleSelectPaneCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
	direction, rest, err := directionalCommandFlagSet("select-pane", args)
	if err != nil {
		return false, err
	}
	if len(rest) != 0 {
		return false, errors.New("select-pane accepts no positional arguments")
	}
	return false, s.commandFocusPaneDirection(direction)
}

func handleResizePaneCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
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

func handleRenameWindowCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
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

func handleRenameSessionCommand(s *Session, c *ClientInstance, args []string) (bool, error) {
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

func handleSetRootCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
	if len(args) > 1 {
		return false, errors.New("set-root accepts an optional path")
	}
	pane, _ := s.ActivePane(clientID0)
	if pane == nil {
		return false, errors.New("set-root requires an active pane")
	}
	raw := ""
	if len(args) == 1 {
		raw = args[0]
	}
	currentCwd := s.rootDir
	if raw == "" || (!filepath.IsAbs(raw) && raw != "~" && !strings.HasPrefix(raw, "~/")) {
		currentCwd = s.observedPaneCwd(pane)
	}
	if raw == "" {
		raw = currentCwd
	}
	resolved, err := resolveRootDirectory(raw, currentCwd)
	if err != nil {
		return false, err
	}
	s.setRoot(resolved)
	return false, nil
}

func (s *Session) observedPaneCwd(pane *Pane) string {
	if pane == nil {
		return s.rootDir
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	key := PaneKey{SessionID: s.ID, PaneID: pane.ID}
	observations := s.processObserver.Observe(ctx, []Anchor{{
		Key:         key,
		Root:        pane.Root,
		PTY:         pane.PTY,
		RootIsShell: len(pane.Launch.RequestedArgv) == 0,
	}})
	if observation := observations[key]; observation.Root != nil && observation.Root.Cwd != "" {
		return observation.Root.Cwd
	}
	if s.sessionPersistence != nil {
		for _, window := range s.sessionPersistence.Plan.Windows {
			for _, persistedPane := range window.Panes {
				if persistedPane.ID == pane.ID && persistedPane.Cwd != "" {
					return persistedPane.Cwd
				}
			}
		}
	}
	if pane.Launch.Cwd != "" {
		return pane.Launch.Cwd
	}
	return s.rootDir
}

func handleCommandPromptCommand(s *Session, _ *ClientInstance, args []string) (bool, error) {
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
	file := fs.String("f", "", ".meja file")
	mode := fs.String("commands", "prepare", "restore command mode")
	var root string
	fs.StringVar(&root, "r", "", "session root")
	fs.StringVar(&root, "root", "", "session root")
	if err := fs.Parse(args); err != nil {
		return commandResult{}, err
	}
	if *name != "" {
		if err := validateSessionName(*name); err != nil {
			return commandResult{}, err
		}
	}
	if *file != "" {
		if root != "" || len(fs.Args()) != 0 {
			return commandResult{}, errors.New("new-session -f cannot be combined with a root or initial command")
		}
		restoreMode, err := parseRestoreCommandMode(*mode)
		if err != nil {
			return commandResult{}, err
		}
		path, err := resolveCommandFilePath(*file, request.WorkingDirectory)
		if err != nil {
			return commandResult{}, err
		}
		operation, err := d.executeSessionOperation("restore-session", commandSessionTarget{
			file:        path,
			newName:     *name,
			restoreMode: restoreMode,
		})
		if err != nil {
			return commandResult{}, err
		}
		return commandResult{bootstrap: &operation.bootstrap, session: operation.session}, nil
	}
	if *mode != "prepare" {
		return commandResult{}, errors.New("new-session --commands requires -f <file>")
	}
	if root == "" {
		root = request.WorkingDirectory
	}
	cols, rows := request.TerminalCols, request.TerminalRows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 23
	}
	operation, err := d.executeSessionOperation("create-session", commandSessionTarget{name: *name})
	if err != nil {
		return commandResult{}, err
	}
	s := operation.session
	if s == nil {
		return commandResult{}, errors.New("created session is unavailable")
	}
	if err := s.coordinate(func() error {
		resolved, err := resolveRootDirectory(root, request.WorkingDirectory)
		if err != nil {
			return err
		}
		s.rootDir = resolved
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
	return commandResult{bootstrap: &operation.bootstrap, session: operation.session}, nil
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
	operation, err := d.executeSessionOperation("connect-session", parsed)
	if err != nil {
		return commandResult{}, err
	}
	return commandResult{bootstrap: &operation.bootstrap, session: operation.session}, nil
}

func (d *Daemon) commandRestoreSession(_ protocol.CommandRequest, args []string) (commandResult, error) {
	fs := commandFlagSet("restore-session")
	target := fs.String("t", "", "persisted session name")
	name := fs.String("s", "", "new session name")
	mode := fs.String("commands", "prepare", "restore command mode")
	if err := fs.Parse(args); err != nil {
		return commandResult{}, err
	}
	if *target == "" || len(fs.Args()) != 0 {
		return commandResult{}, errors.New("restore-session requires -t <session-name>")
	}
	restoreMode, err := parseRestoreCommandMode(*mode)
	if err != nil {
		return commandResult{}, err
	}
	if *name != "" {
		if err := validateSessionName(*name); err != nil {
			return commandResult{}, err
		}
	}
	operation, err := d.executeSessionOperation("restore-session", commandSessionTarget{
		name:        *target,
		newName:     *name,
		restoreMode: restoreMode,
	})
	if err != nil {
		return commandResult{}, err
	}
	return commandResult{bootstrap: &operation.bootstrap, session: operation.session}, nil
}

func parseRestoreCommandMode(raw string) (restoreCommandMode, error) {
	mode := restoreCommandMode(raw)
	switch mode {
	case restoreCommandsPrepare, restoreCommandsSkip, restoreCommandsRun:
		return mode, nil
	default:
		return "", errors.New("--commands must be prepare, skip, or run")
	}
}

func (d *Daemon) commandSaveSession(request protocol.CommandRequest, args []string) (commandResult, error) {
	fs := commandFlagSet("save-session")
	target := fs.String("t", "", "live session target")
	output := fs.String("o", "", "output .meja file")
	force := fs.Bool("f", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return commandResult{}, err
	}
	if *target == "" || *output == "" || len(fs.Args()) != 0 {
		return commandResult{}, errors.New("save-session requires -t <session-id-or-name> -o <file>")
	}
	parsed, err := parseSessionTarget(*target)
	if err != nil {
		return commandResult{}, err
	}
	var session *Session
	d.call(func() {
		if parsed.name != "" {
			session = d.sessionByName(parsed.name)
		} else {
			session = d.sessions[parsed.id]
		}
	})
	if session == nil {
		return commandResult{}, errSessionUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionPersistenceTimeout)
	defer cancel()
	captured, err := session.captureSession(ctx, session.processObserver)
	if err != nil {
		return commandResult{}, fmt.Errorf("capture session: %w", err)
	}
	persisted, err := sessionPlanFromCapture(captured)
	if err != nil {
		return commandResult{}, err
	}
	path, err := resolveCommandFilePath(*output, captured.SessionRoot)
	if err != nil {
		return commandResult{}, err
	}
	report, err := writeUserMejaFile(path, persisted, *force)
	if err != nil {
		return commandResult{}, err
	}
	var outputText strings.Builder
	fmt.Fprintf(&outputText, "Saved %s.\n", filepath.Base(path))
	if report.AbsolutePanePaths > 0 {
		fmt.Fprintf(&outputText, "\nNote: %d pane", report.AbsolutePanePaths)
		verb := " uses"
		if report.AbsolutePanePaths != 1 {
			outputText.WriteByte('s')
			verb = " use"
		}
		outputText.WriteString(verb + " an absolute path outside the session root.\nThis file may not restore portably on another machine.\n")
	}
	outputText.WriteString("Reminder: scrub sensitive values before sharing or committing it.\n")
	return commandResult{stdout: []byte(outputText.String())}, nil
}

func resolveCommandFilePath(path, workingDirectory string) (string, error) {
	if path == "" {
		return "", errors.New("file path must not be empty")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Clean(filepath.Join(home, strings.TrimPrefix(path, "~/"))), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if workingDirectory == "" {
		var err error
		workingDirectory, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	return filepath.Clean(filepath.Join(workingDirectory, path)), nil
}

func (d *Daemon) commandListSessions(args []string) (commandResult, error) {
	if len(args) != 0 {
		return commandResult{}, errors.New("list-sessions accepts no arguments")
	}
	operation, err := d.executeSessionOperation("list-sessions", commandSessionTarget{})
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
	for _, session := range operation.sessions {
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

func (d *Daemon) executeSessionOperation(operation string, target commandSessionTarget) (sessionOperationResult, error) {
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
		return sessionOperationResult{sessions: sessions}, nil
	}

	restoring := operation == "restore-session"
	var restored SessionPlan
	if restoring {
		if target.restoreMode == "" {
			target.restoreMode = restoreCommandsPrepare
		}
		switch target.restoreMode {
		case restoreCommandsPrepare, restoreCommandsSkip, restoreCommandsRun:
		default:
			return sessionOperationResult{}, fmt.Errorf("invalid restore command mode %q", target.restoreMode)
		}
		var err error
		if target.file != "" {
			restored, err = readUserSessionPlan(target.file)
		} else {
			if target.id != 0 || target.name == "" {
				return sessionOperationResult{}, errors.New("restore requires a session name")
			}
			if err := validateSessionName(target.name); err != nil {
				return sessionOperationResult{}, err
			}
			var persistence SessionPersistence
			persistence, err = readSessionPersistence(filepath.Join(d.sessionPersistenceDir, target.name+".session.meja"), target.name)
			if err == nil {
				restored = persistence.Plan
				restored.Name = persistence.Name
				restored.Root = persistence.Root
			}
		}
		if err != nil {
			return sessionOperationResult{}, err
		}
		if target.newName != "" {
			restored.Name = target.newName
		}
		target.name = restored.Name
	}

	var session *Session
	var operationErr error
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
			session.processMonitor = d.processMonitor
			session.startPersistence(d.sessionPersistenceDir)
			d.sessions[d.nextID] = session
			d.reserveSessionName(session, target.name)
			d.nextID++
		case "connect-session":
			if target.name != "" {
				session = d.sessionByName(target.name)
			} else {
				session = d.sessions[target.id]
			}
			if session == nil {
				operationErr = errSessionUnavailable
			}
		case "restore-session":
			if d.sessionByName(target.name) != nil {
				operationErr = fmt.Errorf("session %q already exists; attach to it instead", target.name)
				return
			}
			if d.nextID == 0 {
				operationErr = errors.New("session ID exhausted")
				return
			}
			session = newSession(d.nextID, restored.Name)
			session.daemon = d
			session.processMonitor = d.processMonitor
			session.startPersistence(d.sessionPersistenceDir)
			operationErr = session.restoreSessionPlan(restored, target.restoreMode)
			if operationErr != nil {
				_ = session.shutdown()
				return
			}
			d.sessions[d.nextID] = session
			d.reserveSessionName(session, restored.Name)
			d.nextID++
		default:
			operationErr = fmt.Errorf("unsupported session operation %q", operation)
		}
	})
	if operationErr != nil {
		return sessionOperationResult{}, operationErr
	}
	port, encodedToken, expires, err := d.issueAttachGrant(session)
	if err != nil {
		return sessionOperationResult{}, err
	}
	return sessionOperationResult{bootstrap: protocol.CommandBootstrap{
		Version:        protocol.CommandBootstrapVersion,
		Port:           port,
		AttachToken:    encodedToken,
		ExpiresAt:      expires,
		CertSPKISHA256: d.certHash,
	}, session: session}, nil
}
