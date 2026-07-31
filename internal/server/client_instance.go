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
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

// ClientInstance is the live daemon-side representation of one running meja
// client process. It owns one QUIC transport and its protocol streams. The
// object is discarded when QUIC closes; only its ClientIdentity survives
// so a later reconnect can rebuild it.
type ClientInstance struct {
	clientID   ClientID
	connection *clientConnection

	sessionID    uint64
	terminalCols uint16
	terminalRows uint16
	shell        string
	commands     chan clientInstanceCommand

	ViewLeaseWindowID   uint64
	ViewLeaseGeneration uint64
	ended               bool
	// appliedProjectionRevision orders every daemon decision applied by this
	// transport, including focus-only updates that reuse currentView.Layout's
	// client-view revision.
	appliedProjectionRevision uint64
	Daemon                    *Daemon
	// inputState is the client actor's prefix and directional-focus parser
	// state. It contains no prompt draft, session, viewport, or installed-view
	// data.
	inputState clientInputState
	// currentView is the last daemon-resolved view this client actor
	// successfully published. It is also the exact source for output handoff.
	currentView ClientView

	QUIC   quic.Connection
	Output [protocol.MaxRenderSlots]*OutputLease

	controlOut                chan protocol.Frame
	statusPublicationRevision uint64
	statusMessage             string
	statusMessageID           uint64
	statusMessageDuration     time.Duration
	lifetimeDone              chan struct{}
	frontendInput             frontendInputParser
	heldKeys                  map[frontendHeldKey]uint64
	promptContinuation        promptContinuation
	activePrompt              *activePrompt
	nextPromptID              uint64
	pointerCapture            frontendPaneCapture
	pasteCapture              frontendPaneCapture
}

type clientInstanceCommand struct {
	Transition         *ViewTransition
	RefreshStatus      bool
	ClearStatusMessage uint64
	Status             clientStatusState
	HasStatus          bool
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
	Snapshot           chan<- clientInstanceSnapshot
}

type clientStatusRefresh struct {
	Status    clientStatusState
	HasStatus bool
}

// clientRequiredDelivery is enqueued in daemon-turn order and completed by the
// producer after its canonical turn returns. The persistent connection worker
// waits on reservations in queue order, so concurrent producers cannot reverse
// daemon effect order by racing their later delivery attempts.
type clientRequiredDelivery struct {
	completion chan clientRequiredCompletion
}

type clientRequiredCompletion struct {
	command clientInstanceCommand
	skip    bool
}

func (d *clientRequiredDelivery) finish(command clientInstanceCommand, skip bool) {
	if d == nil {
		return
	}
	select {
	case d.completion <- clientRequiredCompletion{command: command, skip: skip}:
	default:
	}
}

func (d *clientRequiredDelivery) cancel() {
	d.finish(clientInstanceCommand{}, true)
}

// clientInstanceSnapshot is an immutable observation copied by the client
// actor. It exists so diagnostics and tests synchronize through the actor
// rather than reaching into ordinary actor-owned fields.
type clientInstanceSnapshot struct {
	Ended                      bool
	AppliedProjectionRevision  uint64
	StatusMessage              string
	TerminalCols, TerminalRows uint16
	View                       ClientView
}

type clientInputState struct {
	FocusX2       int
	FocusY2       int
	HasFocusPoint bool

	InputState        serverInputState
	PrefixEscape      []byte
	ResizeRepeatUntil time.Time
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
	if plan.ClientID != 0 && plan.ClientID != c.clientID {
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
	if plan.ProjectionRevision <= c.appliedProjectionRevision {
		return errors.New("stale client projection revision")
	}
	if plan.SessionID != 0 && c.sessionID != 0 && plan.SessionID != c.sessionID {
		return errors.New("stale client projection session")
	}
	return nil
}

func (c *ClientInstance) focusPane(paneID uint64) (*Window, error) {
	window, transition, err := c.Daemon.focusClientPane(c.clientID, paneID)
	if err != nil {
		return nil, err
	}
	if transition.Projection.FullSnapshot {
		return window, c.applyViewTransition(transition)
	}
	return window, c.applyFocusTransition(transition)
}

func (c *ClientInstance) activePane() *Pane {
	if c == nil {
		return nil
	}
	return c.currentView.FocusedPane()
}

func (c *ClientInstance) isFocusedPane(paneID uint64) bool {
	return c != nil && c.currentView.Layout.FocusedPaneID == paneID
}

func postClientCommand(connection *clientConnection, command clientInstanceCommand) {
	if connection == nil {
		return
	}
	if command.Transition != nil {
		if command.Transition.deliveryRejected {
			return
		}
		if command.Transition.delivery != nil {
			if command.Close {
				connection.revoke()
			}
			command.Transition.delivery.finish(command, false)
			return
		}
	}
	if command.RefreshStatus && command.Transition == nil && !command.Close &&
		command.FocusDirection == 0 && !command.EnterHistory &&
		!command.RunSendKeys && !command.RunPasteBuffer {
		connection.enqueueStatusRefresh(command.Status, command.HasStatus)
		return
	}
	connection.enqueueRequired(command)
}

func (d *Daemon) clientConnectionIsCurrent(clientID ClientID, connection *clientConnection) bool {
	if d == nil || clientID == 0 || connection == nil {
		return false
	}
	current := false
	d.call(func() {
		identity := d.clients[clientID]
		current = identity != nil && identity.State.Active == connection
	})
	return current
}

func (c *ClientInstance) postCommand(command clientInstanceCommand) {
	if c == nil || c.connection == nil {
		return
	}
	// A nil mailbox is possible only for an unpublished test fixture. Published
	// production instances always have their actor mailbox before callbacks can
	// retain the instance.
	if c.commands == nil {
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
	case command.Snapshot != nil:
		view := c.currentView
		view.Layout.Panes = append([]protocol.PanePlacement(nil), c.currentView.Layout.Panes...)
		view.Panes = append([]ClientPanePlacement(nil), c.currentView.Panes...)
		view.NavigationPanes = append([]protocol.PanePlacement(nil), c.currentView.NavigationPanes...)
		view.Status.Windows = append([]WindowStatus(nil), c.currentView.Status.Windows...)
		if c.currentView.paneByID != nil {
			view.paneByID = make(map[uint64]*Pane, len(c.currentView.paneByID))
			for id, pane := range c.currentView.paneByID {
				view.paneByID[id] = pane
			}
		}
		command.Snapshot <- clientInstanceSnapshot{
			Ended:                     c.ended,
			AppliedProjectionRevision: c.appliedProjectionRevision,
			StatusMessage:             c.statusMessage,
			TerminalCols:              c.terminalCols,
			TerminalRows:              c.terminalRows,
			View:                      view,
		}
	case command.Transition != nil:
		err = c.applyViewTransition(*command.Transition)
	case command.ClearStatusMessage != 0:
		if c.sessionID != 0 && c.statusMessageID == command.ClearStatusMessage {
			c.statusMessage = ""
			err = c.publishClientStatus()
		}
	case command.RefreshStatus:
		if command.HasStatus {
			if !c.installStatusSnapshot(command.Status) {
				break
			}
		}
		err = c.publishClientStatus()
	case command.FocusDirection != 0:
		_, _, err = c.FocusPaneDirection(command.FocusDirection)
	case command.EnterHistory:
		err = c.commandEnterHistory()
	case command.RunSendKeys:
		err = sendKeysToClient(c, command.SendKeys)
	case command.RunPasteBuffer:
		err = pasteBufferToClient(c, command.PasteBuffer)
	case command.Close:
		if command.CloseCode == 0 {
			c.ended = true
		} else if c.QUIC != nil {
			err = c.QUIC.CloseWithError(command.CloseCode, command.CloseReason)
		}
	}
	if command.Done != nil {
		command.Done <- err
	}
	if err != nil && command.Done == nil && c.Daemon != nil {
		c.Daemon.logf("meja server: client event failed client=%d session=%d: %v\n",
			c.clientID, c.sessionID, err)
	}
}

// installStatusSnapshot applies only an advisory snapshot for the currently
// installed session and never lets it move the client actor's status revision
// backward. Daemon-prepared ClientViews use the same rule below.
func (c *ClientInstance) installStatusSnapshot(status clientStatusState) bool {
	if c == nil || status.SessionID != c.sessionID {
		return false
	}
	if c.currentView.StatusValid && status.Revision < c.currentView.Status.Revision {
		return false
	}
	c.currentView.Status = status
	c.currentView.StatusValid = true
	return true
}

func (c *ClientInstance) orderedViewStatus(view ClientView, sessionID uint64) ClientView {
	if !view.StatusValid || view.Status.SessionID != sessionID {
		view.Status = clientStatusState{}
		view.StatusValid = false
		return view
	}
	if c.currentView.StatusValid && view.Status.Revision < c.currentView.Status.Revision {
		if c.currentView.Status.SessionID == sessionID {
			view.Status = c.currentView.Status
			view.StatusValid = true
		} else {
			view.Status = clientStatusState{}
			view.StatusValid = false
		}
	}
	return view
}

type frontendPaneCapture struct {
	paneID uint64
	pane   *Pane
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
	commands   chan clientInstanceCommand
	required   chan *clientRequiredDelivery
	available  chan *clientRequiredDelivery
	refresh    chan clientStatusRefresh
	fence      chan clientInstanceCommand
	done       <-chan struct{}
	revoked    chan struct{}
	revokeOnce sync.Once
	workerOnce sync.Once
	// deliveryConfigured is set before the worker is published and is used only
	// to keep incremental test construction from rewriting live channel fields.
	deliveryConfigured bool
	// workerStarted is an optional construction probe used by deterministic
	// mailbox tests. Production connections leave it nil.
	workerStarted chan<- struct{}
}

// The required path has a total bounded backlog of 64 effects: at most 63
// queued reservations plus the one reservation held by the delivery worker.
// The client actor handoff is unbuffered and therefore adds no second backlog.
const (
	clientRequiredDeliveryCapacity = 64
	clientRequiredQueueCapacity    = clientRequiredDeliveryCapacity - 1
)

func newClientConnection() *clientConnection {
	connection := &clientConnection{
		commands:  make(chan clientInstanceCommand),
		required:  make(chan *clientRequiredDelivery, clientRequiredQueueCapacity),
		available: make(chan *clientRequiredDelivery, clientRequiredDeliveryCapacity),
		refresh:   make(chan clientStatusRefresh, 1),
		fence:     make(chan clientInstanceCommand, 1),
		revoked:   make(chan struct{}),
	}
	connection.initializeRequiredDeliveries()
	return connection
}

func (c *clientConnection) initializeRequiredDeliveries() {
	if c.available == nil {
		c.available = make(chan *clientRequiredDelivery, clientRequiredDeliveryCapacity)
	}
	for len(c.available) < cap(c.available) {
		c.available <- &clientRequiredDelivery{completion: make(chan clientRequiredCompletion, 1)}
	}
}

func (c *clientConnection) revoke() {
	if c == nil {
		return
	}
	c.revokeOnce.Do(func() {
		if c.revoked == nil {
			c.revoked = make(chan struct{})
		}
		close(c.revoked)
	})
}

func (c *clientConnection) isRevoked() bool {
	if c == nil {
		return true
	}
	if c.revoked == nil {
		return false
	}
	select {
	case <-c.revoked:
		return true
	default:
		return false
	}
}

func (c *clientConnection) startDeliveryWorker() {
	if c == nil {
		return
	}
	c.workerOnce.Do(func() {
		if c.commands == nil {
			c.commands = make(chan clientInstanceCommand)
		}
		if c.required == nil {
			c.required = make(chan *clientRequiredDelivery, clientRequiredQueueCapacity)
		}
		c.initializeRequiredDeliveries()
		if c.refresh == nil {
			c.refresh = make(chan clientStatusRefresh, 1)
		}
		if c.fence == nil {
			c.fence = make(chan clientInstanceCommand, 1)
		}
		c.deliveryConfigured = true
		if c.workerStarted != nil {
			c.workerStarted <- struct{}{}
		}
		go c.runDeliveryWorker()
	})
}

func (c *clientConnection) reserveRequired() (*clientRequiredDelivery, bool) {
	if c == nil {
		return nil, false
	}
	c.startDeliveryWorker()
	var delivery *clientRequiredDelivery
	select {
	case delivery = <-c.available:
	default:
		c.fenceSaturatedDelivery()
		return nil, false
	}
	select {
	case c.required <- delivery:
		return delivery, true
	default:
		// Required transitions are never discarded while leaving the transport
		// live. Saturation fences this exact connection; canonical daemon state
		// and unrelated clients continue independently.
		c.available <- delivery
		c.fenceSaturatedDelivery()
		return nil, false
	}
}

func (c *clientConnection) fenceSaturatedDelivery() {
	fence := clientInstanceCommand{
		Close:       true,
		CloseCode:   protocol.RenderOutputErrorCode,
		CloseReason: "client delivery mailbox saturated",
	}
	c.revoke()
	select {
	case c.fence <- fence:
	default:
	}
}

func (c *clientConnection) enqueueRequired(command clientInstanceCommand) bool {
	if command.Close {
		c.revoke()
	}
	delivery, ok := c.reserveRequired()
	if !ok {
		return false
	}
	delivery.finish(command, false)
	return true
}

func (c *clientConnection) enqueueStatusRefresh(status clientStatusState, hasStatus bool) {
	if c == nil {
		return
	}
	c.startDeliveryWorker()
	refresh := clientStatusRefresh{Status: status, HasStatus: hasStatus}
	select {
	case c.refresh <- refresh:
	default:
		// Replace the pending advisory snapshot with the newest one. The
		// connection worker remains the single consumer.
		select {
		case <-c.refresh:
		default:
		}
		select {
		case c.refresh <- refresh:
		default:
		}
	}
}

func (c *clientConnection) runDeliveryWorker() {
	for {
		var command clientInstanceCommand
		var delivery *clientRequiredDelivery
		// A saturation fence supersedes queued work for the unhealthy
		// connection. Otherwise required FIFO work always gets the first chance;
		// status is a capacity-one advisory edge.
		select {
		case command = <-c.fence:
		default:
			select {
			case command = <-c.fence:
			case delivery = <-c.required:
			default:
				select {
				case command = <-c.fence:
				case delivery = <-c.required:
				case refresh := <-c.refresh:
					command = clientInstanceCommand{
						RefreshStatus: true, Status: refresh.Status, HasStatus: refresh.HasStatus,
					}
				case <-c.done:
					return
				}
			}
		}
		if delivery != nil {
			select {
			case completion := <-delivery.completion:
				command = completion.command
				if command.Transition != nil {
					command.Transition.delivery = nil
				}
				c.available <- delivery
				if completion.skip {
					continue
				}
			case fence := <-c.fence:
				command = fence
			case <-c.done:
				return
			}
		}
		select {
		case c.commands <- command:
		case fence := <-c.fence:
			select {
			case c.commands <- fence:
			case <-c.done:
				return
			}
		case <-c.done:
			return
		}
	}
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
	terminalCols       uint16
	terminalRows       uint16
	projectionRevision uint64
	statusRevision     uint64
	shell              string

	// lastAllocatedClientLayoutRevision is the daemon's monotonic allocator
	// state for client-view/cache epochs across disposable transports.
	lastAllocatedClientLayoutRevision protocol.ClientLayoutRevision
}

func newClientInstanceFromAdmission(d *Daemon, admission ClientAdmission) *ClientInstance {
	connection := admission.connection
	if connection == nil {
		connection = newClientConnection()
	}
	if connection.commands == nil {
		connection.commands = make(chan clientInstanceCommand)
	}
	instance := &ClientInstance{
		clientID:     admission.ClientID,
		connection:   connection,
		Daemon:       d,
		shell:        defaultShell(),
		commands:     connection.commands,
		lifetimeDone: make(chan struct{}),
		heldKeys:     make(map[frontendHeldKey]uint64),
	}
	instance.sessionID = admission.SessionID
	if admission.Shell != "" {
		instance.shell = admission.Shell
	}
	instance.terminalCols = admission.Cols
	instance.terminalRows = admission.Rows
	connection.done = instance.lifetimeDone
	connection.startDeliveryWorker()
	return instance
}

func (c *ClientInstance) adoptIdentityTerminalSize() {
	if c == nil || c.Daemon == nil || c.clientID == 0 {
		return
	}
	var cols, rows uint16
	c.Daemon.call(func() {
		if identity := c.Daemon.clients[c.clientID]; identity != nil && identity.State.Active == c.connection {
			cols, rows = identity.terminalCols, identity.terminalRows
		}
	})
	c.terminalCols = cols
	c.terminalRows = rows
}

func sendClientCommand(connection *clientConnection, command clientInstanceCommand) error {
	if connection == nil {
		return errors.New("client connection is unavailable")
	}
	result := make(chan error, 1)
	command.Done = result
	if command.Transition != nil && command.Transition.deliveryRejected {
		return errors.New("target client delivery mailbox saturated")
	}
	if command.Transition != nil && command.Transition.delivery != nil {
		command.Transition.delivery.finish(command, false)
	} else if !connection.enqueueRequired(command) {
		return errors.New("target client delivery mailbox saturated")
	}
	select {
	case err := <-result:
		return err
	case <-connection.done:
		return errors.New("target client disconnected")
	}
}

func sendReservedClientCommand(delivery clientCommandDelivery, command clientInstanceCommand) error {
	if delivery.Connection == nil {
		return errors.New("target client delivery is unavailable")
	}
	if delivery.Rejected || delivery.Required == nil {
		return errors.New("target client delivery mailbox saturated")
	}
	result := make(chan error, 1)
	command.Done = result
	if command.Close {
		delivery.Connection.revoke()
	}
	delivery.Required.finish(command, false)
	select {
	case err := <-result:
		return err
	case <-delivery.Connection.done:
		return errors.New("target client disconnected")
	}
}

func postReservedClientCommand(delivery clientCommandDelivery, command clientInstanceCommand) bool {
	if delivery.Connection == nil || delivery.Rejected || delivery.Required == nil {
		return false
	}
	if command.Close {
		delivery.Connection.revoke()
	}
	delivery.Required.finish(command, false)
	return true
}

// reserveConnectionDeliveryNow is called only while the daemon actor owns the
// canonical effect being prepared.
func reserveConnectionDeliveryNow(connection *clientConnection) clientCommandDelivery {
	if connection == nil {
		return clientCommandDelivery{}
	}
	required, ok := connection.reserveRequired()
	return clientCommandDelivery{Connection: connection, Required: required, Rejected: !ok}
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
	c.activePrompt = nil
	c.promptContinuation = nil
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

// releaseFrontendResources tears down transport-local pane output resources
// after the daemon has fenced and unregistered this instance.
func (c *ClientInstance) releaseFrontendResources() {
	if c == nil {
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

func (d *Daemon) issueAttachGrant(sessionID uint64) (uint16, string, time.Time, error) {
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
		if d.sessions[sessionID] == nil {
			issueErr = errSessionUnavailable
			return
		}
		d.removeExpiredAttachGrants(time.Now())
		d.attachGrants = append(d.attachGrants, attachGrant{Token: token, SessionID: sessionID, ExpiresAt: expiresAt})
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
	Shell       string
	Cols        uint16
	Rows        uint16
	connection  *clientConnection
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
	var attachErr error
	var displaced *clientConnection
	var displacedDelivery clientCommandDelivery
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
		session := d.sessions[sessionID]
		if session == nil {
			attachErr = &clientHandshakeError{reason: "session attachment rejected"}
			return
		}
		encodedReconnectToken := protocol.EncodeAuthToken(reconnectToken)
		if d.nextClientID == 0 {
			d.nextClientID = 1
		}
		connection := newClientConnection()
		identity := &ClientIdentity{
			ID: d.nextClientID, ResumeToken: encodedReconnectToken, SessionID: session.ID,
			State: clientLifecycle{Phase: clientPending, Pending: connection},
			shell: defaultShell(),
		}
		d.nextClientID++
		admission = ClientAdmission{
			ClientID: identity.ID, SessionID: session.ID,
			ResumeToken: identity.ResumeToken, Shell: identity.shell,
			Cols: identity.terminalCols, Rows: identity.terminalRows, connection: connection,
		}
		d.clients[identity.ID] = identity
		d.clientTokens[encodedReconnectToken] = identity.ID
		if previous := d.clients[session.ClientID]; previous != nil && previous != identity {
			previous.TerminalReason = "session was taken over by another client"
			if previous.State.Active != nil {
				displaced = previous.State.Active
				displacedDelivery = reserveConnectionDeliveryNow(displaced)
				identity.State.WaitFor = displaced.done
				previous.State.Phase = clientClosing
			}
		}
	})
	if displaced != nil {
		_ = sendReservedClientCommand(displacedDelivery, clientInstanceCommand{
			Close: true, CloseCode: protocol.SessionReplacedErrorCode,
			CloseReason: "session taken over by another client",
		})
	}
	return admission, attachErr
}

func (d *Daemon) admitResumedConnection(encodedToken string) (ClientAdmission, error) {
	var admission ClientAdmission
	var previous *clientConnection
	var previousDelivery clientCommandDelivery
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
		session := d.sessions[sessionID]
		if session == nil {
			resumeErr = &clientHandshakeError{reason: "session is no longer available"}
			return
		}
		connection := newClientConnection()
		previous = identity.State.Active
		if previous != nil {
			previousDelivery = reserveConnectionDeliveryNow(previous)
		}
		identity.State = clientLifecycle{
			Phase: clientPending, Pending: connection,
		}
		if previous != nil {
			identity.State.Phase = clientReplacing
			identity.State.Active = previous
		}
		admission = ClientAdmission{
			ClientID: identity.ID, SessionID: session.ID, ResumeToken: identity.ResumeToken,
			Shell: identity.shell, Cols: identity.terminalCols, Rows: identity.terminalRows,
			connection: connection,
		}
	})
	if previous != nil {
		_ = sendReservedClientCommand(previousDelivery, clientInstanceCommand{
			Close: true, CloseCode: protocol.SessionReplacedErrorCode,
			CloseReason: "client reconnected elsewhere",
		})
	}
	return admission, resumeErr
}

func (d *Daemon) initializeClient(request ClientInitialized) (ViewTransition, error) {
	var transition ViewTransition
	var sessionID uint64
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
		session := d.sessions[identity.SessionID]
		if session == nil {
			activateErr = &clientHandshakeError{reason: "session is no longer available"}
			return
		}
		sessionID = session.ID
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
		session := d.sessions[identity.SessionID]
		if identity.TerminalReason != "" || session == nil || identity.SessionID != sessionID {
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
		identity.terminalCols = cols
		identity.terminalRows = rows
		session.ClientID = identity.ID
		transition, activateErr = d.prepareAttachedClientViewNow(identity, session, cols, rows)
	})
	return transition, activateErr
}

func (d *Daemon) detachClientInstance(instance *ClientInstance) {
	deactivate := false
	d.call(func() {
		identity := d.clients[instance.clientID]
		if identity == nil {
			return
		}
		switch {
		case identity.State.Active == instance.connection:
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
			_ = d.releaseWindowView(instance.clientID, instance.ViewLeaseWindowID, instance.ViewLeaseGeneration)
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
func (d *Daemon) transitionClientToSession(clientID ClientID, connection *clientConnection, targetSessionID uint64, cols, rows uint16) (ViewTransition, error) {
	var source *SessionState
	var target *SessionState
	var displaced *clientConnection
	var displacedDelivery clientCommandDelivery
	var transition ViewTransition
	var switchErr error
	if clientID == 0 || connection == nil {
		return transition, errors.New("nil client instance")
	}
	if targetSessionID == 0 {
		return transition, errors.New("target session is unavailable")
	}
	d.call(func() {
		instance := d.clients[clientID]
		if instance == nil || instance.State.Active != connection {
			switchErr = errors.New("client instance can no longer switch sessions")
			return
		}
		instance.terminalCols = cols
		instance.terminalRows = rows
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
		if instance.TerminalReason != "" || source.ClientID != instance.ID ||
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
			displacedDelivery = reserveConnectionDeliveryNow(displaced)
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
	if displaced != nil && displaced != connection {
		postReservedClientCommand(displacedDelivery, clientInstanceCommand{
			Close: true, CloseCode: protocol.SessionReplacedErrorCode,
			CloseReason: "session taken over by another client",
		})
	}
	return transition, nil
}

func (d *Daemon) discardPendingClientInstance(instance *ClientInstance) {
	d.call(func() {
		identity := d.clients[instance.clientID]
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
	Slot                  int
	Stream                io.Writer
	frontendTerminalWrite func([]byte) error

	workerOnce sync.Once
	ready      chan confirmerMessage
	failed     chan error
	workerDone chan struct{}
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
	clientInstance := newClientInstanceFromAdmission(d, admission)
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
	outputLeases := make(map[int]*OutputLease, int(protocol.MaxRenderSlots))
	for slot := 0; slot < int(protocol.MaxRenderSlots); slot++ {
		outputStream, err := conn.OpenUniStreamSync(ctx)
		if err != nil {
			return fmt.Errorf("open output stream %d: %w", slot, err)
		}
		if index, ok := protocol.OutputIndexFromStreamID(uint64(outputStream.StreamID())); !ok || int(index) != slot {
			return fmt.Errorf("pane output stream ID %d has index %d, want %d", outputStream.StreamID(), index, slot)
		}
		leaseSlot := slot
		outputLeases[slot] = &OutputLease{
			Slot:                  slot,
			Stream:                outputStream,
			frontendTerminalWrite: clientInstance.writeFrontendTerminal,
			done:                  conn.Context().Done(),
			onFailure: func(writeErr error) {
				_ = conn.CloseWithError(protocol.RenderOutputErrorCode, fmt.Sprintf("pane output slot %d failed: %v", leaseSlot, writeErr))
			},
		}
	}

	clientInstance.QUIC = conn
	for slot := range clientInstance.Output {
		clientInstance.Output[slot] = outputLeases[slot]
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
	exitRequested := false
	requestTerminalExit := func(message string) error {
		if exitRequested {
			return nil
		}
		request := protocol.FrontendExecuteTerminalExitCommand{
			Message: message,
		}
		if err := sendEncoded(
			clientInstance.controlOut,
			protocol.MsgFrontendExecuteTerminalExitCommand,
			request,
			protocol.EncodeFrontendExecuteTerminalExitCommand,
		); err != nil {
			return err
		}
		exitRequested = true
		return nil
	}
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
			message := fmt.Sprintf("[detached (from session %d)]", clientInstance.sessionID)
			if clientInstance.ended {
				message = "[exited]"
			}
			if err := requestTerminalExit(message); err != nil {
				return false, err
			}
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
			if command.Close && command.CloseCode == 0 {
				message := command.CloseReason
				if message == "" {
					message = "[exited]"
				}
				if err := requestTerminalExit(message); err != nil {
					return err
				}
			} else if clientInstance.ended {
				if err := requestTerminalExit("[exited]"); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return context.Cause(conn.Context())
		}
	}
}
