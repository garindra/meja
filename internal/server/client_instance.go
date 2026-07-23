package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

// ClientInstance is the live daemon-side representation of one running meja
// client process. It owns one QUIC transport and its protocol streams. The
// object is discarded when QUIC closes; only its reconnectCredential survives
// so a later reconnect can rebuild it.
type ClientInstance struct {
	// sessionID is the daemon assignment for this transport. The passive state
	// is resolved through Daemon.sessions for each operation; the client actor
	// never retains an authoritative SessionState pointer.
	sessionID           uint64
	credential          *reconnectCredential
	AttachmentID        uint64
	ViewLeaseWindowID   uint64
	ViewLeaseGeneration uint64
	detaching           atomic.Bool
	ended               atomic.Bool
	statusStarted       atomic.Bool
	// highestLayoutRevision is the transport-local projection revision installed
	// for this client's scanout. Window.LayoutRevision remains canonical window
	// state; switching to an older unchanged window advances this counter only.
	highestLayoutRevision atomic.Uint64
	projectionRevision    atomic.Uint64
	terminalCols          atomic.Uint32
	terminalRows          atomic.Uint32
	Daemon                *Daemon
	// clientState is live, attachment-local state. It is deliberately kept on
	// the client actor rather than in SessionState so transport/input state can
	// never become a second durable session authority.
	clientState *ClientState
	// installedBindings is the last projection actually accepted by this
	// client actor. It is the only valid source snapshot for an output
	// handoff; the daemon may already have changed the logical assignment.
	installedBindings []RenderBinding

	QUIC         quic.Connection
	Output       [protocol.MaxRenderSlots]*OutputLease
	StatusOutput io.Writer

	controlOut            chan protocol.Frame
	statusCommands        chan statusCommand
	events                chan func() error
	eventLoopStarted      atomic.Bool
	statusMessage         atomic.Value // string
	statusMessageID       atomic.Uint64
	statusMessageDuration time.Duration
	lifetimeDone          chan struct{}
	shell                 string
	frontendInput         frontendInputParser
	layouts               map[uint64]protocol.WindowLayout
	heldKeys              map[frontendHeldKey]uint64
	promptContinuation    promptContinuation
	pointerCapture        frontendPaneCapture
	pasteCapture          frontendPaneCapture
}

type ClientState struct {
	ActiveWindowID uint64
	FocusedPaneID  uint64
	FocusX2        int
	FocusY2        int
	HasFocusPoint  bool
	TerminalCols   uint16
	TerminalRows   uint16

	RenderBindings    []RenderBinding
	InputState        serverInputState
	PrefixEscape      []byte
	ResizeRepeatUntil time.Time
	Prompt            *PromptState
}

func (c *ClientInstance) ensureClientState() *ClientState {
	if c == nil {
		return nil
	}
	return c.clientState
}

func (c *ClientInstance) commitProjectionPlan(plan ClientProjectionPlan) error {
	previousProjectionRevision := c.projectionRevision.Load()
	if err := c.validateProjectionPlan(plan); err != nil {
		return err
	}
	c.installProjectedLayoutRevision(plan, previousProjectionRevision)
	if plan.AttachmentID != 0 {
		c.ViewLeaseWindowID = plan.WindowID
		c.ViewLeaseGeneration = plan.ViewLeaseGeneration
	}
	state := c.ensureClientState()
	if state != nil {
		state.ActiveWindowID = plan.WindowID
		state.FocusedPaneID = plan.FocusedPaneID
		state.TerminalCols = plan.Cols
		state.TerminalRows = plan.Rows
		state.RenderBindings = append(state.RenderBindings[:0], plan.Bindings...)
	}
	// Publish completion last. Tests and diagnostics may use this atomic as the
	// actor-boundary acknowledgement before reading the newly installed state.
	c.projectionRevision.Store(plan.ProjectionRevision)
	return nil
}

// installProjectedLayoutRevision materializes the transport-local revision
// carried by WINDOW_LAYOUT and START_RENDER. Window.LayoutRevision is only the
// canonical version of that window's inherent layout; returning to an older
// unchanged window still requires a newer client scanout revision.
func (c *ClientInstance) installProjectedLayoutRevision(plan ClientProjectionPlan, previousProjectionRevision uint64) uint64 {
	if !plan.FullSnapshot {
		return c.highestLayoutRevision.Load()
	}
	for {
		current := c.highestLayoutRevision.Load()
		next := plan.LayoutRevision
		if next < current {
			next = current
		}
		if plan.ProjectionRevision > previousProjectionRevision && next <= current {
			next = current + 1
		}
		if next == current || c.highestLayoutRevision.CompareAndSwap(current, next) {
			return next
		}
	}
}

// validateProjectionPlan is side-effect free so asynchronous deliveries can
// reject a stale plan before releasing the currently installed output leases.
func (c *ClientInstance) validateProjectionPlan(plan ClientProjectionPlan) error {
	if c == nil {
		return errors.New("nil client projection")
	}
	if plan.AttachmentID != 0 && plan.AttachmentID != c.AttachmentID {
		return errors.New("stale client projection attachment")
	}
	// Lease generations are monotonic per window, not across windows. A newly
	// created target legitimately starts at generation 1 even when the source
	// window has been leased many times. ProjectionRevision provides the
	// transport-wide stale-plan ordering across window transitions.
	if plan.WindowID == c.ViewLeaseWindowID && plan.ViewLeaseGeneration < c.ViewLeaseGeneration {
		return errors.New("stale client projection lease")
	}
	if plan.ProjectionRevision < c.projectionRevision.Load() {
		return errors.New("stale client projection revision")
	}
	if plan.SessionID != 0 && c.sessionID != 0 && plan.SessionID != c.sessionID {
		return errors.New("stale client projection session")
	}
	return nil
}

func (c *ClientInstance) focusPane(paneID uint64) (*Window, error) {
	window, plan, err := c.Daemon.focusClientPane(c, paneID)
	if err != nil {
		return nil, err
	}
	if err := c.commitProjectionPlan(plan); err != nil {
		return nil, err
	}
	return window, nil
}

func (c *ClientInstance) sessionState() *SessionState {
	if c == nil || c.Daemon == nil {
		return nil
	}
	if state, ok := c.Daemon.sessionIndex.Load(c.sessionID); ok {
		return state.(*SessionState)
	}
	return c.Daemon.sessions[c.sessionID]
}

func (c *ClientInstance) post(run func() error) {
	if c == nil || run == nil || c.events == nil {
		return
	}
	if !c.eventLoopStarted.Load() {
		c.runPostedEvent(run)
		return
	}
	select {
	case c.events <- run:
	case <-c.lifetimeDone:
	}
}

func (c *ClientInstance) runPostedEvent(event func() error) {
	if c == nil || event == nil {
		return
	}
	if err := event(); err != nil && c.Daemon != nil {
		c.Daemon.logf("meja server: client event failed attachment=%d session=%d: %v\n",
			c.AttachmentID, c.sessionID, err)
	}
}

type frontendPaneCapture struct {
	paneID uint64
	active bool
	button uint8
	// mejaSelection distinguishes a pending or active server-owned history
	// selection from mouse capture forwarded directly to an application.
	mejaSelection bool
	// selecting becomes true only after motion leaves the pressed cell and the
	// pane has actually entered history mode.
	selecting     bool
	autoSelection bool
	anchorRow     int
	anchorColumn  int
	rect          protocol.Rect
}

// reconnectCredential is the small, indefinitely retained daemon record for
// one logical client process. Its live ClientInstance exists only while a QUIC
// transport is active and is rebuilt on every reconnect.
type reconnectCredential struct {
	EncodedToken   string
	TerminalReason string
	Instance       *ClientInstance
}

func newClientInstance(d *Daemon, credential *reconnectCredential) *ClientInstance {
	instance := &ClientInstance{
		credential:     credential,
		Daemon:         d,
		lifetimeDone:   make(chan struct{}),
		statusCommands: make(chan statusCommand, 64),
		events:         make(chan func() error, 64),
		shell:          defaultShell(),
		layouts:        make(map[uint64]protocol.WindowLayout),
		heldKeys:       make(map[frontendHeldKey]uint64),
		clientState:    &ClientState{},
	}
	instance.startStatusOutput()
	return instance
}

func (c *ClientInstance) startStatusOutput() {
	if c == nil || c.statusCommands == nil || !c.statusStarted.CompareAndSwap(false, true) {
		return
	}
	go c.runStatusOutput()
}

func (c *ClientInstance) rememberLayout(layout protocol.WindowLayout) {
	if c == nil || layout.LayoutRevision == 0 {
		return
	}
	if c.layouts == nil {
		c.layouts = make(map[uint64]protocol.WindowLayout)
	}
	c.layouts[layout.LayoutRevision] = layout
	if len(c.layouts) <= 8 {
		return
	}
	oldest := layout.LayoutRevision
	for revision := range c.layouts {
		if revision < oldest {
			oldest = revision
		}
	}
	delete(c.layouts, oldest)
}

func (c *ClientInstance) resetInputForSessionSwitch() {
	c.frontendInput.reset()
	clear(c.heldKeys)
	c.pointerCapture = frontendPaneCapture{}
	c.pasteCapture = frontendPaneCapture{}
	latestRevision := c.highestLayoutRevision.Load()
	latestLayout, keepLatest := c.layouts[latestRevision]
	clear(c.layouts)
	if keepLatest {
		c.layouts[latestRevision] = latestLayout
	}
}

func (c *ClientInstance) registerFrontendTerminalExitCommand(data []byte) error {
	if c == nil || c.controlOut == nil {
		return nil
	}
	return sendEncoded(c.controlOut, protocol.MsgFrontendRegisterTerminalExitCommand, protocol.FrontendRegisterTerminalExitCommand{Data: data}, protocol.EncodeFrontendRegisterTerminalExitCommand)
}

func (c *ClientInstance) writeFrontendTerminal(data []byte) error {
	if c == nil || c.controlOut == nil || len(data) == 0 {
		return nil
	}
	return sendEncoded(c.controlOut, protocol.MsgFrontendTerminalWrite, protocol.FrontendTerminalWrite{Data: data}, protocol.EncodeFrontendTerminalWrite)
}

type clientControlEvent struct {
	frame protocol.Frame
	err   error
}

func coalesceQueuedResizeEvents(first clientControlEvent, events <-chan clientControlEvent) []clientControlEvent {
	if first.err != nil || first.frame.Type != protocol.MsgFrontendResize {
		return []clientControlEvent{first}
	}

	latest := first
	for {
		select {
		case event := <-events:
			if event.err == nil && event.frame.Type == protocol.MsgFrontendResize {
				latest = event
				continue
			}
			// A resize burst may be collapsed, but it must never reorder or
			// discard the first input (including a prefix detach command), an
			// exit acknowledgment, or a read error that follows the burst.
			return []clientControlEvent{latest, event}
		default:
			return []clientControlEvent{latest}
		}
	}
}

func readClientControl(decoder *protocol.Decoder, events chan<- clientControlEvent) {
	for {
		frame, err := decoder.ReadFrame()
		// Decoder payloads borrow its reusable read buffer. The control reader
		// can decode the next frame before the client actor consumes this one,
		// so ownership must transfer here rather than letting queued resize and
		// input frames overwrite each other.
		frame.Payload = append([]byte(nil), frame.Payload...)
		events <- clientControlEvent{frame: frame, err: err}
		if err != nil {
			return
		}
	}
}

func (c *ClientInstance) outputLeases() map[int]*OutputLease {
	leases := make(map[int]*OutputLease, len(c.Output))
	for slot, lease := range c.Output {
		if lease != nil {
			leases[slot] = lease
		}
	}
	return leases
}

func isSessionReplacedClose(err error) bool {
	var applicationErr *quic.ApplicationError
	return errors.As(err, &applicationErr) && applicationErr.ErrorCode == protocol.SessionReplacedErrorCode
}

func (c *ClientInstance) attachClientInstance(cols, rows uint16, advanceLayoutRevision bool) error {
	state := c.sessionState()
	if state == nil {
		return errors.New("client instance has no session")
	}
	if c.Daemon == nil {
		c.Daemon = state.daemon
	}
	if c.clientState == nil {
		c.clientState = &ClientState{}
	}
	var previous *ClientInstance
	c.Daemon.call(func() { previous = state.attachedClient() })
	if previous != nil && previous != c && previous.QUIC != nil {
		_ = previous.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "session attached elsewhere")
	}
	c.Daemon.call(func() {
		if c.Daemon.clients == nil {
			c.Daemon.clients = make(map[uint64]*ClientInstance)
		}
		c.Daemon.clients[state.ID] = c
		c.Daemon.clientIndex.Store(state.ID, c)
	})
	if c.StatusOutput != nil {
		if err := c.attachStatusOutput(c.StatusOutput); err != nil {
			return err
		}
	}
	if cols == 0 || rows == 0 {
		pane := c.activePane()
		if pane == nil {
			return errors.New("session has no active pane")
		}
		paneCols, paneRows := pane.TerminalSize()
		cols, rows = uint16(paneCols), uint16(paneRows)
	}
	c.terminalCols.Store(uint32(cols))
	c.terminalRows.Store(uint32(rows))
	transition, err := c.Daemon.attachSessionView(state, cols, rows, advanceLayoutRevision)
	if err != nil {
		return err
	}
	return c.applyViewTransition(transition)
}

func (c *ClientInstance) detachClientInstance() {
	state := c.sessionState()
	if c == nil || state == nil || !c.detaching.Load() {
		return
	}
	var detachErr error
	for _, pane := range state.Panes {
		if err := pane.cancelHistorySelection(); err != nil && detachErr == nil {
			detachErr = err
		}
	}
	if err := c.detachStatusOutput(); err != nil {
		detachErr = err
	}
	if err := c.detachLeases(state, c.outputLeases()); err != nil && detachErr == nil {
		detachErr = err
	}
	// The ownership move must complete even when stream cleanup fails.
	if c.Daemon != nil {
		c.Daemon.call(func() {
			if c.Daemon.clients[state.ID] == c {
				delete(c.Daemon.clients, state.ID)
				c.Daemon.clientIndex.Delete(state.ID)
			}
		})
	}
	if detachErr != nil && c.Daemon != nil {
		c.Daemon.logf("meja client detach: %v\n", detachErr)
	}
}

func (c *ClientInstance) currentOutputLease(slot int) *OutputLease {
	if c == nil || slot < 0 || slot >= len(c.Output) {
		return nil
	}
	return c.Output[slot]
}

type attachGrant struct {
	Token     []byte
	SessionID uint64
	ExpiresAt time.Time
}

type clientAttachError struct {
	reason string
}

func (e *clientAttachError) Error() string { return e.reason }

func listenQUICInRange(tlsConfig *tls.Config) (*quic.Listener, uint16, error) {
	for port := protocol.DefaultUDPMin; port <= protocol.DefaultUDPMax; port++ {
		listener, err := quic.ListenAddr(net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), tlsConfig, &quic.Config{
			MaxIdleTimeout:     quicMaxIdleTimeout,
			KeepAlivePeriod:    quicKeepAlivePeriod,
			MaxIncomingStreams: int64(protocol.MaxRenderSlots),
			InitialPacketSize:  protocol.QUICInitialPacketSize,
		})
		if err == nil {
			return listener, uint16(port), nil
		}
	}
	return nil, 0, errors.New("no UDP port available in 60000-61000")
}

func (d *Daemon) ensureQUIC() (uint16, error) {
	d.quicMu.Lock()
	defer d.quicMu.Unlock()
	if d.quicListener != nil {
		return d.quicPort, nil
	}
	listener, port, err := listenQUICInRange(d.tlsConfig)
	if err != nil {
		return 0, err
	}
	parent := d.serverCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	d.quicListener = listener
	d.quicPort = port
	d.quicCancel = cancel
	go d.runQUIC(ctx, listener)
	return port, nil
}

func (d *Daemon) closeQUIC() {
	d.quicMu.Lock()
	defer d.quicMu.Unlock()
	if d.quicCancel != nil {
		d.quicCancel()
		d.quicCancel = nil
	}
	if d.quicListener != nil {
		_ = d.quicListener.Close()
		d.quicListener = nil
	}
	d.quicPort = 0
}

func (d *Daemon) runQUIC(ctx context.Context, listener *quic.Listener) {
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, quic.ErrServerClosed) {
				return
			}
			d.logf("meja server: accept QUIC connection: %v\n", err)
			d.closeQUIC()
			return
		}
		go func() {
			if err := serveClientInstance(ctx, d, conn); err != nil && !isSessionReplacedClose(err) {
				d.logf("meja server: %v\n", err)
			}
		}()
	}
}

func (d *Daemon) issueAttachGrant(session *SessionState) (uint16, string, time.Time, error) {
	port, err := d.ensureQUIC()
	if err != nil {
		return 0, "", time.Time{}, err
	}
	token, err := protocol.NewAuthToken()
	if err != nil {
		return 0, "", time.Time{}, err
	}
	expiresAt := time.Now().Add(attachTTL)
	var issueErr error
	d.call(func() {
		if d.sessions[session.ID] != session {
			issueErr = errSessionUnavailable
			return
		}
		d.removeExpiredAttachGrants(time.Now())
		d.attachGrants = append(d.attachGrants, attachGrant{Token: token, SessionID: session.ID, ExpiresAt: expiresAt})
	})
	if issueErr != nil {
		return 0, "", time.Time{}, issueErr
	}
	return port, protocol.EncodeAuthToken(token), expiresAt, nil
}

func (d *Daemon) removeExpiredAttachGrants(now time.Time) {
	kept := d.attachGrants[:0]
	for _, grant := range d.attachGrants {
		if now.Before(grant.ExpiresAt) {
			kept = append(kept, grant)
		}
	}
	d.attachGrants = kept
}

func (d *Daemon) discardAttachGrant(encodedToken string) {
	if d == nil || encodedToken == "" {
		return
	}
	d.call(func() {
		for index := 0; index < len(d.attachGrants); index++ {
			if protocol.EqualAuthToken(encodedToken, d.attachGrants[index].Token) {
				d.attachGrants = append(d.attachGrants[:index], d.attachGrants[index+1:]...)
				return
			}
		}
	})
}

func (d *Daemon) beginClientInstance(encodedToken string) (*ClientInstance, *SessionState, error) {
	reconnectToken, err := protocol.NewAuthToken()
	if err != nil {
		return nil, nil, err
	}
	var instance *ClientInstance
	var session *SessionState
	var attachErr error
	var displaced *ClientInstance
	d.call(func() {
		now := time.Now()
		grantIndex := -1
		for i := range d.attachGrants {
			grant := &d.attachGrants[i]
			if now.Before(grant.ExpiresAt) && protocol.EqualAuthToken(encodedToken, grant.Token) {
				grantIndex = i
				break
			}
		}
		if grantIndex < 0 {
			attachErr = &clientAttachError{reason: "session attachment rejected"}
			return
		}
		sessionID := d.attachGrants[grantIndex].SessionID
		d.attachGrants = append(d.attachGrants[:grantIndex], d.attachGrants[grantIndex+1:]...)
		session = d.sessions[sessionID]
		if session == nil {
			attachErr = &clientAttachError{reason: "session attachment rejected"}
			return
		}
		encodedReconnectToken := protocol.EncodeAuthToken(reconnectToken)
		credential := &reconnectCredential{
			EncodedToken: encodedReconnectToken,
		}
		instance = newClientInstance(d, credential)
		instance.sessionID = session.ID
		credential.Instance = instance
		d.reconnectCredentials[encodedReconnectToken] = credential
		displaced = d.assignClientInstanceInActor(credential, session)
	})
	if displaced != nil && displaced.QUIC != nil {
		_ = displaced.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "session taken over by another client")
	}
	return instance, session, attachErr
}

// assignClientInstanceInActor establishes the initial daemon-side
// one-client/one-session relationship. Live moves use
// transitionClientToSession so assignment, lease transfer, and transition
// preparation remain one transaction.
func (d *Daemon) assignClientInstanceInActor(credential *reconnectCredential, session *SessionState) *ClientInstance {
	if d.clients == nil {
		d.clients = make(map[uint64]*ClientInstance)
	}
	if credential != nil && credential.Instance != nil && credential.Instance.AttachmentID == 0 {
		if d.nextAttachmentID == 0 {
			d.nextAttachmentID = 1
		}
		credential.Instance.AttachmentID = d.nextAttachmentID
		d.nextAttachmentID++
	}
	if oldSessionID := d.clientSessions[credential]; oldSessionID != 0 && oldSessionID != session.ID && d.attachments[oldSessionID] == credential {
		delete(d.attachments, oldSessionID)
		delete(d.clients, oldSessionID)
		d.clientIndex.Delete(oldSessionID)
	}
	var displaced *ClientInstance
	if previous := d.attachments[session.ID]; previous != nil && previous != credential {
		delete(d.clientSessions, previous)
		previous.TerminalReason = "session was taken over by another client"
		displaced = previous.Instance
	}
	d.clientSessions[credential] = session.ID
	d.attachments[session.ID] = credential
	if credential != nil && credential.Instance != nil {
		d.clients[session.ID] = credential.Instance
		d.clientIndex.Store(session.ID, credential.Instance)
	}
	return displaced
}

func (d *Daemon) resumeClientInstance(encodedToken string) (*ClientInstance, *SessionState, error) {
	var instance *ClientInstance
	var session *SessionState
	var previous *ClientInstance
	var resumeErr error
	d.call(func() {
		credential := d.reconnectCredentials[encodedToken]
		if credential == nil {
			resumeErr = &clientAttachError{reason: "client reconnect rejected"}
			return
		}
		if credential.TerminalReason != "" {
			resumeErr = &clientAttachError{reason: credential.TerminalReason}
			return
		}
		sessionID := d.clientSessions[credential]
		if sessionID == 0 || d.attachments[sessionID] != credential {
			resumeErr = &clientAttachError{reason: "client instance is not assigned to a session"}
			return
		}
		session = d.sessions[sessionID]
		if session == nil {
			resumeErr = &clientAttachError{reason: "session is no longer available"}
			return
		}
		previous = credential.Instance
		instance = newClientInstance(d, credential)
		instance.sessionID = session.ID
		credential.Instance = instance
		// The replacement is not authoritative until its transport attaches.
		// Stop daemon deliveries from targeting the disconnected instance in
		// the gap between resume authentication and attachClientInstance.
		if d.clients[session.ID] == previous {
			delete(d.clients, session.ID)
			d.clientIndex.Delete(session.ID)
		}
	})
	if previous != nil {
		previous.detaching.Store(true)
		if previous.QUIC != nil {
			_ = previous.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "client reconnected elsewhere")
		}
	}
	return instance, session, resumeErr
}

func (d *Daemon) attachClientInstance(instance *ClientInstance, conn quic.Connection, status io.Writer, output map[int]*OutputLease, controlOut chan protocol.Frame, cols, rows uint16) error {
	var session *SessionState
	var activateErr error
	d.call(func() {
		credential := instance.credential
		if instance.AttachmentID == 0 {
			if d.nextAttachmentID == 0 {
				d.nextAttachmentID = 1
			}
			instance.AttachmentID = d.nextAttachmentID
			d.nextAttachmentID++
		}
		sessionID := d.clientSessions[credential]
		if credential == nil || credential.Instance != instance || credential.TerminalReason != "" || sessionID == 0 || d.attachments[sessionID] != credential {
			reason := ""
			if credential != nil {
				reason = credential.TerminalReason
			}
			if reason == "" {
				reason = "client instance is not assigned to a session"
			}
			activateErr = &clientAttachError{reason: reason}
			return
		}
		session = d.sessions[sessionID]
		if session == nil {
			activateErr = &clientAttachError{reason: "session is no longer available"}
			return
		}
		instance.QUIC = conn
		// A reconnect replaces the transport-local ClientInstance. The
		// credential was updated by resumeClientInstance, but daemon mutations
		// authorize through this registry, so both references must move in the
		// same actor transaction before the client can install its first view.
		d.clients[sessionID] = instance
		d.clientIndex.Store(sessionID, instance)
		instance.StatusOutput = status
		instance.controlOut = controlOut
		for slot := range instance.Output {
			instance.Output[slot] = output[slot]
		}
	})
	if activateErr != nil {
		return activateErr
	}
	instance.sessionID = session.ID
	if err := instance.attachClientInstance(cols, rows, false); err != nil {
		d.detachClientInstance(instance)
		return err
	}
	return nil
}

func (d *Daemon) detachClientInstance(instance *ClientInstance) {
	deactivate := false
	d.call(func() {
		credential := instance.credential
		if credential != nil && credential.Instance == instance {
			instance.detaching.Store(true)
			credential.Instance = nil
			deactivate = true
		} else if instance.detaching.Load() {
			deactivate = true
		}
	})
	if deactivate {
		if instance.ViewLeaseWindowID != 0 {
			_ = d.releaseWindowView(instance.AttachmentID, instance.ViewLeaseWindowID, instance.ViewLeaseGeneration)
			instance.ViewLeaseWindowID = 0
			instance.ViewLeaseGeneration = 0
		}
		instance.detachClientInstance()
	}
}

// transitionClientToSession atomically moves one live client process to a
// target session while retaining its transport, streams, output leases, and
// reconnect token. It commits daemon ownership and returns the exact view for
// the ClientInstance actor to apply; it never installs that view itself.
func (d *Daemon) transitionClientToSession(instance *ClientInstance, targetSessionID uint64, cols, rows uint16) (PreparedViewTransition, error) {
	var source *SessionState
	var target *SessionState
	var displaced *ClientInstance
	var transition PreparedViewTransition
	var switchErr error
	if instance == nil {
		return transition, errors.New("nil client instance")
	}
	if targetSessionID == 0 {
		return transition, errors.New("target session is unavailable")
	}
	// The terminal dimensions are client-owned input to the daemon plan. They
	// are not part of the assignment transaction and may be refreshed before
	// the next projection is installed.
	instance.terminalCols.Store(uint32(cols))
	instance.terminalRows.Store(uint32(rows))
	d.call(func() {
		if instance.credential != nil {
			source = d.sessions[d.clientSessions[instance.credential]]
		}
		if source == nil {
			switchErr = errors.New("client instance is not attached to a session")
			return
		}
		target = d.sessions[targetSessionID]
		if target == nil {
			switchErr = fmt.Errorf("unknown session %d", targetSessionID)
			return
		}
		if source == target {
			if _, switchErr = prepareClientWindowGeometryNow(instance, target, target.ActiveWindowID); switchErr != nil {
				return
			}
			transition = d.prepareViewTransitionNow(viewTransitionSession, instance, target, true)
			return
		}
		credential := instance.credential
		if credential == nil || credential.Instance != instance || credential.TerminalReason != "" ||
			d.attachments[source.ID] != credential || d.sessions[target.ID] != target {
			switchErr = errors.New("client instance can no longer switch sessions")
			return
		}
		sourceWindowID := source.ActiveWindowID
		if sourceWindowID == 0 {
			if state := instance.clientState; state != nil {
				sourceWindowID = state.ActiveWindowID
			}
		}
		targetWindowID := target.ActiveWindowID
		if targetWindowID == 0 {
			ids := target.orderedWindowIDs()
			if len(ids) > 0 {
				targetWindowID = ids[0]
			}
		}
		if sourceWindowID == 0 || targetWindowID == 0 {
			switchErr = errors.New("client switch requires source and target windows")
			return
		}
		sourceLease := d.windowLeases[sourceWindowID]
		if sourceLease == nil || sourceLease.AttachmentID != instance.AttachmentID {
			switchErr = errors.New("stale source window lease")
			return
		}
		targetWindow := target.Windows[targetWindowID]
		if targetWindow == nil {
			switchErr = fmt.Errorf("unknown window %d", targetWindowID)
			return
		}
		targetLease := d.windowLeases[targetWindowID]
		var displacedCredential *reconnectCredential
		if targetLease != nil && targetLease.AttachmentID != instance.AttachmentID {
			assigned := d.attachments[target.ID]
			if assigned == nil || assigned == credential || d.clients[target.ID] == nil {
				switchErr = fmt.Errorf("window %d is currently viewed by another client", targetWindow.DisplayIndex)
				return
			}
			displacedCredential = assigned
		}
		if _, switchErr = prepareClientWindowGeometryNow(instance, target, targetWindowID); switchErr != nil {
			return
		}
		if displacedCredential != nil {
			displaced = displacedCredential.Instance
			displacedCredential.TerminalReason = "session taken over by another client"
			delete(d.clientSessions, displacedCredential)
		}
		// Acquire the target lease before releasing the source lease. The
		// assignment, lease transfer, and immutable projection are one daemon
		// transaction, so a rejected switch leaves every source field intact.
		generation := uint64(1)
		if targetLease != nil {
			generation = targetLease.Generation
			if targetLease.AttachmentID != instance.AttachmentID {
				generation++
			}
		}
		d.windowLeases[targetWindowID] = &WindowViewLease{WindowID: targetWindowID, SessionID: target.ID, AttachmentID: instance.AttachmentID, Generation: generation}
		if sourceWindowID != targetWindowID {
			delete(d.windowLeases, sourceWindowID)
		}
		if target.ActiveWindowID == 0 {
			target.ActiveWindowID = targetWindowID
		}
		delete(d.attachments, source.ID)
		delete(d.clients, source.ID)
		d.clientIndex.Delete(source.ID)
		d.clientSessions[credential] = target.ID
		d.attachments[target.ID] = credential
		d.clients[target.ID] = instance
		d.clientIndex.Store(target.ID, instance)
		transition = d.prepareViewTransitionNow(viewTransitionSession, instance, target, true)
	})
	if switchErr != nil {
		return transition, switchErr
	}
	if displaced != nil && displaced != instance && displaced.QUIC != nil {
		_ = displaced.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "session taken over by another client")
	}
	return transition, nil
}

func (d *Daemon) discardUnattachedClientInstance(instance *ClientInstance) {
	d.call(func() {
		if credential := instance.credential; credential != nil && credential.Instance == instance {
			credential.Instance = nil
		}
	})
}

// OutputLease is one enduring pane-output slot for the lifetime of a live
// client-instance transport. Its Stream is the physical QUIC stream in
// production. Exactly one pane actor or the session's unused pool holds a
// lease at a time.
type OutputLease struct {
	Slot   int
	Stream io.Writer

	workerOnce sync.Once
	available  chan *paneRenderBuffer
	ready      chan paneRenderBatch
	failed     chan error
	done       <-chan struct{}
	onFailure  func(error)
}

const (
	quicMaxIdleTimeout  = 6 * time.Second
	quicKeepAlivePeriod = 2 * time.Second
	// The frontend stays in one rich, attachment-scoped capture mode. Terminals
	// that do not implement Kitty keyboard enhancements safely ignore CSI > u.
	frontendTerminalSetup = "\x1b[>3u\x1b[?1003;1006;1004;2004h"
	// Pop exactly the keyboard-mode entry installed by setup. This is supported
	// by both the Kitty protocol and older iTerm2 implementations, which do not
	// implement the newer CSI = flags ; mode u replacement form.
	frontendTerminalExitCommand = "\x1b[?1003;1006;1004;2004l\x1b[<u"
)

func serveClientInstance(ctx context.Context, d *Daemon, conn quic.Connection) error {
	defer conn.CloseWithError(0, "")

	var err error
	controlStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("accept control stream: %w", err)
	}
	controlDecoder := protocol.NewDecoder(controlStream, protocol.DefaultMaxFrameSize)

	first, err := controlDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read session attachment: %w", err)
	}
	var session *SessionState
	var clientInstance *ClientInstance
	var attachCols, attachRows uint16
	responseType := protocol.MsgSessionAttachOK
	switch first.Type {
	case protocol.MsgSessionAttach:
		attach, decodeErr := protocol.DecodeSessionAttach(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		if attach.Version != protocol.ProtocolVersion {
			return errors.New("unsupported session protocol version")
		}
		clientInstance, session, err = d.beginClientInstance(attach.Token)
		attachCols, attachRows = attach.Cols, attach.Rows
	case protocol.MsgSessionResume:
		resume, decodeErr := protocol.DecodeSessionResume(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		if resume.Version != protocol.ProtocolVersion {
			return errors.New("unsupported session protocol version")
		}
		clientInstance, session, err = d.resumeClientInstance(resume.ResumeToken)
		attachCols, attachRows = resume.Cols, resume.Rows
		responseType = protocol.MsgSessionResumeOK
	default:
		return fmt.Errorf("expected session attachment, got message type %d", first.Type)
	}
	if err != nil {
		reason := "session attachment rejected"
		var attachErr *clientAttachError
		if errors.As(err, &attachErr) {
			reason = attachErr.reason
		}
		_ = sendEncodedDirect(controlStream, protocol.MsgSessionAttachFailed, protocol.SessionAttachFailed{Reason: reason}, protocol.EncodeSessionAttachFailed)
		return err
	}
	defer close(clientInstance.lifetimeDone)
	attached := false
	defer func() {
		if !attached {
			d.discardUnattachedClientInstance(clientInstance)
		}
	}()
	controlFrames := make(chan protocol.Frame, 256)
	clientInstance.controlOut = controlFrames
	writerErrs := make(chan error, 4)
	go writeStream(controlStream, controlFrames, writerErrs)
	defer close(controlFrames)
	if responseType == protocol.MsgSessionResumeOK {
		if err := sendEncoded(controlFrames, protocol.MsgSessionResumeOK, protocol.SessionResumeOK{
			Version: protocol.ProtocolVersion,
		}, protocol.EncodeSessionResumeOK); err != nil {
			return err
		}
	} else if err := sendEncoded(controlFrames, protocol.MsgSessionAttachOK, protocol.SessionAttachOK{
		Version: protocol.ProtocolVersion, ResumeToken: clientInstance.credential.EncodedToken,
	}, protocol.EncodeSessionAttachOK); err != nil {
		return err
	}
	if err := clientInstance.registerFrontendTerminalExitCommand([]byte(frontendTerminalExitCommand)); err != nil {
		return err
	}
	if err := clientInstance.writeFrontendTerminal([]byte(frontendTerminalSetup)); err != nil {
		return err
	}
	d.logSessionAttached(session.ID)
	statusOutput, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open status output stream: %w", err)
	}
	if index, ok := protocol.OutputIndexFromStreamID(uint64(statusOutput.StreamID())); !ok || index != 0 {
		return fmt.Errorf("status output stream ID %d has index %d", statusOutput.StreamID(), index)
	}
	if _, err := statusOutput.Write([]byte{byte(protocol.DisplayOpcodeNoop)}); err != nil {
		return fmt.Errorf("materialize status output stream: %w", err)
	}
	outputLeases := make(map[int]*OutputLease, int(protocol.MaxRenderSlots))
	for slot := 0; slot < int(protocol.MaxRenderSlots); slot++ {
		outputStream, err := conn.OpenUniStreamSync(ctx)
		if err != nil {
			return fmt.Errorf("open output stream %d: %w", slot, err)
		}
		if index, ok := protocol.OutputIndexFromStreamID(uint64(outputStream.StreamID())); !ok || int(index) != slot+1 {
			return fmt.Errorf("pane output stream ID %d has index %d, want %d", outputStream.StreamID(), index, slot+1)
		}
		if _, err := outputStream.Write([]byte{byte(protocol.DisplayOpcodeNoop)}); err != nil {
			return fmt.Errorf("materialize pane output stream %d: %w", slot, err)
		}
		leaseSlot := slot
		outputLeases[slot] = &OutputLease{
			Slot:   slot,
			Stream: outputStream,
			done:   conn.Context().Done(),
			onFailure: func(writeErr error) {
				_ = conn.CloseWithError(protocol.RenderOutputErrorCode, fmt.Sprintf("pane output slot %d failed: %v", leaseSlot, writeErr))
			},
		}
	}

	err = d.attachClientInstance(clientInstance, conn, statusOutput, outputLeases, controlFrames, attachCols, attachRows)
	if err != nil {
		_ = conn.CloseWithError(protocol.SessionReplacedErrorCode, err.Error())
		return err
	}
	attached = true
	defer func() {
		_ = conn.CloseWithError(0, "")
		d.detachClientInstance(clientInstance)
	}()
	// Let the reader get ahead far enough to expose a resize burst to the
	// client actor. The actor collapses consecutive resize frames to the most
	// recent dimensions before doing the expensive projection transaction.
	controlEvents := make(chan clientControlEvent, 256)
	go readClientControl(controlDecoder, controlEvents)
	clientInstance.eventLoopStarted.Store(true)
	exitRequested := false
	handleControlEvent := func(event clientControlEvent) (bool, error) {
		if event.err != nil {
			if errors.Is(event.err, io.EOF) {
				return true, nil
			}
			return false, fmt.Errorf("read control frame: %w", event.err)
		}
		if exitRequested {
			if event.frame.Type == protocol.MsgFrontendTerminalExitComplete {
				if len(event.frame.Payload) != 0 {
					return false, errors.New("frontend terminal exit completion has a payload")
				}
				return true, nil
			}
			// Input and resize frames already queued before the client applied
			// the exit command can arrive before its acknowledgment. Ignore
			// them while retaining the acknowledgment as the close barrier.
			return false, nil
		}
		stopped, err := clientInstance.handleControlFrame(event.frame)
		if stopped {
			if err := sendEncoded(clientInstance.controlOut, protocol.MsgFrontendExecuteTerminalExitCommand, struct{}{}, func(dst []byte, _ struct{}) ([]byte, error) {
				return dst, nil
			}); err != nil {
				return false, err
			}
			exitRequested = true
			return false, nil
		}
		return false, err
	}
	for {
		select {
		case err := <-writerErrs:
			return err
		case event := <-controlEvents:
			for _, queued := range coalesceQueuedResizeEvents(event, controlEvents) {
				done, err := handleControlEvent(queued)
				if err != nil {
					return err
				}
				if done {
					return nil
				}
			}
		case event := <-clientInstance.events:
			clientInstance.runPostedEvent(event)
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return context.Cause(conn.Context())
		}
	}
}
