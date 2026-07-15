package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testBootstrap() Bootstrap {
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i)
	}
	return Bootstrap{Version: ProtocolVersion, SessionID: 7, Port: 60001, AttachToken: EncodeToken(token), ExpiresAt: time.Now().Add(time.Minute), CertSPKISHA256: strings.Repeat("ab", 32)}
}

func TestBootstrapRoundTripUsesSentinelAndNumericID(t *testing.T) {
	b := testBootstrap()
	var output strings.Builder
	if err := WriteBootstrap(&output, b); err != nil {
		t.Fatal(err)
	}
	got, err := ParseBootstrapOutput([]byte("diagnostic\n" + output.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != 7 || got.Port != 60001 || got.AttachToken != b.AttachToken {
		t.Fatalf("bootstrap = %#v", got)
	}
	if !strings.HasPrefix(output.String(), BootstrapPrefix) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestParseSessionIDRejectsNonCanonicalValues(t *testing.T) {
	for _, raw := range []string{"", "0", "+1", "-1", "1.0", "not-a-number"} {
		if _, err := ParseSessionID(raw); err == nil {
			t.Errorf("ParseSessionID(%q) accepted", raw)
		}
	}
	if got, err := ParseSessionID("18446744073709551615"); err != nil || got == 0 {
		t.Fatalf("max uint64: %d, %v", got, err)
	}
}

func TestTokenComparisonIsEncodingStrict(t *testing.T) {
	token := make([]byte, 32)
	if !EqualToken(EncodeToken(token), token) {
		t.Fatal("equal token rejected")
	}
	if EqualToken(EncodeToken(make([]byte, 31)), token) {
		t.Fatal("wrong length token accepted")
	}
}

func TestSocketSelectorResolvesProfilesAndExactPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := (SocketSelector{Profile: "dev.test_1"}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".meja", "dev.test_1", "meja.sock")
	if path != want {
		t.Fatalf("profile path = %q, want %q", path, want)
	}
	defaultPath, err := (SocketSelector{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if defaultPath != filepath.Join(home, ".meja", DefaultProfile, "meja.sock") {
		t.Fatalf("default path = %q", defaultPath)
	}
	exact := filepath.Join(home, "exact.sock")
	if got, err := (SocketSelector{Path: exact}).Resolve(); err != nil || got != exact {
		t.Fatalf("exact path = %q, %v", got, err)
	}
}

func TestSocketSelectorRejectsInvalidCombinations(t *testing.T) {
	for _, selector := range []SocketSelector{
		{Profile: "dev", Path: "/tmp/dev.sock"},
		{Profile: "../dev"},
		{Profile: "dev/work"},
		{Path: "relative.sock"},
	} {
		if _, err := selector.Normalize(); err == nil {
			t.Errorf("Normalize(%#v) accepted", selector)
		}
	}
}

func TestSocketSelectorArgsPreserveProfileOrExactPath(t *testing.T) {
	args, err := (SocketSelector{Profile: "dev"}).Args()
	if err != nil || len(args) != 2 || args[0] != "-L" || args[1] != "dev" {
		t.Fatalf("profile args = %v, %v", args, err)
	}
	args, err = (SocketSelector{Path: "/private/meja.sock"}).Args()
	if err != nil || len(args) != 2 || args[0] != "-S" || args[1] != "/private/meja.sock" {
		t.Fatalf("socket args = %v, %v", args, err)
	}
}

func TestEnsureSocketDirDoesNotChmodExistingParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSocketDir(filepath.Join(parent, "meja.sock")); err == nil {
		t.Fatal("shared parent was accepted")
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("parent mode changed to %o", info.Mode().Perm())
	}
}

func TestServerLockIsExclusivePerSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "profile", "meja.sock")
	first, err := AcquireServerLock(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := AcquireServerLock(socket); !errors.Is(err, ErrServerRunning) {
		t.Fatalf("second lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireServerLock(socket)
	if err != nil {
		t.Fatalf("lock after release = %v", err)
	}
	_ = second.Close()
}

func TestRemoveStaleSocketDoesNotDeleteActiveSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "meja.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := removeStaleSocket(socket); err == nil {
		t.Fatal("active socket was treated as stale")
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("active socket was removed: %v", err)
	}
}

func TestRemoveStaleSocketDeletesUnboundSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "meja.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleSocket(socket); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("stale socket still exists: %v", err)
	}
}

func TestControlRPCUsesPrivateSocketAndDoesNotStartOnConnect(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "meja", "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := testBootstrap()
	ready := make(chan struct{})
	go func() {
		close(ready)
		_ = Serve(ctx, socket, func(operation string, target SessionTarget) (Bootstrap, []SessionInfo, int, error) {
			if operation == "stop-server" {
				return Bootstrap{}, nil, 1234, nil
			}
			if operation == "list-sessions" {
				return Bootstrap{}, []SessionInfo{{ID: 3}, {ID: 7, Name: "work", Attached: true}}, 0, nil
			}
			if operation != "connect-session" || target.ID != want.SessionID {
				t.Errorf("handler received %q %#v", operation, target)
			}
			return want, nil, 0, nil
		})
	}()
	<-ready
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket did not appear")
		}
		time.Sleep(time.Millisecond)
	}
	got, err := ConnectSession(ctx, socket, strconv.FormatUint(want.SessionID, 10))
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != want.SessionID {
		t.Fatalf("session ID = %d", got.SessionID)
	}
	ids, err := ListSessions(ctx, socket)
	if err != nil || len(ids) != 2 || ids[0].ID != 3 || ids[1].ID != 7 || ids[1].Name != "work" || !ids[1].Attached {
		t.Fatalf("ListSessions() = %v, %v", ids, err)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o", info.Mode().Perm())
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	pid, err := StopServer(ctx, socket)
	if err != nil || pid != 1234 {
		t.Fatalf("StopServer() pid=%d err=%v", pid, err)
	}
}
