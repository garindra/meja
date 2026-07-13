package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"tali/internal/control"
	"tali/internal/server"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tali-ctrl server|start-session|connect-session <numeric-session-id>|stop-server")
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	switch args[0] {
	case "server":
		if len(args) != 1 {
			flag.Usage()
			os.Exit(2)
		}
		if err := server.Run(ctx, server.Config{Stdout: os.Stdout, Stderr: os.Stderr}); err != nil {
			fmt.Fprintf(os.Stderr, "tali-ctrl server: %v\n", err)
			os.Exit(1)
		}
	case "start-session":
		if len(args) != 1 {
			flag.Usage()
			os.Exit(2)
		}
		exe, err := control.CurrentExecutable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		bootstrap, err := control.StartSession(ctx, exe)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tali-ctrl start-session: %v\n", err)
			os.Exit(1)
		}
		if err := control.WriteBootstrap(os.Stdout, bootstrap); err != nil {
			fmt.Fprintf(os.Stderr, "tali-ctrl start-session: %v\n", err)
			os.Exit(1)
		}
	case "connect-session":
		if len(args) != 2 {
			flag.Usage()
			os.Exit(2)
		}
		id, err := control.ParseSessionID(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "tali-ctrl connect-session: %v\n", err)
			os.Exit(2)
		}
		socket, err := control.ControlPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		bootstrap, err := control.Call(ctx, socket, "connect-session", id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tali-ctrl connect-session: %v\n", err)
			os.Exit(1)
		}
		if err := control.WriteBootstrap(os.Stdout, bootstrap); err != nil {
			fmt.Fprintf(os.Stderr, "tali-ctrl connect-session: %v\n", err)
			os.Exit(1)
		}
	case "stop-server":
		if len(args) != 1 {
			flag.Usage()
			os.Exit(2)
		}
		socket, err := control.ControlPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		pid, err := control.StopServer(ctx, socket)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tali-ctrl stop-server: %v\n", err)
			os.Exit(1)
		}
		if pid > 0 {
			fmt.Fprintf(os.Stdout, "stopped server PID %d\n", pid)
		} else {
			fmt.Fprintln(os.Stdout, "stopped server (PID unavailable)")
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}
