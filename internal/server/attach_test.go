package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

func newCommandTestDaemon(t *testing.T) *Daemon {
	return newCommandTestDaemonMode(t, false)
}

func admitTestClient(d *Daemon, token string) (*ClientInstance, *SessionState, error) {
	admission, err := d.admitConnection(AdmitConnectionRequest{Kind: admitSessionAttach, Token: token})
	if err != nil {
		return nil, nil, err
	}
	state, _ := d.sessionIndex.Load(admission.SessionID)
	client := newClientInstance(d, admission.identity, admission.connection)
	testClientByIdentity.Store(client.identity, client)
	startTestClientCommandLoop(client)
	return client, state.(*SessionState), nil
}

func resumeTestClient(d *Daemon, token string) (*ClientInstance, *SessionState, error) {
	admission, err := d.admitConnection(AdmitConnectionRequest{Kind: admitClientResume, Token: token})
	if err != nil {
		return nil, nil, err
	}
	state, _ := d.sessionIndex.Load(admission.SessionID)
	client := newClientInstance(d, admission.identity, admission.connection)
	testClientByIdentity.Store(client.identity, client)
	startTestClientCommandLoop(client)
	return client, state.(*SessionState), nil
}

func TestQUICRejectsMismatchedALPN(t *testing.T) {
	d := newCommandTestDaemon(t)
	port, err := d.ensureQUIC()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"meja-quic/previous"},
	}, nil)
	if err == nil {
		_ = conn.CloseWithError(0, "unexpected compatible ALPN")
		t.Fatal("mismatched QUIC ALPN was accepted")
	}
}

func setCommandTestPersistenceDir(t *testing.T, d *Daemon) string {
	t.Helper()
	directory := t.TempDir()
	d.sessionPersistenceDir = directory
	// Stop the writer before TempDir's previously registered cleanup removes
	// its directory. The daemon's generic cleanup was registered too early to
	// provide that ordering on its own.
	t.Cleanup(func() {
		d.stopPersistence()
		if d.persistenceStarted.Load() {
			<-d.persistenceDone
		}
	})
	return directory
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
		nextID:       1,
		sessions:     map[uint64]*SessionState{},
		panes:        map[uint64]*Pane{},
		names:        map[string]*SessionState{},
		windowLeases: map[uint64]*WindowViewLease{},
		clientTokens: map[string]ClientID{},
		clients:      map[ClientID]*ClientIdentity{},
		tlsConfig:    &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13},
		certHash:     hash,
		serverCtx:    ctx,
	}
	d.processObserver = NewProcessObserver()
	d.processObservations = make(map[uint64]ProcessObservation)
	d.processSaveCandidates = make(map[uint64]processSaveCandidate)
	d.sessionPersistions = make(map[uint64]*SessionPersistence)
	d.obsoletePersistenceNames = make(map[uint64]map[string]struct{})
	d.persistenceNow = make(chan struct{}, 1)
	d.persistenceStop = make(chan struct{})
	d.persistenceDone = make(chan struct{})
	d.persistenceUpdates = make(chan persistenceSnapshot, 1)
	var stopActor context.CancelFunc
	if withActor {
		var actorCtx context.Context
		actorCtx, stopActor = context.WithCancel(context.Background())
		d.requests = make(chan daemonRequest, 64)
		go d.runRequests(actorCtx)
	}
	t.Cleanup(func() {
		d.disconnectActiveClients()
		d.stopPersistence()
		if d.persistenceStarted.Load() {
			<-d.persistenceDone
		}
		cancel()
		d.closeQUIC()
		if stopActor != nil {
			stopActor()
		}
	})
	return d
}

func prepareTestDaemonSession(d *Daemon, session *SessionState, cols, rows uint16) {
	session.daemon = d
	d.sessions[session.ID] = session
	d.sessionIndex.Store(session.ID, session)
	d.ensureSessionGroupInActor(session)
	client := newTestClient(session)
	client.setTestTerminalSize(cols, rows)
	paneID, _ := d.allocatePaneIDNow()
	createTestWindow(session, &Pane{ID: paneID, terminal: newTerminal(int(cols), int(rows))})
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
	if _, _, err := admitTestClient(d, b1.bootstrap.AttachToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := admitTestClient(d, b1.bootstrap.AttachToken); err == nil {
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

func TestClientActorReportsCommandFailure(t *testing.T) {
	var log bytes.Buffer
	d := &Daemon{stderr: &log}
	client := newClientInstance(d, &ClientIdentity{ID: 17, SessionID: 9})
	transition := ViewTransition{}
	client.runClientCommand(clientInstanceCommand{Transition: &transition})
	for _, want := range []string{"client event failed", "client=17", "session=9", "client projection has no ordering revision"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("client event log %q missing %q", log.String(), want)
		}
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
	if _, _, err := admitTestClient(d, b.bootstrap.AttachToken); err == nil {
		t.Fatal("expired attach token accepted")
	}
}

func TestReconnectRebuildsInstanceForStableClientIdentity(t *testing.T) {
	d := newCommandTestDaemon(t)
	bootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	instance, _, err := admitTestClient(d, bootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	token := instance.identity.ResumeToken
	for i := 0; i < 2; i++ {
		resumed, _, err := resumeTestClient(d, token)
		if err != nil {
			t.Fatal(err)
		}
		if resumed == instance || resumed.identity != instance.identity {
			t.Fatal("resume did not rebuild the same logical client instance")
		}
		instance = resumed
	}
}

func TestReconnectIdentityPreservesClientLayoutRevisionAllocator(t *testing.T) {
	d := newCommandTestDaemon(t)
	bootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	instance, _, err := admitTestClient(d, bootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	const lastAllocatedRevision = 17
	instance.identity.lastAllocatedClientLayoutRevision = lastAllocatedRevision

	resumed, _, err := resumeTestClient(d, instance.identity.ResumeToken)
	if err != nil {
		t.Fatal(err)
	}
	if got := resumed.currentView.Layout.LayoutRevision; got != 0 {
		t.Fatalf("fresh resumed instance already has layout revision %d", got)
	}
	var nextRevision protocol.ClientLayoutRevision
	d.call(func() { nextRevision = d.allocateClientLayoutRevisionNow(resumed.identity) })
	plan := ClientProjectionPlan{
		SessionID: resumed.sessionID, ProjectionRevision: 1, FullSnapshot: true,
	}
	plan.View.Layout.LayoutRevision = nextRevision
	if err := commitTestProjection(resumed, ViewTransition{Reason: viewTransitionAttach, Projection: plan}); err != nil {
		t.Fatal(err)
	}
	if nextRevision != lastAllocatedRevision+1 {
		t.Fatalf("allocated resumed layout revision = %d, want %d", nextRevision, lastAllocatedRevision+1)
	}
	if got := resumed.currentView.Layout.LayoutRevision; got != nextRevision {
		t.Fatalf("resumed full projection revision = %d, want %d", got, nextRevision)
	}

	// A transport can fail after resume authentication but before promotion.
	// The logical identity must retain its allocator for the next attempt even
	// though the unattached replacement ClientInstance is discarded.
	d.discardPendingClientInstance(resumed)
	again, _, err := resumeTestClient(d, instance.identity.ResumeToken)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.currentView.Layout.LayoutRevision; got != 0 {
		t.Fatalf("second fresh resumed instance already has layout revision %d", got)
	}
	if got := again.identity.lastAllocatedClientLayoutRevision; got != nextRevision {
		t.Fatalf("identity last allocated layout revision = %d, want %d", got, nextRevision)
	}
}

func TestFreshSSHAttachCreatesNewClientInstanceAndSupersedesPrevious(t *testing.T) {
	d := newCommandTestDaemon(t)
	firstBootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := admitTestClient(d, firstBootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	setTestClient(firstBootstrap.session, first)
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
	second, _, err := admitTestClient(d, secondBootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	setTestClient(firstBootstrap.session, second)
	if first == second || first.identity.TerminalReason != "session was taken over by another client" {
		t.Fatalf("client instances = %#v and %#v", first, second)
	}
	if got := d.clients[firstBootstrap.session.ClientID]; got != second.identity {
		t.Fatalf("session owner = %#v, want %#v", got, second.identity)
	}
	if closeCode != protocol.SessionReplacedErrorCode || closeMessage != "session taken over by another client" {
		t.Fatalf("replacement close = (%d, %q)", closeCode, closeMessage)
	}
	if _, _, err := resumeTestClient(d, first.identity.ResumeToken); err == nil || err.Error() != "session was taken over by another client" {
		t.Fatalf("superseded resume error = %v", err)
	}
}

func TestClosedClientInstanceIsDiscardedButClientIdentityPersists(t *testing.T) {
	d := newCommandTestDaemon(t)
	bootstrap, err := d.executeSessionOperation("create-session", commandSessionTarget{})
	if err != nil {
		t.Fatal(err)
	}
	instance, _, err := admitTestClient(d, bootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	conn := &recordingQUICConnection{}
	instance.QUIC = conn
	session := bootstrap.session
	setTestClient(session, instance)
	d.detachClientInstance(instance)
	if instance.identity.State.Phase != clientDetached {
		t.Fatal("closed client identity did not become detached")
	}
	if d.clients[d.clientTokens[instance.identity.ResumeToken]] != instance.identity ||
		instance.identity.SessionID != bootstrap.session.ID {
		t.Fatal("stable reconnect identity or session assignment was discarded")
	}
	resumed, resumedSession, err := resumeTestClient(d, instance.identity.ResumeToken)
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
	old, session, err := admitTestClient(d, bootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	setTestClient(session, old)

	replacement, _, err := resumeTestClient(d, old.identity.ResumeToken)
	if err != nil {
		t.Fatal(err)
	}
	d.discardPendingClientInstance(replacement)
	d.detachClientInstance(old)

	if session.ClientID != 0 {
		t.Fatal("obsolete instance remained attached after its replacement failed")
	}
	if old.identity.State.Phase != clientDetached {
		t.Fatal("failed replacement did not leave the identity detached")
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
	instance, _, err := admitTestClient(d, firstBootstrap.bootstrap.AttachToken)
	if err != nil {
		t.Fatal(err)
	}
	setTestClient(firstBootstrap.session, instance)
	d.call(func() {
		firstBootstrap.session.ClientID = 0
		instance.identity.SessionID = secondBootstrap.session.ID
		secondBootstrap.session.ClientID = instance.identity.ID
	})
	if instance.identity.SessionID != secondBootstrap.session.ID ||
		firstBootstrap.session.ClientID != 0 ||
		secondBootstrap.session.ClientID != instance.identity.ID {
		t.Fatalf("moved client identity = %#v", instance.identity)
	}
}

func TestLiveSwitchMovesClientAssignmentWithoutChangingReconnectToken(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSessionState(1)
	target := NewSessionState(2)
	t.Cleanup(func() { stopState(source) })
	t.Cleanup(func() { stopState(target) })
	prepareTestDaemonSession(d, source, 80, 23)
	// A command launched inside a Meja pane historically measured the already
	// drawable PTY and subtracted the status row again. Model that mismatched
	// target so the switch must reconcile it to the live client's dimensions.
	prepareTestDaemonSession(d, target, 80, 22)

	identity := &ClientIdentity{ResumeToken: "stable-token"}
	instance := newClientInstance(d, identity)
	instance.controlOut = make(chan protocol.Frame, 8)
	var paneOutput bytes.Buffer
	instance.Output[0] = testOutputLease(0, &paneOutput)
	setTestClient(source, instance)
	d.clientTokens[identity.ResumeToken] = identity.ID
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{WindowID: source.ActiveWindowID, SessionID: source.ID, ClientID: instance.identity.ID, Generation: 1}
	instance.ViewLeaseWindowID = source.ActiveWindowID
	instance.ViewLeaseGeneration = 1

	transition, err := d.transitionClientToSession(instance.identity, target.ID, 80, 23)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.applyViewTransition(transition); err != nil {
		t.Fatal(err)
	}
	testClientByState.Delete(source)
	testClientByState.Store(target, instance)
	if instance.sessionState() != target {
		t.Fatalf("switched session = %#v, want target", instance.sessionState())
	}
	if d.clients[d.clientTokens["stable-token"]] != identity || identity.ResumeToken != "stable-token" {
		t.Fatal("switch changed the reconnect-token association")
	}
	if identity.SessionID != target.ID || source.ClientID != 0 || target.ClientID != identity.ID {
		t.Fatalf("client assignment after switch: identity=%#v source=%d target=%d", identity, source.ClientID, target.ClientID)
	}
	if testClientOf(source) != nil || testClientOf(target) != instance {
		t.Fatalf("session clients after switch: source=%#v target=%#v", testClientOf(source), testClientOf(target))
	}
	layout := decodeTestClientLayout(t, <-instance.controlOut)
	if len(layout.Panes) != 1 {
		t.Fatalf("switched layout panes = %#v", layout.Panes)
	}
	commands := decodePendingCommands(t, paneOutput.Bytes())
	if len(commands) == 0 || commands[0].Opcode != protocol.DisplayOpcodeStartRender {
		t.Fatalf("switched pane output = %#v, want START_RENDER", commands)
	}
	if commands[0].LayoutRevision != layout.LayoutRevision || commands[0].GridCols != layout.Panes[0].Rect.Width || commands[0].GridRows != layout.Panes[0].Rect.Height {
		t.Fatalf("switched pane grid = revision %d %dx%d, layout = revision %d %dx%d",
			commands[0].LayoutRevision, commands[0].GridCols, commands[0].GridRows,
			layout.LayoutRevision, layout.Panes[0].Rect.Width, layout.Panes[0].Rect.Height)
	}
	resumed, resumedSession, err := resumeTestClient(d, "stable-token")
	if err != nil {
		t.Fatal(err)
	}
	if resumed == instance || resumedSession != target || resumed.identity != identity {
		t.Fatalf("resume after switch = (%#v, %#v), want rebuilt client targeting %#v", resumed, resumedSession, target)
	}
	if resumed.identity.ID == 0 || resumed.identity.ID != instance.identity.ID {
		t.Fatalf("resumed client ID = %d, want stable ID %d", resumed.identity.ID, instance.identity.ID)
	}
	if identity.State.Phase != clientReplacing ||
		identity.State.Active != instance.connection ||
		identity.State.Pending != resumed.connection {
		t.Fatalf("resume lifecycle = %#v, want active old connection and pending replacement", identity.State)
	}
}

func TestPaneCLINewReconcilesNestedPTYSizeToLiveViewport(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	source := NewSessionState(1)
	prepareTestDaemonSession(d, source, 80, 23)
	source.setSessionName("source")
	d.names[source.Name] = source
	d.nextID = 2
	t.Cleanup(func() { stopState(source) })

	identity := &ClientIdentity{ResumeToken: "contextual-new"}
	instance := newClientInstance(d, identity)
	instance.terminalCols.Store(80)
	instance.terminalRows.Store(23)
	instance.controlOut = make(chan protocol.Frame, 8)
	var paneOutput synchronizedBuffer
	instance.Output[0] = testOutputLease(0, &paneOutput)
	setTestClient(source, instance)
	d.clientTokens[identity.ResumeToken] = identity.ID
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{
		WindowID: source.ActiveWindowID, SessionID: source.ID,
		ClientID: instance.identity.ID, Generation: 1,
	}
	instance.ViewLeaseWindowID = source.ActiveWindowID
	instance.ViewLeaseGeneration = 1
	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"new", "-s", "target", "--", "/bin/sleep", "30"},
		CallerSessionTarget: "source",
		WorkingDirectory:    t.TempDir(),
		TerminalCols:        80,
		// A nested CLI sees the pane's already-drawable PTY and historically
		// reports one row less than the outer ClientInstance viewport.
		TerminalRows: 22,
	})
	if result.exitCode != 0 || result.bootstrap != nil {
		t.Fatalf("pane CLI new result = %#v", result)
	}
	target := d.sessionByName("target")
	if target == nil {
		t.Fatal("pane CLI new did not create target")
	}
	t.Cleanup(func() {
		for _, pane := range target.PanesSnapshot() {
			_ = terminatePane(pane)
		}
		stopState(target)
	})

	layout := decodeTestClientLayout(t, <-instance.controlOut)
	if len(layout.Panes) != 1 {
		t.Fatalf("pane CLI new layout panes = %#v", layout.Panes)
	}
	pane := target.activePane()
	if pane == nil {
		t.Fatal("pane CLI new target has no active pane")
	}
	cols, rows := pane.TerminalSize()
	if cols != layout.Panes[0].Rect.Width || rows != layout.Panes[0].Rect.Height || cols != 80 || rows != 23 {
		t.Fatalf("target pane size = %dx%d, switched layout = %#v", cols, rows, layout.Panes)
	}

	type decodedCommand struct {
		command protocol.DisplayCommand
		err     error
	}
	decoded := make(chan decodedCommand, 1)
	go func() {
		command, _, err := protocol.NewDisplayDecoder(&paneOutput).ReadCommand()
		decoded <- decodedCommand{command: command, err: err}
	}()
	select {
	case got := <-decoded:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.command.Opcode != protocol.DisplayOpcodeStartRender ||
			got.command.LayoutRevision != layout.LayoutRevision ||
			got.command.GridCols != layout.Panes[0].Rect.Width ||
			got.command.GridRows != layout.Panes[0].Rect.Height {
			t.Fatalf("target START_RENDER = %#v, layout = %#v", got.command, layout)
		}
	case <-time.After(time.Second):
		t.Fatal("pane CLI new target did not publish pane output")
	}
}

func TestPaneCLIRestoreResolvesInvokerCwdAndSwitchesOuterClient(t *testing.T) {
	d := newCommandTestDaemon(t)
	setCommandTestPersistenceDir(t, d)
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "dev6.meja"), []byte(`root "."
window {
    pane
}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	source := NewSessionState(17)
	t.Cleanup(func() { stopState(source) })
	source.daemon = d
	source.rootDir = t.TempDir()
	state := newTestClient(source)
	state.setTestTerminalSize(100, 30)
	createTestWindow(source, &Pane{ID: testAddPaneID(source), terminal: newTerminal(100, 30)})
	d.sessions[source.ID] = source

	identity := &ClientIdentity{ResumeToken: "stable-token"}
	instance := newClientInstance(d, identity)
	instance.controlOut = make(chan protocol.Frame, 8)
	setTestClient(source, instance)
	d.clientTokens[identity.ResumeToken] = identity.ID
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{WindowID: source.ActiveWindowID, SessionID: source.ID, ClientID: instance.identity.ID, Generation: 1}
	instance.ViewLeaseWindowID = source.ActiveWindowID
	instance.ViewLeaseGeneration = 1
	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"new", "-f", "dev6.meja", "-s", "mynewsession", "--commands=skip"},
		WorkingDirectory:    project,
		CallerSessionTarget: "17",
	})
	if result.exitCode != 0 || result.bootstrap != nil {
		t.Fatalf("pane CLI restore result = %#v", result)
	}
	restored := d.sessionByName("mynewsession")
	if restored == nil {
		t.Fatal("pane CLI restore did not create mynewsession")
	}
	if restored.rootDir != project {
		t.Fatalf("restored root = %q, want %q", restored.rootDir, project)
	}
	if testClientOf(source) != nil || testClientOf(restored) != instance ||
		identity.SessionID != restored.ID || source.ClientID != 0 || restored.ClientID != identity.ID {
		t.Fatalf("calling client was not activated: source=%#v restored=%#v identity=%#v",
			testClientOf(source), testClientOf(restored), identity)
	}

	d.detachClientInstance(instance)
	for _, pane := range restored.PanesSnapshot() {
		_ = terminatePane(pane)
	}
	stopState(restored)
}

func TestPaneCLIAttachUsesPreparedTransitionToCallingClient(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSessionState(17)
	target := NewSessionState(18)
	t.Cleanup(func() { stopState(source) })
	t.Cleanup(func() { stopState(target) })
	for _, session := range []*SessionState{source, target} {
		prepareTestDaemonSession(d, session, 80, 23)
	}
	target.setSessionName("target")
	d.names[target.Name] = target

	identity := &ClientIdentity{ResumeToken: "stable-token"}
	instance := newClientInstance(d, identity)
	instance.controlOut = make(chan protocol.Frame, 8)
	setTestClient(source, instance)
	d.windowLeases[source.ActiveWindowID] = &WindowViewLease{WindowID: source.ActiveWindowID, SessionID: source.ID, ClientID: instance.identity.ID, Generation: 1}
	instance.ViewLeaseWindowID = source.ActiveWindowID
	instance.ViewLeaseGeneration = 1
	result := d.executeCommand(protocol.CommandRequest{
		Args: []string{"attach", "-t", "target"}, CallerSessionTarget: "17",
	})
	if result.exitCode != 0 || result.bootstrap != nil || testClientOf(target) != instance || testClientOf(source) != nil {
		t.Fatalf("pane CLI attach did not apply its prepared view transition: result=%#v source=%#v target=%#v", result, testClientOf(source), testClientOf(target))
	}
}

func TestRepeatedLiveSwitchKeepsLayoutRevisionsMonotonic(t *testing.T) {
	d := newCommandTestDaemon(t)
	source := NewSessionState(1)
	target := NewSessionState(2)
	t.Cleanup(func() { stopState(source) })
	t.Cleanup(func() { stopState(target) })
	for _, session := range []*SessionState{source, target} {
		prepareTestDaemonSession(d, session, 80, 23)
	}

	identity := &ClientIdentity{ResumeToken: "stable-token"}
	instance := newClientInstance(d, identity)
	instance.controlOut = make(chan protocol.Frame, 8)

	instance.sessionID = source.ID
	setTestClient(source, instance)
	if err := instance.applyCurrentTestViewWithHandoff(nil); err != nil {
		t.Fatal(err)
	}
	first := decodeTestClientLayout(t, <-instance.controlOut)

	transition, err := d.transitionClientToSession(instance.identity, target.ID, 80, 23)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.applyViewTransition(transition); err != nil {
		t.Fatal(err)
	}
	second := decodeTestClientLayout(t, <-instance.controlOut)
	if second.LayoutRevision <= first.LayoutRevision {
		t.Fatalf("first switch revision = %d, want greater than %d", second.LayoutRevision, first.LayoutRevision)
	}
	transition, err = d.transitionClientToSession(instance.identity, source.ID, 80, 23)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.applyViewTransition(transition); err != nil {
		t.Fatal(err)
	}
	third := decodeTestClientLayout(t, <-instance.controlOut)
	if third.LayoutRevision <= second.LayoutRevision {
		t.Fatalf("second switch revision = %d, want greater than %d", third.LayoutRevision, second.LayoutRevision)
	}
}

func decodeTestClientLayout(t *testing.T, frame protocol.Frame) protocol.ClientLayout {
	t.Helper()
	if frame.Type != protocol.MsgClientLayout {
		t.Fatalf("control frame type = %d, want CLIENT_LAYOUT", frame.Type)
	}
	layout, err := protocol.DecodeClientLayout(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return layout
}

func TestQueuedResizeBurstCollapsesBeforeInput(t *testing.T) {
	events := make(chan clientControlEvent, 128)
	first := clientControlEvent{frame: encodedTestFrame(t, protocol.MsgFrontendResize, protocol.FrontendResize{Cols: 1, Rows: 10}, protocol.EncodeFrontendResize)}
	for cols := uint16(2); cols <= 100; cols++ {
		events <- clientControlEvent{frame: encodedTestFrame(t, protocol.MsgFrontendResize, protocol.FrontendResize{Cols: cols, Rows: 10}, protocol.EncodeFrontendResize)}
	}
	input := clientControlEvent{frame: protocol.Frame{Type: protocol.MsgFrontendInputBytes}}
	events <- input

	batch := coalesceQueuedResizeEvents(first, events)
	if len(batch) != 2 || batch[1].frame.Type != protocol.MsgFrontendInputBytes {
		t.Fatalf("coalesced batch types = %#v, want latest resize then input", batch)
	}
	resize, err := protocol.DecodeFrontendResize(batch[0].frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if resize.Cols != 100 {
		t.Fatalf("coalesced resize width = %d, want latest width 100", resize.Cols)
	}
}

func TestDaemonQUICListenerResumesByClientInstance(t *testing.T) {
	d := newCommandTestDaemonWithActor(t)
	setCommandTestPersistenceDir(t, d)
	result := d.executeCommand(protocol.CommandRequest{
		Args:         []string{"new", "--", "/bin/sleep", "30"},
		TerminalCols: 80,
		TerminalRows: 23,
	})
	if result.exitCode != 0 || result.bootstrap == nil {
		t.Fatalf("create result = %#v", result)
	}
	bootstrap := *result.bootstrap

	firstConn, _, firstControl, resumeToken := dialTestClientInstance(t, bootstrap, "")
	firstLayoutFrame, err := firstControl.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if firstLayoutFrame.Type != protocol.MsgClientLayout {
		t.Fatalf("first post-attach control frame type = %d, want CLIENT_LAYOUT", firstLayoutFrame.Type)
	}
	firstLayout, err := protocol.DecodeClientLayout(firstLayoutFrame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var firstInstance *ClientIdentity
	var firstClientID ClientID
	d.call(func() {
		firstInstance = d.clients[result.session.ClientID]
		if firstInstance != nil {
			firstClientID = firstInstance.ID
		}
	})
	if firstInstance == nil || firstClientID == 0 {
		t.Fatal("initial transport was not registered as the live client instance")
	}
	_ = firstConn.CloseWithError(1, "test disconnect")
	secondConn, _, secondControl, resumedToken := dialTestClientInstance(t, bootstrap, resumeToken)
	defer secondConn.CloseWithError(0, "")
	secondLayoutFrame, err := secondControl.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if secondLayoutFrame.Type != protocol.MsgClientLayout {
		t.Fatalf("first post-resume control frame type = %d, want CLIENT_LAYOUT", secondLayoutFrame.Type)
	}
	secondLayout, err := protocol.DecodeClientLayout(secondLayoutFrame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if secondLayout.LayoutRevision <= firstLayout.LayoutRevision {
		t.Fatalf("resumed layout revision = %d, want newer than retained frontend revision %d", secondLayout.LayoutRevision, firstLayout.LayoutRevision)
	}

	if resumeToken == "" || resumedToken != resumeToken {
		t.Fatalf("resume identity changed from %q to %q", resumeToken, resumedToken)
	}
	var secondInstance *ClientIdentity
	var secondClientID ClientID
	deadline := time.Now().Add(time.Second)
	for secondInstance == nil && time.Now().Before(deadline) {
		d.call(func() {
			secondInstance = d.clients[result.session.ClientID]
			if secondInstance != nil {
				secondClientID = secondInstance.ID
			}
		})
		if secondInstance == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if secondInstance == nil || secondInstance != firstInstance {
		t.Fatalf("logical client identity changed across reconnect: got %#v, want %#v", secondInstance, firstInstance)
	}
	if secondClientID == 0 || secondClientID != firstClientID {
		t.Fatalf("reconnect client ID = %d, want stable ID %d", secondClientID, firstClientID)
	}
}

func TestNormalDetachWaitsForFrontendTerminalExitCompletion(t *testing.T) {
	d := newCommandTestDaemonWithActor(t)
	setCommandTestPersistenceDir(t, d)
	result := d.executeCommand(protocol.CommandRequest{
		Args:         []string{"new", "--", "/bin/sleep", "30"},
		TerminalCols: 80,
		TerminalRows: 23,
	})
	if result.exitCode != 0 || result.bootstrap == nil {
		t.Fatalf("create result = %#v", result)
	}

	conn, control, decoder, _ := dialTestClientInstance(t, *result.bootstrap, "")
	defer conn.CloseWithError(0, "")
	input := encodedTestFrame(t, protocol.MsgFrontendInputBytes, protocol.FrontendInputBytes{Data: []byte{0x02, 'd'}}, protocol.EncodeFrontendInputBytes)
	if err := protocol.NewEncoder(control).WriteFrame(input); err != nil {
		t.Fatal(err)
	}

	if err := control.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			t.Fatalf("read terminal exit request: %v", err)
		}
		if frame.Type != protocol.MsgFrontendExecuteTerminalExitCommand {
			continue
		}
		if len(frame.Payload) != 0 {
			t.Fatalf("terminal exit request payload = %q", frame.Payload)
		}
		break
	}

	controlEncoder := protocol.NewEncoder(control)
	queuedResize := encodedTestFrame(t, protocol.MsgFrontendResize, protocol.FrontendResize{Cols: 81, Rows: 24}, protocol.EncodeFrontendResize)
	if err := controlEncoder.WriteFrame(queuedResize); err != nil {
		t.Fatal(err)
	}
	if err := controlEncoder.WriteFrame(protocol.Frame{Type: protocol.MsgFrontendTerminalExitComplete}); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.ReadFrame(); err == nil {
		t.Fatal("connection remained open after terminal exit completion")
	}
}

func TestResizeBurstPreservesDetachInput(t *testing.T) {
	d := newCommandTestDaemonWithActor(t)
	var serverLog bytes.Buffer
	d.stderr = &serverLog
	setCommandTestPersistenceDir(t, d)
	result := d.executeCommand(protocol.CommandRequest{
		Args:         []string{"new", "--", "/bin/sleep", "30"},
		TerminalCols: 80,
		TerminalRows: 23,
	})
	if result.exitCode != 0 || result.bootstrap == nil {
		t.Fatalf("create result = %#v", result)
	}

	conn, control, decoder, _ := dialTestClientInstance(t, *result.bootstrap, "")
	defer conn.CloseWithError(0, "")
	encoder := protocol.NewEncoder(control)
	for index := 0; index < 64; index++ {
		resize := protocol.FrontendResize{Cols: uint16(40 + index%41), Rows: uint16(12 + index%13)}
		if index == 63 {
			resize = protocol.FrontendResize{Cols: 47, Rows: 18}
		}
		frame := encodedTestFrame(t, protocol.MsgFrontendResize, resize, protocol.EncodeFrontendResize)
		if got, err := protocol.DecodeFrontendResize(frame.Payload); err != nil || got != resize {
			t.Fatalf("encoded resize = %#v, %v, want %#v", got, err, resize)
		}
		if err := encoder.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	detach := encodedTestFrame(t, protocol.MsgFrontendInputBytes, protocol.FrontendInputBytes{Data: []byte{0x02, 'd'}}, protocol.EncodeFrontendInputBytes)
	if err := encoder.WriteFrame(detach); err != nil {
		t.Fatal(err)
	}

	if err := control.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			t.Fatalf("detach input was starved behind resize burst: %v; server log: %s", err, serverLog.String())
		}
		if frame.Type == protocol.MsgFrontendExecuteTerminalExitCommand {
			break
		}
	}

	var client *ClientIdentity
	d.call(func() {
		client = d.clients[result.session.ClientID]
	})
	if client == nil {
		t.Fatal("attached client disappeared before detach completed")
	}
	if gotCols, gotRows := client.terminalCols.Load(), client.terminalRows.Load(); gotCols != 47 || gotRows != 18 {
		t.Fatalf("terminal size before detach = %dx%d, want latest queued size 47x18", gotCols, gotRows)
	}
}

func TestPaneCLIRestoreRetargetsLiveInputLoop(t *testing.T) {
	d := newCommandTestDaemonWithActor(t)
	setCommandTestPersistenceDir(t, d)
	sourceRoot := t.TempDir()
	created := d.executeCommand(protocol.CommandRequest{
		Args: []string{"new", "-s", "source", "--", "/bin/sleep", "30"}, WorkingDirectory: sourceRoot,
		TerminalCols: 80, TerminalRows: 23,
	})
	if created.exitCode != 0 || created.bootstrap == nil {
		t.Fatalf("create source = %#v", created)
	}
	conn, input, _, _ := dialTestClientInstance(t, *created.bootstrap, "")
	source := d.sessionByName("source")
	waitForAttachedClient(t, source)

	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "dev.meja"), []byte(`root "."
window {
    pane
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	result := d.executeCommand(protocol.CommandRequest{
		Args:                []string{"new", "-f", "dev.meja", "-s", "target", "--commands=skip"},
		WorkingDirectory:    project,
		CallerSessionTarget: strconv.FormatUint(source.ID, 10),
	})
	if result.exitCode != 0 || result.bootstrap != nil {
		t.Fatalf("pane CLI restore = %#v", result)
	}
	target := d.sessionByName("target")
	if target == nil {
		t.Fatal("pane CLI restore did not create target")
	}

	resize := encodedTestFrame(t, protocol.MsgFrontendResize, protocol.FrontendResize{Cols: 99, Rows: 31}, protocol.EncodeFrontendResize)
	if err := protocol.NewEncoder(input).WriteFrame(resize); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var cols, rows uint16
		if err := runStateOperation(target, func() error {
			if window := target.Windows[target.ActiveWindowID]; window != nil {
				cols, rows = window.Cols, window.Rows
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if cols == 99 && rows == 31 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-transition input remained frozen: target size=%dx%d", cols, rows)
		}
		time.Sleep(time.Millisecond)
	}
	_ = conn.CloseWithError(0, "")
	stopPersistenceTestSession(t, source)
	stopPersistenceTestSession(t, target)
}

func waitForAttachedClient(t *testing.T, session *SessionState) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		attached := false
		if err := runStateOperation(session, func() error {
			attached = session.attachedClient() != nil
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if attached {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session did not attach")
		}
		time.Sleep(time.Millisecond)
	}
}

func stopPersistenceTestSession(t *testing.T, session *SessionState) {
	t.Helper()
	for _, pane := range session.PanesSnapshot() {
		_ = terminatePane(pane)
	}
	stopState(session)
}

func dialTestClientInstance(t *testing.T, bootstrap protocol.CommandBootstrap, token string) (quic.Connection, quic.Stream, *protocol.Decoder, string) {
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
	control, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	controlEncoder := protocol.NewEncoder(control)
	if token == "" {
		if err := controlEncoder.WriteFrame(encodedTestFrame(t, protocol.MsgSessionAttach, protocol.SessionAttach{Token: bootstrap.AttachToken, Cols: 80, Rows: 23}, protocol.EncodeSessionAttach)); err != nil {
			t.Fatal(err)
		}
	} else if err := controlEncoder.WriteFrame(encodedTestFrame(t, protocol.MsgClientResume, protocol.ClientResume{ResumeToken: token, Cols: 80, Rows: 23}, protocol.EncodeClientResume)); err != nil {
		t.Fatal(err)
	}
	controlDecoder := protocol.NewDecoder(control, protocol.DefaultMaxFrameSize)
	frame, err := controlDecoder.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	resumeToken := token
	if token == "" {
		if frame.Type != protocol.MsgSessionAttachOK {
			t.Fatalf("attach frame type = %d", frame.Type)
		}
		attached, err := protocol.DecodeSessionAttachOK(frame.Payload)
		if err != nil {
			t.Fatal(err)
		}
		resumeToken = attached.ResumeToken
	} else {
		if frame.Type != protocol.MsgClientResumeOK {
			t.Fatalf("resume frame type = %d", frame.Type)
		}
		if _, err := protocol.DecodeClientResumeOK(frame.Payload); err != nil {
			t.Fatal(err)
		}
	}
	exitCommandFrame, err := controlDecoder.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if exitCommandFrame.Type != protocol.MsgFrontendRegisterTerminalExitCommand {
		t.Fatalf("frontend terminal exit command frame type = %d", exitCommandFrame.Type)
	}
	exitCommand, err := protocol.DecodeFrontendRegisterTerminalExitCommand(exitCommandFrame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(exitCommand.Data) == 0 {
		t.Fatal("frontend terminal exit command was empty")
	}
	setupFrame, err := controlDecoder.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if setupFrame.Type != protocol.MsgFrontendTerminalWrite {
		t.Fatalf("frontend terminal setup frame type = %d", setupFrame.Type)
	}
	setup, err := protocol.DecodeFrontendTerminalWrite(setupFrame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(setup.Data) == 0 {
		t.Fatal("frontend terminal setup was empty")
	}
	return conn, control, controlDecoder, resumeToken
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
	d := &Daemon{sessions: map[uint64]*SessionState{9: {}, 2: {}, 4: {}}}
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

	if err := d.shutdownSession(first.session); err != nil {
		t.Fatal(err)
	}
	if rebound, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: int(first.bootstrap.Port)}); err == nil {
		_ = rebound.Close()
		t.Fatalf("daemon UDP port %d was released with one session", first.bootstrap.Port)
	}
}
