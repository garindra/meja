package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"tali/internal/server"
)

func main() {
	var (
		listen = flag.String("listen", ":4433", "listen address")
		cert   = flag.String("cert", "", "path to TLS certificate")
		key    = flag.String("key", "", "path to TLS private key")
	)

	flag.Parse()

	if *cert == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "usage: tali-server -listen :4433 -cert server.crt -key server.key")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	cfg := server.Config{
		ListenAddr: *listen,
		CertFile:   *cert,
		KeyFile:    *key,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}

	if err := server.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "tali-server: %v\n", err)
		os.Exit(1)
	}
}
