package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"tali/internal/client"
	"tali/internal/control"
)

func main() {
	port := intFlagValue{value: 4433}
	var (
		cwd            = flag.String("cwd", "", "remote working directory")
		identity       = flag.String("i", "", "path to SSH identity file")
		ctrlPath       = flag.String("ctrl-path", "tali-ctrl", "remote tali-ctrl executable path")
		sessionShort   = flag.String("s", "", "reconnect to an existing numeric session ID")
		sessionLong    = flag.String("session-id", "", "reconnect to an existing numeric session ID")
		debugRender    = flag.Bool("debug-render", false, "enable client redraw logging")
		debugRenderLog = flag.String("debug-render-log", "", "write client redraw logs to this file")
	)
	flag.Var(&port, "port", "remote port")

	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: tali [flags] [user@]host [-s sessionId] [-- command args...]\n")
		os.Exit(2)
	}

	target, err := client.ParseTarget(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse target: %v\n", err)
		os.Exit(1)
	}

	sessionRaw := *sessionShort
	if *sessionLong != "" {
		if sessionRaw != "" && sessionRaw != *sessionLong {
			fmt.Fprintln(os.Stderr, "tali: conflicting session IDs")
			os.Exit(2)
		}
		sessionRaw = *sessionLong
	}
	var cmdArgs []string
	var sessionAfterTarget string
	if len(args) > 1 {
		cmdArgs, sessionAfterTarget, err = parseAfterTarget(args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "tali: %v\n", err)
			os.Exit(2)
		}
	}
	if sessionAfterTarget != "" {
		if sessionRaw != "" && sessionRaw != sessionAfterTarget {
			fmt.Fprintln(os.Stderr, "tali: conflicting session IDs")
			os.Exit(2)
		}
		sessionRaw = sessionAfterTarget
	}
	var sessionID uint64
	if sessionRaw != "" {
		sessionID, err = control.ParseSessionID(sessionRaw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tali: invalid session ID: %v\n", err)
			os.Exit(2)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	cfg := client.Config{
		Target:             target,
		Port:               port.value,
		PortSet:            port.set,
		IdentityFile:       *identity,
		CtrlPath:           *ctrlPath,
		SessionID:          sessionID,
		DebugRender:        *debugRender,
		DebugRenderLogPath: *debugRenderLog,
		Cwd:                *cwd,
		Argv:               cmdArgs,
		Stdin:              os.Stdin,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
	}

	if err := client.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "tali: %v\n", err)
		os.Exit(1)
	}
}

func parseAfterTarget(args []string) (commandArgs []string, sessionID string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(commandArgs, args[i+1:]...), sessionID, nil
		}
		if arg == "-s" || arg == "--session-id" {
			if i+1 >= len(args) || args[i+1] == "" {
				return nil, "", fmt.Errorf("%s requires a numeric session ID", arg)
			}
			sessionID = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "-s=") {
			sessionID = strings.TrimPrefix(arg, "-s=")
			continue
		}
		if strings.HasPrefix(arg, "--session-id=") {
			sessionID = strings.TrimPrefix(arg, "--session-id=")
			continue
		}
		commandArgs = append(commandArgs, arg)
	}
	return commandArgs, sessionID, nil
}

type intFlagValue struct {
	value int
	set   bool
}

func (f *intFlagValue) String() string {
	return strconv.Itoa(f.value)
}

func (f *intFlagValue) Set(raw string) error {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return err
	}
	f.value = v
	f.set = true
	return nil
}
