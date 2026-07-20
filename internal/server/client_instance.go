package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
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
	credential *reconnectCredential
	detaching  atomic.Bool
	switching  atomic.Bool
	// highestLayoutRevision is transport-scoped. Sessions keep their own
	// revision counters, but a live switch retains the client-side scanout and
	// therefore must not let the next session publish an older revision.
	highestLayoutRevision atomic.Uint64
	Daemon                *Daemon

	QUIC         quic.Connection
	Output       [protocol.MaxRenderSlots]*OutputLease
	StatusOutput io.Writer

	controlOut      chan protocol.Frame
	sessionSwitches chan *sessionSwitchRequest
	lifetimeDone    chan struct{}
	shell           string
	frontendInput   frontendInputParser
	layouts         map[uint64]protocol.WindowLayout
	heldKeys        map[frontendHeldKey]uint64
	pointerCapture  frontendPaneCapture
	pasteCapture    frontendPaneCapture
}

type frontendPaneCapture struct {
	paneID    uint64
	active    bool
	selecting bool
	rect      protocol.Rect
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
	return &ClientInstance{
		credential:      credential,
		Daemon:          d,
		sessionSwitches: make(chan *sessionSwitchRequest, 1),
		lifetimeDone:    make(chan struct{}),
		shell:           defaultShell(),
		layouts:         make(map[uint64]protocol.WindowLayout),
		heldKeys:        make(map[frontendHeldKey]uint64),
	}
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

func (c *ClientInstance) requestSessionSwitch(request *sessionSwitchRequest) error {
	if c == nil || request == nil || request.result == nil || c.sessionSwitches == nil || c.lifetimeDone == nil {
		return errors.New("client instance cannot switch sessions")
	}
	select {
	case c.sessionSwitches <- request:
	case <-c.lifetimeDone:
		return errors.New("client instance is no longer connected")
	}
	select {
	case err := <-request.result:
		return err
	case <-c.lifetimeDone:
		select {
		case err := <-request.result:
			return err
		default:
			return errors.New("client instance disconnected during session switch")
		}
	}
}

func completeSessionSwitch(request *sessionSwitchRequest, err error) {
	if request == nil || request.result == nil {
		return
	}
	request.result <- err
}

type clientControlEvent struct {
	frame protocol.Frame
	err   error
}

func readClientControl(decoder *protocol.Decoder, events chan<- clientControlEvent) {
	for {
		frame, err := decoder.ReadFrame()
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

func (d *Daemon) issueAttachGrant(session *Session) (uint16, string, time.Time, error) {
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

func (d *Daemon) beginClientInstance(encodedToken string) (*ClientInstance, *Session, error) {
	reconnectToken, err := protocol.NewAuthToken()
	if err != nil {
		return nil, nil, err
	}
	var instance *ClientInstance
	var session *Session
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
		credential.Instance = instance
		d.reconnectCredentials[encodedReconnectToken] = credential
		displaced = d.assignClientInstanceLocked(credential, session)
	})
	if displaced != nil && displaced.QUIC != nil {
		_ = displaced.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "session taken over by another client")
	}
	return instance, session, attachErr
}

// assignClientInstanceLocked is the one daemon-actor mutation point for the
// one-client/one-session relationship. Initial attach and live switching use
// the same operation before sessions perform their output-lease handoff.
func (d *Daemon) assignClientInstanceLocked(credential *reconnectCredential, session *Session) *ClientInstance {
	if oldSessionID := d.clientSessions[credential]; oldSessionID != 0 && oldSessionID != session.ID && d.attachments[oldSessionID] == credential {
		delete(d.attachments, oldSessionID)
	}
	var displaced *ClientInstance
	if previous := d.attachments[session.ID]; previous != nil && previous != credential {
		delete(d.clientSessions, previous)
		previous.TerminalReason = "session was taken over by another client"
		displaced = previous.Instance
	}
	d.clientSessions[credential] = session.ID
	d.attachments[session.ID] = credential
	return displaced
}

func (d *Daemon) resumeClientInstance(encodedToken string) (*ClientInstance, *Session, error) {
	var instance *ClientInstance
	var session *Session
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
		credential.Instance = instance
	})
	if previous != nil {
		previous.detaching.Store(true)
		if previous.QUIC != nil {
			_ = previous.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "client reconnected elsewhere")
		}
	}
	return instance, session, resumeErr
}

func (d *Daemon) attachClientInstance(instance *ClientInstance, conn quic.Connection, status io.Writer, output map[int]*OutputLease, controlOut chan protocol.Frame, cols, rows uint16) (*Session, error) {
	var session *Session
	var activateErr error
	d.call(func() {
		credential := instance.credential
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
		instance.StatusOutput = status
		instance.controlOut = controlOut
		for slot := range instance.Output {
			instance.Output[slot] = output[slot]
		}
	})
	if activateErr != nil {
		return nil, activateErr
	}
	if err := session.attachClientInstance(instance, cols, rows); err != nil {
		d.detachClientInstance(instance, session)
		return nil, err
	}
	return session, nil
}

func (d *Daemon) detachClientInstance(instance *ClientInstance, session *Session) {
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
		session.detachClientInstance()
	}
}

// switchClientInstance moves one live client process to another session while
// retaining its QUIC transport, streams, output leases, and reconnect token.
// The reconnect credential remains untouched. The daemon changes the live
// assignment and the target-session hint used on a later reconnect; each
// session owns its side of the output and status handoff.
func (d *Daemon) switchClientInstance(instance *ClientInstance, source *Session, rawTarget string, cols, rows uint16) (*Session, error) {
	targetSpec, err := parseSessionTarget(rawTarget)
	if err != nil {
		return nil, err
	}
	var target *Session
	var displaced *ClientInstance
	var switchErr error
	d.call(func() {
		if targetSpec.id != 0 {
			target = d.sessions[targetSpec.id]
		} else {
			target = d.sessionByName(targetSpec.name)
		}
		if target == nil {
			switchErr = fmt.Errorf("unknown session %q", rawTarget)
			return
		}
		if source == target {
			return
		}
		credential := instance.credential
		if credential == nil || credential.Instance != instance || credential.TerminalReason != "" ||
			d.attachments[source.ID] != credential || d.sessions[target.ID] != target {
			switchErr = errors.New("client instance can no longer switch sessions")
			return
		}
		instance.switching.Store(true)
		displaced = d.assignClientInstanceLocked(credential, target)
	})
	if switchErr != nil {
		return nil, switchErr
	}
	if source == target {
		return target, nil
	}
	if displaced != nil && displaced.QUIC != nil {
		_ = displaced.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "session taken over by another client")
	}

	source.detachClientInstance()
	instance.switching.Store(false)
	if err := target.attachClientInstance(instance, cols, rows); err != nil {
		// The daemon assignment has already moved. Return the target with the
		// error so transport cleanup detaches from the correct session and a
		// later reconnect uses the new target hint.
		return target, err
	}
	return target, nil
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
	var s *Session
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
		clientInstance, s, err = d.beginClientInstance(attach.Token)
		attachCols, attachRows = attach.Cols, attach.Rows
	case protocol.MsgSessionResume:
		resume, decodeErr := protocol.DecodeSessionResume(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		if resume.Version != protocol.ProtocolVersion {
			return errors.New("unsupported session protocol version")
		}
		clientInstance, s, err = d.resumeClientInstance(resume.ResumeToken)
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
	d.logSessionAttached(s.ID)
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
		outputLeases[slot] = &OutputLease{Slot: slot, Stream: outputStream}
	}

	s, err = d.attachClientInstance(clientInstance, conn, statusOutput, outputLeases, controlFrames, attachCols, attachRows)
	if err != nil {
		_ = conn.CloseWithError(protocol.SessionReplacedErrorCode, err.Error())
		return err
	}
	attached = true
	defer func() {
		_ = conn.CloseWithError(0, "")
		d.detachClientInstance(clientInstance, s)
	}()
	controlEvents := make(chan clientControlEvent, 1)
	go readClientControl(controlDecoder, controlEvents)
	applySwitch := func(request *sessionSwitchRequest) error {
		target, switchErr := d.switchClientInstance(clientInstance, s, request.rawTarget, request.cols, request.rows)
		if switchErr != nil {
			completeSessionSwitch(request, switchErr)
			if target != nil {
				s = target
				clientInstance.resetInputForSessionSwitch()
				return switchErr
			}
			_ = s.coordinate(func() error {
				s.showStatusMessage(clientID0, switchErr.Error())
				return s.publishStatusBar()
			})
			return nil
		}
		s = target
		clientInstance.resetInputForSessionSwitch()
		completeSessionSwitch(request, nil)
		return nil
	}
	exitRequested := false
	for {
		select {
		case err := <-writerErrs:
			return err
		case event := <-controlEvents:
			if event.err != nil {
				if errors.Is(event.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("read control frame: %w", event.err)
			}
			if exitRequested {
				if event.frame.Type == protocol.MsgFrontendTerminalExitComplete {
					if len(event.frame.Payload) != 0 {
						return errors.New("frontend terminal exit completion has a payload")
					}
					return nil
				}
				// Input and resize frames already queued before the client applied
				// the exit command can arrive before its acknowledgment. Ignore
				// them while retaining the acknowledgment as the close barrier.
				continue
			}
			stopped, err := s.handleControlFrame(clientInstance, event.frame)
			if stopped {
				if err := sendEncoded(clientInstance.controlOut, protocol.MsgFrontendExecuteTerminalExitCommand, struct{}{}, func(dst []byte, _ struct{}) ([]byte, error) {
					return dst, nil
				}); err != nil {
					return err
				}
				exitRequested = true
				continue
			}
			var request *sessionSwitchRequest
			if !errors.As(err, &request) {
				if err != nil {
					return err
				}
				continue
			}
			if err := applySwitch(request); err != nil {
				return err
			}
		case request := <-clientInstance.sessionSwitches:
			if err := applySwitch(request); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return context.Cause(conn.Context())
		}
	}
}
