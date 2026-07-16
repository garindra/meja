package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

type commandResult struct {
	stdout    []byte
	stderr    []byte
	bootstrap *protocol.CommandBootstrap
	exitCode  int
}

const defaultProfile = "default"

type SocketSelector struct {
	Profile string
	Path    string
}

func (s SocketSelector) Normalize() (SocketSelector, error) {
	if s.Profile != "" && s.Path != "" {
		return SocketSelector{}, errors.New("-L and -S are mutually exclusive")
	}
	if s.Path != "" {
		if !filepath.IsAbs(s.Path) {
			return SocketSelector{}, errors.New("-S requires an absolute socket path")
		}
		return SocketSelector{Path: filepath.Clean(s.Path)}, nil
	}
	profile := s.Profile
	if profile == "" {
		profile = defaultProfile
	}
	if err := validateProfile(profile); err != nil {
		return SocketSelector{}, err
	}
	return SocketSelector{Profile: profile}, nil
}

func (s SocketSelector) Resolve() (string, error) {
	normalized, err := s.Normalize()
	if err != nil {
		return "", err
	}
	if normalized.Path != "" {
		return normalized.Path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".meja", normalized.Profile, "meja.sock"), nil
}

func (s SocketSelector) Args() ([]string, error) {
	normalized, err := s.Normalize()
	if err != nil {
		return nil, err
	}
	if normalized.Path != "" {
		return []string{"-S", normalized.Path}, nil
	}
	return []string{"-L", normalized.Profile}, nil
}

func validateProfile(profile string) error {
	if profile == "" || profile == "." || profile == ".." {
		return errors.New("profile must be a non-empty name")
	}
	for _, r := range profile {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("invalid profile %q: use only letters, digits, '.', '_' or '-'", profile)
	}
	return nil
}

func executeCommand(ctx context.Context, cfg Config) (commandResult, error) {
	request := protocol.CommandRequest{
		Args:             cfg.CommandArgs,
		WorkingDirectory: cfg.Cwd,
		TerminalCols:     cfg.TerminalCols,
		TerminalRows:     cfg.TerminalRows,
	}
	if cfg.Local {
		return executeLocalCommand(ctx, cfg.SocketSelector, request)
	}
	return executeRemoteCommand(ctx, cfg, request)
}

func consumeCommandResult(cfg Config, result commandResult) (*protocol.CommandBootstrap, error) {
	if len(result.stdout) > 0 && cfg.Stdout != nil {
		if _, err := cfg.Stdout.Write(result.stdout); err != nil {
			return nil, err
		}
	}
	if result.exitCode != 0 {
		message := strings.TrimSpace(string(result.stderr))
		if message == "" {
			message = fmt.Sprintf("command exited with status %d", result.exitCode)
		}
		return nil, errors.New(message)
	}
	if len(result.stderr) > 0 && cfg.Stderr != nil {
		if _, err := cfg.Stderr.Write(result.stderr); err != nil {
			return nil, err
		}
	}
	if result.bootstrap == nil {
		return nil, nil
	}
	if err := result.bootstrap.Validate(time.Now()); err != nil {
		return nil, err
	}
	return result.bootstrap, nil
}

func ForwardCommand(ctx context.Context, selector SocketSelector, input io.Reader, output io.Writer) error {
	request, err := protocol.ReadCommandRequest(input)
	if err != nil {
		return err
	}
	if request.WorkingDirectory == "" {
		request.WorkingDirectory, _ = os.Getwd()
	}
	socket, err := selector.Resolve()
	if err != nil {
		return err
	}
	conn, err := dialCommandServer(ctx, socket)
	if err != nil {
		if !commandMayStartServer(request.Args) {
			return fmt.Errorf("meja server unavailable at %s", socket)
		}
		if err := startCommandServer(selector); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			conn, err = dialCommandServer(ctx, socket)
			if err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if err != nil {
			return err
		}
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if err := protocol.WriteCommandRequest(conn, request); err != nil {
		return err
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseWrite()
	}
	_, err = io.Copy(output, conn)
	return err
}

func executeLocalCommand(ctx context.Context, selector SocketSelector, request protocol.CommandRequest) (commandResult, error) {
	socket, err := selector.Resolve()
	if err != nil {
		return commandResult{}, err
	}
	conn, err := dialCommandServer(ctx, socket)
	if err != nil {
		if !commandMayStartServer(request.Args) {
			return commandResult{}, fmt.Errorf("meja server unavailable at %s", socket)
		}
		if startErr := startCommandServer(selector); startErr != nil {
			return commandResult{}, startErr
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			conn, err = dialCommandServer(ctx, socket)
			if err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if err != nil {
			return commandResult{}, fmt.Errorf("meja server unavailable at %s", socket)
		}
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if err := protocol.WriteCommandRequest(conn, request); err != nil {
		return commandResult{}, err
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseWrite()
	}
	return readCommandResult(conn)
}

func commandMayStartServer(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "new", "new-session", "restore", "restore-session":
		return true
	default:
		return false
	}
}

func executeRemoteCommand(ctx context.Context, cfg Config, request protocol.CommandRequest) (commandResult, error) {
	remotePath := cfg.RemotePath
	if remotePath == "" {
		remotePath = "meja"
	}
	remoteCommand, err := sshForwardCommand(remotePath, cfg.SocketSelector)
	if err != nil {
		return commandResult{}, err
	}
	var input bytes.Buffer
	if err := protocol.WriteCommandRequest(&input, request); err != nil {
		return commandResult{}, err
	}
	target := cfg.Target.Original
	if target == "" {
		target = cfg.Target.Hostname
		if cfg.Target.Username != "" {
			target = cfg.Target.Username + "@" + target
		}
	}
	args := make([]string, 0, 8)
	if cfg.IdentityFile != "" {
		args = append(args, "-i", cfg.IdentityFile)
	}
	if cfg.PortSet {
		args = append(args, "-p", fmt.Sprintf("%d", cfg.Port))
	}
	args = append(args, target, remoteCommand)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = &input
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return commandResult{}, sshCommandError("SSH command forwarding failed", err, stderr.String())
	}
	result, err := readCommandResult(&stdout)
	if err != nil {
		return commandResult{}, err
	}
	if stderr.Len() > 0 && cfg.Stderr != nil {
		_, _ = stderr.WriteTo(cfg.Stderr)
	}
	return result, nil
}

func sshForwardCommand(remotePath string, selector SocketSelector) (string, error) {
	selectorArgs, err := selector.Args()
	if err != nil {
		return "", err
	}
	command := shellQuote(remotePath)
	for _, arg := range selectorArgs {
		command += " " + shellQuote(arg)
	}
	return command + " __ssh-forward-v1", nil
}

func shellQuote(raw string) string {
	return "'" + strings.ReplaceAll(raw, "'", "'\\''") + "'"
}

func readCommandResult(reader io.Reader) (commandResult, error) {
	var result commandResult
	for {
		frame, err := protocol.ReadCommandFrame(reader)
		if err != nil {
			return commandResult{}, err
		}
		switch frame.Type {
		case protocol.CommandFrameStdout:
			result.stdout = append(result.stdout, frame.Data...)
		case protocol.CommandFrameStderr:
			result.stderr = append(result.stderr, frame.Data...)
		case protocol.CommandFrameAttach:
			if frame.Bootstrap == nil {
				return commandResult{}, errors.New("attach response omitted bootstrap")
			}
			result.bootstrap = frame.Bootstrap
		case protocol.CommandFrameExit:
			result.exitCode = frame.ExitCode
			return result, nil
		default:
			return commandResult{}, fmt.Errorf("unknown command frame %q", frame.Type)
		}
	}
}

func dialCommandServer(ctx context.Context, socket string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", socket)
}

func startCommandServer(selector SocketSelector) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	args, err := selector.Args()
	if err != nil {
		return err
	}
	args = append(args, "start-server")
	cmd := exec.Command(executable, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		_ = devNull.Close()
		return err
	}
	_ = cmd.Process.Release()
	return devNull.Close()
}
