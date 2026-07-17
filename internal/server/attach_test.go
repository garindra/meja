package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

func newCommandTestDaemon(t *testing.T) *Daemon {
	return newCommandTestDaemonMode(t, false)
}

func newCommandTestDaemonWithActor(t *testing.T) *Daemon {
	return newCommandTestDaemonMode(t, true)
}

func newCommandTestDaemonMode(t *testing.T, withActor bool) *Daemon {
	t.Helper()
	cert, hash, err := daemonCertificate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{
		nextID:               1,
		sessions:             map[uint64]*Session{},
		names:                map[string]*Session{},
		reconnectCredentials: map[string]*reconnectCredential{},
		clientSessions:       map[*reconnectCredential]uint64{},
		attachments:          map[uint64]*reconnectCredential{},
		tlsConfig:            &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13},
		certHash:             hash,
		serverCtx:            ctx,
	}
	var stopActor context.CancelFunc
	if withActor {
		var actorCtx context.Context
		actorCtx, stopActor = context.WithCancel(context.Background())
		d.requests = make(chan daemonRequest, 64)
		go d.runRequests(actorCtx)
	}
	t.Cleanup(func() {
		d.disconnectActiveClients()
		cancel()
		d.closeQUIC()
		if stopActor != nil {
			stopActor()
		}
	})
	return d
}

func TestDaemonAllocatesMonotonicSessionIDsAndSingleUseAttach(t *testing.T) {
	d := newCommandTestDaemon(t)
	b1, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if b1.session.ID != 1 || b2.session.ID != 2 {
		t.Fatalf("IDs = %d, %d", b1.session.ID, b2.session.ID)
	}
	if _, _, err := d.beginClientInstance(b1.bootstrap.AttachToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.beginClientInstance(b1.bootstrap.AttachToken); err == nil {
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
	if _, err := d.executeSessionOperation("connect-session", commandSessionTarget{id: 99}); err == nil {
		t.Fatal("connect-session created missing session")
	}
	b, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.bootstrap.ExpiresAt.After(time.Now()) {
		t.Fatal("bootstrap did not expire in the future")
	}
	if _, err := parseCommandSessionID("1"); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRejectsExpiredAttachToken(t *testing.T) {
	d := newCommandTestDaemon(t)
	b, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	d.call(func() { d.attachGrants[0].ExpiresAt = time.Now().Add(-time.Second) })
	if _, _, err := d.beginClientInstance(b.bootstrap.AttachToken); err == nil {
		t.Fatal("expired attach token accepted")
	}
}

func TestClientInstanceReusesReconnectCredential(t *testing.T) {
	d := newCommandTestDaemon(t)
	bootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	instance, _, err := d.beginClientInstance(bootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	token := instance.credential.EncodedToken
	for i := 0; i < 2; i++ {
		resumed, _, err := d.resumeClientInstance(token)
		if err != nil {
			t.Fatal(err)
		}
		if resumed == instance || resumed.credential != instance.credential {
			t.Fatal("resume did not rebuild the same logical client instance")
		}
		instance = resumed
	}
}

func TestFreshSSHAttachCreatesNewClientInstanceAndSupersedesPrevious(t *testing.T) {
	d := newCommandTestDaemon(t)
	firstBootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := d.beginClientInstance(firstBootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	var closeCode quic.ApplicationErrorCode
	var closeMessage string
	first.QUIC = &recordingQUICConnection{closeWithError: func(code quic.ApplicationErrorCode, message string) error {
		closeCode, closeMessage = code, message
		return nil
	}}

	secondBootstrap, err := d.executeSessionOperation("connect-session", commandSessionTarget{id: firstBootstrap.session.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := d.beginClientInstance(secondBootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.credential.TerminalReason != "session was taken over by another client" || d.clientSessions[first.credential] != 0 {
		t.Fatalf("client instances = %#v and %#v", first, second)
	}
	if got := d.attachments[firstBootstrap.session.ID]; got != second.credential {
		t.Fatalf("session owner = %#v, want %#v", got, second.credential)
	}
	if closeCode != protocol.SessionReplacedErrorCode || closeMessage != "session taken over by another client" {
		t.Fatalf("replacement close = (%d, %q)", closeCode, closeMessage)
	}
	if _, _, err := d.resumeClientInstance(first.credential.EncodedToken); err == nil || err.Error() != "session was taken over by another client" {
		t.Fatalf("superseded resume error = %v", err)
	}
}

func TestClosedClientInstanceIsDiscardedButReconnectCredentialPersists(t *testing.T) {
	d := newCommandTestDaemon(t)
	bootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	instance, _, err := d.beginClientInstance(bootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	conn := &recordingQUICConnection{}
	instance.QUIC = conn
	session := bootstrap.session
	session.clientInstance = instance
	d.detachClientInstance(instance, session)
	if instance.credential.Instance != nil {
		t.Fatal("closed client instance remained in the reconnect record")
	}
	if d.reconnectCredentials[instance.credential.EncodedToken] != instance.credential || d.attachments[bootstrap.session.ID] != instance.credential {
		t.Fatal("stable reconnect credential or session assignment was discarded")
	}
	resumed, resumedSession, err := d.resumeClientInstance(instance.credential.EncodedToken)
	if err != nil || resumed == instance || resumedSession != session {
		t.Fatalf("resume after close = (%#v, %#v, %v)", resumed, resumedSession, err)
	}
}

func TestFailedReplacementAllowsObsoleteInstanceToDetachSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	bootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	old, session, err := d.beginClientInstance(bootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	session.clientInstance = old

	replacement, _, err := d.resumeClientInstance(old.credential.EncodedToken)
	if err != nil {
		t.Fatal(err)
	}
	d.discardUnattachedClientInstance(replacement)
	d.detachClientInstance(old, session)

	if session.clientInstance != nil {
		t.Fatal("obsolete instance remained attached after its replacement failed")
	}
	if old.credential.Instance != nil {
		t.Fatal("failed replacement remained in the reconnect record")
	}
}

func TestClientInstanceAssignmentMovesAtomicallyBetweenSessions(t *testing.T) {
	d := newCommandTestDaemon(t)
	firstBootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	secondBootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	instance, _, err := d.beginClientInstance(firstBootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	d.call(func() { d.assignClientInstanceLocked(instance.credential, secondBootstrap.session) })
	if d.clientSessions[instance.credential] != secondBootstrap.session.ID || d.attachments[firstBootstrap.session.ID] != nil || d.attachments[secondBootstrap.session.ID] != instance.credential {
		t.Fatalf("moved instance = %#v, attachments = %#v", instance, d.attachments)
	}
}

func TestLiveSwitchMovesClientAssignmentWithoutChangingReconnectToken(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSession(1)
	target := NewSession(2)
	t.Cleanup(source.stopOperations)
	t.Cleanup(target.stopOperations)
	for _, session := range []*Session{source, target} {
		client := session.NewClient(clientID0)
		client.TerminalCols, client.TerminalRows = 80, 23
		session.CreateWindow(&Pane{ID: session.AddPaneID(), terminal: newTerminal(80, 23)}, clientID0)
		session.daemon = d
		d.sessions[session.ID] = session
	}

	credential := &reconnectCredential{EncodedToken: "stable-token"}
	instance := newClientInstance(d, credential)
	instance.managementOut = make(chan protocol.Frame, 8)
	credential.Instance = instance
	d.reconnectCredentials[credential.EncodedToken] = credential
	d.clientSessions[credential] = source.ID
	d.attachments[source.ID] = credential
	source.clientInstance = instance

	switched, err := d.switchClientInstance(instance, source, "2", 80, 23)
	if err != nil {
		t.Fatal(err)
	}
	if switched != target {
		t.Fatalf("switched session = %#v, want target", switched)
	}
	if d.reconnectCredentials["stable-token"] != credential || credential.EncodedToken != "stable-token" {
		t.Fatal("switch changed the reconnect-token association")
	}
	if d.clientSessions[credential] != target.ID || d.attachments[source.ID] != nil || d.attachments[target.ID] != credential {
		t.Fatalf("client assignment after switch: sessions=%#v attachments=%#v", d.clientSessions, d.attachments)
	}
	if source.clientInstance != nil || target.clientInstance != instance {
		t.Fatalf("session clients after switch: source=%#v target=%#v", source.clientInstance, target.clientInstance)
	}
	resumed, resumedSession, err := d.resumeClientInstance("stable-token")
	if err != nil {
		t.Fatal(err)
	}
	if resumed == instance || resumedSession != target || resumed.credential != credential {
		t.Fatalf("resume after switch = (%#v, %#v), want rebuilt client targeting %#v", resumed, resumedSession, target)
	}
}

func TestDaemonQUICListenerResumesByClientInstance(t *testing.T) {
	d := newCommandTestDaemonWithActor(t)
	d.snapshotDir = t.TempDir()
	result := d.executeCommand(protocol.CommandRequest{
		Args:         []string{"new", "--", "/bin/sleep", "30"},
		TerminalCols: 80,
		TerminalRows: 23,
	})
	if result.exitCode != 0 || result.bootstrap == nil {
		t.Fatalf("create result = %#v", result)
	}
	bootstrap := *result.bootstrap

	firstConn, resumeToken := dialTestClientInstance(t, bootstrap, "")
	_ = firstConn.CloseWithError(1, "test disconnect")
	secondConn, resumedToken := dialTestClientInstance(t, bootstrap, resumeToken)
	defer secondConn.CloseWithError(0, "")

	if resumeToken == "" || resumedToken != resumeToken {
		t.Fatalf("resume credential changed from %q to %q", resumeToken, resumedToken)
	}
}

func dialTestClientInstance(t *testing.T, bootstrap protocol.CommandBootstrap, token string) (quic.Connection, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", bootstrap.Port), &tls.Config{
		InsecureSkipVerify: true, // Test-only certificate; production pins SPKI.
		NextProtos:         []string{protocol.ALPN},
	}, &quic.Config{MaxIncomingUniStreams: int64(protocol.OutputStreamCount)})
	if err != nil {
		t.Fatal(err)
	}
	management, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	managementEncoder := protocol.NewEncoder(management)
	if err := managementEncoder.WriteFrame(encodedTestFrame(t, protocol.MsgOpenManagementStream, protocol.StreamOpen{StreamType: protocol.StreamTypeManagement}, protocol.EncodeStreamOpen)); err != nil {
		t.Fatal(err)
	}
	if token == "" {
		if err := managementEncoder.WriteFrame(encodedTestFrame(t, protocol.MsgSessionAttach, protocol.SessionAttach{Version: protocol.ProtocolVersion, Token: bootstrap.AttachToken, Cols: 80, Rows: 23}, protocol.EncodeSessionAttach)); err != nil {
			t.Fatal(err)
		}
	} else if err := managementEncoder.WriteFrame(encodedTestFrame(t, protocol.MsgSessionResume, protocol.SessionResume{Version: protocol.ProtocolVersion, ResumeToken: token, Cols: 80, Rows: 23}, protocol.EncodeSessionResume)); err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(input).WriteFrame(encodedTestFrame(t, protocol.MsgOpenInputStream, protocol.StreamOpen{StreamType: protocol.StreamTypeInput}, protocol.EncodeStreamOpen)); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.NewDecoder(management, protocol.DefaultMaxFrameSize).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		if frame.Type != protocol.MsgSessionAttachOK {
			t.Fatalf("attach frame type = %d", frame.Type)
		}
		attached, err := protocol.DecodeSessionAttachOK(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		return conn, attached.ResumeToken
	}
	if frame.Type != protocol.MsgSessionResumeOK {
		t.Fatalf("resume frame type = %d", frame.Type)
	}
	resumed, err := protocol.DecodeSessionResumeOK(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Version != protocol.ProtocolVersion {
		t.Fatalf("resume response = %#v", resumed)
	}
	return conn, token
}

func encodedTestFrame[T any](t *testing.T, frameType uint64, message T, encode func([]byte, T) ([]byte, error)) protocol.Frame {
	t.Helper()
	payload, err := encode(nil, message)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Frame{Type: frameType, Payload: payload}
}

func TestDaemonListsSessionIDsInOrder(t *testing.T) {
	d := &Daemon{sessions: map[uint64]*Session{9: {}, 2: {}, 4: {}}}
	operation, err := d.executeSessionOperation("list-sessions", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{2, 4, 9}
	for i := range want {
		if operation.sessions[i].id != want[i] {
			t.Fatalf("session IDs = %v, want %v", operation.sessions, want)
		}
	}
}

func TestDaemonCreatesListsAndConnectsNamedSession(t *testing.T) {
	d := newCommandTestDaemon(t)
	created, err := d.executeSessionOperation("create-session", commandSessionTarget{name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := d.executeSessionOperation("list-sessions", commandSessionTarget{})
	if err != nil || len(listed.sessions) != 1 || listed.sessions[0].name != "work" {
		t.Fatalf("sessions = %#v, err = %v", listed.sessions, err)
	}
	connected, err := d.executeSessionOperation("connect-session", commandSessionTarget{name: "work"})
	if err != nil || connected.session != created.session {
		t.Fatalf("named connect = %#v, err = %v", connected, err)
	}
	if _, err := d.executeSessionOperation("create-session", commandSessionTarget{name: "work"}); err == nil {
		t.Fatal("duplicate session name was accepted")
	}
}

func TestDaemonRenamesSessionUniquely(t *testing.T) {
	d := newCommandTestDaemon(t)
	first, _ := d.executeSessionOperation("create-session", commandSessionTarget{name: "one"})
	_, _ = d.executeSessionOperation("create-session", commandSessionTarget{name: "two"})
	state := first.session
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

func TestSessionsShareDaemonQUICPort(t *testing.T) {
	d := newCommandTestDaemon(t)
	first, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if first.bootstrap.Port != second.bootstrap.Port {
		t.Fatalf("session ports = %d and %d, want one daemon port", first.bootstrap.Port, second.bootstrap.Port)
	}
	reconnect, err := d.executeSessionOperation("connect-session", commandSessionTarget{id: first.session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if reconnect.bootstrap.Port != first.bootstrap.Port {
		t.Fatalf("reconnect port = %d, want stable session port %d", reconnect.bootstrap.Port, first.bootstrap.Port)
	}
	if reconnect.bootstrap.AttachToken == first.bootstrap.AttachToken {
		t.Fatal("reconnect reused the initial attach token")
	}

	if err := first.session.shutdown(); err != nil {
		t.Fatal(err)
	}
	if rebound, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: int(first.bootstrap.Port)}); err == nil {
		_ = rebound.Close()
		t.Fatalf("daemon UDP port %d was released with one session", first.bootstrap.Port)
	}
}
