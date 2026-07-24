package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/quic-go/quic-go"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/garindra/meja/internal/protocol"
	"github.com/garindra/meja/internal/theme"
)

const (
	quicMaxIdleTimeout  = 6 * time.Second
	quicKeepAlivePeriod = 2 * time.Second
	// frontendEscapeDelay resolves the legacy ambiguity between a standalone
	// Escape key and the prefix of a longer terminal sequence. It is measured
	// at the local TTY boundary so transport latency cannot change input
	// semantics.
	frontendEscapeDelay = 25 * time.Millisecond
)

var errDisconnectedInterrupt = errors.New("interrupted while disconnected")

type Target struct {
	Original        string
	Username        string
	Hostname        string
	HasExplicitUser bool
}

type Config struct {
	Local                    bool
	Target                   Target
	Port                     int
	PortSet                  bool
	IdentityFile             string
	RenderDiagnostics        bool
	RenderDiagnosticsLogPath string
	Cwd                      string
	RemotePath               string
	SocketSelector           SocketSelector
	CallerSessionTarget      string
	CallerPaneID             uint64
	CommandArgs              []string
	TerminalCols             uint16
	TerminalRows             uint16
	Stdin                    *os.File
	Stdout                   io.Writer
	Stderr                   io.Writer
}

type runtimeState struct {
	stdout                io.Writer
	events                chan renderEvent
	diagnostics           *renderDiagnostics
	renderDone            chan struct{}
	renderExitCommand     chan []byte
	dropConnectionEvents  atomic.Bool
	appliedLayoutRevision atomic.Uint64
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type renderEvent any
type paintKind uint8

const (
	paintText paintKind = iota + 1
	paintCluster
	paintFill
)

type paintSpan struct {
	kind        paintKind
	row, column int
	styleID     uint32
	cellWidth   uint8
	text        []byte
	fillRune    rune
	fillColumns int
}

type renderFrame struct {
	layoutRevision protocol.ClientLayoutRevision
	cols, rows     int
	styleInstalls  []protocol.StyleDefinition
	scrollDelta    int
	spans          []paintSpan
	cursor         protocol.Cursor
	cursorVisible  bool
	cursorUpdated  bool
}

type paneFrameEvent struct {
	slot  uint8
	frame renderFrame
}
type localInputEvent struct{ data []byte }
type layoutEvent struct{ layout protocol.ClientLayout }
type sizeEvent struct{ cols, rows int }
type reconnectEvent struct {
	reconnecting bool
	lastContact  time.Time
}
type terminalStatusEvent struct{ message string }
type renderBarrierEvent struct{ done chan struct{} }

type terminalWriteEvent struct {
	data []byte
	done chan error
}

type terminalExitCommandEvent struct {
	data []byte
	done chan struct{}
}

type terminalExitEvent struct{ done chan error }

type terminalShutdownEvent struct {
	data []byte
	done chan error
}

type rectangularScrollEvent struct {
	enabled bool
	done    chan struct{}
}

var errRenderShutdown = errors.New("render loop shutdown")

func ParseTarget(raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "@") || strings.HasSuffix(raw, "@") {
		return Target{}, fmt.Errorf("invalid target %q", raw)
	}
	username, hostname, hasUser := strings.Cut(raw, "@")
	if !hasUser {
		username = ""
		hostname = raw
	}
	if hostname == "" || (hasUser && username == "") {
		return Target{}, fmt.Errorf("invalid target %q", raw)
	}
	return Target{
		Original:        raw,
		Username:        username,
		Hostname:        hostname,
		HasExplicitUser: hasUser,
	}, nil
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Stdin == nil {
		return errors.New("stdin is required")
	}
	stdinFD := int(cfg.Stdin.Fd())
	if cfg.Stdout == nil {
		return errors.New("stdout is required")
	}
	cols, rows, terminalErr := terminalSize(stdinFD)
	if terminalErr == nil {
		cfg.TerminalCols = cols
		if cfg.CallerSessionTarget != "" {
			// Commands launched inside a Meja pane inherit that pane's PTY,
			// which is already the drawable area below the outer status bar.
			// Subtracting another row creates a permanently undersized pane whose
			// render grid cannot satisfy the outer client's projection.
			cfg.TerminalRows = rows
		} else {
			cfg.TerminalRows = drawableRows(int(rows))
		}
	}

	result, err := executeCommand(ctx, cfg)
	if err != nil {
		return err
	}
	bootstrapResult, err := consumeCommandResult(cfg, result)
	if err != nil {
		return err
	}
	if bootstrapResult == nil {
		return nil
	}
	if terminalErr != nil {
		return terminalErr
	}
	bootstrap := *bootstrapResult
	hostname, err := resolveConnectionHostname(ctx, cfg)
	if err != nil {
		return err
	}

	clientCtx, cancelClient := context.WithCancelCause(ctx)
	defer cancelClient(nil)

	streamErrs := make(chan error, 32)
	renderLog := cfg.Stderr
	if cfg.RenderDiagnosticsLogPath != "" {
		f, err := os.Create(cfg.RenderDiagnosticsLogPath)
		if err != nil {
			return fmt.Errorf("open render diagnostics log: %w", err)
		}
		defer f.Close()
		renderLog = f
	}

	diagnosticsEnabled := cfg.RenderDiagnostics || cfg.RenderDiagnosticsLogPath != ""
	var diagnostics *renderDiagnostics
	if diagnosticsEnabled {
		diagnostics = newRenderDiagnostics(renderLog)
		defer diagnostics.close()
	}

	ui := &runtimeState{
		stdout:            cfg.Stdout,
		events:            make(chan renderEvent, 256),
		diagnostics:       diagnostics,
		renderDone:        make(chan struct{}),
		renderExitCommand: make(chan []byte, 1),
	}
	ui.dropConnectionEvents.Store(false)
	renderCtx, stopRender := context.WithCancel(context.Background())
	go ui.renderLoop(renderCtx, streamErrs)
	ui.emit(sizeEvent{cols: int(cols), rows: int(rows)})

	var rawState *term.State
	terminalActive := false
	var pendingFrontendInput []byte
	enterTerminal := func() error {
		if terminalActive {
			return nil
		}
		state, err := term.MakeRaw(int(cfg.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("set terminal raw mode: %w", err)
		}
		if err := ui.writeTerminal(clientCtx, []byte("\x1b[?1049h\x1b[H\x1b[2J")); err != nil {
			_ = term.Restore(int(cfg.Stdin.Fd()), state)
			return fmt.Errorf("enter alternate screen: %w", err)
		}
		supported, pending, probeErr := probeRectangularScroll(clientCtx, cfg.Stdin, ui)
		if probeErr != nil {
			_ = term.Restore(int(cfg.Stdin.Fd()), state)
			return fmt.Errorf("probe rectangular scrolling: %w", probeErr)
		}
		pendingFrontendInput = append(pendingFrontendInput, pending...)
		// Some terminals mishandle an unsupported DECRQM query by displaying
		// its final character. Clear the untouched startup row before content
		// rendering begins.
		if err := ui.writeTerminal(clientCtx, []byte("\x1b[H\x1b[2K")); err != nil {
			_ = term.Restore(int(cfg.Stdin.Fd()), state)
			return fmt.Errorf("clear capability probe residue: %w", err)
		}
		if supported {
			if err := ui.writeTerminal(clientCtx, []byte("\x1b[?69h")); err != nil {
				_ = term.Restore(int(cfg.Stdin.Fd()), state)
				return fmt.Errorf("enable rectangular scrolling: %w", err)
			}
		}
		if err := ui.setRectangularScroll(clientCtx, supported); err != nil {
			_ = term.Restore(int(cfg.Stdin.Fd()), state)
			return err
		}
		rawState = state
		terminalActive = true
		return nil
	}
	restoreTerminal := func() {
		fixedExit := fixedTerminalExit(cols)
		fallback := false
		if terminalActive {
			if err := ui.shutdownTerminal(context.Background(), fixedExit); err != nil {
				fallback = true
			}
		}
		stopRender()
		<-ui.renderDone
		pendingExit := <-ui.renderExitCommand
		if terminalActive && (fallback || len(pendingExit) > 0) {
			cleanup := append(append([]byte(nil), pendingExit...), fixedExit...)
			_ = writeAll(cfg.Stdout, cleanup)
		}
		if !terminalActive {
			return
		}
		_ = term.Restore(int(cfg.Stdin.Fd()), rawState)
		terminalActive = false
	}
	defer restoreTerminal()

	var control atomic.Pointer[controlDestination]

	ui.beginConnection(false, time.Now())
	live, err := openConnection(clientCtx, bootstrap, hostname, cols, rows, cfg, "", ui, enterTerminal)
	if err != nil {
		return err
	}
	control.Store(live.controlDestination())
	go forwardInputWithInitial(clientCtx, cfg.Stdin, &control, ui, streamErrs, cancelClient, pendingFrontendInput)
	go forwardResize(clientCtx, stdinFD, &control, ui, streamErrs)

	for {
		select {
		case result := <-live.done:
			ui.stopConnection()
			clearControlDestination(&control, live.controlFrames)
			// A clean server close means this frontend is leaving the
			// attachment. Disable its input-reporting modes immediately, before
			// waiting for transport workers, so key releases generated during
			// teardown cannot be inherited by the caller's shell. On success the
			// render loop clears the registered command; terminal shutdown will
			// therefore not execute it twice. On failure it remains available to
			// the shutdown fallback.
			if result.graceful {
				_ = ui.executeTerminalExitCommand(context.Background())
			}
			live.destroy()
			ui.sync(clientCtx)
			if result.graceful {
				if result.terminalMessage != "" {
					ui.emit(terminalStatusEvent{message: result.terminalMessage})
					ui.sync(clientCtx)
					_ = waitReconnect(clientCtx, 2*time.Second)
				}
				return nil
			}
			if clientCtx.Err() != nil {
				return clientExitError(clientCtx)
			}
			_ = ui.executeTerminalExitCommand(clientCtx)
			lastContact := live.lastContactTime()
			ui.markDisconnected(lastContact)
			resumeToken := live.resumeToken
			backoff := 100 * time.Millisecond
			for {
				if err := waitReconnect(clientCtx, backoff); err != nil {
					return clientExitError(clientCtx)
				}
				reconnectCols, reconnectRows, sizeErr := terminalSize(stdinFD)
				if sizeErr != nil {
					return sizeErr
				}
				ui.beginConnection(true, lastContact)
				candidate, reconnectErr := openConnection(clientCtx, bootstrap, hostname, reconnectCols, reconnectRows, cfg, resumeToken, ui, nil)
				if reconnectErr == nil {
					live = candidate
					ui.beginConnection(false, lastContact)
					control.Store(live.controlDestination())
					break
				}
				var terminalErr *terminalAttachError
				if errors.As(reconnectErr, &terminalErr) {
					ui.emit(terminalStatusEvent{message: terminalErr.Error()})
					ui.sync(clientCtx)
					_ = waitReconnect(clientCtx, 2*time.Second)
					return nil
				}
				if backoff < 2*time.Second {
					backoff *= 2
				}
			}
		case err := <-streamErrs:
			if err != nil {
				clearControlDestination(&control, live.controlFrames)
				live.destroy()
				return err
			}
		case <-clientCtx.Done():
			clearControlDestination(&control, live.controlFrames)
			live.destroy()
			return clientExitError(clientCtx)
		}
	}
}

func fixedTerminalExit(cols uint16) []byte {
	return []byte(fmt.Sprintf("\x1b[?1003;1006;1004;2004l\x1b[r\x1b[1;%ds\x1b[?69l\x1b[?25h\x1b[0m\x1b[?1049l", cols))
}

func clientExitError(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), errDisconnectedInterrupt) {
		return nil
	}
	return ctx.Err()
}

type controlDestination struct {
	frames chan<- protocol.Frame
	done   <-chan struct{}
}

func clearControlDestination(current *atomic.Pointer[controlDestination], frames chan<- protocol.Frame) {
	for {
		destination := current.Load()
		if destination == nil || destination.frames != frames || current.CompareAndSwap(destination, nil) {
			return
		}
	}
}

func sendCurrentControl(current *atomic.Pointer[controlDestination], frame protocol.Frame) error {
	destination := current.Load()
	if destination == nil {
		return nil // disconnected input is deliberately dropped
	}
	select {
	case destination.frames <- frame:
		return nil
	case <-destination.done:
		return nil
	}
}

func sendCurrentControlEncoded[T any](current *atomic.Pointer[controlDestination], msgType uint64, value T, encode func([]byte, T) ([]byte, error)) error {
	payload, err := encode(nil, value)
	if err != nil {
		return err
	}
	return sendCurrentControl(current, protocol.Frame{Type: msgType, Payload: payload})
}

func sendFrontendInput(destination *controlDestination, ui *runtimeState, layoutRevision protocol.ClientLayoutRevision, sourceIdle bool, data, prediction []byte) (bool, error) {
	if destination == nil {
		return false, nil
	}
	payload, err := protocol.EncodeFrontendInputBytes(nil, protocol.FrontendInputBytes{
		LayoutRevision: layoutRevision,
		SourceIdle:     sourceIdle,
		Data:           data,
	})
	if err != nil {
		return true, err
	}
	select {
	case destination.frames <- protocol.Frame{Type: protocol.MsgFrontendInputBytes, Payload: payload}:
		if len(prediction) > 0 {
			ui.emit(localInputEvent{data: append([]byte(nil), prediction...)})
		}
		return true, nil
	case <-destination.done:
		return true, nil
	}
}

type connectionResult struct {
	err             error
	graceful        bool
	terminalMessage string
}

type liveConnection struct {
	conn          quic.Connection
	controlFrames chan protocol.Frame
	cancel        context.CancelFunc
	ctx           context.Context
	done          chan connectionResult
	resumeToken   string
	lastContact   atomic.Int64
	workers       sync.WaitGroup
}

func (c *liveConnection) controlDestination() *controlDestination {
	return &controlDestination{frames: c.controlFrames, done: c.ctx.Done()}
}

func (c *liveConnection) noteContact() { c.lastContact.Store(time.Now().UnixNano()) }

func (c *liveConnection) lastContactTime() time.Time {
	return time.Unix(0, c.lastContact.Load())
}

func (c *liveConnection) start(worker func()) {
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		worker()
	}()
}

func (c *liveConnection) destroy() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.conn != nil {
		_ = c.conn.CloseWithError(0, "")
	}
	c.workers.Wait()
}

type terminalAttachError struct{ reason string }

func (e *terminalAttachError) Error() string { return e.reason }

func openConnection(ctx context.Context, bootstrap protocol.CommandBootstrap, hostname string, cols, rows uint16, cfg Config, resumeToken string, ui *runtimeState, prepareFrontend func() error) (*liveConnection, error) {
	tlsConfig, err := loadTLSConfig(bootstrap.CertSPKISHA256)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(hostname, fmt.Sprintf("%d", bootstrap.Port))
	conn, err := quic.DialAddr(ctx, addr, tlsConfig, &quic.Config{
		MaxIdleTimeout:        quicMaxIdleTimeout,
		KeepAlivePeriod:       quicKeepAlivePeriod,
		MaxIncomingUniStreams: int64(protocol.OutputStreamCount),
		InitialPacketSize:     protocol.QUICInitialPacketSize,
	})
	if err != nil {
		return nil, quicDialError(addr, err)
	}
	connCtx, cancel := context.WithCancel(ctx)
	live := &liveConnection{
		conn:          conn,
		controlFrames: make(chan protocol.Frame, 256),
		cancel:        cancel,
		ctx:           connCtx,
		done:          make(chan connectionResult, 1),
	}
	fail := func(err error) (*liveConnection, error) {
		_ = ui.executeTerminalExitCommand(context.Background())
		ui.stopConnection()
		live.destroy()
		ui.sync(ctx)
		return nil, err
	}
	live.noteContact()
	controlStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fail(fmt.Errorf("open control stream: %w", err))
	}
	errs := make(chan error, 8)
	live.start(func() { writeFrames(connCtx, controlStream, live.controlFrames, errs) })
	if resumeToken == "" {
		err = enqueueEncoded(live.controlFrames, protocol.MsgSessionAttach, protocol.SessionAttach{Token: bootstrap.AttachToken, Cols: cols, Rows: drawableRows(int(rows))}, protocol.EncodeSessionAttach)
	} else {
		err = enqueueEncoded(live.controlFrames, protocol.MsgClientResume, protocol.ClientResume{ResumeToken: resumeToken, Cols: cols, Rows: drawableRows(int(rows))}, protocol.EncodeClientResume)
	}
	if err != nil {
		return fail(err)
	}
	controlDecoder := protocol.NewDecoder(controlStream, protocol.DefaultMaxFrameSize)
	attachResult, err := controlDecoder.ReadFrame()
	if err != nil {
		return fail(fmt.Errorf("read session attachment result: %w", err))
	}
	switch attachResult.Type {
	case protocol.MsgSessionAttachOK:
		msg, decodeErr := protocol.DecodeSessionAttachOK(attachResult.Payload)
		if decodeErr != nil {
			return fail(decodeErr)
		}
		if msg.ResumeToken == "" {
			return fail(errors.New("invalid session attachment response"))
		}
		live.resumeToken = msg.ResumeToken
	case protocol.MsgClientResumeOK:
		_, decodeErr := protocol.DecodeClientResumeOK(attachResult.Payload)
		if decodeErr != nil {
			return fail(decodeErr)
		}
		live.resumeToken = resumeToken
	case protocol.MsgSessionAttachFailed:
		msg, decodeErr := protocol.DecodeSessionAttachFailed(attachResult.Payload)
		if decodeErr != nil {
			return fail(decodeErr)
		}
		return fail(&terminalAttachError{reason: msg.Reason})
	default:
		return fail(fmt.Errorf("unexpected session attachment result %d", attachResult.Type))
	}
	exitCommand, setup, err := readFrontendTerminalConfiguration(controlDecoder)
	if err != nil {
		return fail(err)
	}
	if prepareFrontend != nil {
		if err := prepareFrontend(); err != nil {
			return fail(err)
		}
	}
	if err := ui.registerTerminalExitCommand(ctx, exitCommand); err != nil {
		return fail(err)
	}
	if err := ui.writeTerminal(ctx, setup); err != nil {
		return fail(err)
	}
	outputReady := make(chan struct{})
	live.start(func() { acceptOutputStreams(connCtx, conn, ui, outputReady, errs, live.start, &live.lastContact) })
	select {
	case <-outputReady:
	case streamErr := <-errs:
		return fail(streamErr)
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	live.start(func() { controlLoop(controlDecoder, ui, live.controlFrames, live.done, &live.lastContact) })
	live.start(func() {
		for {
			select {
			case <-errs:
				// The control stream is the authoritative lifecycle signal.
			case <-connCtx.Done():
				return
			}
		}
	})
	return live, nil
}

func readFrontendTerminalConfiguration(decoder *protocol.Decoder) ([]byte, []byte, error) {
	exitCommandFrame, err := decoder.ReadFrame()
	if err != nil {
		return nil, nil, fmt.Errorf("read frontend terminal exit command: %w", err)
	}
	if exitCommandFrame.Type != protocol.MsgFrontendRegisterTerminalExitCommand {
		return nil, nil, fmt.Errorf("expected frontend terminal exit command, got message type %d", exitCommandFrame.Type)
	}
	exitCommand, err := protocol.DecodeFrontendRegisterTerminalExitCommand(exitCommandFrame.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("decode frontend terminal exit command: %w", err)
	}
	// Decoder payloads alias reusable storage. Take ownership before reading the
	// setup frame, which may otherwise overwrite the registered cleanup with the
	// beginning of the setup command.
	exitCommandData := append([]byte(nil), exitCommand.Data...)

	setupFrame, err := decoder.ReadFrame()
	if err != nil {
		return nil, nil, fmt.Errorf("read frontend terminal setup: %w", err)
	}
	if setupFrame.Type != protocol.MsgFrontendTerminalWrite {
		return nil, nil, fmt.Errorf("expected frontend terminal setup, got message type %d", setupFrame.Type)
	}
	setup, err := protocol.DecodeFrontendTerminalWrite(setupFrame.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("decode frontend terminal setup: %w", err)
	}
	return exitCommandData, append([]byte(nil), setup.Data...), nil
}

func quicDialError(addr string, err error) error {
	// QUIC encodes TLS alerts as CRYPTO_ERROR(0x100 + alert). TLS alert 120 is
	// no_application_protocol, which means the server accepted UDP/QUIC but not
	// the complete interactive profile selected through ALPN.
	const noApplicationProtocol = quic.TransportErrorCode(0x100 + 120)
	var transportError *quic.TransportError
	if errors.As(err, &transportError) && transportError.ErrorCode == noApplicationProtocol {
		return fmt.Errorf(
			"server at %s did not accept client QUIC profile %q; the remote meja binary and running server must use the same QUIC profile as this client. Compare %q locally with %q using the same remote transport options, check --remote-path, and restart the remote server: %w",
			addr, protocol.ALPN, "meja version --verbose", "meja server-version", err,
		)
	}
	var idleTimeout *quic.IdleTimeoutError
	if errors.As(err, &idleTimeout) {
		return fmt.Errorf("UDP %s is unreachable: %w", addr, err)
	}
	return fmt.Errorf("dial %s: %w", addr, err)
}

func waitReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func loadTLSConfig(spkiHash string) (*tls.Config, error) {
	want, err := hex.DecodeString(spkiHash)
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("invalid pinned certificate SPKI hash")
	}
	return &tls.Config{
		InsecureSkipVerify: true, // VerifyConnection below is the mandatory trust decision.
		NextProtos:         []string{protocol.ALPN},
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("server sent no certificate")
			}
			got := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if len(want) != len(got) || subtle.ConstantTimeCompare(want, got[:]) != 1 {
				return errors.New("server certificate SPKI pin mismatch")
			}
			return nil
		},
	}, nil
}

func acceptOutputStreams(ctx context.Context, conn quic.Connection, ui *runtimeState, outputReady chan<- struct{}, sessionDone chan<- error, start func(func()), lastContact *atomic.Int64) {
	seen := make(map[uint8]struct{}, int(protocol.OutputStreamCount))
	for i := 0; i < int(protocol.OutputStreamCount); i++ {
		stream, err := conn.AcceptUniStream(ctx)
		if err != nil {
			sendConnectionError(ctx, sessionDone, fmt.Errorf("accept output stream: %w", err))
			return
		}
		index, ok := protocol.OutputIndexFromStreamID(uint64(stream.StreamID()))
		if !ok {
			sendConnectionError(ctx, sessionDone, fmt.Errorf("unexpected output stream ID %d", stream.StreamID()))
			return
		}
		if _, duplicate := seen[index]; duplicate {
			sendConnectionError(ctx, sessionDone, fmt.Errorf("duplicate output stream index %d", index))
			return
		}
		seen[index] = struct{}{}
		slot := protocol.StatusRenderSlot
		if index > 0 {
			slot = index - 1
		}
		start(func() {
			readOutputStream(slot, protocol.NewDisplayDecoder(stream), ui, sessionDone, conn.Context(), lastContact)
		})
	}
	select {
	case outputReady <- struct{}{}:
	case <-ctx.Done():
	}
}

func readOutputStream(slot uint8, decoder *protocol.DisplayDecoder, ui *runtimeState, sessionDone chan<- error, connectionContext context.Context, lastContact *atomic.Int64) {
	state := displayFrameCompiler{
		slot:          slot,
		styles:        defaultStyles(),
		cursorVisible: true,
	}
	if slot == protocol.StatusRenderSlot {
		state.cols, state.rows = int(protocol.MaxGridCols), 1
	}
	for {
		command, wireBytes, err := decoder.ReadCommand()
		if err != nil {
			connectionClosed := connectionContext != nil && connectionContext.Err() != nil
			if errors.Is(err, io.EOF) || isTerminalQUICClose(err) {
				return
			}
			if connectionClosed && errors.Is(err, io.ErrUnexpectedEOF) {
				return
			}
			sendConnectionError(connectionContext, sessionDone, fmt.Errorf("read display stream on slot %d: %w", slot, err))
			return
		}
		if lastContact != nil {
			lastContact.Store(time.Now().UnixNano())
		}
		if ui.diagnostics != nil {
			ui.diagnostics.reportCommand(slot, command, wireBytes)
		}
		if command.Opcode == protocol.DisplayOpcodeNoop {
			continue
		}
		frameReady, err := state.apply(command)
		if err != nil {
			sendConnectionError(connectionContext, sessionDone, err)
			return
		}
		if frameReady {
			ui.emit(paneFrameEvent{slot: slot, frame: state.frame})
		}
	}
}

type displayFrameCompiler struct {
	slot           uint8
	layoutRevision protocol.ClientLayoutRevision
	hasBarrier     bool
	cols, rows     int
	row, column    int
	styleID        uint32
	styles         map[uint32]protocol.Style
	cursor         protocol.Cursor
	cursorVisible  bool
	cursorUpdated  bool
	frame          renderFrame
	frameReady     bool
	paintStarted   bool
}

func (c *displayFrameCompiler) apply(command protocol.DisplayCommand) (bool, error) {
	if c.frameReady {
		c.frame = renderFrame{layoutRevision: c.layoutRevision, cols: c.cols, rows: c.rows}
		c.frameReady = false
		c.cursorUpdated = false
	}
	if command.Opcode == protocol.DisplayOpcodeStartRender {
		if c.slot == protocol.StatusRenderSlot {
			return false, errors.New("START_RENDER on status output")
		}
		if command.GridCols <= 0 || command.GridRows <= 0 || uint64(command.GridCols) > protocol.MaxGridCols || uint64(command.GridRows) > protocol.MaxGridRows {
			return false, fmt.Errorf("invalid display grid %dx%d on slot %d", command.GridCols, command.GridRows, c.slot)
		}
		c.layoutRevision = command.LayoutRevision
		c.hasBarrier = true
		c.cols, c.rows = command.GridCols, command.GridRows
		c.row, c.column = 0, 0
		c.styleID = protocol.CanonicalDefaultStyleID
		c.styles = defaultStyles()
		c.frame = renderFrame{layoutRevision: c.layoutRevision, cols: c.cols, rows: c.rows}
		c.paintStarted = false
		return false, nil
	}
	if !c.hasBarrier && c.slot != protocol.StatusRenderSlot {
		return false, fmt.Errorf("display command 0x%02x on slot %d before START_RENDER", byte(command.Opcode), c.slot)
	}

	switch command.Opcode {
	case protocol.DisplayOpcodeStyleInstall:
		if command.StyleID == protocol.CanonicalDefaultStyleID && !protocol.IsCanonicalDefaultStyle(command.Style) {
			return false, fmt.Errorf("invalid canonical default style on slot %d", c.slot)
		}
		if installed, ok := c.styles[command.StyleID]; ok && installed != command.Style {
			return false, fmt.Errorf("style %d redefined on slot %d", command.StyleID, c.slot)
		}
		c.styles[command.StyleID] = command.Style
		c.frame.styleInstalls = append(c.frame.styleInstalls, protocol.StyleDefinition{ID: command.StyleID, Style: command.Style})
	case protocol.DisplayOpcodeSetWritePosition:
		if command.Row < 0 || command.Row >= c.rows || command.Column < 0 || command.Column >= c.cols {
			return false, fmt.Errorf("write position %d,%d outside %dx%d grid on slot %d", command.Row, command.Column, c.cols, c.rows, c.slot)
		}
		c.row, c.column = command.Row, command.Column
	case protocol.DisplayOpcodeSetWriteStyle:
		if _, ok := c.styles[command.StyleID]; !ok {
			return false, fmt.Errorf("undefined style %d on slot %d", command.StyleID, c.slot)
		}
		c.styleID = command.StyleID
	case protocol.DisplayOpcodeWriteText, protocol.DisplayOpcodeWriteTextUTF8, protocol.DisplayOpcodeWriteTextUTF8Default:
		width := command.Width
		styleID := c.styleID
		if command.Opcode != protocol.DisplayOpcodeWriteText {
			width = 1
		}
		if command.Opcode == protocol.DisplayOpcodeWriteTextUTF8Default {
			styleID = protocol.CanonicalDefaultStyleID
		}
		if err := c.appendText(command.Text, width, styleID); err != nil {
			return false, err
		}
		c.paintStarted = true
	case protocol.DisplayOpcodeWriteCluster:
		if len(command.Text) == 0 || (command.Width != 1 && command.Width != 2) {
			return false, fmt.Errorf("invalid display cluster on slot %d", c.slot)
		}
		if err := c.requireCell(int(command.Width)); err != nil {
			return false, err
		}
		c.frame.spans = append(c.frame.spans, paintSpan{
			kind: paintCluster, row: c.row, column: c.column,
			styleID: c.styleID, cellWidth: command.Width, text: command.Text,
		})
		c.advance(int(command.Width))
		c.paintStarted = true
	case protocol.DisplayOpcodeFill:
		if err := c.appendFill(command.Fill); err != nil {
			return false, err
		}
		c.paintStarted = true
	case protocol.DisplayOpcodeCursorUpdate:
		c.cursor = command.Cursor.Cursor
		c.cursorVisible = command.Cursor.Visible
		c.cursorUpdated = true
	case protocol.DisplayOpcodeScroll:
		if c.paintStarted {
			return false, fmt.Errorf("SCROLL after paint on slot %d", c.slot)
		}
		if command.Delta != 0 {
			if c.frame.scrollDelta != 0 {
				return false, fmt.Errorf("multiple SCROLL commands in one frame on slot %d", c.slot)
			}
			c.frame.scrollDelta = command.Delta
		}
	case protocol.DisplayOpcodePresent:
		c.frame.layoutRevision = c.layoutRevision
		c.frame.cursor = c.cursor
		c.frame.cursorVisible = c.cursorVisible
		c.frame.cursorUpdated = c.cursorUpdated
		c.frameReady = true
		c.paintStarted = false
		return true, nil
	default:
		return false, fmt.Errorf("unexpected display opcode 0x%02x on slot %d", byte(command.Opcode), c.slot)
	}
	return false, nil
}

func (c *displayFrameCompiler) requireCell(width int) error {
	if width <= 0 || c.row < 0 || c.row >= c.rows || c.column < 0 || c.column+width > c.cols {
		return fmt.Errorf("write at %d,%d width %d outside %dx%d grid on slot %d", c.row, c.column, width, c.cols, c.rows, c.slot)
	}
	return nil
}

func (c *displayFrameCompiler) advance(columns int) {
	c.column += columns
	if c.column == c.cols {
		c.row++
		c.column = 0
	}
}

func (c *displayFrameCompiler) appendText(text []byte, width uint8, styleID uint32) error {
	segmentStart := 0
	segmentRow, segmentColumn := c.row, c.column
	for offset := 0; offset < len(text); {
		if err := c.requireCell(int(width)); err != nil {
			return err
		}
		_, size := utf8.DecodeRune(text[offset:])
		offset += size
		previousRow := c.row
		c.advance(int(width))
		if c.row != previousRow {
			c.frame.spans = append(c.frame.spans, paintSpan{kind: paintText, row: segmentRow, column: segmentColumn, styleID: styleID, cellWidth: width, text: text[segmentStart:offset]})
			segmentStart = offset
			segmentRow, segmentColumn = c.row, c.column
		}
	}
	if segmentStart < len(text) {
		c.frame.spans = append(c.frame.spans, paintSpan{kind: paintText, row: segmentRow, column: segmentColumn, styleID: styleID, cellWidth: width, text: text[segmentStart:]})
	}
	return nil
}

func (c *displayFrameCompiler) appendFill(fill protocol.Fill) error {
	width := int(fill.Width)
	if (width != 1 && width != 2) || fill.Columns <= 0 || fill.Columns%width != 0 {
		return fmt.Errorf("invalid fill width %d for %d columns on slot %d", width, fill.Columns, c.slot)
	}
	for remaining := fill.Columns; remaining > 0; {
		if err := c.requireCell(width); err != nil {
			return err
		}
		columns := min(remaining, c.cols-c.column)
		if columns%width != 0 {
			return fmt.Errorf("fill splits a width-%d cell at row %d on slot %d", width, c.row, c.slot)
		}
		c.frame.spans = append(c.frame.spans, paintSpan{kind: paintFill, row: c.row, column: c.column, styleID: c.styleID, cellWidth: fill.Width, fillRune: fill.Rune, fillColumns: columns})
		c.advance(columns)
		remaining -= columns
	}
	return nil
}

func controlLoop(decoder *protocol.Decoder, ui *runtimeState, controlFrames chan<- protocol.Frame, done chan<- connectionResult, lastContact *atomic.Int64) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if isTerminalQUICClose(err) {
				var applicationErr *quic.ApplicationError
				_ = errors.As(err, &applicationErr)
				message := ""
				if applicationErr != nil && applicationErr.ErrorCode == protocol.SessionReplacedErrorCode {
					message = applicationErr.ErrorMessage
				}
				done <- connectionResult{graceful: true, terminalMessage: message}
				return
			}
			if errors.Is(err, io.EOF) {
				done <- connectionResult{}
				return
			}
			done <- connectionResult{err: fmt.Errorf("read control frame: %w", err)}
			return
		}
		if lastContact != nil {
			lastContact.Store(time.Now().UnixNano())
		}
		switch frame.Type {
		case protocol.MsgClientLayout:
			msg, err := protocol.DecodeClientLayout(frame.Payload)
			if err != nil {
				done <- connectionResult{err: fmt.Errorf("decode CLIENT_LAYOUT: %w", err)}
				return
			}
			ui.emit(layoutEvent{layout: msg})
		case protocol.MsgFrontendTerminalWrite:
			msg, err := protocol.DecodeFrontendTerminalWrite(frame.Payload)
			if err != nil {
				done <- connectionResult{err: fmt.Errorf("decode FRONTEND_TERMINAL_WRITE: %w", err)}
				return
			}
			if err := ui.writeTerminal(context.Background(), msg.Data); err != nil {
				done <- connectionResult{err: err}
				return
			}
		case protocol.MsgFrontendRegisterTerminalExitCommand:
			msg, err := protocol.DecodeFrontendRegisterTerminalExitCommand(frame.Payload)
			if err != nil {
				done <- connectionResult{err: fmt.Errorf("decode FRONTEND_REGISTER_TERMINAL_EXIT_COMMAND: %w", err)}
				return
			}
			if err := ui.registerTerminalExitCommand(context.Background(), msg.Data); err != nil {
				done <- connectionResult{err: err}
				return
			}
		case protocol.MsgFrontendExecuteTerminalExitCommand:
			if len(frame.Payload) != 0 {
				done <- connectionResult{err: errors.New("frontend terminal exit request has a payload")}
				return
			}
			if err := ui.executeTerminalExitCommand(context.Background()); err != nil {
				done <- connectionResult{err: err}
				return
			}
			if controlFrames == nil {
				done <- connectionResult{err: errors.New("frontend terminal exit request has no control writer")}
				return
			}
			controlFrames <- protocol.Frame{Type: protocol.MsgFrontendTerminalExitComplete}
		default:
		}
	}
}

func isTerminalQUICClose(err error) bool {
	var applicationErr *quic.ApplicationError
	return errors.As(err, &applicationErr) &&
		(applicationErr.ErrorCode == 0 || applicationErr.ErrorCode == protocol.SessionReplacedErrorCode)
}

func forwardInputWithInitial(ctx context.Context, stdin *os.File, control *atomic.Pointer[controlDestination], ui *runtimeState, errs chan<- error, cancel context.CancelCauseFunc, initial []byte) {
	reads := make(chan terminalInputRead, 16)
	if len(initial) > 0 {
		reads <- terminalInputRead{data: append([]byte(nil), initial...)}
	}
	go readTerminalInput(ctx, stdin, reads)
	forwardInputReads(ctx, reads, control, ui, errs, cancel, frontendEscapeDelay)
}

const rectangularScrollProbeTimeout = 250 * time.Millisecond

// probeRectangularScroll asks the frontend whether DEC private mode 69 is
// recognized. It runs before the normal stdin reader starts so the response is
// consumed locally rather than forwarded to a pane. Any unrelated bytes read
// alongside the response are returned to the normal input path.
func probeRectangularScroll(ctx context.Context, stdin *os.File, ui *runtimeState) (bool, []byte, error) {
	if err := ui.writeTerminal(ctx, []byte("\x1b[?69$p")); err != nil {
		return false, nil, err
	}

	deadline := time.Now().Add(rectangularScrollProbeTimeout)
	var data []byte
	for {
		if err := ctx.Err(); err != nil {
			return false, data, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timeout := int((remaining + time.Millisecond - 1) / time.Millisecond)
		if timeout < 1 {
			timeout = 1
		}
		pollfds := []unix.PollFd{{Fd: int32(stdin.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(pollfds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			break
		}
		if pollfds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
			continue
		}

		buf := make([]byte, 4096)
		count, readErr := stdin.Read(buf)
		if count > 0 {
			data = append(data, buf[:count]...)
			if supported, found, remaining := takeRectangularScrollReply(data); found {
				return supported, remaining, nil
			}
		}
		if readErr != nil {
			break
		}
	}
	return false, data, nil
}

func takeRectangularScrollReply(data []byte) (supported, found bool, remaining []byte) {
	const prefix = "\x1b[?69;"
	start := bytes.Index(data, []byte(prefix))
	if start < 0 {
		return false, false, nil
	}
	statusStart := start + len(prefix)
	endOffset := bytes.Index(data[statusStart:], []byte("$y"))
	if endOffset < 0 {
		return false, false, nil
	}
	status, err := strconv.Atoi(string(data[statusStart : statusStart+endOffset]))
	if err != nil {
		return false, false, nil
	}
	end := statusStart + endOffset + len("$y")
	remaining = append(append([]byte(nil), data[:start]...), data[end:]...)
	return status >= 1 && status <= 3, true, remaining
}

type terminalInputRead struct {
	data []byte
	err  error
}

func readTerminalInput(ctx context.Context, stdin *os.File, reads chan<- terminalInputRead) {
	defer close(reads)
	buf := make([]byte, 4096)
	for {
		n, err := stdin.Read(buf)
		if n == 0 && err == nil {
			continue
		}
		read := terminalInputRead{err: err}
		if n > 0 {
			read.data = append([]byte(nil), buf[:n]...)
		}
		select {
		case reads <- read:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func forwardInputReads(ctx context.Context, reads <-chan terminalInputRead, control *atomic.Pointer[controlDestination], ui *runtimeState, errs chan<- error, cancel context.CancelCauseFunc, escapeDelay time.Duration) {
	var predictionDecoder predictionInputDecoder
	var predictionDestination *controlDestination
	var pendingDestination *controlDestination
	var pendingRevision protocol.ClientLayoutRevision
	var escapeTimer *time.Timer
	var escapeTimerC <-chan time.Time
	stopEscapeTimer := func() {
		if escapeTimer != nil && !escapeTimer.Stop() {
			select {
			case <-escapeTimer.C:
			default:
			}
		}
		escapeTimerC = nil
	}
	armEscapeTimer := func() {
		stopEscapeTimer()
		if escapeTimer == nil {
			escapeTimer = time.NewTimer(escapeDelay)
		} else {
			escapeTimer.Reset(escapeDelay)
		}
		escapeTimerC = escapeTimer.C
	}
	defer stopEscapeTimer()

	sendBytes := func(destination *controlDestination, revision protocol.ClientLayoutRevision, sourceIdle bool, data []byte) (bool, error) {
		prediction := predictionDecoder.Feed(data)
		if sourceIdle {
			prediction = append(prediction, predictionDecoder.FlushLoneEscape()...)
		}
		return sendFrontendInput(destination, ui, revision, sourceIdle, data, prediction)
	}
	flushPendingEscape := func() error {
		stopEscapeTimer()
		destination, revision := pendingDestination, pendingRevision
		pendingDestination = nil
		pendingRevision = 0
		if destination == nil || control.Load() != destination {
			predictionDecoder.reset()
			predictionDestination = nil
			return nil
		}
		_, err := sendBytes(destination, revision, true, []byte{0x1b})
		return err
	}
	handleRead := func(data []byte) error {
		destination := control.Load()
		if destination == nil {
			pendingDestination = nil
			pendingRevision = 0
			stopEscapeTimer()
			predictionDecoder.reset()
			predictionDestination = nil
			if bytes.IndexByte(data, 0x03) >= 0 {
				cancel(errDisconnectedInterrupt)
			}
			return nil
		}
		if predictionDestination != destination {
			predictionDecoder.reset()
			predictionDestination = destination
		}
		stopEscapeTimer()
		revision := protocol.ClientLayoutRevision(ui.appliedLayoutRevision.Load())
		if pendingDestination != nil {
			if pendingDestination == destination {
				data = append([]byte{0x1b}, data...)
				revision = pendingRevision
			} else {
				predictionDecoder.reset()
				predictionDestination = destination
			}
		}
		pendingDestination = nil
		pendingRevision = 0

		if len(data) > 0 && data[len(data)-1] == 0x1b {
			if len(data) > 1 {
				sent, err := sendBytes(destination, revision, false, data[:len(data)-1])
				if err != nil || !sent {
					return err
				}
			}
			pendingDestination = destination
			pendingRevision = revision
			armEscapeTimer()
			return nil
		}
		_, err := sendBytes(destination, revision, false, data)
		return err
	}
	report := func(err error) {
		if err == nil || ctx.Err() != nil {
			return
		}
		select {
		case errs <- err:
		case <-ctx.Done():
		}
	}
	handleReadResult := func(read terminalInputRead, ok bool) bool {
		if !ok {
			report(flushPendingEscape())
			return true
		}
		if len(read.data) > 0 {
			if err := handleRead(read.data); err != nil {
				report(err)
				return true
			}
		}
		if read.err == nil {
			return false
		}
		report(flushPendingEscape())
		if !errors.Is(read.err, io.EOF) {
			report(fmt.Errorf("read stdin: %w", read.err))
		}
		return true
	}

	for {
		select {
		case read, ok := <-reads:
			if handleReadResult(read, ok) {
				return
			}
		case <-escapeTimerC:
			escapeTimerC = nil
			// Prefer bytes already read from the local TTY over an idle timeout
			// whose delivery happened to win the scheduler race.
			select {
			case read, ok := <-reads:
				if handleReadResult(read, ok) {
					return
				}
			default:
				if err := flushPendingEscape(); err != nil {
					report(err)
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

const resizeReadDelay = 16 * time.Millisecond

func forwardResize(ctx context.Context, ttyFD int, control *atomic.Pointer[controlDestination], ui *runtimeState, errs chan<- error) {
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, syscall.SIGWINCH)
	defer signal.Stop(sigch)
	forwardResizeSignals(ctx, ttyFD, control, ui, errs, sigch, resizeReadDelay)
}

func forwardResizeSignals(ctx context.Context, ttyFD int, control *atomic.Pointer[controlDestination], ui *runtimeState, errs chan<- error, sigch <-chan os.Signal, delay time.Duration) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var readSize <-chan time.Time
	var lastCols, lastRows uint16
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigch:
			if readSize == nil {
				timer.Reset(delay)
				readSize = timer.C
			}
		case <-readSize:
			readSize = nil
			cols, rows, err := terminalSize(ttyFD)
			if err != nil {
				errs <- err
				return
			}
			if cols == lastCols && rows == lastRows {
				continue
			}
			lastCols, lastRows = cols, rows
			ui.emit(sizeEvent{cols: int(cols), rows: int(rows)})
			if sendErr := sendCurrentControlEncoded(control, protocol.MsgFrontendResize, protocol.FrontendResize{
				Cols: cols,
				Rows: drawableRows(int(rows)),
			}, protocol.EncodeFrontendResize); sendErr != nil {
				errs <- sendErr
				return
			}
		}
	}
}

func terminalSize(fd int) (uint16, uint16, error) {
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return 0, 0, fmt.Errorf("get terminal size: %w", err)
	}
	return uint16(cols), uint16(rows), nil
}

func writeFrames(ctx context.Context, stream io.Writer, frames <-chan protocol.Frame, errs chan<- error) {
	enc := protocol.NewEncoder(stream)
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-frames:
			if err := enc.WriteFrame(frame); err != nil {
				sendConnectionError(ctx, errs, fmt.Errorf("write frame type %d: %w", frame.Type, err))
				return
			}
		}
	}
}

func sendConnectionError(ctx context.Context, errs chan<- error, err error) {
	if ctx == nil {
		errs <- err
		return
	}
	select {
	case errs <- err:
	case <-ctx.Done():
	}
}

func enqueueEncoded[T any](ch chan<- protocol.Frame, msgType uint64, v T, encode func([]byte, T) ([]byte, error)) error {
	payload, err := encode(nil, v)
	if err != nil {
		return err
	}
	defer func() { recover() }()
	ch <- protocol.Frame{Type: msgType, Payload: payload}
	return nil
}

func (r *runtimeState) beginConnection(reconnecting bool, lastContact time.Time) {
	r.dropConnectionEvents.Store(false)
	r.emit(reconnectEvent{reconnecting: reconnecting, lastContact: lastContact})
}

func (r *runtimeState) stopConnection() {
	r.dropConnectionEvents.Store(true)
}

func (r *runtimeState) markDisconnected(lastContact time.Time) {
	r.emit(reconnectEvent{reconnecting: true, lastContact: lastContact})
}

type scanoutState struct {
	cols, rows        int
	layout            protocol.ClientLayout
	pendingLayouts    map[protocol.ClientLayoutRevision]protocol.ClientLayout
	pendingFrames     map[protocol.ClientLayoutRevision]map[uint8][]renderFrame
	styles            map[uint8]map[uint32]protocol.Style
	cursors           map[uint8]protocol.CursorUpdate
	ansi              bytes.Buffer
	reconnecting      bool
	lastContact       time.Time
	rectangularScroll bool
	caches            map[uint8]*paneScanoutCache
	predictor         inputPredictor
	activeCursor      physicalCursor
}

type physicalCursor struct {
	row, column int
	visible     bool
	valid       bool
}

// scanoutCell is the client-only decoded representation. It is bounded by the
// visible pane grid and deliberately separate from the server's packed store.
type scanoutCell struct {
	Cluster string
	StyleID uint32
	Width   uint8
}

type paneScanoutCache struct {
	cols, rows, head int
	cells            []scanoutCell
}

func newPaneScanoutCache(cols, rows int) *paneScanoutCache {
	cells := make([]scanoutCell, max(0, cols*rows))
	fillBlank(cells)
	return &paneScanoutCache{cols: cols, rows: rows, cells: cells}
}

func (p *paneScanoutCache) row(row int) []scanoutCell {
	physical := (p.head + row) % p.rows
	return p.cells[physical*p.cols : (physical+1)*p.cols]
}

func (p *paneScanoutCache) scroll(delta int) {
	if p.rows == 0 || delta == 0 {
		return
	}
	if delta <= -p.rows || delta >= p.rows {
		fillBlank(p.cells)
		p.head = 0
		return
	}
	if delta < 0 {
		for i := 0; i < -delta; i++ {
			p.head = (p.head + 1) % p.rows
			fillBlank(p.row(p.rows - 1))
		}
		return
	}
	for i := 0; i < delta; i++ {
		p.head = (p.head + p.rows - 1) % p.rows
		fillBlank(p.row(0))
	}
}

func newScanoutState(rectangularScroll bool) *scanoutState {
	return &scanoutState{rectangularScroll: rectangularScroll, caches: make(map[uint8]*paneScanoutCache), pendingLayouts: make(map[protocol.ClientLayoutRevision]protocol.ClientLayout), pendingFrames: make(map[protocol.ClientLayoutRevision]map[uint8][]renderFrame), styles: make(map[uint8]map[uint32]protocol.Style), cursors: make(map[uint8]protocol.CursorUpdate)}
}

func (s *scanoutState) takeANSI() []byte {
	out := append([]byte(nil), s.ansi.Bytes()...)
	s.ansi.Reset()
	return out
}

func (s *scanoutState) acceptLayout(layout protocol.ClientLayout) (bool, error) {
	if layout.LayoutRevision < s.layout.LayoutRevision {
		return false, nil
	}
	if layout.LayoutRevision == s.layout.LayoutRevision && s.layout.LayoutRevision != 0 {
		if !sameClientLayoutGeometry(s.layout, layout) {
			return false, errors.New("client layout with same revision changed geometry")
		}
		focusChanged := layout.FocusedPaneID != s.layout.FocusedPaneID
		if focusChanged {
			if _, err := s.resetPrediction(); err != nil {
				return false, err
			}
		}
		s.layout = layout
		if focusChanged {
			s.emitLayoutBorders(layout)
		}
		s.selectAuthoritativeCursor()
		s.emitActiveCursor()
		return true, nil
	}
	s.pendingLayouts[layout.LayoutRevision] = layout
	return s.tryActivate(layout.LayoutRevision)
}

func sameClientLayoutGeometry(left, right protocol.ClientLayout) bool {
	if left.WindowID != right.WindowID || len(left.Panes) != len(right.Panes) {
		return false
	}
	for index := range left.Panes {
		if left.Panes[index] != right.Panes[index] {
			return false
		}
	}
	return true
}

func (s *scanoutState) acceptFrame(slot uint8, frame renderFrame) (bool, error) {
	if slot == protocol.StatusRenderSlot {
		return true, s.emitFrame(slot, protocol.Rect{Y: max(0, s.rows-1), Width: s.cols, Height: 1}, frame)
	}
	if frame.layoutRevision < s.layout.LayoutRevision {
		return false, nil
	}
	if frame.layoutRevision == s.layout.LayoutRevision && s.layout.LayoutRevision != 0 {
		placement, ok := placementForSlot(s.layout, slot)
		if !ok {
			return false, fmt.Errorf("frame for unbound slot %d", slot)
		}
		if !paneFrameMatchesRect(frame, placement.Rect) {
			return false, nil
		}
		return true, s.emitFrame(slot, placement.Rect, frame)
	}
	if layout, ok := s.pendingLayouts[frame.layoutRevision]; ok {
		if placement, found := placementForSlot(layout, slot); found && !paneFrameMatchesRect(frame, placement.Rect) {
			return false, nil
		}
	}
	bySlot := s.pendingFrames[frame.layoutRevision]
	if bySlot == nil {
		bySlot = make(map[uint8][]renderFrame)
		s.pendingFrames[frame.layoutRevision] = bySlot
	}
	bySlot[slot] = append(bySlot[slot], frame)
	return s.tryActivate(frame.layoutRevision)
}

func (s *scanoutState) tryActivate(revision protocol.ClientLayoutRevision) (bool, error) {
	layout, ok := s.pendingLayouts[revision]
	if !ok {
		return false, nil
	}
	frames := s.pendingFrames[revision]
	for _, placement := range layout.Panes {
		matching := false
		for _, frame := range frames[placement.Slot] {
			if paneFrameMatchesRect(frame, placement.Rect) {
				matching = true
				break
			}
		}
		if !matching {
			return false, nil
		}
	}
	s.predictor.clear()
	s.activeCursor = physicalCursor{}
	s.layout = layout
	statusStyles := s.styles[protocol.StatusRenderSlot]
	s.styles = make(map[uint8]map[uint32]protocol.Style)
	if statusStyles != nil {
		s.styles[protocol.StatusRenderSlot] = statusStyles
	}
	s.cursors = make(map[uint8]protocol.CursorUpdate)
	s.caches = make(map[uint8]*paneScanoutCache)
	for _, placement := range layout.Panes {
		s.caches[placement.Slot] = newPaneScanoutCache(placement.Rect.Width, placement.Rect.Height)
	}
	s.clearContentRows()
	s.emitLayoutBorders(layout)
	for _, placement := range layout.Panes {
		for _, frame := range frames[placement.Slot] {
			if !paneFrameMatchesRect(frame, placement.Rect) {
				continue
			}
			if err := s.emitFrame(placement.Slot, placement.Rect, frame); err != nil {
				return false, err
			}
		}
	}
	for rev := range s.pendingLayouts {
		if rev <= revision {
			delete(s.pendingLayouts, rev)
		}
	}
	for rev := range s.pendingFrames {
		if rev <= revision {
			delete(s.pendingFrames, rev)
		}
	}
	return true, nil
}

func (s *scanoutState) predictionContext() (predictionContext, *paneScanoutCache, protocol.Rect, bool) {
	for _, placement := range s.layout.Panes {
		if placement.PaneID != s.layout.FocusedPaneID {
			continue
		}
		cursor, ok := s.cursors[placement.Slot]
		cache := s.caches[placement.Slot]
		if !ok || cache == nil {
			return predictionContext{}, nil, protocol.Rect{}, false
		}
		return predictionContext{
			target: predictionTarget{paneID: placement.PaneID, slot: placement.Slot, layoutRevision: s.layout.LayoutRevision},
			cursor: cursor.Cursor, cursorVisible: cursor.Visible,
			width: placement.Rect.Width, height: placement.Rect.Height,
		}, cache, placement.Rect, true
	}
	return predictionContext{}, nil, protocol.Rect{}, false
}

func (s *scanoutState) acceptLocalInput(data []byte) (bool, error) {
	context, cache, rect, ok := s.predictionContext()
	if !ok {
		return false, nil
	}
	result, changed := s.predictor.applyLocalInput(data, context, cache)
	if !changed {
		return false, nil
	}
	styles := s.styles[context.target.slot]
	if styles == nil {
		styles = defaultStyles()
		s.styles[context.target.slot] = styles
	}
	if len(result.frame.spans) > 0 {
		s.ansi.WriteString("\x1b[?25l")
		if err := s.emitSpans(context.target.slot, rect, result.frame.spans, styles); err != nil {
			return false, err
		}
	}
	if result.hasCursorOverride {
		s.setActiveCursor(rect, result.cursorOverride)
	} else {
		s.setActiveCursor(rect, s.cursors[context.target.slot])
	}
	s.emitActiveCursor()
	return true, nil
}

func (s *scanoutState) resetPrediction() (bool, error) {
	target := s.predictor.target
	if target == (predictionTarget{}) {
		s.predictor.clear()
		return false, nil
	}
	placement, ok := placementForSlot(s.layout, target.slot)
	cache := s.caches[target.slot]
	if !ok || placement.PaneID != target.paneID || cache == nil {
		s.predictor.clear()
		return false, nil
	}
	frame, changed := s.predictor.reset(s.layout.LayoutRevision, cache)
	if !changed {
		return false, nil
	}
	styles := s.styles[target.slot]
	if styles == nil {
		styles = defaultStyles()
		s.styles[target.slot] = styles
	}
	s.ansi.WriteString("\x1b[?25l")
	if err := s.emitSpans(target.slot, placement.Rect, frame.spans, styles); err != nil {
		return false, err
	}
	if s.isFocusedSlot(target.slot) {
		s.setActiveCursor(placement.Rect, s.cursors[target.slot])
	}
	s.emitActiveCursor()
	return true, nil
}

func (s *scanoutState) clearContentRows() {
	s.ansi.WriteString("\x1b[?25l")
	s.ansi.WriteString(sgrForStyle(protocol.CanonicalDefaultStyle()))
	for row := 0; row < max(0, s.rows-1); row++ {
		writeCursorPosition(&s.ansi, row+1, 1)
		s.ansi.WriteString("\x1b[2K")
	}
}

func (s *scanoutState) emitLayoutBorders(layout protocol.ClientLayout) {
	inactiveStyle := protocol.CanonicalDefaultStyle()
	activeStyle := inactiveStyle
	activeStyle.FG = theme.AccentColor()
	focusedBorders := focusedPaneBorderCells(layout.Panes, layout.FocusedPaneID)
	active := false
	s.ansi.WriteString(sgrForStyle(inactiveStyle))
	for row := 0; row < max(0, s.rows-1); row++ {
		for column := 0; column < s.cols; column++ {
			r := paneBorderRune(layout.Panes, column, row)
			if r == 0 {
				continue
			}
			_, focused := focusedBorders[borderPoint{column: column, row: row}]
			if focused != active {
				if focused {
					s.ansi.WriteString(sgrForStyle(activeStyle))
				} else {
					s.ansi.WriteString(sgrForStyle(inactiveStyle))
				}
				active = focused
			}
			writeCursorPosition(&s.ansi, row+1, column+1)
			s.ansi.WriteRune(r)
		}
	}
}

func placementForSlot(layout protocol.ClientLayout, slot uint8) (protocol.PanePlacement, bool) {
	for _, placement := range layout.Panes {
		if placement.Slot == slot {
			return placement, true
		}
	}
	return protocol.PanePlacement{}, false
}

func paneFrameMatchesRect(frame renderFrame, rect protocol.Rect) bool {
	// Older tests and pre-grid render streams use zero as unspecified. A
	// START_RENDER frame with explicit dimensions, however, belongs only to the
	// projection whose placement has exactly that grid.
	return frame.cols == 0 || (frame.cols == rect.Width && frame.rows == rect.Height)
}

func (s *scanoutState) emitFrame(slot uint8, rect protocol.Rect, frame renderFrame) error {
	if slot != protocol.StatusRenderSlot && !paneFrameMatchesRect(frame, rect) {
		return fmt.Errorf("pane slot %d frame grid %dx%d does not match layout %dx%d", slot, frame.cols, frame.rows, rect.Width, rect.Height)
	}
	styles := s.styles[slot]
	if styles == nil {
		styles = defaultStyles()
		s.styles[slot] = styles
	}
	for _, def := range frame.styleInstalls {
		styles[def.ID] = def.Style
	}
	cache := s.caches[slot]
	evidence := frameEvidence{touched: make(map[cellPosition]authoritativeCellChange), cursorUpdated: frame.cursorUpdated, scrolled: frame.scrollDelta != 0}
	if cache != nil {
		cache.scroll(frame.scrollDelta)
		for _, span := range frame.spans {
			if err := applySpanToCache(cache, span, &evidence); err != nil {
				return err
			}
		}
	}
	s.cursors[slot] = protocol.CursorUpdate{Cursor: frame.cursor, Visible: frame.cursorVisible}
	result := predictionResult{frame: frame}
	if cache != nil {
		if placement, ok := placementForSlot(s.layout, slot); ok {
			result = s.predictor.applyAuthoritativeFrame(predictionTarget{paneID: placement.PaneID, slot: slot, layoutRevision: frame.layoutRevision}, frame, evidence, cache)
		}
	}
	display := result.frame
	s.ansi.WriteString("\x1b[?25l")
	// The OS terminal can already have its new width before this render loop
	// consumes the corresponding sizeEvent. All Meja painting is absolutely
	// positioned, so wrapping is never useful: suppress it for the complete
	// frame to keep stale wide pane or status spans from spilling into adjacent
	// rows. Status span clipping additionally handles frames processed after
	// the sizeEvent has installed the new geometry.
	s.ansi.WriteString("\x1b[?7l")
	defer s.ansi.WriteString("\x1b[?7h")
	fullPaneEmitted := false
	fullWidth := rect.X == 0 && rect.Width == s.cols
	nativeScroll := !result.repaintPane && (fullWidth || s.rectangularScroll)
	if delta := display.scrollDelta; delta != 0 {
		if nativeScroll {
			// Vertical margins are sufficient when the pane spans the full
			// terminal width. Non-full-width panes additionally require
			// DECLRMM/DECSLRM, which is gated by rectangularScroll.
			fmt.Fprintf(&s.ansi, "\x1b[%d;%dr", rect.Y+1, rect.Y+rect.Height)
			if !fullWidth {
				fmt.Fprintf(&s.ansi, "\x1b[%d;%ds", rect.X+1, rect.X+rect.Width)
			}
			fmt.Fprintf(&s.ansi, "\x1b[%d;%dH", rect.Y+1, rect.X+1)
			if delta < 0 {
				fmt.Fprintf(&s.ansi, "\x1b[%dS", -delta)
			} else {
				fmt.Fprintf(&s.ansi, "\x1b[%dT", delta)
			}
			fmt.Fprintf(&s.ansi, "\x1b[r")
			if !fullWidth {
				fmt.Fprintf(&s.ansi, "\x1b[1;%ds", s.cols)
			}
		}
	}
	if cache != nil && display.scrollDelta != 0 && !nativeScroll {
		if err := s.emitCachedPane(rect, cache, styles); err != nil {
			return err
		}
		fullPaneEmitted = true
	}
	if !fullPaneEmitted {
		if err := s.emitSpans(slot, rect, display.spans, styles); err != nil {
			return err
		}
	}
	if s.isFocusedSlot(slot) {
		cursor := s.cursors[slot]
		if result.hasCursorOverride {
			cursor = result.cursorOverride
		}
		s.setActiveCursor(rect, cursor)
	}
	s.emitActiveCursor()
	return nil
}

func (s *scanoutState) emitSpans(slot uint8, rect protocol.Rect, spans []paintSpan, styles map[uint32]protocol.Style) error {
	for _, span := range spans {
		if slot == protocol.StatusRenderSlot {
			var visible bool
			span, visible = clipStatusSpan(span, rect.Width, rect.Height)
			if !visible {
				continue
			}
		}
		style, ok := styles[span.styleID]
		if !ok {
			return fmt.Errorf("undefined style %d on slot %d", span.styleID, slot)
		}
		writeCursorPosition(&s.ansi, rect.Y+span.row+1, rect.X+span.column+1)
		s.ansi.WriteString(sgrForStyle(style))
		if span.kind == paintText || span.kind == paintCluster {
			s.ansi.Write(span.text)
		} else {
			for columns := 0; columns < span.fillColumns; columns += int(span.cellWidth) {
				s.ansi.WriteRune(span.fillRune)
			}
		}
	}
	return nil
}

// Status frames are barrierless and can race terminal resize events. Clip a
// frame produced for an older, wider terminal to the current one-row status
// rectangle so excess cells cannot wrap and scroll pane content upward.
func clipStatusSpan(span paintSpan, width, height int) (paintSpan, bool) {
	if width <= 0 || height <= 0 || span.row < 0 || span.row >= height || span.column < 0 || span.column >= width {
		return paintSpan{}, false
	}
	cellWidth := int(span.cellWidth)
	if cellWidth <= 0 {
		return paintSpan{}, false
	}
	available := width - span.column
	switch span.kind {
	case paintFill:
		span.fillColumns = min(span.fillColumns, available)
		span.fillColumns -= span.fillColumns % cellWidth
		return span, span.fillColumns > 0
	case paintCluster:
		return span, cellWidth <= available
	case paintText:
		maxRunes := available / cellWidth
		if maxRunes <= 0 {
			return paintSpan{}, false
		}
		runes := []rune(string(span.text))
		if len(runes) > maxRunes {
			span.text = []byte(string(runes[:maxRunes]))
		}
		return span, len(span.text) > 0
	default:
		return paintSpan{}, false
	}
}

func applySpanToCache(cache *paneScanoutCache, span paintSpan, evidence *frameEvidence) error {
	if span.row < 0 || span.row >= cache.rows || span.column < 0 || span.column >= cache.cols {
		return errors.New("paint span outside pane cache")
	}
	row, column := cache.row(span.row), span.column
	record := func(column int, cell scanoutCell) {
		position := cellPosition{row: span.row, column: column}
		change, exists := evidence.touched[position]
		if !exists {
			change.before = row[column]
		}
		row[column] = cell
		change.after = cell
		evidence.touched[position] = change
	}
	clearOccupant := func(column int) {
		if column < 0 || column >= len(row) {
			return
		}
		anchor := column
		if row[column].Width == 0 && column > 0 && row[column-1].Width == 2 {
			anchor = column - 1
		}
		styleID := row[anchor].StyleID
		width := row[anchor].Width
		record(anchor, scanoutCell{StyleID: styleID, Width: 1})
		if width == 2 && anchor+1 < len(row) {
			record(anchor+1, scanoutCell{StyleID: styleID, Width: 1})
		}
	}
	write := func(cluster string, width uint8) error {
		if cluster == " " && width == 1 {
			cluster = ""
		}
		if column+int(width) > len(row) {
			return errors.New("paint span exceeds pane cache")
		}
		clearOccupant(column)
		if width == 2 {
			clearOccupant(column + 1)
		}
		record(column, scanoutCell{Cluster: cluster, StyleID: span.styleID, Width: width})
		for n := 1; n < int(width); n++ {
			record(column+n, scanoutCell{StyleID: span.styleID, Width: 0})
		}
		column += int(width)
		return nil
	}
	if span.kind == paintCluster {
		return write(string(span.text), span.cellWidth)
	}
	if span.kind == paintText {
		for _, r := range string(span.text) {
			if err := write(string(r), span.cellWidth); err != nil {
				return err
			}
		}
		return nil
	}
	for columns := 0; columns < span.fillColumns; columns += int(span.cellWidth) {
		cluster := string(span.fillRune)
		if span.fillRune == ' ' {
			cluster = ""
		}
		if err := write(cluster, span.cellWidth); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanoutState) emitCachedPane(rect protocol.Rect, cache *paneScanoutCache, styles map[uint32]protocol.Style) error {
	for row := 0; row < cache.rows; row++ {
		writeCursorPosition(&s.ansi, rect.Y+row+1, rect.X+1)
		var currentStyle uint32
		hasStyle := false
		for _, cell := range cache.row(row) {
			if cell.Width == 0 {
				continue
			}
			style, ok := styles[cell.StyleID]
			if !ok {
				return fmt.Errorf("undefined cached style %d", cell.StyleID)
			}
			if !hasStyle || currentStyle != cell.StyleID {
				s.ansi.WriteString(sgrForStyle(style))
				currentStyle, hasStyle = cell.StyleID, true
			}
			if cell.Cluster == "" {
				s.ansi.WriteByte(' ')
			} else {
				s.ansi.WriteString(cell.Cluster)
			}
		}
	}
	return nil
}

func (s *scanoutState) isFocusedSlot(slot uint8) bool {
	for _, placement := range s.layout.Panes {
		if placement.Slot == slot {
			return placement.PaneID == s.layout.FocusedPaneID
		}
	}
	return false
}

func (s *scanoutState) setActiveCursor(rect protocol.Rect, cursor protocol.CursorUpdate) {
	s.activeCursor = physicalCursor{
		row: rect.Y + cursor.Cursor.Y + 1, column: rect.X + cursor.Cursor.X + 1,
		visible: cursor.Visible, valid: true,
	}
}

func (s *scanoutState) selectAuthoritativeCursor() {
	for _, placement := range s.layout.Panes {
		if placement.PaneID == s.layout.FocusedPaneID {
			s.setActiveCursor(placement.Rect, s.cursors[placement.Slot])
			return
		}
	}
	s.activeCursor = physicalCursor{}
}

func (s *scanoutState) emitActiveCursor() {
	if !s.activeCursor.valid {
		s.ansi.WriteString("\x1b[?25l")
		return
	}
	writeCursorPosition(&s.ansi, s.activeCursor.row, s.activeCursor.column)
	if s.activeCursor.visible {
		s.ansi.WriteString("\x1b[?25h")
	} else {
		s.ansi.WriteString("\x1b[?25l")
	}
}

func (s *scanoutState) setReconnecting(reconnecting bool, lastContact, now time.Time) {
	s.reconnecting, s.lastContact = reconnecting, lastContact
	if !reconnecting || s.cols <= 0 {
		return
	}
	seconds := max(0, int(now.Sub(lastContact)/time.Second))
	message := []rune(" Reconnecting... Press Ctrl+C to exit [Last contact " + strconv.Itoa(seconds) + " seconds ago]")
	if len(message) > s.cols {
		message = message[:s.cols]
	}
	writeCursorPosition(&s.ansi, max(1, s.rows), 1)
	// Keep the reconnect indicator readable as a full-width orange bar.
	s.ansi.WriteString(sgrForStyle(protocol.Style{
		FG: protocol.Color{Mode: "indexed", Index: 0},
		BG: protocol.Color{Mode: "rgb", R: 255, G: 165, B: 0},
	}))
	s.ansi.WriteString(string(message))
	for i := len(message); i < s.cols; i++ {
		s.ansi.WriteByte(' ')
	}
	s.emitActiveCursor()
}

func (s *scanoutState) setTerminalStatus(message string) {
	s.reconnecting = false
	if s.cols <= 0 {
		return
	}
	text := []rune(" " + message)
	if len(text) > s.cols {
		text = text[:s.cols]
	}
	writeCursorPosition(&s.ansi, max(1, s.rows), 1)
	s.ansi.WriteString(sgrForStyle(protocol.Style{
		FG: protocol.Color{Mode: "indexed", Index: 15},
		BG: protocol.Color{Mode: "indexed", Index: 1},
	}))
	s.ansi.WriteString(string(text))
	for i := len(text); i < s.cols; i++ {
		s.ansi.WriteByte(' ')
	}
	s.emitActiveCursor()
}

func (r *runtimeState) renderLoop(ctx context.Context, errs chan<- error) {
	var terminalExitCommand []byte
	if r.renderDone != nil {
		defer close(r.renderDone)
	}
	defer func() {
		if r.renderExitCommand != nil {
			r.renderExitCommand <- append([]byte(nil), terminalExitCommand...)
		}
	}()
	state := newScanoutState(false)
	present := func(reason string) error {
		buf := state.takeANSI()
		if r.diagnostics != nil {
			r.diagnostics.reportRedraw(reason, len(buf))
		}
		if len(buf) == 0 {
			return nil
		}
		if err := writeAll(r.stdout, buf); err != nil {
			return err
		}
		if state.layout.LayoutRevision != 0 {
			r.appliedLayoutRevision.Store(uint64(state.layout.LayoutRevision))
		}
		return nil
	}
	handleEvent := func(event renderEvent) (bool, string, error) {
		needsPresent := false
		reason := ""
		var err error
		switch e := event.(type) {
		case reconnectEvent:
			_, err = state.resetPrediction()
			if err != nil {
				return false, "", err
			}
			state.setReconnecting(e.reconnecting, e.lastContact, time.Now())
			needsPresent = true
			reason = "reconnect state"
		case terminalStatusEvent:
			_, err = state.resetPrediction()
			if err != nil {
				return false, "", err
			}
			state.setTerminalStatus(e.message)
			needsPresent = true
			reason = "terminal status"
		case sizeEvent:
			if state.cols != 0 && (state.cols != e.cols || state.rows != e.rows) {
				needed, resetErr := state.resetPrediction()
				needsPresent = needed
				err = resetErr
			}
			state.cols, state.rows = e.cols, e.rows
			reason = "terminal-size"
		case localInputEvent:
			if r.dropConnectionEvents.Load() {
				return false, "", nil
			}
			needsPresent, err = state.acceptLocalInput(e.data)
			reason = "local-input"
		case terminalWriteEvent:
			if presentErr := present("before terminal control write"); presentErr != nil {
				e.done <- presentErr
				return false, "", presentErr
			}
			err = writeAll(r.stdout, e.data)
			e.done <- err
		case terminalExitCommandEvent:
			terminalExitCommand = append(terminalExitCommand[:0], e.data...)
			close(e.done)
		case terminalExitEvent:
			if presentErr := present("before terminal exit command"); presentErr != nil {
				e.done <- presentErr
				return false, "", presentErr
			}
			exitCommand := append([]byte(nil), terminalExitCommand...)
			err = writeAll(r.stdout, exitCommand)
			if err == nil {
				terminalExitCommand = terminalExitCommand[:0]
			}
			e.done <- err
		case terminalShutdownEvent:
			if presentErr := present("before terminal shutdown"); presentErr != nil {
				e.done <- presentErr
				return false, "", presentErr
			}
			exitCommand := append([]byte(nil), terminalExitCommand...)
			err = writeAll(r.stdout, exitCommand)
			if err == nil {
				terminalExitCommand = terminalExitCommand[:0]
				err = writeAll(r.stdout, e.data)
			}
			e.done <- err
			if err == nil {
				return false, "", errRenderShutdown
			}
		case rectangularScrollEvent:
			state.rectangularScroll = e.enabled
			close(e.done)
		case layoutEvent:
			if r.dropConnectionEvents.Load() {
				return false, "", nil
			}
			needsPresent, err = state.acceptLayout(e.layout)
			if r.diagnostics != nil {
				r.diagnostics.reportProjection(fmt.Sprintf(
					"layout received window=%d revision=%d panes=%d activated=%t current=%d pending_layouts=%d pending_frame_revisions=%d",
					e.layout.WindowID, e.layout.LayoutRevision, len(e.layout.Panes), needsPresent,
					state.layout.LayoutRevision, len(state.pendingLayouts), len(state.pendingFrames)))
			}
			reason = "client-layout"
		case paneFrameEvent:
			if r.dropConnectionEvents.Load() {
				return false, "", nil
			}
			needsPresent, err = state.acceptFrame(e.slot, e.frame)
			if r.diagnostics != nil {
				r.diagnostics.reportProjection(fmt.Sprintf(
					"frame received slot=%d revision=%d grid=%dx%d activated=%t current=%d pending_layouts=%d pending_frame_revisions=%d",
					e.slot, e.frame.layoutRevision, e.frame.cols, e.frame.rows, needsPresent,
					state.layout.LayoutRevision, len(state.pendingLayouts), len(state.pendingFrames)))
			}
			reason = fmt.Sprintf("present slot=%d", e.slot)
		case renderBarrierEvent:
			close(e.done)
		}
		return needsPresent, reason, err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if state.reconnecting {
				state.setReconnecting(true, state.lastContact, time.Now())
				if err := present("reconnect timer"); err != nil {
					if ctx.Err() == nil {
						errs <- err
					}
					return
				}
			}
		case event := <-r.events:
			needsPresent, reason, err := handleEvent(event)
			draining := true
			for draining && err == nil {
				select {
				case next := <-r.events:
					needed, nextReason, nextErr := handleEvent(next)
					needsPresent = needsPresent || needed
					if nextReason != "" {
						reason = nextReason
					}
					err = nextErr
				default:
					draining = false
				}
			}
			if errors.Is(err, errRenderShutdown) {
				return
			}
			if err == nil && needsPresent {
				err = present(reason)
			}
			if err != nil {
				if ctx.Err() == nil {
					errs <- err
				}
				return
			}
		}
	}
}

func (r *runtimeState) emit(event renderEvent) { r.events <- event }

func (r *runtimeState) writeTerminal(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	done := make(chan error, 1)
	event := terminalWriteEvent{data: append([]byte(nil), data...), done: done}
	select {
	case r.events <- event:
	case <-r.renderDone:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-r.renderDone:
		select {
		case err := <-done:
			return err
		default:
			return io.ErrClosedPipe
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtimeState) registerTerminalExitCommand(ctx context.Context, data []byte) error {
	done := make(chan struct{})
	event := terminalExitCommandEvent{data: append([]byte(nil), data...), done: done}
	select {
	case r.events <- event:
	case <-r.renderDone:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-r.renderDone:
		select {
		case <-done:
			return nil
		default:
			return io.ErrClosedPipe
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtimeState) executeTerminalExitCommand(ctx context.Context) error {
	done := make(chan error, 1)
	event := terminalExitEvent{done: done}
	select {
	case r.events <- event:
	case <-r.renderDone:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-r.renderDone:
		select {
		case err := <-done:
			return err
		default:
			return io.ErrClosedPipe
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtimeState) shutdownTerminal(ctx context.Context, data []byte) error {
	done := make(chan error, 1)
	event := terminalShutdownEvent{data: append([]byte(nil), data...), done: done}
	select {
	case r.events <- event:
	case <-r.renderDone:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-r.renderDone:
		select {
		case err := <-done:
			return err
		default:
			return io.ErrClosedPipe
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtimeState) sync(ctx context.Context) {
	done := make(chan struct{})
	select {
	case r.events <- renderBarrierEvent{done: done}:
	case <-r.renderDone:
		return
	case <-ctx.Done():
		return
	}
	select {
	case <-done:
	case <-r.renderDone:
	case <-ctx.Done():
	}
}

func (r *runtimeState) setRectangularScroll(ctx context.Context, enabled bool) error {
	done := make(chan struct{})
	select {
	case r.events <- rectangularScrollEvent{enabled: enabled, done: done}:
	case <-r.renderDone:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-r.renderDone:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func drawableRows(rows int) uint16 {
	if rows <= 1 {
		return 1
	}
	return uint16(rows - 1)
}

func defaultStyles() map[uint32]protocol.Style {
	return map[uint32]protocol.Style{protocol.CanonicalDefaultStyleID: protocol.CanonicalDefaultStyle()}
}

func fillBlank(cells []scanoutCell) {
	for i := range cells {
		cells[i] = scanoutCell{Width: 1}
	}
}

func writeCursorPosition(buf *bytes.Buffer, row, col int) {
	buf.WriteString("\x1b[")
	buf.WriteString(strconv.Itoa(row))
	buf.WriteByte(';')
	buf.WriteString(strconv.Itoa(col))
	buf.WriteByte('H')
}

func paneBorderRune(placements []protocol.PanePlacement, column, row int) rune {
	if !isPaneWireCell(placements, column, row) {
		return 0
	}
	up := isPaneWireCell(placements, column, row-1)
	down := isPaneWireCell(placements, column, row+1)
	left := isPaneWireCell(placements, column-1, row)
	right := isPaneWireCell(placements, column+1, row)
	switch {
	case up && down && left && right:
		return '┼'
	case up && down && right:
		return '├'
	case up && down && left:
		return '┤'
	case left && right && down:
		return '┬'
	case left && right && up:
		return '┴'
	case down && right:
		return '┌'
	case down && left:
		return '┐'
	case up && right:
		return '└'
	case up && left:
		return '┘'
	case up || down:
		return '│'
	case left || right:
		return '─'
	case paneAt(placements, column-1, row) && paneAt(placements, column+1, row):
		return '│'
	case paneAt(placements, column, row-1) && paneAt(placements, column, row+1):
		return '─'
	default:
		return 0
	}
}

func isPaneWireCell(placements []protocol.PanePlacement, column, row int) bool {
	if len(placements) == 0 {
		return false
	}
	minX, minY := placements[0].Rect.X, placements[0].Rect.Y
	maxX := placements[0].Rect.X + placements[0].Rect.Width
	maxY := placements[0].Rect.Y + placements[0].Rect.Height
	for _, placement := range placements[1:] {
		minX = min(minX, placement.Rect.X)
		minY = min(minY, placement.Rect.Y)
		maxX = max(maxX, placement.Rect.X+placement.Rect.Width)
		maxY = max(maxY, placement.Rect.Y+placement.Rect.Height)
	}
	return column >= minX && column < maxX && row >= minY && row < maxY &&
		!paneAt(placements, column, row)
}

func paneAt(placements []protocol.PanePlacement, column, row int) bool {
	for _, placement := range placements {
		rect := placement.Rect
		if column >= rect.X && column < rect.X+rect.Width &&
			row >= rect.Y && row < rect.Y+rect.Height {
			return true
		}
	}
	return false
}

type borderPoint struct {
	column int
	row    int
}

func focusedPaneBorderCells(placements []protocol.PanePlacement, focusedPaneID uint64) map[borderPoint]struct{} {
	borders := paneBorderCells(placements)
	focused := borders[focusedPaneID]
	if len(focused) == 0 {
		return nil
	}
	for paneID, candidate := range borders {
		if paneID == focusedPaneID || !sameBorderCells(focused, candidate) {
			continue
		}
		return splitAmbiguousBorder(placements, focusedPaneID, paneID, focused)
	}
	return focused
}

func paneBorderCells(placements []protocol.PanePlacement) map[uint64]map[borderPoint]struct{} {
	borders := make(map[uint64]map[borderPoint]struct{}, len(placements))
	add := func(paneID uint64, point borderPoint) {
		if borders[paneID] == nil {
			borders[paneID] = make(map[borderPoint]struct{})
		}
		borders[paneID][point] = struct{}{}
	}
	for i := range placements {
		for j := i + 1; j < len(placements); j++ {
			first, second := placements[i], placements[j]
			if second.Rect.X+second.Rect.Width == first.Rect.X-1 {
				first, second = second, first
			}
			if first.Rect.X+first.Rect.Width == second.Rect.X-1 {
				start := max(first.Rect.Y, second.Rect.Y)
				end := min(first.Rect.Y+first.Rect.Height, second.Rect.Y+second.Rect.Height)
				for row := start; row < end; row++ {
					point := borderPoint{column: first.Rect.X + first.Rect.Width, row: row}
					add(first.PaneID, point)
					add(second.PaneID, point)
				}
			}

			first, second = placements[i], placements[j]
			if second.Rect.Y+second.Rect.Height == first.Rect.Y-1 {
				first, second = second, first
			}
			if first.Rect.Y+first.Rect.Height == second.Rect.Y-1 {
				start := max(first.Rect.X, second.Rect.X)
				end := min(first.Rect.X+first.Rect.Width, second.Rect.X+second.Rect.Width)
				for column := start; column < end; column++ {
					point := borderPoint{column: column, row: first.Rect.Y + first.Rect.Height}
					add(first.PaneID, point)
					add(second.PaneID, point)
				}
			}
		}
	}
	for row := 0; row < layoutBottom(placements); row++ {
		for column := 0; column < layoutRight(placements); column++ {
			point := borderPoint{column: column, row: row}
			if paneBorderRune(placements, column, row) == 0 || borderClaimed(borders, point) {
				continue
			}
			for paneID, cells := range borders {
				if adjacentBorderClaimed(cells, point) {
					add(paneID, point)
				}
			}
		}
	}
	return borders
}

func layoutRight(placements []protocol.PanePlacement) int {
	right := 0
	for _, placement := range placements {
		right = max(right, placement.Rect.X+placement.Rect.Width)
	}
	return right
}

func layoutBottom(placements []protocol.PanePlacement) int {
	bottom := 0
	for _, placement := range placements {
		bottom = max(bottom, placement.Rect.Y+placement.Rect.Height)
	}
	return bottom
}

func borderClaimed(borders map[uint64]map[borderPoint]struct{}, point borderPoint) bool {
	for _, cells := range borders {
		if _, ok := cells[point]; ok {
			return true
		}
	}
	return false
}

func adjacentBorderClaimed(cells map[borderPoint]struct{}, point borderPoint) bool {
	for _, adjacent := range [...]borderPoint{
		{column: point.column - 1, row: point.row},
		{column: point.column + 1, row: point.row},
		{column: point.column, row: point.row - 1},
		{column: point.column, row: point.row + 1},
	} {
		if _, ok := cells[adjacent]; ok {
			return true
		}
	}
	return false
}

func sameBorderCells(first, second map[borderPoint]struct{}) bool {
	if len(first) != len(second) {
		return false
	}
	for point := range first {
		if _, ok := second[point]; !ok {
			return false
		}
	}
	return true
}

func splitAmbiguousBorder(placements []protocol.PanePlacement, focusedPaneID, otherPaneID uint64, border map[borderPoint]struct{}) map[borderPoint]struct{} {
	var focused, other *protocol.PanePlacement
	for i := range placements {
		switch placements[i].PaneID {
		case focusedPaneID:
			focused = &placements[i]
		case otherPaneID:
			other = &placements[i]
		}
	}
	if focused == nil || other == nil {
		return border
	}
	vertical := focused.Rect.X != other.Rect.X
	firstHalf := focused.Rect.X < other.Rect.X
	if !vertical {
		firstHalf = focused.Rect.Y < other.Rect.Y
	}
	ordered := make([]borderPoint, 0, len(border))
	for point := range border {
		ordered = append(ordered, point)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if vertical {
			if ordered[i].row != ordered[j].row {
				return ordered[i].row < ordered[j].row
			}
			return ordered[i].column < ordered[j].column
		}
		if ordered[i].column != ordered[j].column {
			return ordered[i].column < ordered[j].column
		}
		return ordered[i].row < ordered[j].row
	})
	result := make(map[borderPoint]struct{}, (len(ordered)+1)/2)
	for index, point := range ordered {
		if (firstHalf && index*2 < len(ordered)) ||
			(!firstHalf && (index+1)*2 > len(ordered)) {
			result[point] = struct{}{}
		}
	}
	return result
}

func sgrForStyle(style protocol.Style) string {
	codes := []string{"0"}
	if style.Bold {
		codes = append(codes, "1")
	}
	if style.Dim {
		codes = append(codes, "2")
	}
	if style.Blink {
		codes = append(codes, "5")
	}
	if style.Italic {
		codes = append(codes, "3")
	}
	if style.Underline {
		codes = append(codes, "4")
	}
	if style.Reverse {
		codes = append(codes, "7")
	}
	if style.Invisible {
		codes = append(codes, "8")
	}
	codes = append(codes, colorCodes(style.FG, true)...)
	codes = append(codes, colorCodes(style.BG, false)...)
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

func colorCodes(c protocol.Color, fg bool) []string {
	switch c.Mode {
	case "indexed":
		if c.Index < 8 {
			if fg {
				return []string{strconv.Itoa(30 + int(c.Index))}
			}
			return []string{strconv.Itoa(40 + int(c.Index))}
		}
		if c.Index < 16 {
			if fg {
				return []string{strconv.Itoa(90 + int(c.Index-8))}
			}
			return []string{strconv.Itoa(100 + int(c.Index-8))}
		}
		prefix := "48"
		if fg {
			prefix = "38"
		}
		return []string{prefix, "5", strconv.Itoa(int(c.Index))}
	case "rgb":
		prefix := "48"
		if fg {
			prefix = "38"
		}
		return []string{prefix, "2", strconv.Itoa(int(c.R)), strconv.Itoa(int(c.G)), strconv.Itoa(int(c.B))}
	default:
		if fg {
			return []string{"39"}
		}
		return []string{"49"}
	}
}
