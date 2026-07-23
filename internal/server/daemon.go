package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

const attachTTL = 2 * time.Minute

type Config struct {
	ControlPath      string
	Stdout           io.Writer
	Stderr           io.Writer
	TerminalDebugLog io.Writer
}

// Daemon owns the server-wide session registry, client-instance registry,
// one-to-one assignments, attach grants, and shared QUIC listener. All
// registry access is serialized by requests; control handlers query it and
// then query Sessions separately, so neither actor waits on the other.
type Daemon struct {
	logMu                sync.Mutex
	shutdownMu           sync.Mutex
	requests             chan daemonRequest
	requestLoopOnce      sync.Once
	requestLoopReady     atomic.Bool
	commandEngineOnce    sync.Once
	commands             *CommandEngine
	nextID               uint64
	nextPaneID           uint64
	nextWindowID         uint64
	nextGroupID          uint64
	nextAttachmentID     uint64
	sessions             map[uint64]*SessionState
	sessionIndex         sync.Map // uint64 -> *SessionState; lock-free client lookup
	groups               map[uint64]*GroupState
	groupIndex           sync.Map // uint64 -> *GroupState
	panes                map[uint64]*Pane
	paneIndex            sync.Map // uint64 -> *Pane; process exits resolve directly
	windowIndex          sync.Map // uint64 -> *Window
	projectionRevisions  map[uint64]uint64
	windowLeases         map[uint64]*WindowViewLease
	names                map[string]*SessionState
	reconnectCredentials map[string]*reconnectCredential
	// clientSessions is separate from reconnectCredentials: it is only the
	// target-session hint consulted when rebuilding a client after reconnect.
	clientSessions           map[*reconnectCredential]uint64
	attachments              map[uint64]*reconnectCredential
	clients                  map[uint64]*ClientInstance
	clientIndex              sync.Map // uint64 -> *ClientInstance; lock-free client lookup
	shutdowns                map[uint64]*sessionShutdown
	attachGrants             []attachGrant
	tlsConfig                *tls.Config
	certHash                 string
	quicMu                   sync.Mutex
	quicListener             *quic.Listener
	quicPort                 uint16
	quicCancel               context.CancelFunc
	serverCtx                context.Context
	stop                     context.CancelFunc
	processMonitor           *ProcessMonitor
	processObserver          ProcessObserver
	processObservations      map[uint64]ProcessObservation
	processSaveCandidates    map[uint64]processSaveCandidate
	sessionPersistions       map[uint64]*SessionPersistence
	persistenceGroups        map[uint64]*GroupState
	obsoletePersistenceNames map[uint64]map[string]struct{}
	persistenceOnce          sync.Once
	persistenceNow           chan struct{}
	persistenceStop          chan struct{}
	persistenceStopOnce      sync.Once
	persistenceDone          chan struct{}
	persistenceStarted       atomic.Bool
	persistenceUpdates       chan persistenceSnapshot
	pasteBuffers             pasteBufferStore
	stderr                   io.Writer
	controlPath              string
	sessionPersistenceDir    string
}

type sessionShutdown struct {
	once     sync.Once
	err      error
	stopping bool
}

type daemonRequest struct {
	run  func()
	done chan struct{}
}

type clientStatusState struct {
	SessionID   uint64
	SessionName string
	Root        string
	Windows     []WindowStatus
}

func (d *Daemon) runRequests(ctx context.Context) {
	for {
		select {
		case request := <-d.requests:
			request.run()
			if request.done != nil {
				close(request.done)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *Daemon) call(run func()) {
	d.ensureRequestLoop()
	done := make(chan struct{})
	d.requests <- daemonRequest{run: run, done: done}
	<-done
}

// clientStatusSnapshot copies every daemon-owned value needed by status
// rendering in one transaction. Client actors must not walk SessionState maps.
func (d *Daemon) clientStatusSnapshot(sessionID uint64) (clientStatusState, bool) {
	var snapshot clientStatusState
	var ok bool
	if d == nil {
		return snapshot, false
	}
	d.call(func() {
		state := d.sessions[sessionID]
		if state == nil {
			return
		}
		snapshot = clientStatusState{
			SessionID: state.ID, SessionName: state.Name, Root: state.rootDir,
			Windows: state.WindowStatuses(),
		}
		ok = true
	})
	return snapshot, ok
}

func (d *Daemon) ensureRequestLoop() {
	if d == nil {
		return
	}
	d.requestLoopOnce.Do(func() {
		if d.requests == nil {
			d.requests = make(chan daemonRequest, 64)
			go d.runRequests(context.Background())
		}
		d.requestLoopReady.Store(true)
	})
}

func (d *Daemon) post(run func()) {
	d.ensureRequestLoop()
	d.requests <- daemonRequest{run: run}
}

func (d *Daemon) processObservationDelivery(sessionID uint64) func(monitoredProcessBatch) {
	return func(batch monitoredProcessBatch) {
		d.post(func() {
			if state := d.sessions[sessionID]; state != nil {
				_ = d.applyMonitoredProcessObservations(state, batch)
				if state.group != nil {
					for _, memberID := range state.group.memberIDsSnapshot() {
						if client := d.clients[memberID]; client != nil {
							client.post(func() error { return client.publishStatusBar() })
						}
					}
				}
			}
		})
	}
}

func (d *Daemon) postPaneProcessExit(paneID uint64) {
	if d == nil || paneID == 0 {
		return
	}
	d.post(func() {
		transitions, clients, terminalStates := d.applyPaneExit(paneID)
		go func() {
			for index, client := range clients {
				transition := transitions[index]
				plan := transition.Projection
				client.post(func() error {
					d.logf("meja pane-exit: deliver pane=%d attachment=%d session=%d window=%d projection=%d layout=%d close=%t\n",
						paneID, client.AttachmentID, plan.SessionID, plan.WindowID, plan.ProjectionRevision, plan.LayoutRevision, plan.Close)
					if err := client.applyViewTransition(transition); err != nil {
						if staleErr := client.validateProjectionPlan(plan); staleErr != nil {
							d.logf("meja pane-exit: discard stale pane=%d attachment=%d projection=%d: %v\n",
								paneID, client.AttachmentID, plan.ProjectionRevision, staleErr)
							return nil
						}
						return fmt.Errorf("pane-exit pane=%d projection=%d: %w", paneID, plan.ProjectionRevision, err)
					}
					d.logf("meja pane-exit: published pane=%d attachment=%d window=%d projection=%d layout=%d\n",
						paneID, client.AttachmentID, plan.WindowID, plan.ProjectionRevision, client.highestLayoutRevision.Load())
					return nil
				})
			}
			if len(terminalStates) > 0 {
				for _, state := range terminalStates {
					_ = d.shutdownSession(state)
				}
			}
		}()
	})
}

// applyPaneExit reconciles a pane exactly once from its daemon-global identity.
// It returns immutable client plans; all transport work is performed after the
// transaction has returned.
func (d *Daemon) applyPaneExit(paneID uint64) ([]PreparedViewTransition, []*ClientInstance, []*SessionState) {
	var transitions []PreparedViewTransition
	var clients []*ClientInstance
	var terminalStates []*SessionState
	value, ok := d.paneIndex.Load(paneID)
	if !ok || value == nil {
		return nil, nil, nil
	}
	pane := value.(*Pane)
	windowValue, ok := d.windowIndex.Load(pane.WindowID)
	if !ok || windowValue == nil {
		return nil, nil, nil
	}
	window := windowValue.(*Window)
	groupValue, ok := d.groupIndex.Load(window.GroupID)
	if !ok || groupValue == nil {
		return nil, nil, nil
	}
	group := groupValue.(*GroupState)
	var owner *SessionState
	for _, memberID := range group.memberIDsSnapshot() {
		if state := d.sessions[memberID]; state != nil {
			owner = state
			break
		}
	}
	if owner == nil {
		return nil, nil, nil
	}
	removedPane, affectedWindow, removed, err := owner.removeGroupPaneNow(paneID)
	if err != nil || !removed || removedPane == nil || affectedWindow == nil {
		return nil, nil, nil
	}
	pane = removedPane
	d.unwatchPaneProcesses(paneID)
	for _, memberID := range group.memberIDsSnapshot() {
		state := d.sessions[memberID]
		if state == nil {
			continue
		}
		if len(state.Windows) == 0 {
			terminalStates = append(terminalStates, state)
		}
		client := d.clients[state.ID]
		if client == nil {
			continue
		}
		if state.ActiveWindowID != 0 {
			_, _ = prepareClientWindowGeometryNow(client, state, state.ActiveWindowID)
		}
		transition := d.prepareViewTransitionNow(viewTransitionPaneExit, client, state, true, pane)
		if state.ActiveWindowID == 0 {
			transition.Projection.Close = true
			transition.Projection.CloseReason = "no viewable fallback window"
		}
		transitions = append(transitions, transition)
		clients = append(clients, client)
	}
	return transitions, clients, terminalStates
}

func Run(ctx context.Context, cfg Config) error {
	serverCtx, stop := context.WithCancel(context.Background())
	defer stop()
	setTerminalDebugLogger(cfg.TerminalDebugLog)
	socket := cfg.ControlPath
	if socket == "" {
		var err error
		socket, err = defaultCommandSocketPath()
		if err != nil {
			return err
		}
	}
	lock, err := acquireCommandServerLock(socket)
	if err != nil {
		return err
	}
	defer lock.Close()
	cert, hash, err := daemonCertificate()
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	d := &Daemon{
		requests:              make(chan daemonRequest, 64),
		nextID:                1,
		nextPaneID:            1,
		nextWindowID:          1,
		nextGroupID:           1,
		sessions:              make(map[uint64]*SessionState),
		groups:                make(map[uint64]*GroupState),
		panes:                 make(map[uint64]*Pane),
		windowLeases:          make(map[uint64]*WindowViewLease),
		names:                 make(map[string]*SessionState),
		reconnectCredentials:  make(map[string]*reconnectCredential),
		clientSessions:        make(map[*reconnectCredential]uint64),
		attachments:           make(map[uint64]*reconnectCredential),
		clients:               make(map[uint64]*ClientInstance),
		shutdowns:             make(map[uint64]*sessionShutdown),
		tlsConfig:             tlsConfig,
		certHash:              hash,
		serverCtx:             serverCtx,
		stop:                  stop,
		stderr:                cfg.Stderr,
		controlPath:           socket,
		sessionPersistenceDir: sessionPersistenceDirectory(socket),
	}
	d.commands = newCommandEngine(d)
	d.processMonitor = NewProcessMonitor(serverCtx, nil)
	d.processObserver = NewProcessObserver()
	d.processObservations = make(map[uint64]ProcessObservation)
	d.processSaveCandidates = make(map[uint64]processSaveCandidate)
	d.sessionPersistions = make(map[uint64]*SessionPersistence)
	d.persistenceGroups = make(map[uint64]*GroupState)
	d.obsoletePersistenceNames = make(map[uint64]map[string]struct{})
	d.persistenceNow = make(chan struct{}, 1)
	d.persistenceStop = make(chan struct{})
	d.persistenceDone = make(chan struct{})
	d.persistenceUpdates = make(chan persistenceSnapshot, 1)
	actorCtx, stopActor := context.WithCancel(context.Background())
	go d.runRequests(actorCtx)
	defer func() {
		d.disconnectActiveClients()
		d.stopPersistence()
		if d.persistenceStarted.Load() {
			<-d.persistenceDone
		}
		d.closeQUIC()
		stopActor()
	}()
	go func() {
		select {
		case <-ctx.Done():
			d.disconnectActiveClients()
			stop()
		case <-serverCtx.Done():
		}
	}()
	err = serveCommandSocket(serverCtx, socket, d)
	if err != nil {
		stop()
	}
	return err
}

func sessionPersistenceDirectory(controlPath string) string {
	return filepath.Join(filepath.Dir(controlPath), "sessions")
}

func (d *Daemon) logSessionAttached(sessionID uint64) {
	d.logf("meja server: session %d attached\n", sessionID)
}

func (d *Daemon) logf(format string, args ...any) {
	if d.stderr == nil {
		return
	}
	d.logMu.Lock()
	defer d.logMu.Unlock()
	fmt.Fprintf(d.stderr, format, args...)
}

func daemonCertificate() (tls.Certificate, string, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "meja-daemon"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"meja-daemon"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, publicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	hash := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	return cert, hex.EncodeToString(hash[:]), nil
}

// disconnectActiveClients shuts sessions down concurrently so pane termination
// retains one global grace period instead of multiplying it by session count.
// Clients receive the same clean QUIC close used by an explicit detach.
func (d *Daemon) disconnectActiveClients() {
	var sessions []*SessionState
	d.call(func() {
		sessions = make([]*SessionState, 0, len(d.sessions))
		for _, session := range d.sessions {
			sessions = append(sessions, session)
		}
	})
	var wait sync.WaitGroup
	wait.Add(len(sessions))
	for _, session := range sessions {
		go func() {
			defer wait.Done()
			_ = d.shutdownSession(session)
		}()
	}
	wait.Wait()
}

func (d *Daemon) sessionShutdownState(id uint64) *sessionShutdown {
	d.shutdownMu.Lock()
	defer d.shutdownMu.Unlock()
	if d.shutdowns == nil {
		d.shutdowns = make(map[uint64]*sessionShutdown)
	}
	state := d.shutdowns[id]
	if state == nil {
		state = &sessionShutdown{}
		d.shutdowns[id] = state
	}
	return state
}

func (d *Daemon) isSessionStopping(state *SessionState) bool {
	if d == nil || state == nil {
		return false
	}
	d.shutdownMu.Lock()
	defer d.shutdownMu.Unlock()
	return d.shutdowns[state.ID] != nil && d.shutdowns[state.ID].stopping
}

func (d *Daemon) shutdownSession(state *SessionState) error {
	return d.shutdownSessionWithTimeouts(state, defaultPaneTerminationTimeouts)
}

func (d *Daemon) shutdownSessionWithTimeouts(state *SessionState, timeouts paneTerminationTimeouts) error {
	if d == nil || state == nil {
		return nil
	}
	lifecycle := d.sessionShutdownState(state.ID)
	lifecycle.once.Do(func() {
		d.shutdownMu.Lock()
		lifecycle.stopping = true
		d.shutdownMu.Unlock()

		var panes []*Pane
		lastGroupMember := !state.isGrouped()
		if lastGroupMember {
			panes = make([]*Pane, 0, len(state.Panes))
			for _, pane := range state.Panes {
				panes = append(panes, pane)
			}
		}
		d.shutdownSessionNow(state)
		if remaining := terminatePanesAndWait(panes, timeouts); len(remaining) > 0 {
			cleanupErr := fmt.Errorf("%d pane process(es) did not exit before the shutdown deadline", len(remaining))
			d.logf("meja server: shut down session %d: %v\n", state.ID, cleanupErr)
			lifecycle.err = errors.Join(lifecycle.err, cleanupErr)
		}
	})
	return lifecycle.err
}

func (d *Daemon) startPersistence(sessionPersistenceDir string) {
	if d == nil || sessionPersistenceDir == "" {
		return
	}
	d.persistenceOnce.Do(func() {
		d.persistenceStarted.Store(true)
		go func() {
			defer close(d.persistenceDone)
			d.runPersistence(context.Background(), sessionPersistenceDir)
		}()
	})
}

func (d *Daemon) stopPersistence() {
	if d == nil || !d.persistenceStarted.Load() {
		return
	}
	d.persistenceStopOnce.Do(func() { close(d.persistenceStop) })
}

func (d *Daemon) runPersistence(ctx context.Context, sessionPersistenceDir string) {
	for {
		select {
		case <-d.persistenceNow:
			select {
			case update := <-d.persistenceUpdates:
				saveCtx, cancel := context.WithTimeout(ctx, sessionPersistenceTimeout)
				_, err := flushPersistenceSnapshot(saveCtx, sessionPersistenceDir, update)
				cancel()
				if err != nil {
					d.logf("meja server: persist snapshot: %v\n", err)
				}
			default:
			}
		case <-d.persistenceStop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (d *Daemon) shutdownSessionNow(state *SessionState) {
	if d == nil || state == nil {
		return
	}
	d.call(func() { d.shutdownSessionInActor(state) })
}

// shutdownSessionInActor performs registry mutation in the daemon actor.
// Waiting for pane processes and persistence happens after this transaction.
func (d *Daemon) shutdownSessionInActor(state *SessionState) {
	if d.processMonitor != nil {
		var transferred bool
		if state.isGrouped() {
			for memberID := range state.group.SessionIDs {
				if memberID == state.ID {
					continue
				}
				if member := d.sessions[memberID]; member != nil {
					d.processMonitor.TransferSession(state.ID, member.ID, d.processObservationDelivery(member.ID))
					transferred = true
					break
				}
			}
		}
		if !transferred {
			d.processMonitor.DropSession(state.ID)
		}
	}
	client := d.clients[state.ID]
	delete(d.clients, state.ID)
	if client != nil && client.QUIC != nil {
		_ = client.QUIC.CloseWithError(0, "")
	}
	d.removeSession(state)
}

func newSession(id uint64, name string) *SessionState {
	session := NewSessionState(id)
	session.setSessionName(name)
	return session
}

func (d *Daemon) allocatePaneID() (uint64, error) {
	var id uint64
	var err error
	d.call(func() {
		id, err = d.allocatePaneIDNow()
	})
	return id, err
}

func (d *Daemon) resizeClientView(client *ClientInstance, cols, rows uint16) (PreparedViewTransition, error) {
	var transition PreparedViewTransition
	var err error
	d.call(func() {
		if client == nil {
			err = errSessionUnavailable
			return
		}
		state := d.sessions[client.sessionID]
		if state == nil || d.clients[state.ID] != client {
			err = errSessionUnavailable
			return
		}
		err = resizeSessionWindowModelNow(state, state.ActiveWindowID, cols, rows)
		if err == nil {
			transition = d.prepareViewTransitionNow(viewTransitionResize, client, state, true)
		}
	})
	return transition, err
}

func (d *Daemon) prepareViewTransitionNow(reason ViewTransitionReason, client *ClientInstance, state *SessionState, advance bool, removedPanes ...*Pane) PreparedViewTransition {
	plan := d.projectionPlanLockedWithRevision(client, state, advance)
	transition := PreparedViewTransition{Reason: reason, Projection: plan, RemovedPanes: append([]*Pane(nil), removedPanes...)}
	if plan.Close {
		return transition
	}
	for _, projected := range plan.Panes {
		value, ok := d.paneIndex.Load(projected.PaneID)
		if !ok || value == nil {
			continue
		}
		pane := value.(*Pane)
		cols, rows := pane.TerminalSize()
		if cols == projected.Rect.Width && rows == projected.Rect.Height {
			continue
		}
		transition.PaneResizes = append(transition.PaneResizes, PreparedPaneResize{Pane: pane, Rect: projected.Rect})
	}
	return transition
}

// prepareClientWindowGeometryNow is the geometry half of window entry. Callers
// use it before committing lease/assignment changes. Physical pane grids are
// deliberately deferred into PreparedViewTransition.
func prepareClientWindowGeometryNow(client *ClientInstance, state *SessionState, windowID uint64) (bool, error) {
	if client == nil || state == nil {
		return false, errSessionUnavailable
	}
	window := state.Windows[windowID]
	if window == nil {
		return false, errors.New("no viewable window")
	}
	cols, rows := clientViewportSize(client, window)
	if window.Cols == cols && window.Rows == rows {
		return false, nil
	}
	if state.daemon != nil {
		state.daemon.logf("meja window-entry: reconcile attachment=%d session=%d window=%d from=%dx%d to=%dx%d\n",
			client.AttachmentID, state.ID, windowID, window.Cols, window.Rows, cols, rows)
	}
	if err := resizeSessionWindowModelNow(state, windowID, cols, rows); err != nil {
		if state.daemon != nil {
			state.daemon.logf("meja window-entry: resize failed attachment=%d session=%d window=%d: %v\n",
				client.AttachmentID, state.ID, windowID, err)
		}
		return false, fmt.Errorf("prepare window %d for client viewport: %w", windowID, err)
	}
	if state.daemon != nil {
		state.daemon.logf("meja window-entry: resized attachment=%d session=%d window=%d size=%dx%d layout=%d\n",
			client.AttachmentID, state.ID, windowID, cols, rows, window.LayoutRevision)
	}
	return true, nil
}

func (d *Daemon) projectionPlanLockedWithRevision(client *ClientInstance, state *SessionState, advance bool) ClientProjectionPlan {
	plan := ClientProjectionPlan{AttachmentID: client.AttachmentID, SessionID: state.ID, FullSnapshot: true}
	plan.WindowID = state.ActiveWindowID
	if plan.WindowID == 0 && len(state.Windows) > 0 {
		ids := state.orderedWindowIDs()
		plan.WindowID = ids[0]
	}
	if lease := d.windowLeases[plan.WindowID]; lease != nil && lease.AttachmentID == client.AttachmentID {
		plan.ViewLeaseGeneration = lease.Generation
	}
	plan.Cols, plan.Rows = uint16(client.terminalCols.Load()), uint16(client.terminalRows.Load())
	if plan.Cols == 0 || plan.Rows == 0 {
		if window := state.Windows[plan.WindowID]; window != nil {
			plan.Cols, plan.Rows = window.Cols, window.Rows
		}
	}
	window := state.Windows[plan.WindowID]
	if window != nil {
		plan.LayoutRevision = window.LayoutRevision
		placements := visibleWindowPlacementsForSession(state, window, Rect{Width: int(plan.Cols), Height: int(plan.Rows)})
		plan.Panes = make([]RenderPane, 0, len(placements))
		plan.Bindings = make([]RenderBinding, 0, len(placements))
		for slot, placement := range placements {
			plan.Panes = append(plan.Panes, RenderPane(placement))
			plan.Bindings = append(plan.Bindings, RenderBinding{Slot: slot, PaneID: placement.PaneID})
		}
		view := state.groupWindowViewLocked(plan.WindowID)
		plan.FocusedPaneID = view.FocusedPaneID
		if plan.FocusedPaneID == 0 {
			plan.FocusedPaneID = window.ActivePaneID
		}
		plan.Status = StatusUpdate{Width: int(plan.Cols), StyleID: statusNormalStyleID}
		var status strings.Builder
		for index, item := range state.WindowStatuses() {
			if index > 0 {
				status.WriteByte(' ')
			}
			if item.Active {
				status.WriteByte('[')
			}
			status.WriteString(item.Title)
			if item.Active {
				status.WriteByte(']')
			}
		}
		plan.Status.Text = status.String()
		plan.Status.Location = state.rootDir
	}
	if d.projectionRevisions == nil {
		d.projectionRevisions = make(map[uint64]uint64)
	}
	if d.projectionRevisions[client.AttachmentID] == 0 {
		d.projectionRevisions[client.AttachmentID] = 1
	} else if advance {
		d.projectionRevisions[client.AttachmentID]++
	}
	plan.ProjectionRevision = d.projectionRevisions[client.AttachmentID]
	return plan
}

func (d *Daemon) windowForAttachmentLocked(attachmentID uint64) uint64 {
	for windowID, lease := range d.windowLeases {
		if lease != nil && lease.AttachmentID == attachmentID {
			return windowID
		}
	}
	return 0
}

func (d *Daemon) attachSessionView(state *SessionState, cols, rows uint16, advanceLayoutRevision bool) (PreparedViewTransition, error) {
	var transition PreparedViewTransition
	var err error
	d.call(func() {
		if state == nil {
			err = errSessionUnavailable
			return
		}
		client := d.clients[state.ID]
		if client == nil {
			err = errSessionUnavailable
			return
		}
		if d.windowLeases == nil {
			d.windowLeases = make(map[uint64]*WindowViewLease)
		}
		targetWindowID := state.ActiveWindowID
		if targetWindowID == 0 {
			ids := state.orderedWindowIDs()
			if len(ids) > 0 {
				targetWindowID = ids[0]
			}
		}
		target := state.Windows[targetWindowID]
		if target == nil {
			err = errSessionUnavailable
			return
		}
		sizeChanged, prepareErr := prepareClientWindowGeometryNow(client, state, targetWindowID)
		if prepareErr != nil {
			err = prepareErr
			return
		}
		current := d.windowLeases[targetWindowID]
		if current != nil && current.AttachmentID != client.AttachmentID {
			err = fmt.Errorf("window %d is currently viewed by another client", target.DisplayIndex)
			return
		}
		oldWindowID := d.windowForAttachmentLocked(client.AttachmentID)
		if oldWindowID != 0 && oldWindowID != targetWindowID {
			old := d.windowLeases[oldWindowID]
			if old == nil || old.AttachmentID != client.AttachmentID {
				err = errors.New("stale client window lease")
				return
			}
		}
		generation := uint64(1)
		if current != nil {
			generation = current.Generation + 1
		}
		if oldWindowID == targetWindowID && current != nil {
			generation = current.Generation
		}
		d.windowLeases[targetWindowID] = &WindowViewLease{WindowID: targetWindowID, SessionID: state.ID, AttachmentID: client.AttachmentID, Generation: generation}
		if oldWindowID != 0 && oldWindowID != targetWindowID {
			delete(d.windowLeases, oldWindowID)
		}
		transition = d.prepareViewTransitionNow(viewTransitionAttach, client, state, advanceLayoutRevision || sizeChanged)
	})
	return transition, err
}

// allocatePaneIDNow runs on the daemon actor. IDs are intentionally never
// decremented or recycled, including when pane creation or restoration fails.
func (d *Daemon) allocatePaneIDNow() (uint64, error) {
	if d.nextPaneID == 0 {
		d.nextPaneID = 1
	}
	if d.nextPaneID == ^uint64(0) {
		return 0, errors.New("pane ID exhausted")
	}
	id := d.nextPaneID
	d.nextPaneID++
	return id, nil
}

// sessionByName runs only on the daemon actor.
func (d *Daemon) sessionByName(name string) *SessionState {
	if state := d.names[name]; state != nil {
		return state
	}
	for _, state := range d.sessions {
		if state != nil && state.SessionName() == name {
			return state
		}
	}
	return nil
}

func (d *Daemon) renameSession(state *SessionState, name string) error {
	var renameErr error
	d.call(func() {
		renameErr = d.validateSessionRename(state, name)
		if renameErr == nil {
			d.reserveSessionName(state, name)
			if state.Name != name {
				state.setSessionName(name)
				state.markSessionChangedForPersistence()
			}
		}
	})
	return renameErr
}

func (d *Daemon) validateSessionRename(state *SessionState, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	if d.sessions[state.ID] != state {
		return errSessionUnavailable
	}
	if existing := d.sessionByName(name); existing != nil && existing != state {
		return fmt.Errorf("session %q already exists", name)
	}
	return nil
}

func sessionPersistenceFileExists(sessionPersistenceDir, name string) (bool, error) {
	_, err := os.Stat(filepath.Join(sessionPersistenceDir, name+".session.meja"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (d *Daemon) reserveSessionName(state *SessionState, name string) {
	if d.names == nil {
		d.names = make(map[string]*SessionState)
	}
	for existingName, existing := range d.names {
		if existing == state {
			delete(d.names, existingName)
		}
	}
	if name != "" {
		d.names[name] = state
	}
}

func (d *Daemon) removeSession(state *SessionState) {
	if state == nil {
		return
	}
	if d.sessions[state.ID] != state {
		return
	}
	for index := 0; index < len(d.attachGrants); index++ {
		if d.attachGrants[index].SessionID == state.ID {
			d.attachGrants = append(d.attachGrants[:index], d.attachGrants[index+1:]...)
			index--
		}
	}
	if credential := d.attachments[state.ID]; credential != nil {
		delete(d.attachments, state.ID)
		delete(d.clientSessions, credential)
		credential.TerminalReason = "session is no longer available"
	}
	delete(d.sessions, state.ID)
	d.sessionIndex.Delete(state.ID)
	d.reserveSessionName(state, "")
	for windowID, lease := range d.windowLeases {
		if lease.SessionID == state.ID {
			delete(d.windowLeases, windowID)
		}
	}
	if state.group != nil {
		group := state.group
		delete(group.SessionIDs, state.ID)
		group.publishMembers()
		if len(group.SessionIDs) == 0 {
			delete(d.groups, group.ID)
			d.groupIndex.Delete(group.ID)
		} else {
			for memberID := range group.SessionIDs {
				if member := d.sessions[memberID]; member != nil {
					member.syncGroupLinksLocked()
					member.grouped.Store(len(group.SessionIDs) > 1)
					member.persistSessionForPersistence()
				}
			}
		}
	}
}

func terminatePane(pane *Pane) error {
	if pane == nil {
		return nil
	}
	if pane.Process != nil && pane.Process.Process != nil {
		_ = pane.Process.Process.Signal(syscall.SIGHUP)
	}
	pane.stop()
	return nil
}

type paneTerminationTimeouts struct {
	hangup    time.Duration
	terminate time.Duration
	kill      time.Duration
}

var defaultPaneTerminationTimeouts = paneTerminationTimeouts{
	hangup:    time.Second,
	terminate: time.Second,
	kill:      time.Second,
}

// terminatePanesAndWait preserves terminal hangup semantics first, then
// escalates stubborn pane leaders under one shared deadline per stage. The
// process waiter remains the sole owner of Process.Wait and closes processDone.
func terminatePanesAndWait(panes []*Pane, timeouts paneTerminationTimeouts) []*Pane {
	pending := livePaneProcesses(panes)
	for _, pane := range pending {
		_ = pane.Process.Process.Signal(syscall.SIGHUP)
		pane.stop()
	}
	pending = waitForPaneProcesses(pending, timeouts.hangup)
	for _, pane := range pending {
		_ = pane.Process.Process.Signal(syscall.SIGTERM)
	}
	pending = waitForPaneProcesses(pending, timeouts.terminate)
	for _, pane := range pending {
		_ = pane.Process.Process.Signal(syscall.SIGKILL)
	}
	return waitForPaneProcesses(pending, timeouts.kill)
}

func livePaneProcesses(panes []*Pane) []*Pane {
	live := make([]*Pane, 0, len(panes))
	for _, pane := range panes {
		if pane == nil || pane.Process == nil || pane.Process.Process == nil || pane.processDone == nil {
			continue
		}
		select {
		case <-pane.processDone:
		default:
			live = append(live, pane)
		}
	}
	return live
}

func waitForPaneProcesses(panes []*Pane, timeout time.Duration) []*Pane {
	pending := livePaneProcesses(panes)
	if len(pending) == 0 || timeout <= 0 {
		return pending
	}

	exited := make(chan *Pane, len(pending))
	stop := make(chan struct{})
	for _, pane := range pending {
		go func(pane *Pane) {
			select {
			case <-pane.processDone:
				exited <- pane
			case <-stop:
			}
		}(pane)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	remaining := make(map[*Pane]struct{}, len(pending))
	for _, pane := range pending {
		remaining[pane] = struct{}{}
	}
	for len(remaining) > 0 {
		select {
		case pane := <-exited:
			delete(remaining, pane)
		case <-timer.C:
			close(stop)
			out := make([]*Pane, 0, len(remaining))
			for pane := range remaining {
				out = append(out, pane)
			}
			return out
		}
	}
	close(stop)
	return nil
}

func sendEncoded[T any](ch chan<- protocol.Frame, msgType uint64, msg T, encode func([]byte, T) ([]byte, error)) error {
	payload, err := encode(nil, msg)
	if err != nil {
		return err
	}
	defer func() { recover() }()
	ch <- protocol.Frame{Type: msgType, Payload: payload}
	return nil
}

func sendEncodedDirect[T any](w io.Writer, msgType uint64, msg T, encode func([]byte, T) ([]byte, error)) error {
	// Kept separate from the asynchronous stream writer for pre-attachment
	// failures, where no session state may be touched.
	payload, err := encode(nil, msg)
	if err != nil {
		return err
	}
	return protocol.NewEncoder(w).WriteFrame(protocol.Frame{Type: msgType, Payload: payload})
}

func writeStream(stream io.Writer, frames <-chan protocol.Frame, errs chan<- error) {
	enc := protocol.NewEncoder(stream)
	for frame := range frames {
		if err := enc.WriteFrame(frame); err != nil {
			errs <- fmt.Errorf("write frame type %d: %w", frame.Type, err)
			return
		}
	}
}
