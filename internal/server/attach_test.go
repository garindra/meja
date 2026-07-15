package server

import (
	"bytes"
	"testing"
	"time"

	"github.com/garindra/meja/internal/control"
)

func TestDaemonAllocatesMonotonicSessionIDsAndSingleUseAttach(t *testing.T) {
	d := &Daemon{nextID: 1, sessions: map[uint64]*Session{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b1, _, _, err := d.handleControl("create-session", control.SessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	b2, _, _, err := d.handleControl("create-session", control.SessionTarget{})
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
	d := &Daemon{stderr: &log}
	d.logSessionAttached(7)
	d.logSessionAttached(7)
	if got, want := log.String(), "meja server: session 7 attached\nmeja server: session 7 attached\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestDaemonConnectSessionDoesNotCreateMissingSession(t *testing.T) {
	d := &Daemon{nextID: 1, sessions: map[uint64]*Session{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if _, _, _, err := d.handleControl("connect-session", control.SessionTarget{ID: 99}); err == nil {
		t.Fatal("connect-session created missing session")
	}
	b, _, _, err := d.handleControl("create-session", control.SessionTarget{})
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
	d := &Daemon{nextID: 1, sessions: map[uint64]*Session{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b, _, _, err := d.handleControl("create-session", control.SessionTarget{})
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
	if _, err := d.attach(b.SessionID, b.AttachToken); err == nil {
		t.Fatal("expired attach token accepted")
	}
}

func TestDaemonListsSessionIDsInOrder(t *testing.T) {
	d := &Daemon{sessions: map[uint64]*Session{9: {}, 2: {}, 4: {}}}
	_, ids, _, err := d.handleControl("list-sessions", control.SessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{2, 4, 9}
	for i := range want {
		if ids[i].ID != want[i] {
			t.Fatalf("session IDs = %v, want %v", ids, want)
		}
	}
}

func TestDaemonCreatesListsAndConnectsNamedSession(t *testing.T) {
	d := &Daemon{nextID: 1, sessions: map[uint64]*Session{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	created, _, _, err := d.handleControl("create-session", control.SessionTarget{Name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	_, sessions, _, err := d.handleControl("list-sessions", control.SessionTarget{})
	if err != nil || len(sessions) != 1 || sessions[0].Name != "work" {
		t.Fatalf("sessions = %#v, err = %v", sessions, err)
	}
	connected, _, _, err := d.handleControl("connect-session", control.SessionTarget{Name: "work"})
	if err != nil || connected.SessionID != created.SessionID {
		t.Fatalf("named connect = %#v, err = %v", connected, err)
	}
	if _, _, _, err := d.handleControl("create-session", control.SessionTarget{Name: "work"}); err == nil {
		t.Fatal("duplicate session name was accepted")
	}
}

func TestDaemonRenamesSessionUniquely(t *testing.T) {
	d := &Daemon{nextID: 1, sessions: map[uint64]*Session{}, port: 60001, certHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	first, _, _, _ := d.handleControl("create-session", control.SessionTarget{Name: "one"})
	_, _, _, _ = d.handleControl("create-session", control.SessionTarget{Name: "two"})
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
