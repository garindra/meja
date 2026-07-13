package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	ProtocolVersion = 1
	BootstrapPrefix = "TALI_BOOTSTRAP_V1 "
	DefaultUDPMin   = 60000
	DefaultUDPMax   = 61000
	controlTimeout  = 2 * time.Second
)

var (
	ErrDaemonUnavailable  = errors.New("tali daemon unavailable")
	ErrSessionUnavailable = errors.New("tali session unavailable")
)

// Bootstrap is the only data printed to stdout by tali-ctrl start-session and
// connect-session. Tokens are intentionally never included in error strings.
type Bootstrap struct {
	Version        int       `json:"version"`
	SessionID      uint64    `json:"sessionId"`
	Port           uint16    `json:"port"`
	AttachToken    string    `json:"attachToken"`
	ExpiresAt      time.Time `json:"expiresAt"`
	CertSPKISHA256 string    `json:"certSpkiSha256"`
}

func (b Bootstrap) Validate(now time.Time) error {
	if b.Version != ProtocolVersion {
		return fmt.Errorf("unsupported bootstrap version %d", b.Version)
	}
	if b.SessionID == 0 || b.Port == 0 || b.Port < DefaultUDPMin || b.Port > DefaultUDPMax {
		return errors.New("invalid bootstrap session or port")
	}
	if b.ExpiresAt.IsZero() || !b.ExpiresAt.After(now) {
		return errors.New("bootstrap has expired")
	}
	token, err := base64.RawURLEncoding.DecodeString(b.AttachToken)
	if err != nil || len(token) != 32 {
		return errors.New("invalid bootstrap attach token")
	}
	if len(b.CertSPKISHA256) != sha256HexLen {
		return errors.New("invalid certificate SPKI hash")
	}
	if _, err := hex.DecodeString(b.CertSPKISHA256); err != nil {
		return errors.New("invalid certificate SPKI hash")
	}
	return nil
}

const sha256HexLen = 64

func NewToken() ([]byte, error) {
	token := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return nil, err
	}
	return token, nil
}

func EncodeToken(token []byte) string { return base64.RawURLEncoding.EncodeToString(token) }

func EqualToken(encoded string, token []byte) bool {
	got, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare(got, token) == 1
}

func ControlPath() (string, error) {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		if filepath.IsAbs(runtimeDir) {
			return filepath.Join(runtimeDir, "tali", "control.sock"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".tali", "control.sock"), nil
}

type request struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	SessionID uint64 `json:"sessionId,omitempty"`
}

type response struct {
	Version   int        `json:"version"`
	OK        bool       `json:"ok"`
	Error     string     `json:"error,omitempty"`
	Bootstrap *Bootstrap `json:"bootstrap,omitempty"`
	PID       int        `json:"pid,omitempty"`
}

type Handler func(operation string, sessionID uint64) (Bootstrap, int, error)

// Serve accepts the small control RPC. The caller owns listener shutdown via
// ctx; malformed requests are answered without exposing token material.
func Serve(ctx context.Context, socket string, handler Handler) error {
	if err := EnsureSocketDir(socket); err != nil {
		return err
	}
	if err := removeStaleSocket(socket); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen control socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0o600); err != nil {
		return fmt.Errorf("protect control socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go serveConn(conn, handler)
	}
}

func serveConn(conn net.Conn, handler Handler) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlTimeout))
	var req request
	decoder := json.NewDecoder(io.LimitReader(conn, 64<<10))
	reply := response{Version: ProtocolVersion}
	if err := decoder.Decode(&req); err != nil || req.Version != ProtocolVersion {
		reply.Error = "invalid control request"
	} else {
		b, pid, err := handler(req.Operation, req.SessionID)
		if err != nil {
			reply.Error = err.Error()
		} else {
			reply.OK = true
			reply.Bootstrap = &b
			reply.PID = pid
		}
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

func Call(ctx context.Context, socket, operation string, sessionID uint64) (Bootstrap, error) {
	if operation != "create-session" && operation != "connect-session" {
		return Bootstrap{}, errors.New("unsupported control operation")
	}
	reply, err := call(ctx, socket, operation, sessionID)
	if err != nil {
		return Bootstrap{}, err
	}
	if reply.Bootstrap == nil {
		return Bootstrap{}, errors.New("control response omitted bootstrap")
	}
	if err := reply.Bootstrap.Validate(time.Now()); err != nil {
		return Bootstrap{}, fmt.Errorf("invalid bootstrap from daemon: %w", err)
	}
	return *reply.Bootstrap, nil
}

func StopServer(ctx context.Context, socket string) (int, error) {
	reply, err := call(ctx, socket, "stop-server", 0)
	if err != nil {
		return 0, err
	}
	return reply.PID, nil
}

func call(ctx context.Context, socket, operation string, sessionID uint64) (response, error) {
	dialCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
	if err != nil {
		return response{}, fmt.Errorf("%w: connect control socket", ErrDaemonUnavailable)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlTimeout))
	if err := json.NewEncoder(conn).Encode(request{Version: ProtocolVersion, Operation: operation, SessionID: sessionID}); err != nil {
		return response{}, fmt.Errorf("control request: %w", err)
	}
	var reply response
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&reply); err != nil {
		return response{}, fmt.Errorf("control response: %w", err)
	}
	if reply.Version != ProtocolVersion {
		return response{}, fmt.Errorf("unsupported control response version %d", reply.Version)
	}
	if !reply.OK {
		if reply.Error == "" {
			return response{}, ErrSessionUnavailable
		}
		return response{}, errors.New(reply.Error)
	}
	return reply, nil
}

func WriteBootstrap(w io.Writer, b Bootstrap) error {
	if err := b.Validate(time.Now()); err != nil {
		return err
	}
	payload, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s%s\n", BootstrapPrefix, payload)
	return err
}

func ParseBootstrapOutput(output []byte) (Bootstrap, error) {
	var found []byte
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, BootstrapPrefix) {
			if found != nil {
				return Bootstrap{}, errors.New("multiple bootstrap records")
			}
			found = []byte(strings.TrimSpace(strings.TrimPrefix(line, BootstrapPrefix)))
		}
	}
	if len(found) == 0 {
		return Bootstrap{}, errors.New("SSH command did not return a Tali bootstrap")
	}
	var b Bootstrap
	if err := json.Unmarshal(found, &b); err != nil {
		return Bootstrap{}, fmt.Errorf("parse bootstrap: %w", err)
	}
	if err := b.Validate(time.Now()); err != nil {
		return Bootstrap{}, fmt.Errorf("validate bootstrap: %w", err)
	}
	return b, nil
}

func StartSession(ctx context.Context, executable string) (Bootstrap, error) {
	socket, err := ControlPath()
	if err != nil {
		return Bootstrap{}, err
	}
	if b, callErr := Call(ctx, socket, "create-session", 0); callErr == nil {
		return b, nil
	} else if !isUnavailable(callErr) {
		return Bootstrap{}, callErr
	}
	if err := removeStaleSocket(socket); err != nil {
		return Bootstrap{}, err
	}
	if err := startDetachedServer(executable); err != nil {
		return Bootstrap{}, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, callErr := Call(ctx, socket, "create-session", 0)
		if callErr == nil {
			return b, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return Bootstrap{}, ErrDaemonUnavailable
}

func isUnavailable(err error) bool { return errors.Is(err, ErrDaemonUnavailable) }

func startDetachedServer(executable string) error {
	if executable == "" || !filepath.IsAbs(executable) {
		return errors.New("controller executable path is not absolute")
	}
	cmd := exec.Command(executable, "server")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open daemon stdio: %w", err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		_ = devNull.Close()
		return fmt.Errorf("start tali daemon: %w", err)
	}
	_ = cmd.Process.Release()
	_ = devNull.Close()
	return nil
}

func removeStaleSocket(socket string) error {
	info, err := os.Lstat(socket)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("control path is not a Unix socket")
	}
	if !ownedByCurrentUID(info) {
		return errors.New("control socket is not owned by the current user")
	}
	parentInfo, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		return fmt.Errorf("inspect control directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0o700 || !ownedByCurrentUID(parentInfo) {
		return errors.New("control socket directory is not a private directory")
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale control socket: %w", err)
	}
	return nil
}

func ownedByCurrentUID(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint32(os.Getuid()) == stat.Uid
}

func EnsureSocketDir(socket string) error {
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create control directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect control directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUID(info) {
		return errors.New("control directory is not private")
	}
	return nil
}

func ParseSessionID(raw string) (uint64, error) {
	if raw == "" || strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") {
		return 0, errors.New("session ID must be a numeric uint64")
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("session ID must be a numeric uint64")
	}
	return id, nil
}

// ShellQuote is used only for the remote command string passed to OpenSSH.
// It produces a single POSIX shell word and never interpolates user data.
func ShellQuote(raw string) string {
	return "'" + strings.ReplaceAll(raw, "'", "'\\''") + "'"
}

func CurrentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve controller executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("resolve controller executable: %w", err)
	}
	return exe, nil
}
