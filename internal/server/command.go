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
	"github.com/garindra/meja/internal/version"
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
	detached    bool
}

type commandSessionInfo struct {
	id       uint64
	name     string
	attached bool
	format   formatSessionSnapshot
}

type commandOperationResult struct {
	bootstrap protocol.CommandBootstrap
	sessionID uint64
	sessions  []commandSessionInfo
}

type commandResult struct {
	stdout     []byte
	stderr     []byte
	bootstrap  *protocol.CommandBootstrap
	sessionID  uint64
	exitCode   int
	stopServer bool
}

// sessionCommandResult is the daemon-model result of creating or resolving a
// session. It is converted either into an attachment bootstrap or into a live
// client view transition; it is not a command-socket response.
type sessionCommandResult struct {
	stdout    []byte
	bootstrap *protocol.CommandBootstrap
	sessionID uint64
}

func (d *Daemon) executeCommand(request protocol.CommandRequest) commandResult {
	result, err := d.executeCommandNow(request)
	if err == nil {
		return result
	}
	return commandResult{stderr: []byte(err.Error() + "\n"), exitCode: 1}
}

func (d *Daemon) executeCommandNow(request protocol.CommandRequest) (commandResult, error) {
	commandCtx := commandContextForRequest(request)
	outcome, err := d.commandEngine().run(commandCtx, request.Args)
	if err != nil {
		return commandResult{}, err
	}
	if err := d.applyExternalCommandAction(outcome.Action); err != nil {
		return commandResult{}, err
	}
	result := commandResult{stdout: outcome.Stdout, stderr: outcome.Stderr, bootstrap: outcome.Bootstrap, sessionID: outcome.sessionID}
	if _, stop := outcome.Action.(stopServerAction); stop {
		result.stopServer = true
	}
	return result, nil
}

func (d *Daemon) applyExternalCommandAction(action commandAction) error {
	switch action := action.(type) {
	case nil, stopServerAction:
		return nil
	case applyViewTransitionAction:
		plan := action.Transition.Projection
		var connection *clientConnection
		d.call(func() {
			session := d.sessions[plan.SessionID]
			if session == nil {
				return
			}
			candidate := d.clients[session.ClientID]
			if candidate != nil && candidate.ID == plan.ClientID {
				connection = candidate.State.Active
			}
		})
		if connection == nil {
			action.Transition.delivery.cancel()
			return errors.New("view transition target client is no longer attached")
		}
		return sendClientCommand(connection, clientInstanceCommand{Transition: &action.Transition})
	case publishClientStatusAction:
		var connection *clientConnection
		d.call(func() {
			for _, candidate := range d.clients {
				if candidate != nil && candidate.ID == action.ClientID {
					connection = candidate.State.Active
					break
				}
			}
		})
		if connection == nil {
			return errors.New("status update target client is no longer attached")
		}
		postClientCommand(connection, clientInstanceCommand{
			RefreshStatus: true, Status: action.Status, HasStatus: true,
		})
		return nil
	case focusClientDirectionAction:
		client := commandClientValue(d, CommandContext{Caller: CommandCaller{Origin: CommandOriginStandaloneCLI}}, action.SessionID)
		if client == nil || client.ID != action.ClientID {
			action.Delivery.Required.cancel()
			return errors.New("focus target client is no longer attached")
		}
		return sendReservedClientCommand(action.Delivery, clientInstanceCommand{FocusDirection: action.Direction})
	case enterClientHistoryAction:
		client := commandClientValue(d, CommandContext{Caller: CommandCaller{Origin: CommandOriginStandaloneCLI}}, action.SessionID)
		if client == nil || client.ID != action.ClientID {
			action.Delivery.Required.cancel()
			return errors.New("history target client is no longer attached")
		}
		return sendReservedClientCommand(action.Delivery, clientInstanceCommand{EnterHistory: true})
	case sendClientKeysAction:
		client := commandClientValue(d, CommandContext{Caller: CommandCaller{Origin: CommandOriginStandaloneCLI}}, action.SessionID)
		if client == nil || client.ID != action.ClientID {
			action.Delivery.Required.cancel()
			return errors.New("send-keys target client is no longer attached")
		}
		return sendReservedClientCommand(action.Delivery, clientInstanceCommand{RunSendKeys: true, SendKeys: action.Args})
	case pasteClientBufferAction:
		client := commandClientValue(d, CommandContext{Caller: CommandCaller{Origin: CommandOriginStandaloneCLI}}, action.SessionID)
		if client == nil || client.ID != action.ClientID {
			action.Delivery.Required.cancel()
			return errors.New("paste-buffer target client is no longer attached")
		}
		return sendReservedClientCommand(action.Delivery, clientInstanceCommand{RunPasteBuffer: true, PasteBuffer: action.Args})
	case detachClientAction:
		return errors.New("external detach action is not implemented")
	case promptAction:
		return errors.New("interactive prompt requires the attached UI")
	default:
		return fmt.Errorf("unsupported external command action %T", action)
	}
}

func (c *ClientInstance) applyCommandViewTransition(transition ViewTransition) error {
	if c == nil {
		return errors.New("view transition target client is unavailable")
	}
	result := make(chan error, 1)
	c.postCommand(clientInstanceCommand{Transition: &transition, Done: result})
	if c.lifetimeDone == nil {
		return <-result
	}
	select {
	case err := <-result:
		return err
	case <-c.lifetimeDone:
		select {
		case err := <-result:
			return err
		default:
			return errors.New("target client disconnected during view transition")
		}
	}
}

func (c *ClientInstance) publishCommandStatus() error {
	if c == nil {
		return errors.New("status update target client is unavailable")
	}
	result := make(chan error, 1)
	c.postCommand(clientInstanceCommand{RefreshStatus: true, Done: result})
	if c.lifetimeDone == nil {
		return <-result
	}
	select {
	case err := <-result:
		return err
	case <-c.lifetimeDone:
		return errors.New("target client disconnected during status update")
	}
}

type CommandOrigin uint8

const (
	CommandOriginAttachedUI CommandOrigin = iota + 1
	CommandOriginPaneCLI
	CommandOriginStandaloneCLI
)

// CommandCaller is a value snapshot of the invocation site. Command handlers
// receive identities and dimensions, never live ClientInstance or SessionState
// pointers. They resolve only the semantics their own command requires.
type CommandCaller struct {
	Origin              CommandOrigin
	ClientID            ClientID
	SessionID           uint64
	PaneID              uint64
	WorkingDirectory    string
	TerminalCols        uint16
	TerminalRows        uint16
	CallerSessionTarget string
}

type CommandContext struct {
	Caller CommandCaller
}

type commandAction interface{ commandAction() }

type detachClientAction struct{ ClientID ClientID }

func (detachClientAction) commandAction() {}

type stopServerAction struct{}

func (stopServerAction) commandAction() {}

type applyViewTransitionAction struct{ Transition ViewTransition }

func (applyViewTransitionAction) commandAction() {}

type publishClientStatusAction struct {
	ClientID ClientID
	Status   clientStatusState
}

func (publishClientStatusAction) commandAction() {}

type focusClientDirectionAction struct {
	ClientID  ClientID
	SessionID uint64
	Direction byte
	Delivery  clientCommandDelivery
}

func (focusClientDirectionAction) commandAction() {}

type enterClientHistoryAction struct {
	ClientID  ClientID
	SessionID uint64
	Delivery  clientCommandDelivery
}

func (enterClientHistoryAction) commandAction() {}

type sendClientKeysAction struct {
	ClientID  ClientID
	SessionID uint64
	Args      []string
	Delivery  clientCommandDelivery
}

func (sendClientKeysAction) commandAction() {}

type pasteClientBufferAction struct {
	ClientID  ClientID
	SessionID uint64
	Args      []string
	Delivery  clientCommandDelivery
}

func (pasteClientBufferAction) commandAction() {}

type PromptRequest struct {
	Mode     PromptMode
	Label    string
	Initial  string
	OnSubmit func(*Daemon, CommandContext, string) (commandOutcome, error)
	OnCancel func(*Daemon, CommandContext) (commandOutcome, error)
}

type promptAction struct{ Request PromptRequest }

func (promptAction) commandAction() {}

type commandOutcome struct {
	Stdout    []byte
	Stderr    []byte
	Bootstrap *protocol.CommandBootstrap
	Action    commandAction
	sessionID uint64
}

type commandHandler func(*Daemon, CommandContext, []string) (commandOutcome, error)

type commandClientSnapshot struct {
	ID           ClientID
	SessionID    uint64
	Connection   *clientConnection
	TerminalCols uint16
	TerminalRows uint16
	Shell        string
	Status       clientStatusState
}

type clientCommandDelivery struct {
	Connection *clientConnection
	Required   *clientRequiredDelivery
	Rejected   bool
}

func reserveCommandActionDelivery(d *Daemon, ctx CommandContext, client *commandClientSnapshot) (clientCommandDelivery, error) {
	if ctx.Caller.Origin == CommandOriginAttachedUI {
		return clientCommandDelivery{}, nil
	}
	return reserveCommandTargetDelivery(d, client)
}

func reserveCommandTargetDelivery(d *Daemon, client *commandClientSnapshot) (clientCommandDelivery, error) {
	if d == nil || client == nil {
		return clientCommandDelivery{}, errors.New("command target client is unavailable")
	}
	var delivery clientCommandDelivery
	d.call(func() {
		identity := d.clients[client.ID]
		if identity == nil || identity.SessionID != client.SessionID || identity.State.Active == nil {
			return
		}
		delivery.Connection = identity.State.Active
		delivery.Required, _ = delivery.Connection.reserveRequired()
		delivery.Rejected = delivery.Required == nil
	})
	if delivery.Connection == nil {
		return delivery, errors.New("command target client is no longer attached")
	}
	return delivery, nil
}

type commandSessionSnapshot struct {
	ID              uint64
	ActivePane      *Pane
	CallerPane      *Pane
	HasActiveWindow bool
	Client          *commandClientSnapshot
	Format          formatSessionSnapshot
}

func commandClientSnapshotNow(client *ClientIdentity) *commandClientSnapshot {
	if client == nil {
		return nil
	}
	return &commandClientSnapshot{
		ID:           client.ID,
		SessionID:    client.SessionID,
		Connection:   client.State.Active,
		TerminalCols: client.terminalCols,
		TerminalRows: client.terminalRows,
		Shell:        client.shell,
	}
}

func commandSessionSnapshotNow(d *Daemon, state *SessionState, callerPaneID uint64) *commandSessionSnapshot {
	if state == nil {
		return nil
	}
	snapshot := &commandSessionSnapshot{
		ID:              state.ID,
		ActivePane:      state.activePane(),
		CallerPane:      state.Panes[callerPaneID],
		HasActiveWindow: state.Windows[state.ActiveWindowID] != nil,
		Format:          state.formatSnapshot(),
	}
	if d != nil {
		snapshot.Client = commandClientSnapshotNow(d.clients[state.ClientID])
		if snapshot.Client != nil {
			snapshot.Client.Status = d.clientStatusSnapshotNow(d.clients[state.ClientID], state)
		}
	}
	return snapshot
}

func resolveCommandSessionValue(d *Daemon, ctx CommandContext, raw string) (*commandSessionSnapshot, error) {
	if strings.HasPrefix(raw, "@") {
		groupID, err := strconv.ParseUint(strings.TrimPrefix(raw, "@"), 10, 64)
		if err != nil || groupID == 0 || d == nil {
			return nil, fmt.Errorf("unknown session %q", raw)
		}
		var session *commandSessionSnapshot
		d.call(func() {
			group := d.groups[groupID]
			if group == nil {
				return
			}
			// A grouped pane inherits the stable group target because its process
			// can outlive the session that created it. While the pane is visible,
			// its canonical window lease identifies the exact member session whose
			// client invoked the command; prefer that over an arbitrary group member.
			if pane := group.Panes[ctx.Caller.PaneID]; pane != nil {
				if lease := d.windowLeases[pane.WindowID]; lease != nil {
					candidate := d.sessions[lease.SessionID]
					if candidate != nil && candidate.group == group {
						session = commandSessionSnapshotNow(d, candidate, ctx.Caller.PaneID)
						return
					}
				}
			}
			ids := make([]uint64, 0, len(group.SessionIDs))
			for id := range group.SessionIDs {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			for _, id := range ids {
				if candidate := d.sessions[id]; candidate != nil {
					session = commandSessionSnapshotNow(d, candidate, ctx.Caller.PaneID)
					break
				}
			}
		})
		if session == nil {
			return nil, fmt.Errorf("unknown session %q", raw)
		}
		return session, nil
	}
	target, err := parseSessionTarget(raw)
	if err != nil {
		return nil, err
	}
	if ctx.Caller.SessionID != 0 && target.id == ctx.Caller.SessionID {
		var session *commandSessionSnapshot
		d.call(func() { session = commandSessionSnapshotNow(d, d.sessions[ctx.Caller.SessionID], ctx.Caller.PaneID) })
		if session != nil {
			return session, nil
		}
	}
	var session *commandSessionSnapshot
	if d != nil {
		d.call(func() {
			if target.id != 0 {
				session = commandSessionSnapshotNow(d, d.sessions[target.id], ctx.Caller.PaneID)
			} else {
				session = commandSessionSnapshotNow(d, d.sessionByName(target.name), ctx.Caller.PaneID)
			}
		})
	}
	if session == nil {
		return nil, fmt.Errorf("unknown session %q", raw)
	}
	return session, nil
}

var errNoImplicitCommandSession = errors.New("caller has no current Meja session")

// resolveCommandCallerSession is the single definition of "the session this
// command was invoked from". Explicit command targets do not pass through it.
func resolveCommandCallerSession(d *Daemon, ctx CommandContext) (*commandSessionSnapshot, error) {
	// AttachedUI always carries the daemon session-map key. Production keys
	// are nonzero; accepting zero here keeps the resolver faithful to daemon
	// ownership and permits synthetic actor fixtures that use session zero.
	if ctx.Caller.Origin == CommandOriginAttachedUI || ctx.Caller.SessionID != 0 {
		var session *commandSessionSnapshot
		if d != nil {
			d.call(func() {
				session = commandSessionSnapshotNow(d, d.sessions[ctx.Caller.SessionID], ctx.Caller.PaneID)
			})
		}
		if session == nil {
			return nil, errSessionUnavailable
		}
		return session, nil
	}
	if ctx.Caller.CallerSessionTarget != "" {
		return resolveCommandSessionValue(d, ctx, ctx.Caller.CallerSessionTarget)
	}
	return nil, errNoImplicitCommandSession
}

func commandClientValue(d *Daemon, ctx CommandContext, sessionID uint64) *commandClientSnapshot {
	if d == nil {
		return nil
	}
	var client *commandClientSnapshot
	d.call(func() {
		session := d.sessions[sessionID]
		if session == nil {
			return
		}
		candidate := d.clients[session.ClientID]
		if candidate != nil && (ctx.Caller.Origin != CommandOriginAttachedUI || candidate.ID == ctx.Caller.ClientID) {
			client = commandClientSnapshotNow(candidate)
			client.Status = d.clientStatusSnapshotNow(candidate, session)
		}
	})
	return client
}

type commandDefinition struct {
	name        string
	aliases     []string
	usage       string
	description string
	hidden      bool
	run         commandHandler
}

type Command struct {
	Name        string
	Aliases     []string
	Usage       string
	Description string
	Hidden      bool
	run         commandHandler
}

// CommandEngine is the daemon-owned immutable command registry. The ordered
// slice is the single source for help/listing; byName contains both canonical
// names and aliases.
type CommandEngine struct {
	daemon  *Daemon
	ordered []Command
	byName  map[string]Command
}

func newCommandEngine(daemon *Daemon) *CommandEngine {
	engine := &CommandEngine{
		daemon: daemon,
		byName: make(map[string]Command),
	}
	for _, definition := range commandDefinitions() {
		definition := definition
		command := Command{
			Name: definition.name, Aliases: append([]string(nil), definition.aliases...),
			Usage: definition.usage, Description: definition.description, Hidden: definition.hidden,
		}
		command.run = definition.run
		if command.run == nil {
			panic("server command " + command.Name + " has no handler")
		}
		engine.ordered = append(engine.ordered, command)
		if command.Name == "" {
			panic("server command has no name")
		}
		if _, exists := engine.byName[command.Name]; exists {
			panic("duplicate server command " + command.Name)
		}
		engine.byName[command.Name] = command
		for _, alias := range command.Aliases {
			if _, exists := engine.byName[alias]; exists {
				panic("duplicate server command alias " + alias)
			}
			engine.byName[alias] = command
		}
	}
	return engine
}

func commandContextForRequest(request protocol.CommandRequest) CommandContext {
	origin := CommandOriginStandaloneCLI
	if request.CallerSessionTarget != "" {
		origin = CommandOriginPaneCLI
	}
	return CommandContext{Caller: CommandCaller{
		Origin:              origin,
		PaneID:              request.CallerPaneID,
		WorkingDirectory:    request.WorkingDirectory,
		TerminalCols:        request.TerminalCols,
		TerminalRows:        request.TerminalRows,
		CallerSessionTarget: request.CallerSessionTarget,
	}}
}

func (c *ClientInstance) commandContext() CommandContext {
	caller := CommandCaller{Origin: CommandOriginAttachedUI}
	if c == nil {
		return CommandContext{Caller: caller}
	}
	caller.ClientID = c.clientID
	caller.SessionID = c.sessionID
	caller.TerminalCols = c.terminalCols
	caller.TerminalRows = c.terminalRows
	paneID := c.currentView.Layout.FocusedPaneID
	cwd, paneFound := c.observedPaneCwd(paneID)
	caller.WorkingDirectory = cwd
	if paneFound {
		caller.PaneID = paneID
	}
	return CommandContext{Caller: caller}
}

func (d *Daemon) commandEngine() *CommandEngine {
	if d == nil {
		return nil
	}
	d.commandEngineOnce.Do(func() {
		if d.commands == nil {
			d.commands = newCommandEngine(d)
		}
	})
	return d.commands
}

func (e *CommandEngine) lookup(name string) (Command, bool) {
	if e == nil {
		return Command{}, false
	}
	command, ok := e.byName[name]
	return command, ok
}

// commandDefinitions is the one canonical ordered declaration list.
func commandDefinitions() []commandDefinition {
	return []commandDefinition{
		{name: "help", aliases: []string{"--help"}, usage: "help [command]", description: "Show this reference or help for one server command.", run: runHelpCommand},
		{name: "server-version", usage: "server-version", description: "Show the running daemon build and protocol versions.", run: runServerVersionCommand},
		{name: "new-session", aliases: []string{"new"}, usage: "new-session [-d] [-P] [-F format] [-s name] [-r directory] [-t base] [-- command args...] | new-session -f file [-s name] [--run | --commands=prepare|skip|run]", description: "Create a session, optionally from a .meja file, and attach unless -d is supplied.", run: runNewSessionCommand},
		{name: "attach-session", aliases: []string{"attach", "a"}, usage: "attach-session -t session-id-or-name", description: "Attach to an existing session.", run: runAttachSessionCommand},
		{name: "restore-session", aliases: []string{"restore"}, usage: "restore-session -t name [-s new-name] [--run | --commands=prepare|skip|run]", description: "Restore a named session's automatic snapshot and attach.", run: runRestoreSessionCommand},
		{name: "save-session", aliases: []string{"save"}, usage: "save-session [-t session-id-or-name] -o file [-f]", description: "Save a live session to a .meja file.", run: runSaveSessionCommand},
		{name: "list-sessions", aliases: []string{"ls"}, usage: "list-sessions [-F format]", description: "List active sessions.", run: runListSessionsCommand},
		{name: "kill-session", usage: "kill-session [-t session]", description: "Terminate a session and its panes.", run: runKillSessionCommand},
		{name: "kill-server", usage: "kill-server", description: "Stop the selected server.", run: runKillServerCommand},
		{name: "new-window", aliases: []string{"neww"}, usage: "new-window [-t session]", description: "Create a window in a session.", run: runNewWindowCommand},
		{name: "next-layout", usage: "next-layout [-t session]", description: "Cycle the active window to its next layout.", run: runNextLayoutCommand},
		{name: "split-window", aliases: []string{"splitw"}, usage: "split-window [-t session] [-h | -v]", description: "Split the active pane left/right or top/bottom.", run: runSplitWindowCommand},
		{name: "detach-client", aliases: []string{"detach"}, usage: "detach-client [-t session]", description: "Detach the client from a session.", run: runDetachClientCommand},
		{name: "next-window", aliases: []string{"next"}, usage: "next-window [-t session]", description: "Select the next window.", run: runRelativeWindowCommand("next-window", 1, false)},
		{name: "previous-window", aliases: []string{"prev"}, usage: "previous-window [-t session]", description: "Select the previous window.", run: runRelativeWindowCommand("previous-window", -1, false)},
		{name: "last-window", aliases: []string{"last"}, usage: "last-window [-t session]", description: "Select the last active window.", run: runRelativeWindowCommand("last-window", 0, true)},
		{name: "select-window", aliases: []string{"selectw"}, usage: "select-window -t session:window", description: "Select a window by index.", run: runSelectWindowCommand},
		{name: "kill-pane", aliases: []string{"killp"}, usage: "kill-pane [-t session]", description: "Close the active pane.", run: runKillPaneCommand},
		{name: "copy-mode", usage: "copy-mode [-t session]", description: "Browse pane history; Space starts a selection and Enter copies it.", run: runCopyModeCommand},
		{name: "swap-pane", aliases: []string{"swapp"}, usage: "swap-pane [-t session] (-U | -D)", description: "Swap the active pane with its neighbor.", run: runSwapPaneCommand},
		{name: "select-pane", aliases: []string{"selectp"}, usage: "select-pane [-t session] (-U | -D | -L | -R)", description: "Select an adjacent pane.", run: runSelectPaneCommand},
		{name: "resize-pane", aliases: []string{"resizep"}, usage: "resize-pane [-t session] ((-U | -D | -L | -R) [amount] | -Z)", description: "Resize the active pane or toggle zoom.", run: runResizePaneCommand},
		{name: "send-keys", aliases: []string{"send"}, usage: "send-keys [-t session] [-X copy-mode-command | -l] key...", description: "Send keys or run a copy-mode command in the active pane.", run: runSendKeysCommand},
		{name: "capture-pane", aliases: []string{"capturep"}, usage: "capture-pane [-t session] [-p] [-b buffer-name] [-S start-line] [-E end-line] [-e] [-C] [-J] [-N]", description: "Capture the active pane into stdout or a paste buffer.", run: capturePaneCommand()},
		{name: "list-panes", aliases: []string{"lsp"}, usage: "list-panes [-t session] [-F format]", description: "List panes and pane mode values.", run: listPanesCommand()},
		{name: "set-buffer", aliases: []string{"setb"}, usage: "set-buffer [-a] [-b buffer-name] [-n new-buffer-name] data", description: "Create or update a paste buffer.", run: handleSetBufferCommand},
		{name: "show-buffer", aliases: []string{"showb"}, usage: "show-buffer [-b buffer-name]", description: "Print a paste buffer.", run: handleShowBufferCommand},
		{name: "list-buffers", aliases: []string{"lsb"}, usage: "list-buffers", description: "List paste buffers.", run: handleListBuffersCommand},
		{name: "delete-buffer", aliases: []string{"deleteb"}, usage: "delete-buffer [-b buffer-name]", description: "Delete a paste buffer.", run: handleDeleteBufferCommand},
		{name: "load-buffer", aliases: []string{"loadb"}, usage: "load-buffer [-b buffer-name] path", description: "Load a file into a paste buffer.", run: handleLoadBufferCommand},
		{name: "save-buffer", aliases: []string{"saveb"}, usage: "save-buffer [-a] [-b buffer-name] path", description: "Save a paste buffer to a file.", run: handleSaveBufferCommand},
		{name: "paste-buffer", aliases: []string{"pasteb"}, usage: "paste-buffer [-t session] [-b buffer-name] [-dprS] [-s separator]", description: "Paste a buffer into the active pane.", run: runPasteBufferCommand},
		{name: "rename-window", aliases: []string{"renamew"}, usage: "rename-window [-t session:window] [name]", description: "Rename a window, prompting when no name is supplied.", run: runRenameWindowCommand},
		{name: "rename-session", aliases: []string{"rename", "renames"}, usage: "rename-session [-t session] [name]", description: "Rename a session, prompting when no name is supplied.", run: runRenameSessionCommand},
		{name: "set-root", usage: "set-root [-t session] [directory]", description: "Set the session root directory.", run: runSetRootCommand},
		{name: "switch-session", usage: "switch-session -t session-id-or-name", description: "Move the attached client to another session.", run: runSwitchSessionCommand},
	}
}

func runServerVersionCommand(_ *Daemon, _ CommandContext, args []string) (commandOutcome, error) {
	if len(args) != 0 {
		return commandOutcome{}, errors.New("server-version accepts no arguments")
	}
	return commandOutcome{Stdout: []byte(fmt.Sprintf(
		"server:           meja %s\ncommand protocol: %d\nQUIC profile:     %s\n",
		version.Current(), protocol.CommandProtocolVersion, protocol.ALPN,
	))}, nil
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

func (e *CommandEngine) helpOutput(args []string) ([]byte, error) {
	if len(args) > 1 {
		return nil, errors.New("help accepts at most one command")
	}
	if len(args) == 1 {
		command, ok := e.lookup(args[0])
		if !ok || command.Hidden {
			return nil, fmt.Errorf("unknown command %q", args[0])
		}
		var output strings.Builder
		fmt.Fprintf(&output, "usage: meja [transport-options] %s\n\n%s\n", command.Usage, command.Description)
		if len(command.Aliases) > 0 {
			fmt.Fprintf(&output, "\naliases: %s\n", strings.Join(command.Aliases, ", "))
		}
		return []byte(output.String()), nil
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
	for _, command := range e.ordered {
		if command.Hidden {
			continue
		}
		name := command.Name
		if len(command.Aliases) > 0 {
			name += " (" + strings.Join(command.Aliases, ", ") + ")"
		}
		fmt.Fprintf(table, "  %s\t%s\n", name, command.Description)
	}
	if err := table.Flush(); err != nil {
		return nil, err
	}
	output.WriteString("\nRun 'meja help <command>' or 'meja <command> --help' for command usage.\n")
	return []byte(output.String()), nil
}

// run encapsulates help rewriting, lookup, alias resolution, and handler
// invocation for command-socket requests, prefix bindings, and the attached
// command prompt.
func (e *CommandEngine) run(ctx CommandContext, argv []string) (commandOutcome, error) {
	if len(argv) == 0 {
		return commandOutcome{}, errors.New("missing command")
	}
	if commandRequestsHelp(argv) {
		argv = []string{"help", argv[0]}
	}
	command, ok := e.lookup(argv[0])
	if !ok {
		return commandOutcome{}, fmt.Errorf("unknown command %q", argv[0])
	}
	return command.run(e.daemon, ctx, argv[1:])
}

// executeAttachedCommand adapts the shared engine outcome to the attached UI.
// Registry execution stays in CommandEngine; applying transport-local actions
// stays on the ClientInstance actor.
func (c *ClientInstance) executeAttachedCommand(argv []string) (bool, error) {
	outcome, err := c.Daemon.commandEngine().run(c.commandContext(), argv)
	if err != nil {
		return false, err
	}
	return c.applyAttachedCommandOutcome(outcome)
}

func (c *ClientInstance) applyAttachedCommandOutcome(outcome commandOutcome) (bool, error) {
	if len(outcome.Stdout) > 0 || len(outcome.Stderr) > 0 {
		return false, errors.New("command output is only available through the CLI")
	}
	if outcome.Action != nil && c.Daemon != nil &&
		!c.Daemon.clientConnectionIsCurrent(c.clientID, c.connection) {
		cancelCommandActionDelivery(outcome.Action)
		return false, errors.New("command client connection is no longer active")
	}
	switch action := outcome.Action.(type) {
	case nil:
		return false, nil
	case detachClientAction:
		return true, nil
	case promptAction:
		mode := action.Request.Mode
		if mode == 0 {
			mode = PromptModeText
		}
		_, err := c.beginPrompt(mode, action.Request.Label, action.Request.Initial, func(result promptResult) (bool, error) {
			ctx := c.commandContext()
			var next commandOutcome
			var callbackErr error
			if result.Submitted {
				if action.Request.OnSubmit != nil {
					next, callbackErr = action.Request.OnSubmit(c.Daemon, ctx, result.Text)
				}
			} else if action.Request.OnCancel != nil {
				next, callbackErr = action.Request.OnCancel(c.Daemon, ctx)
			}
			if callbackErr != nil {
				return false, callbackErr
			}
			var detach bool
			detach, callbackErr = c.applyAttachedCommandOutcome(next)
			if callbackErr != nil {
				return false, callbackErr
			}
			if detach {
				return true, nil
			}
			if result.Submitted {
				c.refreshCommandStatusSnapshot()
			}
			return false, c.publishClientStatus()
		})
		if err != nil {
			return false, err
		}
		return false, c.publishClientStatus()
	case applyViewTransitionAction:
		if action.Transition.Projection.ClientID != c.clientID {
			return false, errors.New("view transition belongs to another client")
		}
		return false, c.applyViewTransition(action.Transition)
	case publishClientStatusAction:
		if action.ClientID != c.clientID {
			return false, errors.New("status update belongs to another client")
		}
		if !c.installStatusSnapshot(action.Status) {
			return false, nil
		}
		return false, c.publishClientStatus()
	case focusClientDirectionAction:
		if action.ClientID != c.clientID {
			return false, errors.New("focus action belongs to another client")
		}
		_, _, err := c.FocusPaneDirection(action.Direction)
		return false, err
	case enterClientHistoryAction:
		if action.ClientID != c.clientID {
			return false, errors.New("history action belongs to another client")
		}
		return false, c.commandEnterHistory()
	case sendClientKeysAction:
		if action.ClientID != c.clientID {
			return false, errors.New("send-keys action belongs to another client")
		}
		return false, sendKeysToClient(c, action.Args)
	case pasteClientBufferAction:
		if action.ClientID != c.clientID {
			return false, errors.New("paste-buffer action belongs to another client")
		}
		return false, pasteBufferToClient(c, action.Args)
	default:
		return false, fmt.Errorf("command returned unsupported attached action %T", action)
	}
}

func cancelCommandActionDelivery(action commandAction) {
	switch action := action.(type) {
	case applyViewTransitionAction:
		action.Transition.delivery.cancel()
	case focusClientDirectionAction:
		action.Delivery.Required.cancel()
	case enterClientHistoryAction:
		action.Delivery.Required.cancel()
	case sendClientKeysAction:
		action.Delivery.Required.cancel()
	case pasteClientBufferAction:
		action.Delivery.Required.cancel()
	}
}

// refreshCommandStatusSnapshot is used only after an interactive command has
// submitted a canonical mutation. Ordinary status paints remain entirely
// connection-local.
func (c *ClientInstance) refreshCommandStatusSnapshot() {
	if c == nil || c.Daemon == nil {
		return
	}
	var status clientStatusState
	var ok bool
	c.Daemon.call(func() {
		if state := c.Daemon.sessions[c.sessionID]; state != nil {
			status = c.Daemon.clientStatusSnapshotNow(c.Daemon.clients[c.clientID], state)
			ok = true
		}
	})
	if ok {
		c.installStatusSnapshot(status)
	}
}

func runKillPaneCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if ctx.Caller.Origin != CommandOriginAttachedUI {
		rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
		if err != nil {
			return commandOutcome{}, err
		}
		if len(remaining) != 0 {
			return commandOutcome{}, errors.New("kill-pane accepts only -t <session-target>")
		}
		var session *commandSessionSnapshot
		if hasTarget {
			session, err = resolveCommandSessionValue(d, ctx, rawTarget)
		} else {
			session, err = resolveCommandCallerSession(d, ctx)
			if errors.Is(err, errNoImplicitCommandSession) {
				err = errors.New("kill-pane requires -t <session-target>")
			}
		}
		if err != nil {
			return commandOutcome{}, err
		}
		paneID := uint64(0)
		if !hasTarget && ctx.Caller.PaneID != 0 && session.CallerPane != nil {
			paneID = ctx.Caller.PaneID
		}
		if paneID == 0 {
			pane := session.ActivePane
			if pane == nil {
				return commandOutcome{}, errors.New("kill-pane requires an active pane")
			}
			paneID = pane.ID
		}
		transition, err := d.killCommandPaneNow(session.ID, paneID)
		if err != nil {
			return commandOutcome{}, err
		}
		return viewTransitionOutcome(transition), nil
	}
	if err := requireNoCommandArgs("kill-pane", args); err != nil {
		return commandOutcome{}, err
	}
	clientID, sessionID, paneID := ctx.Caller.ClientID, ctx.Caller.SessionID, ctx.Caller.PaneID
	if sessionID == 0 || paneID == 0 {
		return commandOutcome{}, errors.New("kill-pane requires an active pane")
	}
	return commandOutcome{Action: promptAction{Request: PromptRequest{
		Mode:  PromptModeConfirm,
		Label: "kill-pane? (y/N) ",
		OnSubmit: func(d *Daemon, fresh CommandContext, _ string) (commandOutcome, error) {
			if fresh.Caller.ClientID != clientID || fresh.Caller.SessionID != sessionID {
				return commandOutcome{}, errors.New("kill-pane caller changed while confirmation was open")
			}
			transition, err := d.closeCommandPane(clientID, sessionID, paneID)
			if err != nil {
				return commandOutcome{}, err
			}
			return viewTransitionOutcome(transition), nil
		},
		OnCancel: func(*Daemon, CommandContext) (commandOutcome, error) {
			return commandOutcome{}, nil
		},
	}}}, nil
}

func resolveCommandSessionID(d *Daemon, ctx CommandContext, args []string) (uint64, []string, error) {
	rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
	if err != nil {
		return 0, nil, err
	}
	var session *commandSessionSnapshot
	if hasTarget {
		session, err = resolveCommandSessionValue(d, ctx, rawTarget)
	} else {
		session, err = resolveCommandCallerSession(d, ctx)
		if errors.Is(err, errNoImplicitCommandSession) {
			err = errors.New("command requires -t <session-target>")
		}
	}
	if err != nil {
		return 0, nil, err
	}
	return session.ID, remaining, nil
}

func runNewWindowCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	sessionID, remaining, err := resolveCommandSessionID(d, ctx, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if err := requireNoCommandArgs("new-window", remaining); err != nil {
		return commandOutcome{}, err
	}
	transition, err := d.createCommandWindow(ctx.Caller.ClientID, sessionID, ctx.Caller.TerminalCols, ctx.Caller.TerminalRows)
	if err != nil {
		return commandOutcome{}, err
	}
	return viewTransitionOutcome(transition), nil
}

func runSplitWindowCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	sessionID, remaining, err := resolveCommandSessionID(d, ctx, args)
	if err != nil {
		return commandOutcome{}, err
	}
	fs := commandFlagSet("split-window")
	horizontal := fs.Bool("h", false, "split left/right")
	vertical := fs.Bool("v", false, "split top/bottom")
	if err := fs.Parse(remaining); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 0 || (*horizontal && *vertical) {
		return commandOutcome{}, errors.New("split-window accepts one of -h or -v")
	}
	direction := SplitHorizontal
	if *horizontal {
		direction = SplitVertical
	}
	transition, err := d.splitCommandWindow(ctx.Caller.ClientID, sessionID, direction)
	if err != nil {
		return commandOutcome{}, err
	}
	return viewTransitionOutcome(transition), nil
}

func runRenameSessionCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	sessionID, remaining, err := resolveCommandSessionID(d, ctx, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if len(remaining) == 0 && ctx.Caller.Origin == CommandOriginAttachedUI {
		var name string
		d.call(func() {
			if state := d.sessions[sessionID]; state != nil {
				name = state.Name
			}
		})
		return commandOutcome{Action: promptAction{Request: PromptRequest{
			Label:   "(rename-session) ",
			Initial: name,
			OnSubmit: func(d *Daemon, _ CommandContext, answer string) (commandOutcome, error) {
				return renameSessionAnswer(d, sessionID, answer)
			},
		}}}, nil
	}
	if len(remaining) != 1 {
		return commandOutcome{}, errors.New("rename-session accepts one name")
	}
	if err := d.renameCommandSession(sessionID, remaining[0]); err != nil {
		return commandOutcome{}, err
	}
	if client := commandClientValue(d, ctx, sessionID); client != nil {
		return commandOutcome{Action: publishClientStatusAction{ClientID: client.ID, Status: client.Status}}, nil
	}
	return commandOutcome{}, nil
}

func renameSessionAnswer(d *Daemon, sessionID uint64, name string) (commandOutcome, error) {
	var currentName string
	var validationErr error
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			validationErr = errSessionUnavailable
			return
		}
		currentName = state.Name
		validationErr = d.validateSessionRename(state, name)
	})
	if validationErr != nil {
		return commandOutcome{}, validationErr
	}
	if currentName != name {
		exists, err := sessionPersistenceFileExists(d.sessionPersistenceDir, name)
		if err != nil {
			return commandOutcome{}, err
		}
		if exists {
			return commandOutcome{Action: promptAction{Request: PromptRequest{
				Mode:  PromptModeConfirm,
				Label: fmt.Sprintf("persisted session %q exists; overwrite? (y/N) ", name),
				OnSubmit: func(d *Daemon, _ CommandContext, _ string) (commandOutcome, error) {
					if err := d.renameCommandSession(sessionID, name); err != nil {
						return commandOutcome{}, err
					}
					return commandOutcome{}, nil
				},
			}}}, nil
		}
	}
	if err := d.renameCommandSession(sessionID, name); err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{}, nil
}

func runRenameWindowCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
	if err != nil {
		return commandOutcome{}, err
	}
	sessionID := uint64(0)
	windowIndex := -1
	if hasTarget {
		sessionTarget := ""
		windowTarget := rawTarget
		if separator := strings.IndexByte(rawTarget, ':'); separator >= 0 {
			sessionTarget, windowTarget = rawTarget[:separator], rawTarget[separator+1:]
		}
		if sessionTarget != "" {
			session, resolveErr := resolveCommandSessionValue(d, ctx, sessionTarget)
			if resolveErr != nil {
				return commandOutcome{}, resolveErr
			}
			sessionID = session.ID
		} else {
			session, resolveErr := resolveCommandCallerSession(d, ctx)
			if resolveErr != nil {
				if errors.Is(resolveErr, errNoImplicitCommandSession) {
					return commandOutcome{}, errors.New("CLI window targets must be session:window")
				}
				return commandOutcome{}, resolveErr
			}
			sessionID = session.ID
		}
		windowIndex, err = strconv.Atoi(strings.TrimPrefix(windowTarget, ":"))
		if err != nil || windowIndex < 0 {
			return commandOutcome{}, fmt.Errorf("invalid window target %q", rawTarget)
		}
	} else {
		session, resolveErr := resolveCommandCallerSession(d, ctx)
		if resolveErr != nil {
			if errors.Is(resolveErr, errNoImplicitCommandSession) {
				return commandOutcome{}, errors.New("rename-window requires -t session:window")
			}
			return commandOutcome{}, resolveErr
		}
		sessionID = session.ID
	}
	windowID, currentName, err := d.commandWindowByIndex(sessionID, windowIndex)
	if err != nil {
		return commandOutcome{}, err
	}
	if len(remaining) == 0 && ctx.Caller.Origin == CommandOriginAttachedUI {
		return commandOutcome{Action: promptAction{Request: PromptRequest{
			Label:   "(rename-window) ",
			Initial: currentName,
			OnSubmit: func(d *Daemon, fresh CommandContext, answer string) (commandOutcome, error) {
				if err := d.renameCommandWindow(sessionID, windowID, answer); err != nil {
					return commandOutcome{}, err
				}
				return commandOutcome{}, nil
			},
		}}}, nil
	}
	if len(remaining) != 1 {
		return commandOutcome{}, errors.New("rename-window requires one name")
	}
	if err := d.renameCommandWindow(sessionID, windowID, remaining[0]); err != nil {
		return commandOutcome{}, err
	}
	if client := commandClientValue(d, ctx, sessionID); client != nil {
		return commandOutcome{Action: publishClientStatusAction{ClientID: client.ID, Status: client.Status}}, nil
	}
	return commandOutcome{}, nil
}

func (d *Daemon) commandWindowByIndex(sessionID uint64, index int) (uint64, string, error) {
	var id uint64
	var name string
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			return
		}
		if index < 0 {
			id = state.ActiveWindowID
		} else {
			for _, window := range state.Windows {
				if window.DisplayIndex == index {
					id = window.ID
					break
				}
			}
		}
		if window := state.Windows[id]; window != nil {
			name = window.Name
		}
	})
	if id == 0 {
		return 0, "", errors.New("unknown window")
	}
	return id, name, nil
}

func (d *Daemon) renameCommandWindow(sessionID, windowID uint64, name string) error {
	_, err := d.renameSessionWindow(sessionID, windowID, name)
	return err
}

func runSetRootCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	sessionID, remaining, err := resolveCommandSessionID(d, ctx, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if len(remaining) > 1 {
		return commandOutcome{}, errors.New("set-root accepts an optional path")
	}
	raw := ""
	if len(remaining) == 1 {
		raw = remaining[0]
	}
	callerIsPane := ctx.Caller.Origin == CommandOriginAttachedUI || ctx.Caller.Origin == CommandOriginPaneCLI
	if err := d.setCommandRoot(sessionID, raw, ctx.Caller.WorkingDirectory, ctx.Caller.PaneID, callerIsPane); err != nil {
		return commandOutcome{}, err
	}
	if client := commandClientValue(d, ctx, sessionID); client != nil {
		return commandOutcome{Action: publishClientStatusAction{ClientID: client.ID, Status: client.Status}}, nil
	}
	return commandOutcome{}, nil
}

func runResizePaneCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	sessionID, remaining, err := resolveCommandSessionID(d, ctx, args)
	if err != nil {
		return commandOutcome{}, err
	}
	fs := commandFlagSet("resize-pane")
	up := fs.Bool("U", false, "up")
	down := fs.Bool("D", false, "down")
	left := fs.Bool("L", false, "left")
	right := fs.Bool("R", false, "right")
	zoom := fs.Bool("Z", false, "toggle zoom")
	if err := fs.Parse(remaining); err != nil {
		return commandOutcome{}, err
	}
	if *zoom {
		if *up || *down || *left || *right || len(fs.Args()) != 0 {
			return commandOutcome{}, errors.New("resize-pane -Z cannot be combined with a direction or amount")
		}
		transition, err := d.resizeCommandPane(ctx.Caller.ClientID, sessionID, 0, 0, true)
		if err != nil {
			return commandOutcome{}, err
		}
		return viewTransitionOutcome(transition), nil
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
		return commandOutcome{}, errors.New("resize-pane requires exactly one of -U, -D, -L, or -R and an optional amount")
	}
	amount := 1
	if len(fs.Args()) == 1 {
		amount, err = strconv.Atoi(fs.Args()[0])
		if err != nil || amount <= 0 {
			return commandOutcome{}, errors.New("resize-pane amount must be a positive integer")
		}
	}
	transition, err := d.resizeCommandPane(ctx.Caller.ClientID, sessionID, direction, amount, false)
	if err != nil {
		return commandOutcome{}, err
	}
	return viewTransitionOutcome(transition), nil
}

func viewTransitionOutcome(transition *ViewTransition) commandOutcome {
	if transition == nil {
		return commandOutcome{}
	}
	return commandOutcome{Action: applyViewTransitionAction{Transition: *transition}}
}

func (d *Daemon) resizeCommandPane(clientID ClientID, sessionID uint64, direction PaneResizeDirection, amount int, zoom bool) (*ViewTransition, error) {
	state := d.commandSessionAndClient(clientID, sessionID)
	if state == nil {
		return nil, errSessionUnavailable
	}
	client := state.Client
	if client != nil {
		var transition ViewTransition
		var err error
		if zoom {
			_, transition, _, err = d.toggleClientZoom(client.ID)
		} else {
			_, transition, _, err = d.resizeClientPane(client.ID, direction, amount)
		}
		return &transition, err
	}
	var err error
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			err = errSessionUnavailable
			return
		}
		if zoom {
			_, _, err = state.toggleZoomNow()
		} else {
			_, _, err = state.resizeFocusedPaneNow(direction, amount)
		}
	})
	return nil, err
}

func runSwitchSessionCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
	if err != nil {
		return commandOutcome{}, err
	}
	if !hasTarget {
		return commandOutcome{}, errors.New("switch-session requires -t <session-target>")
	}
	if len(remaining) != 0 {
		return commandOutcome{}, errors.New("switch-session accepts only -t <session-target>")
	}
	target, err := resolveCommandSessionValue(d, ctx, rawTarget)
	if err != nil {
		return commandOutcome{}, err
	}
	client, err := commandOriginClient(d, ctx)
	if err != nil {
		return commandOutcome{}, err
	}
	if client == nil {
		return commandOutcome{}, errors.New("switch-session requires an existing client")
	}
	transition, err := d.transitionClientToSession(client.ID, client.Connection, target.ID, client.TerminalCols, client.TerminalRows)
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{Action: applyViewTransitionAction{Transition: transition}}, nil
}

func (d *Daemon) renameCommandSession(sessionID uint64, name string) error {
	return d.renameSessionID(sessionID, name)
}

func (d *Daemon) setCommandRoot(sessionID uint64, raw, callerWorkingDirectory string, callerPaneID uint64, callerIsPane bool) error {
	var exists bool
	var current string
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			return
		}
		exists = true
		current = state.rootDir
		if pane := state.activePane(); pane != nil && pane.Launch.Cwd != "" {
			current = pane.Launch.Cwd
		}
	})
	if !exists {
		return errSessionUnavailable
	}
	if callerWorkingDirectory != "" {
		current = callerWorkingDirectory
	}
	if raw == "" {
		raw = current
	}
	resolved, err := resolveRootDirectory(raw, current)
	if err != nil {
		return err
	}
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			err = errSessionUnavailable
			return
		}
		rootChanged := filepath.Clean(resolved) != state.rootDir
		cwdChanged := false
		if callerIsPane && callerWorkingDirectory != "" && state.Panes[callerPaneID] != nil {
			cwd := filepath.Clean(callerWorkingDirectory)
			cwdChanged = cwd != state.Panes[callerPaneID].KnownCwd
			state.Panes[callerPaneID].KnownCwd = cwd
		}
		state.setRoot(resolved)
		if cwdChanged && !rootChanged {
			state.markSessionChangedForPersistence()
		}
	})
	return err
}

type commandTargetKind uint8

const (
	sessionTarget commandTargetKind = iota
	windowTarget
)

func commandOriginClient(d *Daemon, ctx CommandContext) (*commandClientSnapshot, error) {
	switch ctx.Caller.Origin {
	case CommandOriginAttachedUI:
		client := commandClientValue(d, ctx, ctx.Caller.SessionID)
		if client == nil || client.ID != ctx.Caller.ClientID {
			return nil, errors.New("attached command client is no longer available")
		}
		return client, nil
	case CommandOriginPaneCLI:
		source, err := resolveCommandCallerSession(d, ctx)
		if err != nil {
			return nil, err
		}
		client := source.Client
		if client == nil {
			return nil, errors.New("the invoking Meja session is no longer attached")
		}
		return client, nil
	case CommandOriginStandaloneCLI:
		return nil, nil
	default:
		return nil, errors.New("unknown command origin")
	}
}

func commandViewport(ctx CommandContext, client *commandClientSnapshot) (string, uint16, uint16) {
	cols, rows := ctx.Caller.TerminalCols, ctx.Caller.TerminalRows
	if client != nil {
		cols = client.TerminalCols
		rows = client.TerminalRows
	}
	return ctx.Caller.WorkingDirectory, cols, rows
}

// sessionActivationOutcome assigns an existing client to the resulting
// session and returns the transition that installs that session's active view.
// Without an existing client, it preserves the bootstrap for a new attachment.
func sessionActivationOutcome(d *Daemon, client *commandClientSnapshot, result sessionCommandResult) (commandOutcome, error) {
	if client == nil || result.sessionID == 0 {
		return commandOutcome{Stdout: result.stdout, Bootstrap: result.bootstrap, sessionID: result.sessionID}, nil
	}
	if result.bootstrap != nil {
		d.discardAttachGrant(result.bootstrap.AttachToken)
	}
	transition, err := d.transitionClientToSession(client.ID, client.Connection, result.sessionID, client.TerminalCols, client.TerminalRows)
	if err != nil {
		return commandOutcome{}, err
	}
	result.bootstrap = nil
	return commandOutcome{Stdout: result.stdout, Action: applyViewTransitionAction{Transition: transition}}, nil
}

func runNewSessionCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	var client *commandClientSnapshot
	var err error
	if !newSessionRequestsDetached(args) {
		client, err = commandOriginClient(d, ctx)
		if err != nil {
			return commandOutcome{}, err
		}
	}
	workingDirectory, cols, rows := commandViewport(ctx, client)
	result, err := d.commandNewSessionState(workingDirectory, cols, rows, args, client == nil)
	if err != nil {
		return commandOutcome{}, err
	}
	return sessionActivationOutcome(d, client, result)
}

func runAttachSessionCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	client, err := commandOriginClient(d, ctx)
	if err != nil {
		return commandOutcome{}, err
	}
	result, err := d.commandAttachSession(args, client == nil)
	if err != nil {
		return commandOutcome{}, err
	}
	return sessionActivationOutcome(d, client, result)
}

func runRestoreSessionCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	client, err := commandOriginClient(d, ctx)
	if err != nil {
		return commandOutcome{}, err
	}
	result, err := d.commandRestoreSession(args, client == nil)
	if err != nil {
		return commandOutcome{}, err
	}
	return sessionActivationOutcome(d, client, result)
}

func resolveSessionCommandContextValue(d *Daemon, ctx CommandContext, kind commandTargetKind, args []string) (*commandSessionSnapshot, *commandClientSnapshot, []string, error) {
	rawTarget, remaining, hasTarget, err := extractCommandTarget(args)
	if err != nil {
		return nil, nil, nil, err
	}
	var targetSession *commandSessionSnapshot
	normalized := remaining
	if hasTarget {
		switch kind {
		case windowTarget:
			separator := strings.IndexByte(rawTarget, ':')
			if separator >= 0 {
				sessionTargetValue := rawTarget[:separator]
				window := rawTarget[separator+1:]
				if window == "" {
					return nil, nil, nil, errors.New("window target must include an index")
				}
				if sessionTargetValue != "" {
					targetSession, err = resolveCommandSessionValue(d, ctx, sessionTargetValue)
				} else {
					targetSession, err = resolveCommandCallerSession(d, ctx)
				}
				normalized = append([]string{"-t", ":" + window}, remaining...)
			} else {
				targetSession, err = resolveCommandCallerSession(d, ctx)
				if errors.Is(err, errNoImplicitCommandSession) {
					return nil, nil, nil, errors.New("CLI window targets must be session:window")
				}
				normalized = append([]string{"-t", rawTarget}, remaining...)
			}
		case sessionTarget:
			targetSession, err = resolveCommandSessionValue(d, ctx, rawTarget)
		}
	} else {
		targetSession, err = resolveCommandCallerSession(d, ctx)
		if errors.Is(err, errNoImplicitCommandSession) {
			err = errors.New("command requires -t <session-target>")
		}
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if targetSession == nil {
		return nil, nil, nil, errors.New("session target is required")
	}
	return targetSession, commandClientValue(d, ctx, targetSession.ID), normalized, nil
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

func requireCommandOutputCLI(ctx CommandContext, name string) error {
	if ctx.Caller.Origin == CommandOriginAttachedUI {
		return fmt.Errorf("%s output is only available through the CLI", name)
	}
	return nil
}

func runHelpCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if err := requireCommandOutputCLI(ctx, "help"); err != nil {
		return commandOutcome{}, err
	}
	output, err := d.commandEngine().helpOutput(args)
	return commandOutcome{Stdout: output}, err
}

func runSaveSessionCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if err := requireCommandOutputCLI(ctx, "save-session"); err != nil {
		return commandOutcome{}, err
	}
	fs := commandFlagSet("save-session")
	target := fs.String("t", "", "live session target")
	outputPath := fs.String("o", "", "output .meja file")
	force := fs.Bool("f", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 0 || *outputPath == "" {
		return commandOutcome{}, errors.New("save-session requires -o <file> and accepts no positional arguments")
	}
	session, err := resolveCommandSessionTarget(d, ctx, *target, "save-session")
	if err != nil {
		return commandOutcome{}, err
	}
	output, err := d.saveSessionOutput(session.ID, *outputPath, *force, ctx.Caller.WorkingDirectory)
	return commandOutcome{Stdout: output}, err
}

func runListSessionsCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if err := requireCommandOutputCLI(ctx, "list-sessions"); err != nil {
		return commandOutcome{}, err
	}
	output, err := d.listSessionsOutput(args)
	return commandOutcome{Stdout: output}, err
}

func requireDaemonCLICommand(ctx CommandContext) error {
	if ctx.Caller.Origin == CommandOriginAttachedUI {
		return errors.New("command is only available through the daemon CLI")
	}
	return nil
}

func runKillSessionCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if err := requireDaemonCLICommand(ctx); err != nil {
		return commandOutcome{}, err
	}
	fs := commandFlagSet("kill-session")
	target := fs.String("t", "", "session target")
	if err := fs.Parse(args); err != nil {
		return commandOutcome{}, err
	}
	if len(fs.Args()) != 0 {
		return commandOutcome{}, errors.New("kill-session accepts no positional arguments")
	}
	session, err := resolveCommandSessionTarget(d, ctx, *target, "kill-session")
	if err != nil {
		return commandOutcome{}, err
	}
	if err := d.shutdownSessionID(session.ID, defaultPaneTerminationTimeouts); err != nil {
		return commandOutcome{}, fmt.Errorf("kill-session: %w", err)
	}
	return commandOutcome{}, nil
}

func resolveCommandSessionTarget(d *Daemon, ctx CommandContext, explicitTarget, commandName string) (*commandSessionSnapshot, error) {
	if explicitTarget != "" {
		return resolveCommandSessionValue(d, ctx, explicitTarget)
	}
	session, err := resolveCommandCallerSession(d, ctx)
	if errors.Is(err, errNoImplicitCommandSession) {
		return nil, fmt.Errorf("%s requires -t <session-id-or-name> outside a Meja pane", commandName)
	}
	return session, err
}

func runKillServerCommand(_ *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	if err := requireDaemonCLICommand(ctx); err != nil {
		return commandOutcome{}, err
	}
	if err := requireNoCommandArgs("kill-server", args); err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{Stdout: []byte(fmt.Sprintf("stopped server PID %d\n", os.Getpid())), Action: stopServerAction{}}, nil
}

func requireNoCommandArgs(name string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s accepts no arguments", name)
	}
	return nil
}

func transitionCommandTarget(d *Daemon, ctx CommandContext, kind commandTargetKind, args []string) (*commandSessionSnapshot, *commandClientSnapshot, []string, error) {
	session, client, normalized, err := resolveSessionCommandContextValue(d, ctx, kind, args)
	if err != nil {
		return nil, nil, nil, err
	}
	if client == nil {
		return nil, nil, nil, errors.New("command requires an attached target client")
	}
	return session, client, normalized, nil
}

func runNextLayoutCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	_, client, remaining, err := transitionCommandTarget(d, ctx, sessionTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if err := requireNoCommandArgs("next-layout", remaining); err != nil {
		return commandOutcome{}, err
	}
	_, transition, _, err := d.cycleWindowLayout(client.ID)
	if err != nil {
		return commandOutcome{}, err
	}
	return viewTransitionOutcome(&transition), nil
}

func runRelativeWindowCommand(name string, delta int, last bool) commandHandler {
	return func(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
		session, client, remaining, err := transitionCommandTarget(d, ctx, sessionTarget, args)
		if err != nil {
			return commandOutcome{}, err
		}
		if err := requireNoCommandArgs(name, remaining); err != nil {
			return commandOutcome{}, err
		}
		windowID, ok := d.windowSelectionTarget(session.ID, delta, last)
		if !ok {
			return commandOutcome{}, nil
		}
		transition, err := d.selectWindow(client.ID, session.ID, windowID)
		if err != nil {
			return commandOutcome{}, err
		}
		return viewTransitionOutcome(&transition), nil
	}
}

func runSelectWindowCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	session, client, normalized, err := transitionCommandTarget(d, ctx, windowTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	fs := commandFlagSet("select-window")
	target := fs.String("t", "", "window target")
	if err := fs.Parse(normalized); err != nil {
		return commandOutcome{}, err
	}
	if *target == "" || len(fs.Args()) != 0 {
		return commandOutcome{}, errors.New("select-window requires -t <window-index>")
	}
	index, err := strconv.Atoi(strings.TrimPrefix(*target, ":"))
	if err != nil || index < 0 {
		return commandOutcome{}, fmt.Errorf("invalid window target %q", *target)
	}
	windowID, ok := d.clientWindowIDByIndex(session.ID, index)
	if !ok {
		return commandOutcome{}, fmt.Errorf("unknown window %d", index)
	}
	transition, err := d.selectWindow(client.ID, session.ID, windowID)
	if err != nil {
		return commandOutcome{}, err
	}
	return viewTransitionOutcome(&transition), nil
}

func runSwapPaneCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	_, client, remaining, err := transitionCommandTarget(d, ctx, sessionTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	direction, rest, err := directionalCommandFlagSet("swap-pane", remaining)
	if err != nil {
		return commandOutcome{}, err
	}
	if len(rest) != 0 || (direction != 'A' && direction != 'B') {
		return commandOutcome{}, errors.New("swap-pane requires exactly one of -U or -D")
	}
	swap := SwapPanePrevious
	if direction == 'B' {
		swap = SwapPaneNext
	}
	_, transition, _, err := d.swapClientPane(client.ID, swap)
	if err != nil {
		return commandOutcome{}, err
	}
	return viewTransitionOutcome(&transition), nil
}

func runSelectPaneCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	session, client, remaining, err := transitionCommandTarget(d, ctx, sessionTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	direction, rest, err := directionalCommandFlagSet("select-pane", remaining)
	if err != nil {
		return commandOutcome{}, err
	}
	if len(rest) != 0 {
		return commandOutcome{}, errors.New("select-pane accepts no positional arguments")
	}
	if !session.HasActiveWindow {
		return commandOutcome{}, nil
	}
	delivery, err := reserveCommandActionDelivery(d, ctx, client)
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{Action: focusClientDirectionAction{
		ClientID: client.ID, SessionID: session.ID, Direction: direction, Delivery: delivery,
	}}, nil
}

func runDetachClientCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	session, client, remaining, err := resolveSessionCommandContextValue(d, ctx, sessionTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if err := requireNoCommandArgs("detach-client", remaining); err != nil {
		return commandOutcome{}, err
	}
	if client == nil {
		return commandOutcome{}, errors.New("command requires an attached client")
	}
	if ctx.Caller.SessionID != session.ID {
		delivery, reserveErr := reserveCommandTargetDelivery(d, client)
		if reserveErr != nil {
			return commandOutcome{}, reserveErr
		}
		postReservedClientCommand(delivery, clientInstanceCommand{
			Close:       true,
			CloseReason: fmt.Sprintf("[detached (from session %d)]", session.ID),
		})
		return commandOutcome{}, nil
	}
	return commandOutcome{Action: detachClientAction{ClientID: client.ID}}, nil
}

func runCopyModeCommand(d *Daemon, ctx CommandContext, args []string) (commandOutcome, error) {
	_, client, remaining, err := resolveSessionCommandContextValue(d, ctx, sessionTarget, args)
	if err != nil {
		return commandOutcome{}, err
	}
	if err := requireNoCommandArgs("copy-mode", remaining); err != nil {
		return commandOutcome{}, err
	}
	if client == nil {
		return commandOutcome{}, errors.New("command requires an attached client")
	}
	delivery, err := reserveCommandActionDelivery(d, ctx, client)
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{Action: enterClientHistoryAction{
		ClientID: client.ID, SessionID: client.SessionID, Delivery: delivery,
	}}, nil
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

type observedPaneCwdSnapshot struct {
	anchor       Anchor
	knownCwd     string
	persistedCwd string
	launchCwd    string
	rootDir      string
	observer     ProcessObserver
	paneFound    bool
}

func (c *ClientInstance) observedPaneCwd(paneID uint64) (string, bool) {
	if c == nil || c.Daemon == nil {
		return "", false
	}
	var snapshot observedPaneCwdSnapshot
	c.Daemon.call(func() {
		state := c.Daemon.sessions[c.sessionID]
		if state == nil {
			return
		}
		snapshot.rootDir = state.rootDir
		snapshot.observer = c.Daemon.processObserver
		pane := state.Panes[paneID]
		if pane == nil {
			return
		}
		snapshot.paneFound = true
		snapshot.anchor = Anchor{
			Key:         PaneKey{PaneID: pane.ID},
			Root:        pane.Root,
			PTY:         pane.PTY,
			RootIsShell: len(pane.Launch.RequestedArgv) == 0,
		}
		snapshot.knownCwd = pane.KnownCwd
		snapshot.launchCwd = pane.Launch.Cwd
		if persisted := state.persistenceRecord(); persisted != nil {
			for _, window := range persisted.Plan.Windows {
				for _, persistedPane := range window.Panes {
					if persistedPane.ID == pane.ID && persistedPane.Cwd != "" {
						snapshot.persistedCwd = persistedPane.Cwd
						return
					}
				}
			}
		}
	})
	if !snapshot.paneFound {
		return snapshot.rootDir, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	observer := snapshot.observer
	if observer == nil {
		observer = NewProcessObserver()
	}
	observations := observer.Observe(ctx, []Anchor{snapshot.anchor})
	if observation := observations[snapshot.anchor.Key]; observation.Root != nil && observation.Root.Cwd != "" {
		return observation.Root.Cwd, true
	}
	if snapshot.knownCwd != "" {
		return snapshot.knownCwd, true
	}
	if snapshot.persistedCwd != "" {
		return snapshot.persistedCwd, true
	}
	if snapshot.launchCwd != "" {
		return snapshot.launchCwd, true
	}
	return snapshot.rootDir, true
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

func (d *Daemon) commandNewSessionState(workingDirectory string, terminalCols, terminalRows uint16, args []string, issueBootstrap bool) (sessionCommandResult, error) {
	fs := commandFlagSet("new-session")
	detached := fs.Bool("d", false, "create without attaching")
	printOutput := fs.Bool("P", false, "print created session")
	format := fs.String("F", "#{session_id}:#{pane_id}", "creation output format")
	name := fs.String("s", "", "session name")
	baseTarget := fs.String("t", "", "base session target for a mirror")
	file := fs.String("f", "", ".meja file")
	mode := fs.String("commands", "prepare", "restore command mode")
	run := fs.Bool("run", false, "run restored commands")
	var root string
	fs.StringVar(&root, "r", "", "session root")
	fs.StringVar(&root, "root", "", "session root")
	if err := fs.Parse(args); err != nil {
		return sessionCommandResult{}, err
	}
	if *baseTarget != "" {
		if !*detached {
			return sessionCommandResult{}, errors.New("new-session -t requires detached creation (-d)")
		}
		if *name == "" {
			return sessionCommandResult{}, errors.New("new-session -t requires -s <mirror-name>")
		}
		if *file != "" || root != "" || len(fs.Args()) != 0 {
			return sessionCommandResult{}, errors.New("new-session -t cannot create an initial command, root, or restore file")
		}
		parsed, err := parseSessionTarget(*baseTarget)
		if err != nil {
			return sessionCommandResult{}, err
		}
		if err := validateSessionName(*name); err != nil {
			return sessionCommandResult{}, err
		}
		var mirrorID uint64
		var createErr error
		d.call(func() {
			var base *SessionState
			if parsed.id != 0 {
				base = d.sessions[parsed.id]
			} else {
				base = d.sessionByName(parsed.name)
			}
			if base == nil {
				createErr = fmt.Errorf("unknown session %q", *baseTarget)
				return
			}
			if d.sessionByName(*name) != nil {
				createErr = fmt.Errorf("session %q already exists", *name)
				return
			}
			if d.nextID == 0 {
				createErr = errors.New("session ID exhausted")
				return
			}
			mirror := newSession(d.nextID, *name)
			mirror.daemon = d
			d.sessions[d.nextID] = mirror
			d.reserveSessionName(mirror, *name)
			if createErr = d.groupSessionInActor(base, mirror); createErr != nil {
				d.removeSession(mirror)
				return
			}
			mirrorID = mirror.ID
			d.nextID++
		})
		if createErr != nil {
			return sessionCommandResult{}, createErr
		}
		d.startPersistence(d.sessionPersistenceDir)
		// Mirror creation copies only view metadata. The linked Window and Pane
		// objects, including their process identities, remain canonical in base.
		if *printOutput {
			var snapshot formatSessionSnapshot
			d.call(func() {
				if mirror := d.sessions[mirrorID]; mirror != nil {
					snapshot = mirror.formatSnapshot()
				}
			})
			if len(snapshot.Panes) == 0 {
				return sessionCommandResult{}, errors.New("created mirror has no active pane")
			}
			return sessionCommandResult{stdout: []byte(expandFormat(*format, formatContext{session: &snapshot, pane: &snapshot.Panes[0]}) + "\n")}, nil
		}
		return sessionCommandResult{}, nil
	}
	if *name != "" {
		if err := validateSessionName(*name); err != nil {
			return sessionCommandResult{}, err
		}
	}
	if *file != "" {
		if *detached || *printOutput || commandArgsContainFlag(args, "-F") {
			return sessionCommandResult{}, errors.New("new-session -f does not support -d, -P, or -F")
		}
		if root != "" || len(fs.Args()) != 0 {
			return sessionCommandResult{}, errors.New("new-session -f cannot be combined with a root or initial command")
		}
		restoreMode, err := resolveRestoreCommandMode(fs, "new-session", *mode, *run)
		if err != nil {
			return sessionCommandResult{}, err
		}
		path, err := resolveCommandFilePath(*file, workingDirectory)
		if err != nil {
			return sessionCommandResult{}, err
		}
		operation, err := d.executeSessionOperation("restore-session", commandSessionTarget{
			file:        path,
			newName:     *name,
			restoreMode: restoreMode,
			detached:    !issueBootstrap,
		})
		if err != nil {
			return sessionCommandResult{}, err
		}
		return sessionCommandResult{bootstrap: &operation.bootstrap, sessionID: operation.sessionID}, nil
	}
	if *run {
		return sessionCommandResult{}, errors.New("new-session --run requires -f <file>")
	}
	if *mode != "prepare" {
		return sessionCommandResult{}, errors.New("new-session --commands requires -f <file>")
	}
	if root == "" {
		root = workingDirectory
	}
	cols, rows := terminalCols, terminalRows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 23
	}
	operation, err := d.executeSessionOperation("create-session", commandSessionTarget{name: *name, detached: *detached || !issueBootstrap})
	if err != nil {
		return sessionCommandResult{}, err
	}
	sessionID := operation.sessionID
	if sessionID == 0 {
		return sessionCommandResult{}, errors.New("created session is unavailable")
	}
	resolved, err := resolveRootDirectory(root, workingDirectory)
	if err == nil {
		d.call(func() {
			if state := d.sessions[sessionID]; state != nil {
				state.rootDir = resolved
			} else {
				err = errSessionUnavailable
			}
		})
	}
	if err == nil {
		_, _, err = d.startSessionWindowID(sessionID, resolved, fs.Args(), cols, rows, defaultShell())
	}
	if err != nil {
		_ = d.shutdownSessionID(sessionID, defaultPaneTerminationTimeouts)
		return sessionCommandResult{}, err
	}
	result := sessionCommandResult{sessionID: sessionID}
	if *printOutput {
		var snapshot formatSessionSnapshot
		d.call(func() {
			if state := d.sessions[sessionID]; state != nil {
				snapshot = state.formatSnapshot()
			}
		})
		if len(snapshot.Panes) == 0 {
			return sessionCommandResult{}, errors.New("created session has no initial pane")
		}
		result.stdout = []byte(expandFormat(*format, formatContext{session: &snapshot, pane: &snapshot.Panes[0]}) + "\n")
	}
	if !*detached && issueBootstrap {
		result.bootstrap = &operation.bootstrap
	}
	return result, nil
}

func (d *Daemon) commandAttachSession(args []string, issueBootstrap bool) (sessionCommandResult, error) {
	fs := commandFlagSet("attach-session")
	target := fs.String("t", "", "session target")
	if err := fs.Parse(args); err != nil {
		return sessionCommandResult{}, err
	}
	if *target == "" || len(fs.Args()) != 0 {
		return sessionCommandResult{}, errors.New("attach-session requires -t <session-id-or-name>")
	}
	parsed, err := parseSessionTarget(*target)
	if err != nil {
		return sessionCommandResult{}, err
	}
	parsed.detached = !issueBootstrap
	operation, err := d.executeSessionOperation("connect-session", parsed)
	if err != nil {
		return sessionCommandResult{}, err
	}
	return sessionCommandResult{bootstrap: &operation.bootstrap, sessionID: operation.sessionID}, nil
}

func (d *Daemon) commandRestoreSession(args []string, issueBootstrap bool) (sessionCommandResult, error) {
	fs := commandFlagSet("restore-session")
	target := fs.String("t", "", "persisted session name")
	name := fs.String("s", "", "new session name")
	mode := fs.String("commands", "prepare", "restore command mode")
	run := fs.Bool("run", false, "run restored commands")
	if err := fs.Parse(args); err != nil {
		return sessionCommandResult{}, err
	}
	if *target == "" || len(fs.Args()) != 0 {
		return sessionCommandResult{}, errors.New("restore-session requires -t <session-name>")
	}
	restoreMode, err := resolveRestoreCommandMode(fs, "restore-session", *mode, *run)
	if err != nil {
		return sessionCommandResult{}, err
	}
	if *name != "" {
		if err := validateSessionName(*name); err != nil {
			return sessionCommandResult{}, err
		}
	}
	operation, err := d.executeSessionOperation("restore-session", commandSessionTarget{
		name:        *target,
		newName:     *name,
		restoreMode: restoreMode,
		detached:    !issueBootstrap,
	})
	if err != nil {
		return sessionCommandResult{}, err
	}
	return sessionCommandResult{bootstrap: &operation.bootstrap, sessionID: operation.sessionID}, nil
}

func resolveRestoreCommandMode(fs *flag.FlagSet, commandName, raw string, run bool) (restoreCommandMode, error) {
	if run {
		commandsSet := false
		fs.Visit(func(f *flag.Flag) {
			commandsSet = commandsSet || f.Name == "commands"
		})
		if commandsSet {
			return "", fmt.Errorf("%s --run cannot be combined with --commands", commandName)
		}
		raw = string(restoreCommandsRun)
	}
	return parseRestoreCommandMode(raw)
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

func (d *Daemon) saveSessionOutput(sessionID uint64, output string, force bool, callerWorkingDirectory string) ([]byte, error) {
	if sessionID == 0 {
		return nil, errSessionUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionPersistenceTimeout)
	defer cancel()
	observer := d.processObserver
	if observer == nil {
		observer = NewProcessObserver()
	}
	captured, err := d.captureSessionID(sessionID, ctx, observer)
	if err != nil {
		return nil, fmt.Errorf("capture session: %w", err)
	}
	persisted, err := sessionPlanFromCapture(captured)
	if err != nil {
		return nil, err
	}
	path, err := resolveCommandFilePath(output, captured.SessionRoot)
	if err != nil {
		return nil, err
	}
	report, err := writeUserMejaFile(path, persisted, force)
	if err != nil {
		return nil, err
	}
	var outputText strings.Builder
	outputText.WriteString("Saved session.\n")
	fmt.Fprintf(&outputText, "Session root: %s\n", captured.SessionRoot)
	fmt.Fprintf(&outputText, "Written to: %s\n", path)
	if callerWorkingDirectory != "" && filepath.Clean(callerWorkingDirectory) != filepath.Clean(captured.SessionRoot) {
		fmt.Fprintf(&outputText, "\nWarning: save was run from the current directory:\n  %s\nwhich differs from the current session root:\n  %s\n", callerWorkingDirectory, captured.SessionRoot)
		outputText.WriteString("\nIf the current directory is the intended project root, run `meja set-root .` here and save again. This makes reconstructed pane paths relative to that project root and makes reconstruction more portable if the project directory is mirrored on another machine.\n")
	}
	if report.AbsolutePanePaths > 0 {
		fmt.Fprintf(&outputText, "\nNote: %d pane", report.AbsolutePanePaths)
		verb := " uses"
		if report.AbsolutePanePaths != 1 {
			outputText.WriteByte('s')
			verb = " use"
		}
		outputText.WriteString(verb + " an absolute path outside the session root.\nThis file may not restore portably on another machine.\n")
	}
	outputText.WriteString("\nNote: command capture is best-effort.\n")
	outputText.WriteString("  - Review commands before restoring.\n")
	outputText.WriteString("  - Remove sensitive values before publishing.\n")
	return []byte(outputText.String()), nil
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

func (d *Daemon) listSessionsOutput(args []string) ([]byte, error) {
	fs := commandFlagSet("list-sessions")
	format := fs.String("F", "", "format")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(fs.Args()) != 0 {
		return nil, errors.New("list-sessions accepts no positional arguments")
	}
	operation, err := d.executeSessionOperation("list-sessions", commandSessionTarget{})
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	formatProvided := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == "F" {
			formatProvided = true
		}
	})
	if formatProvided {
		for _, session := range operation.sessions {
			line := expandFormat(*format, formatContext{session: &session.format, pane: session.format.ActivePane})
			if _, err := fmt.Fprintln(&output, line); err != nil {
				return nil, err
			}
		}
		return output.Bytes(), nil
	}
	if _, err := fmt.Fprintln(&output, "Active Sessions"); err != nil {
		return nil, err
	}
	table := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tNAME\tSTATUS"); err != nil {
		return nil, err
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
			return nil, err
		}
	}
	if err := table.Flush(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func commandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func commandArgsContainFlag(args []string, flagName string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == flagName || strings.HasPrefix(arg, flagName+"=") {
			return true
		}
	}
	return false
}

func newSessionRequestsDetached(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		switch {
		case arg == "-d" || arg == "--d":
			return true
		case strings.HasPrefix(arg, "-d=") || strings.HasPrefix(arg, "--d="):
			value, err := strconv.ParseBool(strings.SplitN(arg, "=", 2)[1])
			return err == nil && value
		}
	}
	return false
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

func (d *Daemon) executeSessionOperation(operation string, target commandSessionTarget) (commandOperationResult, error) {
	if operation == "list-sessions" {
		type listedSession struct {
			id    uint64
			state *SessionState
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
				snapshot := listed.state.formatSnapshot()
				sessions = append(sessions, commandSessionInfo{id: listed.id, name: snapshot.Name, attached: snapshot.Attached, format: snapshot})
			}
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].id < sessions[j].id })
		return commandOperationResult{sessions: sessions}, nil
	}

	restoring := operation == "restore-session"
	var restored SessionPlan
	var restoredPersistence *SessionPersistence
	if restoring {
		if target.restoreMode == "" {
			target.restoreMode = restoreCommandsPrepare
		}
		switch target.restoreMode {
		case restoreCommandsPrepare, restoreCommandsSkip, restoreCommandsRun:
		default:
			return commandOperationResult{}, fmt.Errorf("invalid restore command mode %q", target.restoreMode)
		}
		var err error
		if target.file != "" {
			restored, err = readUserSessionPlan(target.file)
		} else {
			if target.id != 0 || target.name == "" {
				return commandOperationResult{}, errors.New("restore requires a session name")
			}
			if err := validateSessionName(target.name); err != nil {
				return commandOperationResult{}, err
			}
			var persistence SessionPersistence
			persistence, err = readSessionPersistence(filepath.Join(d.sessionPersistenceDir, target.name+".session.meja"), target.name)
			if err == nil {
				restoredPersistence = &persistence
				restored = persistence.Plan
				restored.Name = persistence.Name
				restored.Root = persistence.Root
			}
		}
		if err != nil {
			return commandOperationResult{}, err
		}
		if target.newName != "" {
			restored.Name = target.newName
		}
		target.name = restored.Name
	}

	var sessionID uint64
	var operationErr error
	var cleanupPlan sessionShutdownPlan
	d.call(func() {
		var session *SessionState
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
			d.ensureSessionGroupInActor(session)
			d.startPersistence(d.sessionPersistenceDir)
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
			var existingGroup *GroupState
			if restoredPersistence != nil && restoredPersistence.GroupID != 0 {
				existingGroup = d.persistenceGroups[restoredPersistence.GroupID]
			}
			if existingGroup == nil {
				d.ensureSessionGroupInActor(session)
			}
			// Register the passive state before restoration starts panes. Live
			// ClientInstances resolve their state through this daemon registry.
			d.sessions[d.nextID] = session
			if existingGroup != nil {
				operationErr = d.restoreSessionView(session, *restoredPersistence)
			} else {
				operationErr = d.restoreSessionPlan(session, restored, restoredPersistence, target.restoreMode)
				if operationErr == nil && restoredPersistence != nil && restoredPersistence.GroupID != 0 {
					if d.persistenceGroups == nil {
						d.persistenceGroups = make(map[uint64]*GroupState)
					}
					d.persistenceGroups[restoredPersistence.GroupID] = session.group
				}
			}
			if operationErr != nil {
				// We are already inside the daemon transaction. Remove the
				// registry entry now; process termination is handled after the
				// transaction returns.
				cleanupPlan = d.shutdownSessionInActor(session)
				return
			}
			d.startPersistence(d.sessionPersistenceDir)
			d.reserveSessionName(session, restored.Name)
			d.nextID++
		default:
			operationErr = fmt.Errorf("unsupported session operation %q", operation)
		}
		if operationErr == nil && session != nil {
			sessionID = session.ID
		}
	})
	d.applySessionShutdownPlan(cleanupPlan)
	if operationErr != nil {
		return commandOperationResult{}, operationErr
	}
	if target.detached {
		return commandOperationResult{sessionID: sessionID}, nil
	}
	port, encodedToken, expires, err := d.issueAttachGrant(sessionID)
	if err != nil {
		_ = d.shutdownSessionID(sessionID, defaultPaneTerminationTimeouts)
		return commandOperationResult{}, err
	}
	return commandOperationResult{bootstrap: protocol.CommandBootstrap{
		Version:        protocol.CommandBootstrapVersion,
		Port:           port,
		AttachToken:    encodedToken,
		ExpiresAt:      expires,
		CertSPKISHA256: d.certHash,
	}, sessionID: sessionID}, nil
}
