package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/garindra/meja/internal/client"
	"github.com/garindra/meja/internal/protocol"
	"github.com/garindra/meja/internal/server"
	"github.com/garindra/meja/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if usageErr, ok := err.(usageError); ok {
			if usageErr.text != "" {
				fmt.Fprintf(os.Stderr, "meja: %s\n", usageErr.text)
			}
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "meja: %v\n", err)
		os.Exit(1)
	}
}

type usageError struct{ text string }

func (e usageError) Error() string { return e.text }

const usage = `usage:
  meja version
  meja [transport-options] [command [command-args...]]

transport options (removed before forwarding):
  -L profile              select a named server socket
  -S socket-path          select an exact server socket
  -h, --host user@host    run the command on an SSH host
  -i identity-file        use an SSH identity file
  --port port             use an SSH port
  --remote-path path      remote meja executable (default: meja)

Transport options must appear before the command. With no command, Meja runs
new-session. Use --help for the server-backed command reference.`

func run(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) error {
	cfg, err := parseInvocation(args, stdin, stdout, stderr)
	if err != nil {
		return usageError{err.Error()}
	}
	command := cfg.CommandArgs[0]
	switch command {
	case "start-server":
		if !cfg.Local {
			return usageError{"start-server is local; invoke it through SSH to start a remote server"}
		}
		if len(cfg.CommandArgs) != 1 {
			return usageError{"start-server accepts no arguments"}
		}
		socket, err := cfg.SocketSelector.Resolve()
		if err != nil {
			return err
		}
		return server.Run(ctx, server.Config{ControlPath: socket, Stdout: stdout, Stderr: stderr})
	case "version":
		if len(cfg.CommandArgs) == 1 {
			fmt.Fprintf(stdout, "meja %s\n", version.Current())
			return nil
		}
		if len(cfg.CommandArgs) == 2 && cfg.CommandArgs[1] == "--verbose" {
			fmt.Fprintf(stdout, "meja:             %s\ncommand protocol: %d\nQUIC profile:     %s\n", version.Current(), protocol.CommandProtocolVersion, protocol.ALPN)
			return nil
		}
		return usageError{"version accepts only --verbose"}
	case "__ssh-forward-v1":
		if !cfg.Local || len(cfg.CommandArgs) != 1 {
			return usageError{"__ssh-forward-v1 accepts no arguments"}
		}
		return client.ForwardCommand(ctx, cfg.SocketSelector, stdin, stdout)
	}
	return client.Run(ctx, cfg)
}

type invocationOptions struct {
	profile    string
	socket     string
	host       string
	identity   string
	remotePath string
	port       int
	portSet    bool
}

func parseInvocation(args []string, stdin *os.File, stdout, stderr io.Writer) (client.Config, error) {
	options := invocationOptions{remotePath: "meja", port: 4433}
	callerSessionTarget := ""
	var callerPaneID uint64
	commandArgs := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			commandArgs = append(commandArgs, args[index:]...)
			break
		}
		name, inlineValue, hasInlineValue, recognized := transportOption(arg)
		if !recognized {
			// The first non-transport argument is the command name. From this
			// point on, command flags are opaque to the transport parser; in
			// particular, -h and -S may belong to the sub-command.
			commandArgs = append(commandArgs, args[index:]...)
			break
		}
		value := inlineValue
		if !hasInlineValue {
			index++
			if index >= len(args) {
				return client.Config{}, fmt.Errorf("%s requires a value", name)
			}
			value = args[index]
		}
		if value == "" {
			return client.Config{}, fmt.Errorf("%s requires a non-empty value", name)
		}
		if err := options.set(name, value); err != nil {
			return client.Config{}, err
		}
	}
	if options.host == "" && options.profile == "" && options.socket == "" {
		contextSocket := os.Getenv("MEJA_SOCKET")
		contextTarget := os.Getenv("MEJA_SESSION_TARGET")
		if contextSocket != "" && contextTarget != "" {
			options.socket = contextSocket
			callerSessionTarget = contextTarget
			if rawPaneID := os.Getenv("MEJA_PANE_ID"); rawPaneID != "" {
				parsedPaneID, parseErr := strconv.ParseUint(rawPaneID, 10, 64)
				if parseErr != nil || parsedPaneID == 0 {
					return client.Config{}, fmt.Errorf("invalid MEJA_PANE_ID %q", rawPaneID)
				}
				callerPaneID = parsedPaneID
			}
		}
	}
	selector, err := (client.SocketSelector{Profile: options.profile, Path: options.socket}).Normalize()
	if err != nil {
		return client.Config{}, err
	}
	if len(commandArgs) == 0 {
		commandArgs = []string{"new-session"}
	}
	cfg := client.Config{
		Local:               options.host == "",
		Port:                options.port,
		PortSet:             options.portSet,
		IdentityFile:        options.identity,
		RemotePath:          options.remotePath,
		SocketSelector:      selector,
		CallerSessionTarget: callerSessionTarget,
		CallerPaneID:        callerPaneID,
		CommandArgs:         commandArgs,
		Stdin:               stdin,
		Stdout:              stdout,
		Stderr:              stderr,
	}
	applyDebugEnvironment(&cfg)
	if options.host != "" {
		cfg.Target, err = client.ParseTarget(options.host)
		if err != nil {
			return client.Config{}, err
		}
	} else {
		cfg.Cwd, err = os.Getwd()
		if err != nil {
			return client.Config{}, fmt.Errorf("resolve current working directory: %w", err)
		}
	}
	return cfg, nil
}

func transportOption(arg string) (name, value string, hasValue, recognized bool) {
	name, value, hasValue = strings.Cut(arg, "=")
	switch name {
	case "-L", "-S", "-h", "--host", "-i", "--port", "-port", "--remote-path", "-remote-path":
		return name, value, hasValue, true
	default:
		return "", "", false, false
	}
}

func (options *invocationOptions) set(name, value string) error {
	switch name {
	case "-L":
		options.profile = value
	case "-S":
		options.socket = value
	case "-h", "--host":
		if options.host != "" {
			return fmt.Errorf("SSH host was specified more than once")
		}
		options.host = value
	case "-i":
		options.identity = value
	case "--remote-path", "-remote-path":
		options.remotePath = value
	case "--port", "-port":
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid SSH port %q", value)
		}
		options.port, options.portSet = port, true
	}
	return nil
}

func applyDebugEnvironment(cfg *client.Config) {
	cfg.RenderDiagnostics = cfg.RenderDiagnostics || debugEnvironmentEnabled("MEJA_DEBUG") || debugEnvironmentEnabled("MEJA_DEBUG_RENDER")
	if cfg.RenderDiagnosticsLogPath == "" {
		cfg.RenderDiagnosticsLogPath = os.Getenv("MEJA_DEBUG_LOG")
	}
	if cfg.RenderDiagnosticsLogPath != "" {
		cfg.RenderDiagnostics = true
	}
}

func debugEnvironmentEnabled(name string) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return false
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}
