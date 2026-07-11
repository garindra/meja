package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"tali/internal/client"
)

func main() {
	port := intFlagValue{value: 4433}
	var (
		ca             = flag.String("ca", "", "path to CA certificate file")
		cwd            = flag.String("cwd", "", "remote working directory")
		identity       = flag.String("i", "", "path to SSH identity file")
		debugRender    = flag.Bool("debug-render", false, "enable client redraw logging")
		debugRenderLog = flag.String("debug-render-log", "", "write client redraw logs to this file")
	)
	flag.Var(&port, "port", "remote port")

	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: tali [flags] [user@]host [-- command args...]\n")
		os.Exit(2)
	}

	target, err := client.ParseTarget(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse target: %v\n", err)
		os.Exit(1)
	}

	var cmdArgs []string
	if len(args) > 1 {
		cmdArgs = args[1:]
		if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
			cmdArgs = cmdArgs[1:]
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	cfg := client.Config{
		Target:             target,
		Port:               port.value,
		PortSet:            port.set,
		CAFile:             *ca,
		IdentityFile:       *identity,
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
