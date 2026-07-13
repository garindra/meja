package control

import (
	"context"
	"net"
	"os"
	"path/filepath"
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

func TestControlRPCUsesPrivateSocketAndDoesNotStartOnConnect(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "tali", "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := testBootstrap()
	ready := make(chan struct{})
	go func() {
		close(ready)
		_ = Serve(ctx, socket, func(operation string, id uint64) (Bootstrap, int, error) {
			if operation == "stop-server" {
				return Bootstrap{}, 1234, nil
			}
			if operation != "connect-session" || id != want.SessionID {
				t.Errorf("handler received %q %d", operation, id)
			}
			return want, 0, nil
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
	got, err := Call(ctx, socket, "connect-session", want.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != want.SessionID {
		t.Fatalf("session ID = %d", got.SessionID)
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
