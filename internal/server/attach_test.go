package server

import (
	"bytes"
	"testing"
	"time"

	"tali/internal/control"
)

func TestDaemonAllocatesMonotonicSessionIDsAndSingleUseAttach(t *testing.T) {
	d := &daemon{nextID: 1, sessions: map[uint64]*sessionState{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b1, _, _, err := d.handleControl("create-session", 0)
	if err != nil {
		t.Fatal(err)
	}
	b2, _, _, err := d.handleControl("create-session", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b1.SessionID != 1 || b2.SessionID != 2 {
		t.Fatalf("IDs = %d, %d", b1.SessionID, b2.SessionID)
	}
	if _, err := d.attach(b1.SessionID, b1.AttachToken); err != nil {
		t.Fatal(err)
	}
	if _, err := d.attach(b1.SessionID, b1.AttachToken); err == nil {
		t.Fatal("single-use attach token accepted twice")
	}
}

func TestSessionAttachedLogIsEmittedForEveryAttach(t *testing.T) {
	var log bytes.Buffer
	d := &daemon{stderr: &log}
	d.logSessionAttached(7)
	d.logSessionAttached(7)
	if got, want := log.String(), "tali server: session 7 attached\ntali server: session 7 attached\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestDaemonConnectSessionDoesNotCreateMissingSession(t *testing.T) {
	d := &daemon{nextID: 1, sessions: map[uint64]*sessionState{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if _, _, _, err := d.handleControl("connect-session", 99); err == nil {
		t.Fatal("connect-session created missing session")
	}
	b, _, _, err := d.handleControl("create-session", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !b.ExpiresAt.After(time.Now()) {
		t.Fatal("bootstrap did not expire in the future")
	}
	if _, err := control.ParseSessionID("1"); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRejectsExpiredAttachToken(t *testing.T) {
	d := &daemon{nextID: 1, sessions: map[uint64]*sessionState{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b, _, _, err := d.handleControl("create-session", 0)
	if err != nil {
		t.Fatal(err)
	}
	s := d.sessions[b.SessionID]
	s.attachMu.Lock()
	s.attachExpires = time.Now().Add(-time.Second)
	s.attachMu.Unlock()
	if _, err := d.attach(b.SessionID, b.AttachToken); err == nil {
		t.Fatal("expired attach token accepted")
	}
}

func TestDaemonListsSessionIDsInOrder(t *testing.T) {
	d := &daemon{sessions: map[uint64]*sessionState{9: {}, 2: {}, 4: {}}}
	_, ids, _, err := d.handleControl("list-sessions", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{2, 4, 9}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("session IDs = %v, want %v", ids, want)
		}
	}
}
