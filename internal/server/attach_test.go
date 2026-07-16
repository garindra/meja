package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func newCommandTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	cert, hash, err := daemonCertificate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{
		nextID:    1,
		sessions:  map[uint64]*Session{},
		names:     map[string]*Session{},
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13},
		certHash:  hash,
		serverCtx: ctx,
	}
	t.Cleanup(func() {
		d.disconnectActiveClients()
		cancel()
	})
	return d
}

func TestDaemonAllocatesMonotonicSessionIDsAndSingleUseAttach(t *testing.T) {
	d := newCommandTestDaemon(t)
	b1, _, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	b2, _, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if b1.SessionID != 1 || b2.SessionID != 2 {
		t.Fatalf("IDs = %d, %d", b1.SessionID, b2.SessionID)
	}
	firstSession := d.sessions[b1.SessionID]
	if err := firstSession.consumeAttachToken(b1.AttachToken); err != nil {
		t.Fatal(err)
	}
	if err := firstSession.consumeAttachToken(b1.AttachToken); err == nil {
		t.Fatal("single-use attach token accepted twice")
	}
}

func TestSessionAttachedLogIsEmittedForEveryAttach(t *testing.T) {
	var log bytes.Buffer
	d := &Daemon{stderr: &log}
	d.logSessionAttached(7)
	d.logSessionAttached(7)
	if got, want := log.String(), "meja server: session 7 attached\nmeja server: session 7 attached\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestDaemonConnectSessionDoesNotCreateMissingSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	if _, _, err := d.executeSessionOperation("connect-session", commandSessionTarget{id: 99}); err == nil {
		t.Fatal("connect-session created missing session")
	}
	b, _, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.ExpiresAt.After(time.Now()) {
		t.Fatal("bootstrap did not expire in the future")
	}
	if _, err := parseCommandSessionID("1"); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRejectsExpiredAttachToken(t *testing.T) {
	d := newCommandTestDaemon(t)
	b, _, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	s := d.sessions[b.SessionID]
	if err := s.coordinate(func() error {
		s.attachExpires = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.consumeAttachToken(b.AttachToken); err == nil {
		t.Fatal("expired attach token accepted")
	}
}

func TestDaemonListsSessionIDsInOrder(t *testing.T) {
	d := &Daemon{sessions: map[uint64]*Session{9: {}, 2: {}, 4: {}}}
	_, ids, err := d.executeSessionOperation("list-sessions", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{2, 4, 9}
	for i := range want {
		if ids[i].id != want[i] {
			t.Fatalf("session IDs = %v, want %v", ids, want)
		}
	}
}

func TestDaemonCreatesListsAndConnectsNamedSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	created, _, err := d.executeSessionOperation("create-session", commandSessionTarget{name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	_, sessions, err := d.executeSessionOperation("list-sessions", commandSessionTarget{})
	if err != nil || len(sessions) != 1 || sessions[0].name != "work" {
		t.Fatalf("sessions = %#v, err = %v", sessions, err)
	}
	connected, _, err := d.executeSessionOperation("connect-session", commandSessionTarget{name: "work"})
	if err != nil || connected.SessionID != created.SessionID {
		t.Fatalf("named connect = %#v, err = %v", connected, err)
	}
	if _, _, err := d.executeSessionOperation("create-session", commandSessionTarget{name: "work"}); err == nil {
		t.Fatal("duplicate session name was accepted")
	}
}

func TestDaemonRenamesSessionUniquely(t *testing.T) {
	d := newCommandTestDaemon(t)
	first, _, _ := d.executeSessionOperation("create-session", commandSessionTarget{name: "one"})
	_, _, _ = d.executeSessionOperation("create-session", commandSessionTarget{name: "two"})
	state := d.sessions[first.SessionID]
	if err := d.renameSession(state, "renamed"); err != nil {
		t.Fatal(err)
	}
	if got := state.SessionName(); got != "renamed" {
		t.Fatalf("session name = %q", got)
	}
	if err := d.renameSession(state, "two"); err == nil {
		t.Fatal("rename to an existing name was accepted")
	}
}

func TestSessionQUICPortsAreUniqueAndReleasedOnShutdown(t *testing.T) {
	d := newCommandTestDaemon(t)
	first, _, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Port == second.Port {
		t.Fatalf("sessions share UDP port %d", first.Port)
	}
	reconnect, _, err := d.executeSessionOperation("connect-session", commandSessionTarget{id: first.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if reconnect.Port != first.Port {
		t.Fatalf("reconnect port = %d, want stable session port %d", reconnect.Port, first.Port)
	}
	if reconnect.AttachToken == first.AttachToken {
		t.Fatal("reconnect reused the initial attach token")
	}

	firstSession := d.sessions[first.SessionID]
	if err := firstSession.shutdown(); err != nil {
		t.Fatal(err)
	}
	rebound, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: int(first.Port)})
	if err != nil {
		t.Fatalf("session UDP port %d remained bound after shutdown: %v", first.Port, err)
	}
	_ = rebound.Close()
}
