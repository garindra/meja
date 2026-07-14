package client

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"tali/internal/control"
)

func fetchBootstrap(ctx context.Context, cfg Config) (control.Bootstrap, error) {
	if cfg.Local {
		socket, err := cfg.SocketSelector.Resolve()
		if err != nil {
			return control.Bootstrap{}, err
		}
		if cfg.SessionID != 0 {
			return control.Call(ctx, socket, "connect-session", cfg.SessionID)
		}
		executable, err := control.CurrentExecutable()
		if err != nil {
			return control.Bootstrap{}, err
		}
		return control.StartSession(ctx, executable, cfg.SocketSelector)
	}
	return fetchRemoteBootstrap(ctx, cfg)
}

func fetchRemoteBootstrap(ctx context.Context, cfg Config) (control.Bootstrap, error) {
	remotePath := cfg.RemotePath
	if remotePath == "" {
		remotePath = "tali"
	}
	command, err := controllerCommand(remotePath, cfg.SocketSelector, cfg.SessionID)
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return control.Bootstrap{}, sshCommandError("SSH bootstrap failed", err, stderr.String())
	}
	if stderr.Len() > 0 && cfg.Stderr != nil {
		_, _ = stderr.WriteTo(cfg.Stderr)
	}
	bootstrap, err := control.ParseBootstrapOutput(stdout.Bytes())
	if err != nil {
		return control.Bootstrap{}, err
	}
	return bootstrap, nil
}

func resolveConnectionHostname(ctx context.Context, cfg Config) (string, error) {
	if cfg.Local {
		return "127.0.0.1", nil
	}
	return resolveSSHHostname(ctx, cfg.Target)
}

func resolveSSHHostname(ctx context.Context, target Target) (string, error) {
	sshTarget := target.Original
	if sshTarget == "" {
		sshTarget = target.Hostname
		if target.Username != "" {
			sshTarget = target.Username + "@" + sshTarget
		}
	}
	cmd := exec.CommandContext(ctx, "ssh", "-G", sshTarget)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve SSH hostname: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[0], "hostname") && fields[1] != "" {
			return fields[1], nil
		}
	}
	if target.Hostname == "" {
		return "", fmt.Errorf("resolve SSH hostname: OpenSSH returned no hostname")
	}
	return target.Hostname, nil
}

func controllerCommand(remotePath string, selector control.SocketSelector, sessionID uint64) (string, error) {
	command, err := controllerCommandPrefix(remotePath, selector)
	if err != nil {
		return "", err
	}
	if sessionID == 0 {
		return command + " __control-v1 start-session", nil
	}
	if _, err := control.ParseSessionID(strconv.FormatUint(sessionID, 10)); err != nil {
		return "", err
	}
	return command + " __control-v1 connect-session " + strconv.FormatUint(sessionID, 10), nil
}

func controllerCommandPrefix(remotePath string, selector control.SocketSelector) (string, error) {
	if remotePath == "" {
		remotePath = "tali"
	}
	selectorArgs, err := selector.Args()
	if err != nil {
		return "", err
	}
	command := control.ShellQuote(remotePath)
	for _, arg := range selectorArgs {
		command += " " + control.ShellQuote(arg)
	}
	return command, nil
}

func ListSessions(ctx context.Context, cfg Config) ([]uint64, error) {
	if cfg.Local {
		socket, err := cfg.SocketSelector.Resolve()
		if err != nil {
			return nil, err
		}
		return control.ListSessions(ctx, socket)
	}
	command, err := controllerCommandPrefix(cfg.RemotePath, cfg.SocketSelector)
	if err != nil {
		return nil, err
	}
	command += " __control-v1 list-sessions"
	target := cfg.Target.Original
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, sshCommandError("SSH session list failed", err, stderr.String())
	}
	if stderr.Len() > 0 && cfg.Stderr != nil {
		_, _ = stderr.WriteTo(cfg.Stderr)
	}
	return control.ParseSessionListOutput(stdout.Bytes())
}

func sshCommandError(operation string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, detail)
}
