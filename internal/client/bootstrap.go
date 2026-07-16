package client

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/garindra/meja/internal/protocol"
)

func fetchBootstrap(ctx context.Context, cfg Config) (protocol.CommandBootstrap, error) {
	result, err := executeCommand(ctx, cfg)
	if err != nil {
		return protocol.CommandBootstrap{}, err
	}
	bootstrap, err := consumeCommandResult(cfg, result)
	if err != nil {
		return protocol.CommandBootstrap{}, err
	}
	if bootstrap == nil {
		return protocol.CommandBootstrap{}, errors.New("command did not attach a session")
	}
	return *bootstrap, nil
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

func sshCommandError(operation string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, detail)
}
