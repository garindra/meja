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
	"unicode"
	"unicode/utf8"
)

const (
	ProtocolVersion   = 2
	BootstrapPrefix   = "TALI_BOOTSTRAP_V2 "
	SessionListPrefix = "TALI_SESSION_LIST_V2 "
	DefaultUDPMin     = 60000
	DefaultUDPMax     = 61000
	controlTimeout    = 2 * time.Second
)

var (
	ErrDaemonUnavailable  = errors.New("tali daemon unavailable")
	ErrSessionUnavailable = errors.New("tali session unavailable")
	ErrServerRunning      = errors.New("tali server already running")
)

const DefaultProfile = "default"

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
		profile = DefaultProfile
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
	return filepath.Join(home, ".tali", normalized.Profile, "tali.sock"), nil
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

// Bootstrap is the only data printed to stdout by the versioned control
// start/connect operations. Tokens are intentionally never included in errors.
type Bootstrap struct {
	Version        int       `json:"version"`
	SessionID      uint64    `json:"sessionId"`
	Port           uint16    `json:"port"`
	AttachToken    string    `json:"attachToken"`
	ExpiresAt      time.Time `json:"expiresAt"`
	CertSPKISHA256 string    `json:"certSpkiSha256"`
}

type SessionList struct {
	Version  int           `json:"version"`
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	ID       uint64 `json:"id"`
	Name     string `json:"name,omitempty"`
	Attached bool   `json:"attached"`
}

type SessionTarget struct {
	ID   uint64
	Name string
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
	return (SocketSelector{}).Resolve()
}

type request struct {
	Version     int    `json:"version"`
	Operation   string `json:"operation"`
	SessionID   uint64 `json:"sessionId,omitempty"`
	SessionName string `json:"sessionName,omitempty"`
}

type response struct {
	Version   int           `json:"version"`
	OK        bool          `json:"ok"`
	Error     string        `json:"error,omitempty"`
	Bootstrap *Bootstrap    `json:"bootstrap,omitempty"`
	PID       int           `json:"pid,omitempty"`
	Sessions  []SessionInfo `json:"sessions,omitempty"`
}

type Handler func(operation string, target SessionTarget) (Bootstrap, []SessionInfo, int, error)

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
		b, sessions, pid, err := handler(req.Operation, SessionTarget{ID: req.SessionID, Name: req.SessionName})
		if err != nil {
			reply.Error = err.Error()
		} else {
			reply.OK = true
			reply.Bootstrap = &b
			reply.Sessions = sessions
			reply.PID = pid
		}
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

func CreateSession(ctx context.Context, socket, name string) (Bootstrap, error) {
	if name != "" {
		if err := ValidateSessionName(name); err != nil {
			return Bootstrap{}, err
		}
	}
	return callBootstrap(ctx, socket, "create-session", SessionTarget{Name: name})
}

func ConnectSession(ctx context.Context, socket, rawTarget string) (Bootstrap, error) {
	target, err := ParseSessionTarget(rawTarget)
	if err != nil {
		return Bootstrap{}, err
	}
	return callBootstrap(ctx, socket, "connect-session", target)
}

func callBootstrap(ctx context.Context, socket, operation string, target SessionTarget) (Bootstrap, error) {
	reply, err := call(ctx, socket, operation, target)
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

func ListSessions(ctx context.Context, socket string) ([]SessionInfo, error) {
	reply, err := call(ctx, socket, "list-sessions", SessionTarget{})
	if err != nil {
		return nil, err
	}
	return append([]SessionInfo(nil), reply.Sessions...), nil
}

func StopServer(ctx context.Context, socket string) (int, error) {
	reply, err := call(ctx, socket, "stop-server", SessionTarget{})
	if err != nil {
		return 0, err
	}
	return reply.PID, nil
}

func call(ctx context.Context, socket, operation string, target SessionTarget) (response, error) {
	dialCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
	if err != nil {
		return response{}, fmt.Errorf("%w: connect control socket %s", ErrDaemonUnavailable, socket)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlTimeout))
	if err := json.NewEncoder(conn).Encode(request{Version: ProtocolVersion, Operation: operation, SessionID: target.ID, SessionName: target.Name}); err != nil {
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

func WriteSessionList(w io.Writer, sessions []SessionInfo) error {
	payload, err := json.Marshal(SessionList{Version: ProtocolVersion, Sessions: sessions})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s%s\n", SessionListPrefix, payload)
	return err
}

func ParseSessionListOutput(output []byte) ([]SessionInfo, error) {
	var found []byte
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, SessionListPrefix) {
			if found != nil {
				return nil, errors.New("multiple session-list records")
			}
			found = []byte(strings.TrimSpace(strings.TrimPrefix(line, SessionListPrefix)))
		}
	}
	if len(found) == 0 {
		return nil, errors.New("SSH command did not return a Tali session list")
	}
	var list SessionList
	if err := json.Unmarshal(found, &list); err != nil {
		return nil, fmt.Errorf("parse session list: %w", err)
	}
	if list.Version != ProtocolVersion {
		return nil, fmt.Errorf("unsupported session-list version %d", list.Version)
	}
	return list.Sessions, nil
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

func StartSession(ctx context.Context, executable string, selector SocketSelector, name string) (Bootstrap, error) {
	socket, err := selector.Resolve()
	if err != nil {
		return Bootstrap{}, err
	}
	if b, callErr := CreateSession(ctx, socket, name); callErr == nil {
		return b, nil
	} else if !isUnavailable(callErr) {
		return Bootstrap{}, callErr
	}
	if err := EnsureSocketDir(socket); err != nil {
		return Bootstrap{}, err
	}
	if err := startDetachedServer(executable, selector); err != nil {
		return Bootstrap{}, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, callErr := CreateSession(ctx, socket, name)
		if callErr == nil {
			return b, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return Bootstrap{}, fmt.Errorf("%w at %s", ErrDaemonUnavailable, socket)
}

func isUnavailable(err error) bool { return errors.Is(err, ErrDaemonUnavailable) }

func startDetachedServer(executable string, selector SocketSelector) error {
	if executable == "" || !filepath.IsAbs(executable) {
		return errors.New("controller executable path is not absolute")
	}
	args, err := selector.Args()
	if err != nil {
		return err
	}
	args = append(args, "server", "run")
	cmd := exec.Command(executable, args...)
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
	conn, dialErr := net.DialTimeout("unix", socket, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("control socket %s is already accepting connections", socket)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !os.IsNotExist(dialErr) {
		return fmt.Errorf("inspect existing control socket %s: %w", socket, dialErr)
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
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUID(info) {
		return fmt.Errorf("control directory %q must be owned by the current user with mode 0700", dir)
	}
	return nil
}

type ServerLock struct {
	file *os.File
}

func AcquireServerLock(socket string) (*ServerLock, error) {
	if err := EnsureSocketDir(socket); err != nil {
		return nil, err
	}
	path := socket + ".lock"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server lock: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("protect server lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w for socket %s", ErrServerRunning, socket)
		}
		return nil, fmt.Errorf("lock server socket: %w", err)
	}
	return &ServerLock{file: file}, nil
}

func (l *ServerLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
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

func ParseSessionTarget(raw string) (SessionTarget, error) {
	if raw == "" {
		return SessionTarget{}, errors.New("session target must not be empty")
	}
	if isDecimal(raw) {
		id, err := ParseSessionID(raw)
		if err != nil {
			return SessionTarget{}, err
		}
		return SessionTarget{ID: id}, nil
	}
	if err := ValidateSessionName(raw); err != nil {
		return SessionTarget{}, err
	}
	return SessionTarget{Name: raw}, nil
}

func ValidateSessionName(name string) error {
	if name == "" {
		return errors.New("session name must not be empty")
	}
	if len(name) > 128 || !utf8.ValidString(name) {
		return errors.New("session name must be valid UTF-8 and at most 128 bytes")
	}
	if isDecimal(name) {
		return errors.New("session name must not be entirely numeric")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("session name must not contain control characters")
		}
	}
	return nil
}

func isDecimal(raw string) bool {
	if raw == "" {
		return false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
