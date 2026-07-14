package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"

	"tali/internal/client"
	"tali/internal/control"
	"tali/internal/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if usageErr, ok := err.(usageError); ok {
			if usageErr.text != "" {
				fmt.Fprintf(os.Stderr, "tali: %s\n", usageErr.text)
			}
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "tali: %v\n", err)
		os.Exit(1)
	}
}

type usageError struct{ text string }

func (e usageError) Error() string { return e.text }

const usage = `usage:
  tali [-L profile | -S socket-path]
  tali [-L profile | -S socket-path] <host> [-- command args...]
  tali [-L profile | -S socket-path] new [-s session-name] [-c directory] [options] [host] [-- command args...]
  tali [-L profile | -S socket-path] attach|a -t <session-id-or-name> [host]
  tali [-L profile | -S socket-path] ls [host]
  tali [-L profile | -S socket-path] server run|stop`

func run(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) error {
	selector, args, err := parseGlobalOptions(args)
	if err != nil {
		if err == flag.ErrHelp {
			fmt.Fprintln(stdout, usage)
			return nil
		}
		return usageError{err.Error()}
	}
	if len(args) == 0 {
		cfg := client.Config{Local: true, SocketSelector: selector, Stdin: stdin, Stdout: stdout, Stderr: stderr}
		if err := setDefaultLocalCwd(&cfg); err != nil {
			return err
		}
		return client.Run(ctx, cfg)
	}
	switch args[0] {
	case "new":
		return runNew(ctx, selector, args[1:], stdin, stdout, stderr)
	case "attach", "a":
		return runAttach(ctx, selector, args[1:], stdin, stdout, stderr)
	case "ls":
		return runList(ctx, selector, args[1:], stdin, stdout, stderr)
	case "server":
		return runServer(ctx, selector, args[1:], stdout, stderr)
	case "__control-v1":
		return runControl(ctx, selector, args[1:], stdout)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return nil
	default:
		return runNew(ctx, selector, args, stdin, stdout, stderr)
	}
}

func parseGlobalOptions(args []string) (control.SocketSelector, []string, error) {
	fs := flag.NewFlagSet("tali", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	profile := fs.String("L", "", "server profile")
	socket := fs.String("S", "", "exact server socket path")
	if err := fs.Parse(args); err != nil {
		return control.SocketSelector{}, nil, err
	}
	selector, err := (control.SocketSelector{Profile: *profile, Path: *socket}).Normalize()
	if err != nil {
		return control.SocketSelector{}, nil, err
	}
	return selector, fs.Args(), nil
}

type connectionFlags struct {
	identity       string
	remotePath     string
	port           intFlagValue
	debugRender    bool
	debugRenderLog string
}

func (f *connectionFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.identity, "i", "", "path to SSH identity file")
	fs.StringVar(&f.remotePath, "remote-path", "tali", "remote tali executable path")
	f.port.value = 4433
	fs.Var(&f.port, "port", "SSH port")
	fs.BoolVar(&f.debugRender, "debug-render", false, "enable client redraw logging")
	fs.StringVar(&f.debugRenderLog, "debug-render-log", "", "write client redraw logs to this file")
}

func (f connectionFlags) config(selector control.SocketSelector, stdin *os.File, stdout, stderr io.Writer) client.Config {
	return client.Config{
		Port:               f.port.value,
		PortSet:            f.port.set,
		IdentityFile:       f.identity,
		RemotePath:         f.remotePath,
		SocketSelector:     selector,
		DebugRender:        f.debugRender,
		DebugRenderLogPath: f.debugRenderLog,
		Stdin:              stdin,
		Stdout:             stdout,
		Stderr:             stderr,
	}
}

func runNew(ctx context.Context, selector control.SocketSelector, args []string, stdin *os.File, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var flags connectionFlags
	flags.register(fs)
	sessionName := fs.String("s", "", "session name")
	var cwd string
	fs.StringVar(&cwd, "c", "", "starting directory for new panes")
	fs.StringVar(&cwd, "cwd", "", "starting directory for new panes")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	remaining := fs.Args()
	if *sessionName != "" {
		if err := control.ValidateSessionName(*sessionName); err != nil {
			return err
		}
	}
	cfg := flags.config(selector, stdin, stdout, stderr)
	cfg.SessionName = *sessionName
	cfg.Cwd = cwd
	if len(remaining) == 0 {
		cfg.Local = true
		if err := setDefaultLocalCwd(&cfg); err != nil {
			return err
		}
		return client.Run(ctx, cfg)
	}
	target, err := client.ParseTarget(remaining[0])
	if err != nil {
		return err
	}
	command, err := commandAfterTarget(remaining[1:])
	if err != nil {
		return err
	}
	cfg.Target = target
	cfg.Argv = command
	return client.Run(ctx, cfg)
}

func setDefaultLocalCwd(cfg *client.Config) error {
	if cfg.Cwd != "" {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve current working directory: %w", err)
	}
	cfg.Cwd = cwd
	return nil
}

func runAttach(ctx context.Context, selector control.SocketSelector, args []string, stdin *os.File, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var flags connectionFlags
	flags.register(fs)
	sessionRaw := fs.String("t", "", "session ID or name")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if *sessionRaw == "" {
		return usageError{"attach requires -t <session-id-or-name>"}
	}
	_, err := control.ParseSessionTarget(*sessionRaw)
	if err != nil {
		return err
	}
	remaining := fs.Args()
	if len(remaining) > 1 {
		return usageError{"attach accepts at most one host"}
	}
	cfg := flags.config(selector, stdin, stdout, stderr)
	cfg.SessionTarget = *sessionRaw
	if len(remaining) == 0 {
		cfg.Local = true
	} else {
		cfg.Target, err = client.ParseTarget(remaining[0])
		if err != nil {
			return err
		}
	}
	return client.Run(ctx, cfg)
}

func runList(ctx context.Context, selector control.SocketSelector, args []string, stdin *os.File, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var flags connectionFlags
	flags.register(fs)
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	remaining := fs.Args()
	if len(remaining) > 1 {
		return usageError{"ls accepts at most one host"}
	}
	cfg := flags.config(selector, stdin, stdout, stderr)
	var err error
	if len(remaining) == 0 {
		cfg.Local = true
	} else {
		cfg.Target, err = client.ParseTarget(remaining[0])
		if err != nil {
			return err
		}
	}
	sessions, err := client.ListSessions(ctx, cfg)
	if err != nil {
		return err
	}
	return writeSessionList(stdout, sessions)
}

func writeSessionList(w io.Writer, sessions []control.SessionInfo) error {
	if _, err := fmt.Fprintln(w, "Active Sessions"); err != nil {
		return err
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tNAME\tSTATUS"); err != nil {
		return err
	}
	for _, session := range sessions {
		name := session.Name
		if name == "" {
			name = "<unnamed>"
		}
		status := "detached"
		if session.Attached {
			status = "attached"
		}
		if _, err := fmt.Fprintf(table, "%d\t%s\t%s\n", session.ID, name, status); err != nil {
			return err
		}
	}
	return table.Flush()
}

func runServer(ctx context.Context, selector control.SocketSelector, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		return usageError{"server requires run or stop"}
	}
	switch args[0] {
	case "run":
		socket, err := selector.Resolve()
		if err != nil {
			return err
		}
		return server.Run(ctx, server.Config{ControlPath: socket, Stdout: stdout, Stderr: stderr})
	case "stop":
		socket, err := selector.Resolve()
		if err != nil {
			return err
		}
		pid, err := control.StopServer(ctx, socket)
		if err != nil {
			return err
		}
		if pid > 0 {
			fmt.Fprintf(stdout, "stopped server PID %d\n", pid)
		} else {
			fmt.Fprintln(stdout, "stopped server (PID unavailable)")
		}
		return nil
	default:
		return usageError{"server requires run or stop"}
	}
}

func runControl(ctx context.Context, selector control.SocketSelector, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{"__control-v1 requires an operation"}
	}
	switch args[0] {
	case "start-session":
		if len(args) > 2 {
			return usageError{"__control-v1 start-session accepts at most one session name"}
		}
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		executable, err := control.CurrentExecutable()
		if err != nil {
			return err
		}
		bootstrap, err := control.StartSession(ctx, executable, selector, name)
		if err != nil {
			return err
		}
		return control.WriteBootstrap(stdout, bootstrap)
	case "connect-session":
		if len(args) != 2 {
			return usageError{"__control-v1 connect-session requires a session ID or name"}
		}
		socket, err := selector.Resolve()
		if err != nil {
			return err
		}
		bootstrap, err := control.ConnectSession(ctx, socket, args[1])
		if err != nil {
			return err
		}
		return control.WriteBootstrap(stdout, bootstrap)
	case "list-sessions":
		if len(args) != 1 {
			return usageError{"__control-v1 list-sessions accepts no arguments"}
		}
		socket, err := selector.Resolve()
		if err != nil {
			return err
		}
		sessions, err := control.ListSessions(ctx, socket)
		if err != nil {
			return err
		}
		return control.WriteSessionList(stdout, sessions)
	default:
		return usageError{"unsupported __control-v1 operation"}
	}
}

func commandAfterTarget(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if args[0] != "--" {
		return nil, usageError{"remote command must follow --"}
	}
	return append([]string(nil), args[1:]...), nil
}

type intFlagValue struct {
	value int
	set   bool
}

func (f *intFlagValue) String() string { return strconv.Itoa(f.value) }

func (f *intFlagValue) Set(raw string) error {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return err
	}
	f.value = value
	f.set = true
	return nil
}
