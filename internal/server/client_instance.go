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
// object is discarded when QUIC closes; only its ClientIdentity survives
// so a later reconnect can rebuild it.
type ClientInstance struct {
	identity   *ClientIdentity
	connection *clientConnection

	sessionID    uint64
	detaching    atomic.Bool
	terminalCols atomic.Uint32
	terminalRows atomic.Uint32
	shell        string
	commands     chan clientInstanceCommand

	ViewLeaseWindowID   uint64
	ViewLeaseGeneration uint64
	ended               atomic.Bool
	statusStarted       atomic.Bool
	// appliedProjectionRevision orders every daemon decision applied by this
	// transport, including focus-only updates that reuse currentView.Layout's
	// client-view revision.
	appliedProjectionRevision atomic.Uint64
	Daemon                    *Daemon
	// inputState is the client actor's prefix, prompt, and directional-focus
	// parser state. It contains no session, viewport, or installed-view data.
	inputState clientInputState
	// currentView is the last daemon-resolved view this client actor
	// successfully published. It is also the exact source for output handoff.
	currentView ClientView

	QUIC         quic.Connection
	Output       [protocol.MaxRenderSlots]*OutputLease
	StatusOutput io.Writer

	controlOut            chan protocol.Frame
	statusCommands        chan statusCommand
	eventLoopStarted      atomic.Bool
	statusMessage         atomic.Value // string
	statusMessageID       atomic.Uint64
	statusMessageDuration time.Duration
	lifetimeDone          chan struct{}
	frontendInput         frontendInputParser
	heldKeys              map[frontendHeldKey]uint64
	promptContinuation    promptContinuation
	pointerCapture        frontendPaneCapture
	pasteCapture          frontendPaneCapture
}

type clientInstanceCommand struct {
	Transition         *ViewTransition
	RefreshStatus      bool
	ClearStatusMessage uint64
	FocusDirection     byte
	EnterHistory       bool
	RunSendKeys        bool
	SendKeys           []string
	RunPasteBuffer     bool
	PasteBuffer        []string
	Close              bool
	CloseCode          quic.ApplicationErrorCode
	CloseReason        string
	Done               chan<- error
}

type clientInputState struct {
	FocusX2       int
	FocusY2       int
	HasFocusPoint bool

	InputState        serverInputState
	PrefixEscape      []byte
	ResizeRepeatUntil time.Time
	Prompt            *PromptState
}

func (c *ClientInstance) commitProjectionPlan(plan ClientProjectionPlan) error {
	if err := c.validateProjectionPlan(plan); err != nil {
		return err
	}
	if !plan.FullSnapshot && plan.View.Layout.LayoutRevision != c.currentView.Layout.LayoutRevision {
		return errors.New("focus projection changed client layout revision")
	}
	if plan.ClientID != 0 {
		c.ViewLeaseWindowID = plan.View.Layout.WindowID
		c.ViewLeaseGeneration = plan.ViewLeaseGeneration
	}
	return nil
}

// validateProjectionPlan is side-effect free so asynchronous deliveries can
// reject a stale plan before releasing the currently installed output leases.
func (c *ClientInstance) validateProjectionPlan(plan ClientProjectionPlan) error {
	if c == nil {
		return errors.New("nil client projection")
	}
	if plan.ClientID != 0 && plan.ClientID != c.identity.ID {
		return errors.New("stale client projection")
	}
	// Lease generations are monotonic per window, not across windows. A newly
	// created target legitimately starts at generation 1 even when the source
	// window has been leased many times. ProjectionRevision provides the
	// transport-wide stale-plan ordering across window transitions.
	if plan.View.Layout.WindowID == c.ViewLeaseWindowID && plan.ViewLeaseGeneration < c.ViewLeaseGeneration {
		return errors.New("stale client projection lease")
	}
	if plan.ProjectionRevision == 0 {
		return errors.New("client projection has no ordering revision")
	}
	if plan.ProjectionRevision <= c.appliedProjectionRevision.Load() {
		return errors.New("stale client projection revision")
	}
	if plan.SessionID != 0 && c.sessionID != 0 && plan.SessionID != c.sessionID {
		return errors.New("stale client projection session")
	}
	return nil
}

func (c *ClientInstance) focusPane(paneID uint64) (*Window, error) {
	window, transition, err := c.Daemon.focusClientPane(c.identity, paneID)
	if err != nil {
		return nil, err
	}
	if transition.Projection.FullSnapshot {
		return window, c.applyViewTransition(transition)
	}
	return window, c.applyFocusTransition(transition)
}

func (c *ClientInstance) sessionState() *SessionState {
	if c == nil || c.Daemon == nil {
		return nil
	}
	if state, ok := c.Daemon.sessionIndex.Load(c.sessionID); ok {
		return state.(*SessionState)
	}
	return nil
}

func (c *ClientInstance) activePane() *Pane {
	if c == nil || c.Daemon == nil {
		return nil
	}
	value, ok := c.Daemon.paneIndex.Load(c.currentView.Layout.FocusedPaneID)
	if !ok || value == nil {
		return nil
	}
	return value.(*Pane)
}

func (c *ClientInstance) activeWindow() *Window {
	if c == nil {
		return nil
	}
	state := c.sessionState()
	if state == nil {
		return nil
	}
	return cloneWindow(state.Windows[c.currentView.Layout.WindowID])
}

func (c *ClientInstance) currentPanePlacements() []protocol.PanePlacement {
	if c == nil {
		return nil
	}
	return append([]protocol.PanePlacement(nil), c.currentView.Layout.Panes...)
}

func (c *ClientInstance) isFocusedPane(paneID uint64) bool {
	return c != nil && c.currentView.Layout.FocusedPaneID == paneID
}

func postClientCommand(connection *clientConnection, command clientInstanceCommand) {
	if connection == nil || connection.commands == nil {
		return
	}
	connection.commands <- command
}

func (d *Daemon) clientConnectionIsActive(identity *ClientIdentity, connection *clientConnection, sessionID uint64) bool {
	if d == nil || identity == nil || connection == nil {
		return false
	}
	active := false
	d.call(func() {
		session := d.sessions[sessionID]
		active = session != nil && session.ClientID == identity.ID &&
			d.clients[identity.ID] == identity && identity.State.Active == connection
	})
	return active
}

func (d *Daemon) clientConnectionIsCurrent(identity *ClientIdentity, connection *clientConnection) bool {
	if d == nil || identity == nil || connection == nil {
		return false
	}
	current := false
	d.call(func() {
		current = d.clients[identity.ID] == identity && identity.State.Active == connection
	})
	return current
}

func (c *ClientInstance) postCommand(command clientInstanceCommand) {
	if c == nil || c.connection == nil {
		return
	}
	if !c.eventLoopStarted.Load() {
		c.runClientCommand(command)
		return
	}
	select {
	case c.commands <- command:
	case <-c.lifetimeDone:
	}
}

func (c *ClientInstance) runClientCommand(command clientInstanceCommand) {
	if c == nil {
		return
	}
	var err error
	switch {
	case command.Transition != nil:
		err = c.applyViewTransition(*command.Transition)
	case command.ClearStatusMessage != 0:
		if c.sessionState() != nil && c.statusMessageID.Load() == command.ClearStatusMessage {
			c.statusMessage.Store("")
			err = c.publishStatusBar()
		}
	case command.RefreshStatus:
		err = c.publishStatusBar()
	case command.FocusDirection != 0:
		_, _, err = c.FocusPaneDirection(command.FocusDirection)
	case command.EnterHistory:
		err = c.commandEnterHistory()
	case command.RunSendKeys:
		err = sendKeysToClient(c, command.SendKeys)
	case command.RunPasteBuffer:
		err = pasteBufferToClient(c, command.PasteBuffer)
	case command.Close:
		if c.QUIC != nil {
			err = c.QUIC.CloseWithError(command.CloseCode, command.CloseReason)
		}
	}
	if command.Done != nil {
		command.Done <- err
	}
	if err != nil && command.Done == nil && c.Daemon != nil {
		c.Daemon.logf("meja server: client event failed client=%d session=%d: %v\n",
			c.identity.ID, c.sessionID, err)
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

type ClientID uint64

type clientPhase uint8

const (
	clientDetached clientPhase = iota
	clientPending
	clientActive
	clientReplacing
	clientClosing
)

// clientConnection is a passive address for one ClientInstance goroutine.
// Channel identity distinguishes overlapping old and replacement connections.
type clientConnection struct {
	commands chan clientInstanceCommand
	done     <-chan struct{}
}

type clientLifecycle struct {
	Phase       clientPhase
	Active      *clientConnection
	Pending     *clientConnection
	PendingCols uint16
	PendingRows uint16
	WaitFor     <-chan struct{}
}

// ClientIdentity is the daemon-owned canonical record for one resumable
// logical client.
type ClientIdentity struct {
	ID          ClientID
	ResumeToken string
	SessionID   uint64
	State       clientLifecycle

	TerminalReason     string
	terminalCols       atomic.Uint32
	terminalRows       atomic.Uint32
	projectionRevision uint64
	shell              string

	// lastAllocatedClientLayoutRevision is the daemon's monotonic allocator
	// state for client-view/cache epochs across disposable transports.
	lastAllocatedClientLayoutRevision protocol.ClientLayoutRevision
}

func newClientInstance(d *Daemon, identity *ClientIdentity, connections ...*clientConnection) *ClientInstance {
	var connection *clientConnection
	if len(connections) > 0 {
		connection = connections[0]
	}
	if connection == nil {
		connection = &clientConnection{commands: make(chan clientInstanceCommand, 64)}
	}
	if connection.commands == nil {
		connection.commands = make(chan clientInstanceCommand, 64)
	}
	instance := &ClientInstance{
		identity:       identity,
		connection:     connection,
		Daemon:         d,
		shell:          defaultShell(),
		commands:       connection.commands,
		lifetimeDone:   make(chan struct{}),
		statusCommands: make(chan statusCommand, 64),
		heldKeys:       make(map[frontendHeldKey]uint64),
	}
	if identity != nil {
		instance.sessionID = identity.SessionID
		if identity.shell != "" {
			instance.shell = identity.shell
		}
		instance.terminalCols.Store(identity.terminalCols.Load())
		instance.terminalRows.Store(identity.terminalRows.Load())
	}
	connection.done = instance.lifetimeDone
	instance.startStatusOutput()
	return instance
}

func (c *ClientInstance) adoptIdentityTerminalSize() {
	if c == nil || c.identity == nil {
		return
	}
	c.terminalCols.Store(c.identity.terminalCols.Load())
	c.terminalRows.Store(c.identity.terminalRows.Load())
}

func sendClientCommand(connection *clientConnection, command clientInstanceCommand) error {
	if connection == nil || connection.commands == nil {
		return errors.New("client connection is unavailable")
	}
	if connection.done == nil {
		connection.commands <- command
		return nil
	}
	result := make(chan error, 1)
	command.Done = result
	select {
	case connection.commands <- command:
	case <-connection.done:
		return errors.New("target client disconnected")
	}
	select {
	case err := <-result:
		return err
	case <-connection.done:
		return errors.New("target client disconnected")
	}
}

func (c *ClientInstance) startStatusOutput() {
	if c == nil || c.statusCommands == nil || !c.statusStarted.CompareAndSwap(false, true) {
		return
	}
	go c.runStatusOutput()
}

func (c *ClientInstance) inputLayoutForRevision(revision protocol.ClientLayoutRevision) (protocol.ClientLayout, bool) {
	if c == nil || revision == 0 || c.currentView.Layout.LayoutRevision != revision {
		return protocol.ClientLayout{}, false
	}
	return c.currentView.Layout, true
}

func (c *ClientInstance) resetInputForSessionSwitch() {
	c.frontendInput.reset()
	clear(c.heldKeys)
	c.pointerCapture = frontendPaneCapture{}
	c.pasteCapture = frontendPaneCapture{}
	c.currentView.Layout = protocol.ClientLayout{}
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

// releaseFrontendResources tears down transport-local status and pane output
// resources after the daemon has fenced and unregistered this instance.
func (c *ClientInstance) releaseFrontendResources() {
	if c == nil || !c.detaching.Load() {
		return
	}
	var detachErr error
	panes := make([]*Pane, 0, len(c.currentView.Panes))
	for _, resolved := range c.currentView.Panes {
		if resolved.Pane == nil {
			continue
		}
		panes = append(panes, resolved.Pane)
		if err := resolved.Pane.cancelHistorySelection(); err != nil && detachErr == nil {
			detachErr = err
		}
	}
	if err := c.detachStatusOutput(); err != nil {
		detachErr = err
	}
	if err := c.detachLeases(panes, c.outputLeases()); err != nil && detachErr == nil {
		detachErr = err
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

type clientHandshakeError struct {
	reason string
}

func (e *clientHandshakeError) Error() string { return e.reason }

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

type connectionAdmissionKind uint8

const (
	admitSessionAttach connectionAdmissionKind = iota + 1
	admitClientResume
)

type AdmitConnectionRequest struct {
	Kind  connectionAdmissionKind
	Token string
}

type ClientAdmission struct {
	ClientID    ClientID
	SessionID   uint64
	ResumeToken string

	identity   *ClientIdentity
	connection *clientConnection
}

type ClientInitialized struct {
	ClientID ClientID
	Cols     uint16
	Rows     uint16

	connection *clientConnection
}

func (d *Daemon) admitConnection(request AdmitConnectionRequest) (ClientAdmission, error) {
	switch request.Kind {
	case admitSessionAttach:
		return d.admitSessionConnection(request.Token)
	case admitClientResume:
		return d.admitResumedConnection(request.Token)
	default:
		return ClientAdmission{}, &clientHandshakeError{reason: "unknown client admission kind"}
	}
}

func (d *Daemon) admitSessionConnection(encodedToken string) (ClientAdmission, error) {
	reconnectToken, err := protocol.NewAuthToken()
	if err != nil {
		return ClientAdmission{}, err
	}
	var admission ClientAdmission
	var session *SessionState
	var attachErr error
	var displaced *clientConnection
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
			attachErr = &clientHandshakeError{reason: "session attachment rejected"}
			return
		}
		sessionID := d.attachGrants[grantIndex].SessionID
		d.attachGrants = append(d.attachGrants[:grantIndex], d.attachGrants[grantIndex+1:]...)
		session = d.sessions[sessionID]
		if session == nil {
			attachErr = &clientHandshakeError{reason: "session attachment rejected"}
			return
		}
		encodedReconnectToken := protocol.EncodeAuthToken(reconnectToken)
		if d.nextClientID == 0 {
			d.nextClientID = 1
		}
		connection := &clientConnection{commands: make(chan clientInstanceCommand, 64)}
		identity := &ClientIdentity{
			ID: d.nextClientID, ResumeToken: encodedReconnectToken, SessionID: session.ID,
			State: clientLifecycle{Phase: clientPending, Pending: connection},
			shell: defaultShell(),
		}
		d.nextClientID++
		admission = ClientAdmission{
			ClientID: identity.ID, SessionID: session.ID,
			ResumeToken: identity.ResumeToken, identity: identity, connection: connection,
		}
		d.clients[identity.ID] = identity
		d.clientTokens[encodedReconnectToken] = identity.ID
		if previous := d.clients[session.ClientID]; previous != nil && previous != identity {
			previous.TerminalReason = "session was taken over by another client"
			if previous.State.Active != nil {
				displaced = previous.State.Active
				identity.State.WaitFor = displaced.done
				previous.State.Phase = clientClosing
			}
		}
	})
	if displaced != nil {
		_ = sendClientCommand(displaced, clientInstanceCommand{
			Close: true, CloseCode: protocol.SessionReplacedErrorCode,
			CloseReason: "session taken over by another client",
		})
	}
	return admission, attachErr
}

func (d *Daemon) admitResumedConnection(encodedToken string) (ClientAdmission, error) {
	var admission ClientAdmission
	var session *SessionState
	var previous *clientConnection
	var resumeErr error
	d.call(func() {
		identity := d.clients[d.clientTokens[encodedToken]]
		if identity == nil {
			resumeErr = &clientHandshakeError{reason: "client reconnect rejected"}
			return
		}
		if identity.TerminalReason != "" {
			resumeErr = &clientHandshakeError{reason: identity.TerminalReason}
			return
		}
		sessionID := identity.SessionID
		if sessionID == 0 {
			resumeErr = &clientHandshakeError{reason: "client instance is not assigned to a session"}
			return
		}
		session = d.sessions[sessionID]
		if session == nil {
			resumeErr = &clientHandshakeError{reason: "session is no longer available"}
			return
		}
		connection := &clientConnection{commands: make(chan clientInstanceCommand, 64)}
		previous = identity.State.Active
		identity.State = clientLifecycle{
			Phase: clientPending, Pending: connection,
		}
		if previous != nil {
			identity.State.Phase = clientReplacing
			identity.State.Active = previous
		}
		admission = ClientAdmission{
			ClientID: identity.ID, SessionID: session.ID, ResumeToken: identity.ResumeToken,
			identity: identity, connection: connection,
		}
	})
	if previous != nil {
		_ = sendClientCommand(previous, clientInstanceCommand{
			Close: true, CloseCode: protocol.SessionReplacedErrorCode,
			CloseReason: "client reconnected elsewhere",
		})
	}
	return admission, resumeErr
}

func (d *Daemon) initializeClient(request ClientInitialized) (ViewTransition, error) {
	var transition ViewTransition
	var session *SessionState
	var previousDone <-chan struct{}
	var activateErr error
	d.call(func() {
		identity := d.clients[request.ClientID]
		if identity == nil || identity.State.Pending == nil ||
			(request.connection != nil && identity.State.Pending != request.connection) {
			activateErr = &clientHandshakeError{reason: "client admission is no longer active"}
			return
		}
		if identity.TerminalReason != "" {
			activateErr = &clientHandshakeError{reason: identity.TerminalReason}
			return
		}
		session = d.sessions[identity.SessionID]
		if session == nil {
			activateErr = &clientHandshakeError{reason: "session is no longer available"}
			return
		}
		if request.Cols == 0 || request.Rows == 0 {
			if window := session.Windows[session.ActiveWindowID]; window != nil {
				request.Cols, request.Rows = window.Cols, window.Rows
			}
		}
		identity.State.PendingCols, identity.State.PendingRows = request.Cols, request.Rows
		if identity.State.Active != nil {
			previousDone = identity.State.Active.done
		} else {
			previousDone = identity.State.WaitFor
		}
	})
	if activateErr != nil {
		return transition, activateErr
	}
	if previousDone != nil {
		<-previousDone
	}
	d.call(func() {
		identity := d.clients[request.ClientID]
		if identity == nil || identity.State.Pending == nil ||
			(request.connection != nil && identity.State.Pending != request.connection) {
			activateErr = &clientHandshakeError{reason: "client admission is no longer active"}
			return
		}
		if identity.TerminalReason != "" || d.sessions[identity.SessionID] != session {
			reason := identity.TerminalReason
			if reason == "" {
				reason = "session is no longer available"
			}
			activateErr = &clientHandshakeError{reason: reason}
			return
		}
		pending := identity.State.Pending
		cols, rows := identity.State.PendingCols, identity.State.PendingRows
		if current := d.clients[session.ClientID]; current != nil && current != identity &&
			current.State.Phase != clientDetached {
			activateErr = &clientHandshakeError{reason: "session is still attached to another client"}
			return
		}
		identity.State = clientLifecycle{Phase: clientActive, Active: pending}
		identity.terminalCols.Store(uint32(cols))
		identity.terminalRows.Store(uint32(rows))
		session.ClientID = identity.ID
		transition, activateErr = d.prepareAttachedClientViewNow(identity, session, cols, rows)
	})
	return transition, activateErr
}

func (d *Daemon) detachClientInstance(instance *ClientInstance) {
	deactivate := false
	d.call(func() {
		identity := instance.identity
		if identity == nil {
			return
		}
		switch {
		case identity.State.Active == instance.connection:
			instance.detaching.Store(true)
			identity.State.Active = nil
			if identity.State.Pending != nil {
				identity.State.Phase = clientPending
			} else {
				identity.State.Phase = clientDetached
			}
			deactivate = true
		case identity.State.Pending == instance.connection:
			identity.State.Pending = nil
			if identity.State.Active != nil {
				identity.State.Phase = clientActive
			} else {
				identity.State.Phase = clientDetached
			}
			deactivate = true
		case instance.detaching.Load():
			deactivate = true
		}
		if !deactivate {
			return
		}
		if session := d.sessions[instance.sessionID]; session != nil &&
			session.ClientID == identity.ID && identity.State.Active == nil {
			session.ClientID = 0
		}
	})
	if deactivate {
		if instance.ViewLeaseWindowID != 0 {
			_ = d.releaseWindowView(instance.identity.ID, instance.ViewLeaseWindowID, instance.ViewLeaseGeneration)
			instance.ViewLeaseWindowID = 0
			instance.ViewLeaseGeneration = 0
		}
		instance.releaseFrontendResources()
	}
}

// transitionClientToSession atomically moves one live client process to a
// target session while retaining its transport, streams, output leases, and
// reconnect token. It commits daemon ownership and returns the exact view for
// the ClientInstance actor to apply; it never installs that view itself.
func (d *Daemon) transitionClientToSession(instance *ClientIdentity, targetSessionID uint64, cols, rows uint16) (ViewTransition, error) {
	var source *SessionState
	var target *SessionState
	var displaced *clientConnection
	var transition ViewTransition
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
		source = d.sessions[instance.SessionID]
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
			transition = d.prepareViewTransitionNow(viewTransitionSession, instance, target)
			return
		}
		if d.clients[instance.ID] != instance || instance.State.Active == nil ||
			instance.TerminalReason != "" || source.ClientID != instance.ID ||
			d.sessions[target.ID] != target {
			switchErr = errors.New("client instance can no longer switch sessions")
			return
		}
		sourceWindowID := source.ActiveWindowID
		if sourceWindowID == 0 {
			sourceWindowID = d.windowForClientNow(instance.ID)
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
		if sourceLease == nil || sourceLease.ClientID != instance.ID {
			switchErr = errors.New("stale source window lease")
			return
		}
		targetWindow := target.Windows[targetWindowID]
		if targetWindow == nil {
			switchErr = fmt.Errorf("unknown window %d", targetWindowID)
			return
		}
		targetLease := d.windowLeases[targetWindowID]
		var displacedIdentity *ClientIdentity
		if targetLease != nil && targetLease.ClientID != instance.ID {
			assigned := d.clients[target.ClientID]
			if assigned == nil || assigned == instance || assigned.State.Active == nil {
				switchErr = fmt.Errorf("window %d is currently viewed by another client", targetWindow.DisplayIndex)
				return
			}
			displacedIdentity = assigned
		}
		if _, switchErr = prepareClientWindowGeometryNow(instance, target, targetWindowID); switchErr != nil {
			return
		}
		if displacedIdentity != nil {
			displaced = displacedIdentity.State.Active
			displacedIdentity.TerminalReason = "session taken over by another client"
			displacedIdentity.State.Phase = clientClosing
		}
		// Acquire the target lease before releasing the source lease. The
		// assignment, lease transfer, and immutable projection are one daemon
		// transaction, so a rejected switch leaves every source field intact.
		generation := uint64(1)
		if targetLease != nil {
			generation = targetLease.Generation
			if targetLease.ClientID != instance.ID {
				generation++
			}
		}
		d.windowLeases[targetWindowID] = &WindowViewLease{WindowID: targetWindowID, SessionID: target.ID, ClientID: instance.ID, Generation: generation}
		if sourceWindowID != targetWindowID {
			delete(d.windowLeases, sourceWindowID)
		}
		if target.ActiveWindowID == 0 {
			target.ActiveWindowID = targetWindowID
		}
		source.ClientID = 0
		instance.SessionID = target.ID
		target.ClientID = instance.ID
		transition = d.prepareViewTransitionNow(viewTransitionSession, instance, target)
	})
	if switchErr != nil {
		return transition, switchErr
	}
	if displaced != nil && displaced != instance.State.Active {
		postClientCommand(displaced, clientInstanceCommand{
			Close: true, CloseCode: protocol.SessionReplacedErrorCode,
			CloseReason: "session taken over by another client",
		})
	}
	return transition, nil
}

func (d *Daemon) discardPendingClientInstance(instance *ClientInstance) {
	d.call(func() {
		identity := instance.identity
		if identity == nil {
			return
		}
		if identity.State.Pending == instance.connection {
			identity.State.Pending = nil
			if identity.State.Active != nil {
				identity.State.Phase = clientActive
			} else {
				identity.State.Phase = clientDetached
			}
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
	// The frontend stays in one rich, connection-scoped capture mode. Terminals
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
	var admission ClientAdmission
	var attachCols, attachRows uint16
	responseType := protocol.MsgSessionAttachOK
	switch first.Type {
	case protocol.MsgSessionAttach:
		attach, decodeErr := protocol.DecodeSessionAttach(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		admission, err = d.admitConnection(AdmitConnectionRequest{Kind: admitSessionAttach, Token: attach.Token})
		attachCols, attachRows = attach.Cols, attach.Rows
	case protocol.MsgClientResume:
		resume, decodeErr := protocol.DecodeClientResume(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		admission, err = d.admitConnection(AdmitConnectionRequest{Kind: admitClientResume, Token: resume.ResumeToken})
		attachCols, attachRows = resume.Cols, resume.Rows
		responseType = protocol.MsgClientResumeOK
	default:
		return fmt.Errorf("expected session attachment, got message type %d", first.Type)
	}
	if err != nil {
		reason := "session attachment rejected"
		var attachErr *clientHandshakeError
		if errors.As(err, &attachErr) {
			reason = attachErr.reason
		}
		_ = sendEncodedDirect(controlStream, protocol.MsgSessionAttachFailed, protocol.SessionAttachFailed{Reason: reason}, protocol.EncodeSessionAttachFailed)
		return err
	}
	clientInstance := newClientInstance(d, admission.identity, admission.connection)
	defer close(clientInstance.lifetimeDone)
	attached := false
	defer func() {
		if !attached {
			d.discardPendingClientInstance(clientInstance)
		}
	}()
	controlFrames := make(chan protocol.Frame, 256)
	clientInstance.controlOut = controlFrames
	writerErrs := make(chan error, 4)
	go writeStream(controlStream, controlFrames, writerErrs)
	defer close(controlFrames)
	if responseType == protocol.MsgClientResumeOK {
		if err := sendEncoded(controlFrames, protocol.MsgClientResumeOK, protocol.ClientResumeOK{}, protocol.EncodeClientResumeOK); err != nil {
			return err
		}
	} else if err := sendEncoded(controlFrames, protocol.MsgSessionAttachOK, protocol.SessionAttachOK{
		ResumeToken: admission.ResumeToken,
	}, protocol.EncodeSessionAttachOK); err != nil {
		return err
	}
	if err := clientInstance.registerFrontendTerminalExitCommand([]byte(frontendTerminalExitCommand)); err != nil {
		return err
	}
	if err := clientInstance.writeFrontendTerminal([]byte(frontendTerminalSetup)); err != nil {
		return err
	}
	d.logSessionAttached(admission.SessionID)
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

	clientInstance.QUIC = conn
	clientInstance.StatusOutput = statusOutput
	for slot := range clientInstance.Output {
		clientInstance.Output[slot] = outputLeases[slot]
	}
	if err := clientInstance.attachStatusOutput(statusOutput); err != nil {
		return err
	}
	transition, err := d.initializeClient(ClientInitialized{
		ClientID: admission.ClientID, Cols: attachCols, Rows: attachRows,
		connection: admission.connection,
	})
	if err == nil {
		// Admission commits the effective initial dimensions, including the
		// fallback for clients which supplied zero. The disposable instance was
		// created before that commit, so adopt them before its first status
		// publication and view installation.
		clientInstance.adoptIdentityTerminalSize()
		err = clientInstance.applyViewTransition(transition)
	}
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
		case command := <-clientInstance.commands:
			clientInstance.runClientCommand(command)
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return context.Cause(conn.Context())
		}
	}
}
