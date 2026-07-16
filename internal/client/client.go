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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/term"

	"github.com/garindra/meja/internal/control"
	"github.com/garindra/meja/internal/protocol"
)

const (
	quicMaxIdleTimeout  = 6 * time.Second
	quicKeepAlivePeriod = 2 * time.Second
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
	Argv                     []string
	RemotePath               string
	SocketSelector           control.SocketSelector
	SessionID                uint64
	SessionTarget            string
	SessionName              string
	Stdin                    *os.File
	Stdout                   io.Writer
	Stderr                   io.Writer
}

type runtimeState struct {
	stdout               io.Writer
	events               chan renderEvent
	diagnostics          *renderDiagnostics
	renderDone           chan struct{}
	dropConnectionEvents atomic.Bool
	rectangularScroll    bool
}

type renderEvent any
type paintKind uint8

const (
	paintText paintKind = iota + 1
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
	layoutRevision uint64
	styleInstalls  []protocol.StyleDefinition
	scrollDeltas   []int
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
type inputPredictionResetEvent struct{}
type layoutEvent struct{ layout protocol.WindowLayout }
type sizeEvent struct{ cols, rows int }
type reconnectEvent struct {
	reconnecting bool
	lastContact  time.Time
}
type renderBarrierEvent struct{ done chan struct{} }

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
	if cfg.Stdout == nil {
		return errors.New("stdout is required")
	}
	cols, rows, err := terminalSize(cfg.Stdin)
	if err != nil {
		return err
	}

	// Keep the user's normal terminal active for initial SSH diagnostics,
	// authentication prompts, and host-key handling. Reconnect bootstrap occurs
	// later inside the already-active Meja display.
	bootstrap, err := fetchBootstrap(ctx, cfg)
	if err != nil {
		return err
	}
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
		rectangularScroll: true,
	}
	ui.dropConnectionEvents.Store(false)
	go ui.renderLoop(clientCtx, streamErrs)
	ui.emit(sizeEvent{cols: int(cols), rows: int(rows)})

	var terminalMu sync.Mutex
	var rawState *term.State
	terminalActive := false
	enterTerminal := func() error {
		terminalMu.Lock()
		defer terminalMu.Unlock()
		if terminalActive {
			return nil
		}
		state, err := term.MakeRaw(int(cfg.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("set terminal raw mode: %w", err)
		}
		if _, err := io.WriteString(cfg.Stdout, "\x1b[?1049h\x1b[?69h\x1b[H\x1b[2J"); err != nil {
			_ = term.Restore(int(cfg.Stdin.Fd()), state)
			return fmt.Errorf("enter alternate screen: %w", err)
		}
		rawState = state
		terminalActive = true
		return nil
	}
	restoreTerminal := func() {
		terminalMu.Lock()
		defer terminalMu.Unlock()
		if !terminalActive {
			return
		}
		_, _ = fmt.Fprintf(cfg.Stdout, "\x1b[r\x1b[1;%ds\x1b[?69l\x1b[?25h\x1b[0m\x1b[?1049l", cols)
		_ = term.Restore(int(cfg.Stdin.Fd()), rawState)
		terminalActive = false
	}
	defer restoreTerminal()

	restoreSignals := make(chan os.Signal, 1)
	signal.Notify(restoreSignals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(restoreSignals)
	go func() {
		select {
		case <-ctx.Done():
		case <-restoreSignals:
			restoreTerminal()
		}
	}()

	var input atomic.Pointer[inputDestination]

	ui.beginConnection(false, time.Now())
	live, err := openConnection(clientCtx, bootstrap, hostname, cols, rows, cfg, "", 0, ui, enterTerminal)
	if err != nil {
		return err
	}
	input.Store(live.inputDestination())
	go forwardInput(clientCtx, cfg.Stdin, &input, ui, streamErrs, cancelClient)
	go forwardResize(clientCtx, cfg.Stdin, &input, ui, streamErrs)

	for {
		select {
		case result := <-live.done:
			ui.stopConnection()
			clearInputDestination(&input, live.inputFrames)
			live.destroy()
			ui.sync(clientCtx)
			if result.graceful {
				return nil
			}
			if clientCtx.Err() != nil {
				return clientExitError(clientCtx)
			}
			lastContact := live.lastContactTime()
			ui.markDisconnected(lastContact)
			resumeToken, generation := live.resumeToken, live.generation
			backoff := 100 * time.Millisecond
			for {
				if err := waitReconnect(clientCtx, backoff); err != nil {
					return clientExitError(clientCtx)
				}
				ui.beginConnection(true, lastContact)
				candidate, reconnectErr := openConnection(clientCtx, bootstrap, hostname, cols, rows, cfg, resumeToken, generation, ui, nil)
				if reconnectErr != nil {
					fallbackCfg := cfg
					fallbackCfg.SessionID = bootstrap.SessionID
					fallbackCfg.SessionTarget = fmt.Sprintf("%d", bootstrap.SessionID)
					fallbackCfg.Stderr = io.Discard
					fallback, fallbackErr := fetchBootstrap(clientCtx, fallbackCfg)
					if fallbackErr == nil {
						fallbackHost, hostErr := resolveConnectionHostname(clientCtx, cfg)
						if hostErr == nil {
							ui.beginConnection(true, lastContact)
							candidate, reconnectErr = openConnection(clientCtx, fallback, fallbackHost, cols, rows, cfg, "", 0, ui, nil)
							if reconnectErr == nil {
								bootstrap, hostname = fallback, fallbackHost
							}
						}
					}
				}
				if reconnectErr == nil {
					live = candidate
					ui.beginConnection(false, lastContact)
					input.Store(live.inputDestination())
					break
				}
				if backoff < 2*time.Second {
					backoff *= 2
				}
			}
		case err := <-streamErrs:
			if err != nil {
				clearInputDestination(&input, live.inputFrames)
				live.destroy()
				return err
			}
		case <-clientCtx.Done():
			clearInputDestination(&input, live.inputFrames)
			live.destroy()
			return clientExitError(clientCtx)
		}
	}
}

func clientExitError(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), errDisconnectedInterrupt) {
		return nil
	}
	return ctx.Err()
}

type inputDestination struct {
	frames chan<- protocol.Frame
	done   <-chan struct{}
}

func clearInputDestination(current *atomic.Pointer[inputDestination], frames chan<- protocol.Frame) {
	for {
		destination := current.Load()
		if destination == nil || destination.frames != frames || current.CompareAndSwap(destination, nil) {
			return
		}
	}
}

func sendCurrentInput(current *atomic.Pointer[inputDestination], frame protocol.Frame) error {
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

func sendCurrentInputEncoded[T any](current *atomic.Pointer[inputDestination], msgType uint64, value T, encode func([]byte, T) ([]byte, error)) error {
	payload, err := encode(nil, value)
	if err != nil {
		return err
	}
	return sendCurrentInput(current, protocol.Frame{Type: msgType, Payload: payload})
}

func sendCurrentPredictedInput(current *atomic.Pointer[inputDestination], ui *runtimeState, data []byte) (bool, error) {
	destination := current.Load()
	if destination == nil {
		return false, nil
	}
	payload, err := protocol.EncodeInputBytes(nil, protocol.InputBytes{Data: data})
	if err != nil {
		return true, err
	}
	ui.emit(localInputEvent{data: append([]byte(nil), data...)})
	select {
	case destination.frames <- protocol.Frame{Type: protocol.MsgInputBytes, Payload: payload}:
		return true, nil
	case <-destination.done:
		ui.emit(inputPredictionResetEvent{})
		return true, nil
	}
}

type connectionResult struct {
	err      error
	graceful bool
}

type liveConnection struct {
	conn        quic.Connection
	mgmtFrames  chan protocol.Frame
	inputFrames chan protocol.Frame
	cancel      context.CancelFunc
	ctx         context.Context
	done        chan connectionResult
	resumeToken string
	generation  uint64
	lastContact atomic.Int64
	workers     sync.WaitGroup
}

func (c *liveConnection) inputDestination() *inputDestination {
	return &inputDestination{frames: c.inputFrames, done: c.ctx.Done()}
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

func openConnection(ctx context.Context, bootstrap control.Bootstrap, hostname string, cols, rows uint16, cfg Config, resumeToken string, generation uint64, ui *runtimeState, onFirstManagementFrame func() error) (*liveConnection, error) {
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
		conn:        conn,
		mgmtFrames:  make(chan protocol.Frame, 64),
		inputFrames: make(chan protocol.Frame, 64),
		cancel:      cancel,
		ctx:         connCtx,
		done:        make(chan connectionResult, 1),
	}
	fail := func(err error) (*liveConnection, error) {
		ui.stopConnection()
		live.destroy()
		ui.sync(ctx)
		return nil, err
	}
	live.noteContact()
	mgmtStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fail(fmt.Errorf("open management stream: %w", err))
	}
	inputStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fail(fmt.Errorf("open input stream: %w", err))
	}
	errs := make(chan error, 8)
	live.start(func() { writeFrames(connCtx, mgmtStream, live.mgmtFrames, errs) })
	live.start(func() { writeFrames(connCtx, inputStream, live.inputFrames, errs) })
	if err := enqueueEncoded(live.mgmtFrames, protocol.MsgOpenManagementStream, protocol.StreamOpen{StreamType: protocol.StreamTypeManagement}, protocol.EncodeStreamOpen); err != nil {
		return fail(err)
	}
	if resumeToken == "" {
		err = enqueueEncoded(live.mgmtFrames, protocol.MsgSessionAttach, protocol.SessionAttach{Version: protocol.ProtocolVersion, SessionID: bootstrap.SessionID, Token: bootstrap.AttachToken}, protocol.EncodeSessionAttach)
	} else {
		err = enqueueEncoded(live.mgmtFrames, protocol.MsgSessionResume, protocol.SessionResume{Version: protocol.ProtocolVersion, SessionID: bootstrap.SessionID, ResumeToken: resumeToken, Generation: generation}, protocol.EncodeSessionResume)
	}
	if err != nil {
		return fail(err)
	}
	if err := enqueueEncoded(live.inputFrames, protocol.MsgOpenInputStream, protocol.StreamOpen{StreamType: protocol.StreamTypeInput}, protocol.EncodeStreamOpen); err != nil {
		return fail(err)
	}
	mgmtDecoder := protocol.NewDecoder(mgmtStream, protocol.DefaultMaxFrameSize)
	attachResult, err := readInitialManagementFrame(mgmtDecoder, onFirstManagementFrame)
	if err != nil {
		return fail(err)
	}
	switch attachResult.Type {
	case protocol.MsgSessionAttachOK:
		msg, decodeErr := protocol.DecodeSessionAttachOK(attachResult.Payload)
		if decodeErr != nil {
			return fail(decodeErr)
		}
		live.resumeToken, live.generation = msg.ResumeToken, msg.Generation
	case protocol.MsgSessionResumeOK:
		msg, decodeErr := protocol.DecodeSessionResumeOK(attachResult.Payload)
		if decodeErr != nil {
			return fail(decodeErr)
		}
		live.resumeToken, live.generation = msg.ResumeToken, msg.Generation
	case protocol.MsgSessionAttachFailed:
		return fail(errors.New("session attachment rejected"))
	default:
		return fail(fmt.Errorf("unexpected session attachment result %d", attachResult.Type))
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
	if err := enqueueEncoded(live.mgmtFrames, protocol.MsgCreatePane, protocol.CreatePane{Cwd: cfg.Cwd, Argv: cfg.Argv, Cols: cols, Rows: drawableRows(int(rows))}, protocol.EncodeCreatePane); err != nil {
		return fail(err)
	}
	createdFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fail(fmt.Errorf("read pane created: %w", err))
	}
	if createdFrame.Type != protocol.MsgPaneCreated {
		return fail(fmt.Errorf("unexpected pane message type %d", createdFrame.Type))
	}
	if _, err := protocol.DecodePaneCreated(createdFrame.Payload); err != nil {
		return fail(fmt.Errorf("decode pane created: %w", err))
	}
	live.start(func() { managementLoop(mgmtDecoder, ui, live.done, &live.lastContact) })
	live.start(func() {
		for {
			select {
			case <-errs:
				// The management stream is the authoritative lifecycle signal.
			case <-connCtx.Done():
				return
			}
		}
	})
	return live, nil
}

func quicDialError(addr string, err error) error {
	var idleTimeout *quic.IdleTimeoutError
	if errors.As(err, &idleTimeout) {
		return fmt.Errorf("UDP %s is unreachable: %w", addr, err)
	}
	return fmt.Errorf("dial %s: %w", addr, err)
}

func readInitialManagementFrame(decoder *protocol.Decoder, onFrame func() error) (protocol.Frame, error) {
	frame, err := decoder.ReadFrame()
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("read session attachment result: %w", err)
	}
	if onFrame != nil {
		if err := onFrame(); err != nil {
			return protocol.Frame{}, err
		}
	}
	return frame, nil
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
	layoutRevision uint64
	hasBarrier     bool
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
		c.frame = renderFrame{layoutRevision: c.layoutRevision}
		c.frameReady = false
		c.cursorUpdated = false
	}
	if command.Opcode == protocol.DisplayOpcodeRelayoutBarrier {
		if c.slot == protocol.StatusRenderSlot {
			return false, errors.New("RELAYOUT_BARRIER on status output")
		}
		c.layoutRevision = command.LayoutRevision
		c.hasBarrier = true
		c.row, c.column = 0, 0
		c.styleID = protocol.CanonicalDefaultStyleID
		c.styles = defaultStyles()
		c.frame = renderFrame{layoutRevision: c.layoutRevision}
		c.paintStarted = false
		return false, nil
	}
	if !c.hasBarrier && c.slot != protocol.StatusRenderSlot {
		return false, fmt.Errorf("display command 0x%02x on slot %d before RELAYOUT_BARRIER", byte(command.Opcode), c.slot)
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
		span := paintSpan{kind: paintText, row: c.row, column: c.column, styleID: styleID, cellWidth: width, text: command.Text}
		c.frame.spans = append(c.frame.spans, span)
		for range string(command.Text) {
			c.column += int(width)
		}
		c.paintStarted = true
	case protocol.DisplayOpcodeFill:
		c.frame.spans = append(c.frame.spans, paintSpan{
			kind: paintFill, row: c.row, column: c.column, styleID: c.styleID,
			cellWidth: command.Fill.Width, fillRune: command.Fill.Rune, fillColumns: command.Fill.Columns,
		})
		c.column += command.Fill.Columns
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
			c.frame.scrollDeltas = append(c.frame.scrollDeltas, command.Delta)
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

func managementLoop(decoder *protocol.Decoder, ui *runtimeState, done chan<- connectionResult, lastContact *atomic.Int64) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if isTerminalQUICClose(err) {
				done <- connectionResult{graceful: true}
				return
			}
			if errors.Is(err, io.EOF) {
				done <- connectionResult{}
				return
			}
			done <- connectionResult{err: fmt.Errorf("read management frame: %w", err)}
			return
		}
		if lastContact != nil {
			lastContact.Store(time.Now().UnixNano())
		}
		switch frame.Type {
		case protocol.MsgWindowLayout:
			msg, err := protocol.DecodeWindowLayout(frame.Payload)
			if err != nil {
				done <- connectionResult{err: fmt.Errorf("decode WINDOW_LAYOUT: %w", err)}
				return
			}
			ui.emit(layoutEvent{layout: msg})
		default:
		}
	}
}

func isTerminalQUICClose(err error) bool {
	var applicationErr *quic.ApplicationError
	return errors.As(err, &applicationErr) &&
		(applicationErr.ErrorCode == 0 || applicationErr.ErrorCode == protocol.SessionReplacedErrorCode)
}

func forwardInput(ctx context.Context, stdin *os.File, input *atomic.Pointer[inputDestination], ui *runtimeState, errs chan<- error, cancel context.CancelCauseFunc) {
	buf := make([]byte, 4096)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			connected, sendErr := sendCurrentPredictedInput(input, ui, data)
			if sendErr != nil {
				if ctx.Err() != nil {
					return
				}
				errs <- sendErr
				return
			}
			if !connected && bytes.IndexByte(data, 0x03) >= 0 {
				cancel(errDisconnectedInterrupt)
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return
			}
			errs <- fmt.Errorf("read stdin: %w", err)
			return
		}
	}
}

func forwardResize(ctx context.Context, tty *os.File, input *atomic.Pointer[inputDestination], ui *runtimeState, errs chan<- error) {
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, syscall.SIGWINCH)
	defer signal.Stop(sigch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigch:
			cols, rows, err := terminalSize(tty)
			if err != nil {
				errs <- err
				return
			}
			ui.emit(sizeEvent{cols: int(cols), rows: int(rows)})
			if sendErr := sendCurrentInputEncoded(input, protocol.MsgResizePane, protocol.ResizePane{
				Cols: cols,
				Rows: drawableRows(int(rows)),
			}, protocol.EncodeResizePane); sendErr != nil {
				errs <- sendErr
				return
			}
		}
	}
}

func terminalSize(f *os.File) (uint16, uint16, error) {
	cols, rows, err := term.GetSize(int(f.Fd()))
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
	layout            protocol.WindowLayout
	pendingLayouts    map[uint64]protocol.WindowLayout
	pendingFrames     map[uint64]map[uint8][]renderFrame
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

type paneScanoutCache struct {
	cols, rows, head int
	cells            []protocol.Cell
}

func newPaneScanoutCache(cols, rows int) *paneScanoutCache {
	cells := make([]protocol.Cell, max(0, cols*rows))
	fillBlank(cells)
	return &paneScanoutCache{cols: cols, rows: rows, cells: cells}
}

func (p *paneScanoutCache) row(row int) []protocol.Cell {
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
	return &scanoutState{rectangularScroll: rectangularScroll, caches: make(map[uint8]*paneScanoutCache), pendingLayouts: make(map[uint64]protocol.WindowLayout), pendingFrames: make(map[uint64]map[uint8][]renderFrame), styles: make(map[uint8]map[uint32]protocol.Style), cursors: make(map[uint8]protocol.CursorUpdate)}
}

func (s *scanoutState) takeANSI() []byte {
	out := append([]byte(nil), s.ansi.Bytes()...)
	s.ansi.Reset()
	return out
}

func (s *scanoutState) acceptLayout(layout protocol.WindowLayout) (bool, error) {
	if layout.LayoutRevision < s.layout.LayoutRevision {
		return false, nil
	}
	if layout.LayoutRevision == s.layout.LayoutRevision && s.layout.LayoutRevision != 0 {
		if layout.FocusedPaneID != s.layout.FocusedPaneID {
			if _, err := s.resetPrediction(); err != nil {
				return false, err
			}
		}
		s.layout = layout
		s.selectAuthoritativeCursor()
		s.emitActiveCursor()
		return true, nil
	}
	s.pendingLayouts[layout.LayoutRevision] = layout
	return s.tryActivate(layout.LayoutRevision)
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
		return true, s.emitFrame(slot, placement.Rect, frame)
	}
	bySlot := s.pendingFrames[frame.layoutRevision]
	if bySlot == nil {
		bySlot = make(map[uint8][]renderFrame)
		s.pendingFrames[frame.layoutRevision] = bySlot
	}
	bySlot[slot] = append(bySlot[slot], frame)
	return s.tryActivate(frame.layoutRevision)
}

func (s *scanoutState) tryActivate(revision uint64) (bool, error) {
	layout, ok := s.pendingLayouts[revision]
	if !ok {
		return false, nil
	}
	frames := s.pendingFrames[revision]
	for _, placement := range layout.Panes {
		if len(frames[placement.Slot]) == 0 {
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

func (s *scanoutState) emitLayoutBorders(layout protocol.WindowLayout) {
	s.ansi.WriteString(sgrForStyle(protocol.CanonicalDefaultStyle()))
	for row := 0; row < max(0, s.rows-1); row++ {
		for column := 0; column < s.cols; column++ {
			r := paneBorderRune(layout.Panes, column, row)
			if r == 0 {
				continue
			}
			writeCursorPosition(&s.ansi, row+1, column+1)
			s.ansi.WriteRune(r)
		}
	}
}

func placementForSlot(layout protocol.WindowLayout, slot uint8) (protocol.PanePlacement, bool) {
	for _, placement := range layout.Panes {
		if placement.Slot == slot {
			return placement, true
		}
	}
	return protocol.PanePlacement{}, false
}

func (s *scanoutState) emitFrame(slot uint8, rect protocol.Rect, frame renderFrame) error {
	styles := s.styles[slot]
	if styles == nil {
		styles = defaultStyles()
		s.styles[slot] = styles
	}
	for _, def := range frame.styleInstalls {
		styles[def.ID] = def.Style
	}
	cache := s.caches[slot]
	evidence := frameEvidence{touched: make(map[cellPosition]authoritativeCellChange), cursorUpdated: frame.cursorUpdated, scrolled: len(frame.scrollDeltas) > 0}
	if cache != nil {
		for _, delta := range frame.scrollDeltas {
			cache.scroll(delta)
		}
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
	fullPaneEmitted := false
	for _, delta := range display.scrollDeltas {
		if delta == 0 {
			continue
		}
		if s.rectangularScroll && !result.repaintPane {
			// DECLRMM is enabled for the session. Margins are reset immediately.
			fmt.Fprintf(&s.ansi, "\x1b[%d;%dr\x1b[%d;%ds\x1b[%d;%dH", rect.Y+1, rect.Y+rect.Height, rect.X+1, rect.X+rect.Width, rect.Y+1, rect.X+1)
			if delta < 0 {
				fmt.Fprintf(&s.ansi, "\x1b[%dS", -delta)
			} else {
				fmt.Fprintf(&s.ansi, "\x1b[%dT", delta)
			}
			fmt.Fprintf(&s.ansi, "\x1b[r\x1b[1;%ds", s.cols)
		}
	}
	if cache != nil && len(display.scrollDeltas) > 0 && (!s.rectangularScroll || result.repaintPane) {
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
		style, ok := styles[span.styleID]
		if !ok {
			return fmt.Errorf("undefined style %d on slot %d", span.styleID, slot)
		}
		writeCursorPosition(&s.ansi, rect.Y+span.row+1, rect.X+span.column+1)
		s.ansi.WriteString(sgrForStyle(style))
		if span.kind == paintText {
			s.ansi.Write(span.text)
		} else {
			for columns := 0; columns < span.fillColumns; columns += int(span.cellWidth) {
				s.ansi.WriteRune(span.fillRune)
			}
		}
	}
	return nil
}

func applySpanToCache(cache *paneScanoutCache, span paintSpan, evidence *frameEvidence) error {
	if span.row < 0 || span.row >= cache.rows || span.column < 0 || span.column >= cache.cols {
		return errors.New("paint span outside pane cache")
	}
	row, column := cache.row(span.row), span.column
	write := func(r rune, width uint8) error {
		if column+int(width) > len(row) {
			return errors.New("paint span exceeds pane cache")
		}
		position := cellPosition{row: span.row, column: column}
		change, exists := evidence.touched[position]
		if !exists {
			change.before = row[column]
		}
		row[column] = protocol.Cell{Rune: r, StyleID: span.styleID, Width: width}
		change.after = row[column]
		evidence.touched[position] = change
		for n := 1; n < int(width); n++ {
			continuation := cellPosition{row: span.row, column: column + n}
			continuationChange, continuationExists := evidence.touched[continuation]
			if !continuationExists {
				continuationChange.before = row[column+n]
			}
			row[column+n] = protocol.Cell{StyleID: span.styleID}
			continuationChange.after = row[column+n]
			evidence.touched[continuation] = continuationChange
		}
		column += int(width)
		return nil
	}
	if span.kind == paintText {
		for _, r := range string(span.text) {
			if err := write(r, span.cellWidth); err != nil {
				return err
			}
		}
		return nil
	}
	for columns := 0; columns < span.fillColumns; columns += int(span.cellWidth) {
		if err := write(span.fillRune, span.cellWidth); err != nil {
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
			r := cell.Rune
			if r == 0 {
				r = ' '
			}
			s.ansi.WriteRune(r)
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

func (r *runtimeState) renderLoop(ctx context.Context, errs chan<- error) {
	if r.renderDone != nil {
		defer close(r.renderDone)
	}
	state := newScanoutState(r.rectangularScroll)
	present := func(reason string) error {
		buf := state.takeANSI()
		if r.diagnostics != nil {
			r.diagnostics.reportRedraw(reason, len(buf))
		}
		if len(buf) == 0 {
			return nil
		}
		_, err := r.stdout.Write(buf)
		return err
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
		case inputPredictionResetEvent:
			needsPresent, err = state.resetPrediction()
			reason = "input-drop"
		case layoutEvent:
			if r.dropConnectionEvents.Load() {
				return false, "", nil
			}
			needsPresent, err = state.acceptLayout(e.layout)
			reason = "window-layout"
		case paneFrameEvent:
			if r.dropConnectionEvents.Load() {
				return false, "", nil
			}
			needsPresent, err = state.acceptFrame(e.slot, e.frame)
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

func drawableRows(rows int) uint16 {
	if rows <= 1 {
		return 1
	}
	return uint16(rows - 1)
}

func defaultStyles() map[uint32]protocol.Style {
	return map[uint32]protocol.Style{protocol.CanonicalDefaultStyleID: protocol.CanonicalDefaultStyle()}
}

func fillBlank(cells []protocol.Cell) {
	for i := range cells {
		cells[i] = protocol.Cell{Rune: ' ', Width: 1}
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
	var left, right, above, below bool
	for _, placement := range placements {
		rect := placement.Rect
		if row >= rect.Y && row < rect.Y+rect.Height {
			left = left || rect.X+rect.Width == column
			right = right || rect.X == column+1
		}
		if column >= rect.X && column < rect.X+rect.Width {
			above = above || rect.Y+rect.Height == row
			below = below || rect.Y == row+1
		}
	}
	vertical := left && right
	horizontal := above && below
	switch {
	case vertical && horizontal:
		return '┼'
	case vertical:
		return '│'
	case horizontal:
		return '─'
	default:
		return 0
	}
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
