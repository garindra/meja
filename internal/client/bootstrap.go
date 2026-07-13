package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"

	"tali/internal/control"
)

func fetchBootstrap(ctx context.Context, cfg Config) (control.Bootstrap, error) {
	remotePath := cfg.CtrlPath
	if remotePath == "" {
		remotePath = "tali-ctrl"
	}
	command, err := controllerCommand(remotePath, cfg.SessionID)
	if err != nil {
		return control.Bootstrap{}, err
	}
	target := cfg.Target.Original
	if target == "" {
		if cfg.Target.Username != "" {
			target = cfg.Target.Username + "@"
		}
		target += cfg.Target.Hostname
	}
	args := make([]string, 0, 6)
	if cfg.IdentityFile != "" {
		args = append(args, "-i", cfg.IdentityFile)
	}
	if cfg.PortSet {
		args = append(args, "-p", fmt.Sprintf("%d", cfg.Port))
	}
	args = append(args, target, command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = cfg.Stdin
	cmd.Stderr = cfg.Stderr
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return control.Bootstrap{}, fmt.Errorf("SSH bootstrap failed: %w", err)
	}
	bootstrap, err := control.ParseBootstrapOutput(stdout.Bytes())
	if err != nil {
		return control.Bootstrap{}, err
	}
	return bootstrap, nil
}

func controllerCommand(remotePath string, sessionID uint64) (string, error) {
	if remotePath == "" {
		remotePath = "tali-ctrl"
	}
	if sessionID == 0 {
		return control.ShellQuote(remotePath) + " start-session", nil
	}
	if _, err := control.ParseSessionID(strconv.FormatUint(sessionID, 10)); err != nil {
		return "", err
	}
	return control.ShellQuote(remotePath) + " connect-session " + strconv.FormatUint(sessionID, 10), nil
}

func writeSSHDiagnostic(w io.Writer, message string) {
	if w != nil {
		_, _ = fmt.Fprintln(w, message)
	}
}
