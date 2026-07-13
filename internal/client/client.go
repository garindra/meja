package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	osuser "os/user"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/term"

	"tali/internal/auth"
	"tali/internal/client/render"
	"tali/internal/protocol"
	"tali/internal/sshconfig"
)

const (
	quicMaxIdleTimeout  = 60 * time.Second
	quicKeepAlivePeriod = 10 * time.Second
	clientPingPeriod    = 15 * time.Second
	incomingBurstWindow = 50 * time.Millisecond
)

type Target struct {
	Username        string
	Hostname        string
	HasExplicitUser bool
}

type Config struct {
	Target             Target
	Port               int
	PortSet            bool
	CAFile             string
	IdentityFile       string
	DebugRender        bool
	DebugRenderLogPath string
	Cwd                string
	Argv               []string
	Stdin              *os.File
	Stdout             io.Writer
	Stderr             io.Writer
}

type runtimeState struct {
	stdout         io.Writer
	stderr         io.Writer
	events         chan renderEvent
	debugRender    bool
	redrawRequests uint64
	redrawWrites   uint64

	incomingMu              sync.Mutex
	incomingBurstStarted    time.Time
	incomingBurstTimer      *time.Timer
	incomingClosed          bool
	incomingWireBytes       uint64
	incomingPayloadBytes    uint64
	incomingCommandCount    uint64
	incomingMessageTypeHits map[uint64]uint64
	incomingWriteStyleHits  map[renderStyleKey]uint64
	installedRenderStyles   map[renderStyleKey]protocol.Style
}

type renderStyleKey struct {
	slot uint8
	id   uint32
}

type renderEvent interface{ renderEvent() }
type paneBatchEvent struct {
	slot           uint8
	layoutRevision uint64
	commands       []protocol.Frame
}
type layoutEvent struct{ layout protocol.WindowLayout }
type statusEvent struct{ status protocol.StatusBar }
type sizeEvent struct{ cols, rows int }

func (paneBatchEvent) renderEvent() {}
func (layoutEvent) renderEvent()    {}
func (statusEvent) renderEvent()    {}
func (sizeEvent) renderEvent()      {}

func ParseTarget(raw string) (Target, error) {
	parsed, err := sshconfig.ParseTarget(raw)
	if err != nil {
		return Target{}, fmt.Errorf("invalid target %q: %w", raw, err)
	}
	return Target{
		Username:        parsed.Username,
		Hostname:        parsed.Host,
		HasExplicitUser: parsed.HasExplicitUser,
	}, nil
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Stdin == nil {
		return errors.New("stdin is required")
	}
	if cfg.Stdout == nil {
		return errors.New("stdout is required")
	}
	renderLog := cfg.Stderr
	if cfg.DebugRenderLogPath != "" {
		f, err := os.Create(cfg.DebugRenderLogPath)
		if err != nil {
			return fmt.Errorf("open render log: %w", err)
		}
		defer f.Close()
		renderLog = f
	}

	localUser, err := currentUsername()
	if err != nil {
		return err
	}
	resolved, err := sshconfig.Resolve(sshconfig.ParsedTarget{
		Host:            cfg.Target.Hostname,
		Username:        cfg.Target.Username,
		HasExplicitUser: cfg.Target.HasExplicitUser,
	}, sshconfig.ResolveOptions{
		ExplicitIdentityFile: cfg.IdentityFile,
		ExplicitPort:         cfg.Port,
		ExplicitPortSet:      cfg.PortSet,
		LocalUsername:        localUser,
	})
	if err != nil {
		return err
	}
	identity, err := auth.SelectIdentity(auth.SelectOptions{
		IdentityFiles:  resolved.IdentityFiles,
		IdentitiesOnly: resolved.IdentitiesOnly,
	})
	if err != nil {
		return err
	}
	tlsConfig, err := loadTLSConfig(cfg.CAFile, resolved.Hostname)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(resolved.Hostname, fmt.Sprintf("%d", resolved.Port))
	conn, err := quic.DialAddr(ctx, addr, tlsConfig, &quic.Config{
		MaxIdleTimeout:     quicMaxIdleTimeout,
		KeepAlivePeriod:    quicKeepAlivePeriod,
		MaxIncomingStreams: int64(protocol.MaxRenderSlots),
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.CloseWithError(0, "")

	mgmtStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open management stream: %w", err)
	}
	inputStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open input stream: %w", err)
	}

	mgmtFrames := make(chan protocol.Frame, 64)
	inputFrames := make(chan protocol.Frame, 64)
	streamErrs := make(chan error, 8)
	go writeFrames(mgmtStream, mgmtFrames, streamErrs)
	go writeFrames(inputStream, inputFrames, streamErrs)
	defer close(mgmtFrames)
	defer close(inputFrames)

	if err := enqueueEncoded(mgmtFrames, protocol.MsgOpenManagementStream, protocol.StreamOpen{StreamType: protocol.StreamTypeManagement}, protocol.EncodeStreamOpen); err != nil {
		return err
	}
	if err := enqueueEncoded(mgmtFrames, protocol.MsgClientHello, protocol.ClientHello{Version: 1}, protocol.EncodeClientHello); err != nil {
		return err
	}
	if err := enqueueEncoded(inputFrames, protocol.MsgOpenInputStream, protocol.StreamOpen{StreamType: protocol.StreamTypeInput}, protocol.EncodeStreamOpen); err != nil {
		return err
	}

	mgmtDecoder := protocol.NewDecoder(mgmtStream, protocol.DefaultMaxFrameSize)
	ui := &runtimeState{
		stdout:      cfg.Stdout,
		stderr:      renderLog,
		events:      make(chan renderEvent, 256),
		debugRender: cfg.DebugRender,
	}
	defer ui.closeIncomingRenderLog()
	go ui.renderLoop(ctx, streamErrs)
	outputReady := make(chan struct{}, 1)
	sessionDone := make(chan error, 2)
	go acceptOutputStreams(ctx, conn, ui, outputReady, sessionDone)

	if err := enqueueEncoded(mgmtFrames, protocol.MsgAuthBegin, protocol.AuthBegin{
		Username:  resolved.Username,
		PublicKey: identity.AuthorizedKey(),
	}, protocol.EncodeAuthBegin); err != nil {
		return err
	}

	challengeFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth challenge: %w", err)
	}
	if challengeFrame.Type == protocol.MsgAuthFailed {
		failed, err := protocol.DecodeAuthFailed(challengeFrame.Payload)
		if err != nil {
			return err
		}
		return fmt.Errorf("authentication failed: %s", failed.Reason)
	}
	if challengeFrame.Type != protocol.MsgAuthChallenge {
		return fmt.Errorf("unexpected auth message type %d", challengeFrame.Type)
	}
	challenge, err := protocol.DecodeAuthChallenge(challengeFrame.Payload)
	if err != nil {
		return err
	}
	signature, err := auth.SignTranscript(identity.Signer, auth.BuildTranscript(
		resolved.Username,
		identity.Fingerprint(),
		challenge.ChallengeID,
		challenge.Nonce,
		challenge.ExpiresAt,
	))
	if err != nil {
		return err
	}
	if err := enqueueEncoded(mgmtFrames, protocol.MsgAuthResponse, protocol.AuthResponse{
		ChallengeID: challenge.ChallengeID,
		Signature:   signature,
	}, protocol.EncodeAuthResponse); err != nil {
		return err
	}

	authResult, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth result: %w", err)
	}
	switch authResult.Type {
	case protocol.MsgAuthOK:
		if _, err := protocol.DecodeAuthOK(authResult.Payload); err != nil {
			return err
		}
	case protocol.MsgAuthFailed:
		failed, err := protocol.DecodeAuthFailed(authResult.Payload)
		if err != nil {
			return err
		}
		return fmt.Errorf("authentication failed: %s", failed.Reason)
	default:
		return fmt.Errorf("unexpected auth result type %d", authResult.Type)
	}

	cols, rows, err := terminalSize(cfg.Stdin)
	if err != nil {
		return err
	}
	ui.emit(sizeEvent{cols: int(cols), rows: int(rows)})

	rawState, err := term.MakeRaw(int(cfg.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	if _, err := io.WriteString(cfg.Stdout, "\x1b[?1049h\x1b[H\x1b[2J"); err != nil {
		_ = term.Restore(int(cfg.Stdin.Fd()), rawState)
		return fmt.Errorf("enter alternate screen: %w", err)
	}
	var restoreOnce sync.Once
	restoreTerminal := func() {
		restoreOnce.Do(func() {
			_, _ = io.WriteString(cfg.Stdout, "\x1b[?25h\x1b[0m\x1b[?1049l")
			_ = term.Restore(int(cfg.Stdin.Fd()), rawState)
		})
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

	if err := enqueueEncoded(mgmtFrames, protocol.MsgCreatePane, protocol.CreatePane{
		Cwd:  cfg.Cwd,
		Argv: cfg.Argv,
		Cols: cols,
		Rows: drawableRows(int(rows)),
	}, protocol.EncodeCreatePane); err != nil {
		return err
	}

	createdFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read pane created: %w", err)
	}
	if createdFrame.Type != protocol.MsgPaneCreated {
		return fmt.Errorf("unexpected pane message type %d", createdFrame.Type)
	}
	created, err := protocol.DecodePaneCreated(createdFrame.Payload)
	if err != nil {
		return fmt.Errorf("decode pane created: %w", err)
	}
	_ = created

	select {
	case <-outputReady:
	case err := <-sessionDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	mgmtDone := make(chan error, 1)
	go managementLoop(mgmtDecoder, ui, mgmtDone)

	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	clientDone := make(chan error, 1)
	go forwardInput(copyCtx, cfg.Stdin, inputFrames, streamErrs, clientDone)
	go forwardResize(copyCtx, cfg.Stdin, inputFrames, ui, streamErrs)
	go sendPeriodicPing(copyCtx, mgmtFrames, streamErrs)

	for {
		select {
		case err := <-streamErrs:
			if err != nil {
				return err
			}
		case err := <-sessionDone:
			return err
		case err := <-mgmtDone:
			return err
		case err := <-clientDone:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func loadTLSConfig(caFile, serverName string) (*tls.Config, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("append CA file: no certificates found")
		}
	}
	return &tls.Config{
		RootCAs:    roots,
		NextProtos: []string{protocol.ALPN},
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}, nil
}

func acceptOutputStreams(ctx context.Context, conn quic.Connection, ui *runtimeState, outputReady chan<- struct{}, sessionDone chan<- error) {
	for i := 0; i < int(protocol.MaxRenderSlots); i++ {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			sessionDone <- fmt.Errorf("accept output stream: %w", err)
			return
		}
		decoder := protocol.NewDecoder(stream, protocol.DefaultMaxFrameSize)
		openFrame, err := decoder.ReadFrame()
		if err != nil {
			sessionDone <- fmt.Errorf("read output stream open: %w", err)
			return
		}
		if openFrame.Type != protocol.MsgOpenPaneOutputStream {
			sessionDone <- fmt.Errorf("unexpected output stream opener %d", openFrame.Type)
			return
		}
		open, err := protocol.DecodeStreamOpen(openFrame.Payload)
		if err != nil {
			sessionDone <- err
			return
		}
		if int(open.Slot) != i {
			sessionDone <- fmt.Errorf("unexpected output stream slot %d, want %d", open.Slot, i)
			return
		}
		go readOutputStream(open.Slot, decoder, ui, sessionDone)
	}
	outputReady <- struct{}{}
}

func readOutputStream(slot uint8, decoder *protocol.Decoder, ui *runtimeState, sessionDone chan<- error) {
	var layoutRevision uint64
	var pending []protocol.Frame
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			sessionDone <- fmt.Errorf("read output frame: %w", err)
			return
		}
		ui.recordIncomingRenderFrame(slot, frame)
		if frame.Type == protocol.MsgRelayoutBarrier {
			msg, err := protocol.DecodeRelayoutBarrier(frame.Payload)
			if err != nil {
				sessionDone <- fmt.Errorf("decode RELAYOUT_BARRIER on slot %d: %w", slot, err)
				return
			}
			pending = pending[:0]
			layoutRevision = msg.LayoutRevision
			continue
		}
		if frame.Type != protocol.MsgPresent {
			pending = append(pending, protocol.Frame{
				Type:    frame.Type,
				Payload: append([]byte(nil), frame.Payload...),
			})
			continue
		}
		if _, err := protocol.DecodePresent(frame.Payload); err != nil {
			sessionDone <- fmt.Errorf("decode PRESENT on slot %d: %w", slot, err)
			return
		}
		if layoutRevision == 0 {
			sessionDone <- fmt.Errorf("PRESENT on slot %d before RELAYOUT_BARRIER", slot)
			return
		}
		batch := paneBatchEvent{slot: slot, layoutRevision: layoutRevision, commands: append([]protocol.Frame(nil), pending...)}
		pending = pending[:0]
		ui.emit(batch)
	}
}

func applyDisplayCommand(s *render.ClientState, slot uint8, frame protocol.Frame) error {
	valid := false
	switch frame.Type {
	case protocol.MsgStyleInstall:
		m, e := protocol.DecodeStyleInstall(frame.Payload)
		if e != nil {
			return fmt.Errorf("decode STYLE_INSTALL: %w", e)
		}
		valid = s.InstallStyle(slot, m)
	case protocol.MsgSetWritePosition:
		m, e := protocol.DecodeSetWritePosition(frame.Payload)
		if e != nil {
			return fmt.Errorf("decode SET_WRITE_POSITION: %w", e)
		}
		valid = s.SetWritePosition(slot, m)
	case protocol.MsgSetWriteStyle:
		m, e := protocol.DecodeSetWriteStyle(frame.Payload)
		if e != nil {
			return fmt.Errorf("decode SET_WRITE_STYLE: %w", e)
		}
		valid = s.SetWriteStyle(slot, m)
	case protocol.MsgWriteText:
		m, e := protocol.DecodeWriteText(frame.Payload)
		if e != nil {
			return fmt.Errorf("decode WRITE_TEXT: %w", e)
		}
		valid = s.WriteText(slot, m)
	case protocol.MsgFill:
		m, e := protocol.DecodeFill(frame.Payload)
		if e != nil {
			return fmt.Errorf("decode FILL: %w", e)
		}
		valid = s.Fill(slot, m)
	case protocol.MsgCursorUpdate:
		m, e := protocol.DecodeCursorUpdate(frame.Payload)
		if e != nil {
			return fmt.Errorf("decode CURSOR_UPDATE: %w", e)
		}
		valid = s.UpdateCursor(slot, m)
	case protocol.MsgScroll:
		m, e := protocol.DecodeScroll(frame.Payload)
		if e != nil {
			return fmt.Errorf("decode SCROLL: %w", e)
		}
		valid = s.ApplyScroll(slot, m.Delta)
	default:
		return fmt.Errorf("unexpected display command %d", frame.Type)
	}
	if !valid {
		return fmt.Errorf("invalid display command %d on slot %d", frame.Type, slot)
	}
	return nil
}

func managementLoop(decoder *protocol.Decoder, ui *runtimeState, done chan<- error) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			done <- fmt.Errorf("read management frame: %w", err)
			return
		}
		switch frame.Type {
		case protocol.MsgWindowLayout:
			msg, err := protocol.DecodeWindowLayout(frame.Payload)
			if err != nil {
				done <- fmt.Errorf("decode WINDOW_LAYOUT: %w", err)
				return
			}
			ui.emit(layoutEvent{layout: msg})
		case protocol.MsgStatusBar:
			msg, err := protocol.DecodeStatusBar(frame.Payload)
			if err != nil {
				done <- fmt.Errorf("decode STATUS_BAR: %w", err)
				return
			}
			ui.emit(statusEvent{status: msg})
		case protocol.MsgPong:
			if _, err := protocol.DecodePong(frame.Payload); err != nil {
				done <- err
				return
			}
		default:
		}
	}
}

func forwardInput(ctx context.Context, stdin *os.File, inputFrames chan<- protocol.Frame, errs chan<- error, done chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if sendErr := sendInputBytes(inputFrames, append([]byte(nil), buf[:n]...)); sendErr != nil {
				if ctx.Err() != nil {
					return
				}
				errs <- sendErr
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

func forwardResize(ctx context.Context, tty *os.File, inputFrames chan<- protocol.Frame, ui *runtimeState, errs chan<- error) {
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
			if sendErr := enqueueEncoded(inputFrames, protocol.MsgResizePane, protocol.ResizePane{
				Cols: cols,
				Rows: drawableRows(int(rows)),
			}, protocol.EncodeResizePane); sendErr != nil {
				errs <- sendErr
				return
			}
		}
	}
}

func sendPeriodicPing(ctx context.Context, mgmtFrames chan<- protocol.Frame, errs chan<- error) {
	ticker := time.NewTicker(clientPingPeriod)
	defer ticker.Stop()

	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seq++
			if err := enqueueEncoded(mgmtFrames, protocol.MsgPing, protocol.Ping{
				Seq:           seq,
				SentUnixMilli: time.Now().UnixMilli(),
			}, protocol.EncodePing); err != nil {
				if ctx.Err() != nil {
					return
				}
				errs <- err
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

func writeFrames(stream io.Writer, frames <-chan protocol.Frame, errs chan<- error) {
	enc := protocol.NewEncoder(stream)
	for frame := range frames {
		if err := enc.WriteFrame(frame); err != nil {
			errs <- fmt.Errorf("write frame type %d: %w", frame.Type, err)
			return
		}
	}
}

func sendInputBytes(ch chan<- protocol.Frame, data []byte) error {
	return enqueueEncoded(ch, protocol.MsgInputBytes, protocol.InputBytes{Data: data}, protocol.EncodeInputBytes)
}

func enqueueFrame(ch chan<- protocol.Frame, frame protocol.Frame) error {
	defer func() { recover() }()
	ch <- frame
	return nil
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

func currentUsername() (string, error) {
	current, err := osuser.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current username: %w", err)
	}
	return strings.TrimSpace(current.Username), nil
}

func (r *runtimeState) renderLoop(ctx context.Context, errs chan<- error) {
	state := render.NewClientState()
	pending := make(map[uint64][]paneBatchEvent)
	slotRevision := make(map[uint8]uint64)
	present := func(reason string) error {
		r.redrawRequests++
		r.logRenderf("redraw request #%d: %s", r.redrawRequests, reason)
		buf := render.RenderANSI(state)
		r.redrawWrites++
		r.logRenderf("redraw write #%d bytes=%d", r.redrawWrites, len(buf))
		_, err := r.stdout.Write(buf)
		return err
	}
	applyBatch := func(batch paneBatchEvent) error {
		if slotRevision[batch.slot] != batch.layoutRevision {
			if !state.ResetStream(batch.slot) {
				return fmt.Errorf("barrier for unbound output slot %d", batch.slot)
			}
			slotRevision[batch.slot] = batch.layoutRevision
		}
		for _, command := range batch.commands {
			if err := applyDisplayCommand(state, batch.slot, command); err != nil {
				return err
			}
		}
		return nil
	}
	handleEvent := func(event renderEvent) (bool, string, error) {
		needsPresent := false
		reason := ""
		var err error
		switch e := event.(type) {
		case sizeEvent:
			state.SetTerminalSize(e.cols, e.rows)
		case statusEvent:
			state.ApplyStatusBar(e.status)
			needsPresent = true
			reason = "status-bar"
		case layoutEvent:
			if state.ApplyWindowLayout(e.layout) {
				needsPresent = true
				reason = "window-layout"
			}
			for revision := range pending {
				if revision < e.layout.LayoutRevision {
					delete(pending, revision)
				}
			}
			for _, batch := range pending[e.layout.LayoutRevision] {
				if err = applyBatch(batch); err != nil {
					break
				}
				needsPresent = true
				reason = "layout batches"
			}
			delete(pending, e.layout.LayoutRevision)
		case paneBatchEvent:
			current := state.Layout.LayoutRevision
			if e.layoutRevision > current {
				pending[e.layoutRevision] = append(pending[e.layoutRevision], e)
			} else if e.layoutRevision == current {
				err = applyBatch(e)
				needsPresent = err == nil
				reason = fmt.Sprintf("present slot=%d", e.slot)
			}
		}
		return needsPresent, reason, err
	}
	for {
		select {
		case <-ctx.Done():
			return
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

func (r *runtimeState) logRenderf(format string, args ...any) {
	if !r.debugRender || r.stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(r.stderr, "tali render: "+format+"\n", args...)
}

func (r *runtimeState) recordIncomingRenderFrame(slot uint8, frame protocol.Frame) {
	if !r.debugRender || r.stderr == nil {
		return
	}

	r.incomingMu.Lock()
	if r.incomingClosed {
		r.incomingMu.Unlock()
		return
	}
	if r.incomingBurstStarted.IsZero() {
		r.incomingBurstStarted = time.Now()
		r.incomingMessageTypeHits = make(map[uint64]uint64)
		r.incomingWriteStyleHits = make(map[renderStyleKey]uint64)
		r.incomingBurstTimer = time.AfterFunc(incomingBurstWindow, r.flushIncomingRender)
	}
	r.incomingWireBytes += uint64(encodedFrameSize(frame))
	r.incomingPayloadBytes += uint64(len(frame.Payload))
	r.incomingCommandCount++
	r.incomingMessageTypeHits[frame.Type]++
	key := renderStyleKey{slot: slot}
	if frame.Type == protocol.MsgStyleInstall {
		if msg, err := protocol.DecodeStyleInstall(frame.Payload); err == nil {
			key.id = msg.ID
			if r.installedRenderStyles == nil {
				r.installedRenderStyles = make(map[renderStyleKey]protocol.Style)
			}
			r.installedRenderStyles[key] = msg.Style
		}
	}
	if frame.Type == protocol.MsgSetWriteStyle {
		if msg, err := protocol.DecodeSetWriteStyle(frame.Payload); err == nil {
			key.id = msg.StyleID
			r.incomingWriteStyleHits[key]++
		}
	}
	r.incomingMu.Unlock()
}

func (r *runtimeState) flushIncomingRender() {
	r.incomingMu.Lock()
	defer r.incomingMu.Unlock()
	if r.incomingBurstStarted.IsZero() {
		return
	}
	if r.incomingBurstTimer != nil {
		r.incomingBurstTimer.Stop()
		r.incomingBurstTimer = nil
	}
	startedAt := r.incomingBurstStarted
	types := formatIncomingRenderTypes(r.incomingMessageTypeHits)
	writeStyles := formatIncomingWriteStyles(r.incomingWriteStyleHits, r.installedRenderStyles)
	r.logRenderf(
		"incoming burst at=%s window=%s elapsed=%s wire_bytes=%d payload_bytes=%d commands=%d types=%s write_styles=%s",
		time.Now().Format(time.RFC3339Nano),
		incomingBurstWindow,
		time.Since(startedAt).Round(time.Millisecond),
		r.incomingWireBytes,
		r.incomingPayloadBytes,
		r.incomingCommandCount,
		types,
		writeStyles,
	)
	r.incomingBurstStarted = time.Time{}
	r.incomingWireBytes = 0
	r.incomingPayloadBytes = 0
	r.incomingCommandCount = 0
	r.incomingMessageTypeHits = nil
	r.incomingWriteStyleHits = nil
}

func formatIncomingWriteStyles(hits map[renderStyleKey]uint64, styles map[renderStyleKey]protocol.Style) string {
	if len(hits) == 0 {
		return "none"
	}
	keys := make([]renderStyleKey, 0, len(hits))
	for key := range hits {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].slot != keys[j].slot {
			return keys[i].slot < keys[j].slot
		}
		return keys[i].id < keys[j].id
	})
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		style, ok := styles[key]
		description := "unknown"
		if ok {
			description = formatRenderStyle(style)
		}
		parts = append(parts, fmt.Sprintf("slot%d/id%d:%d{%s}", key.slot, key.id, hits[key], description))
	}
	return strings.Join(parts, ",")
}
func formatRenderStyle(style protocol.Style) string {
	flags := make([]string, 0, 5)
	if style.Bold {
		flags = append(flags, "bold")
	}
	if style.Dim {
		flags = append(flags, "dim")
	}
	if style.Italic {
		flags = append(flags, "italic")
	}
	if style.Underline {
		flags = append(flags, "underline")
	}
	if style.Reverse {
		flags = append(flags, "reverse")
	}
	if len(flags) == 0 {
		flags = append(flags, "plain")
	}
	return fmt.Sprintf("%s,fg=%s,bg=%s", strings.Join(flags, "+"), formatRenderColor(style.FG), formatRenderColor(style.BG))
}
func formatRenderColor(color protocol.Color) string {
	switch color.Mode {
	case "indexed":
		return fmt.Sprintf("idx%d", color.Index)
	case "rgb":
		return fmt.Sprintf("#%02x%02x%02x", color.R, color.G, color.B)
	default:
		return "default"
	}
}

func (r *runtimeState) closeIncomingRenderLog() {
	r.incomingMu.Lock()
	r.incomingClosed = true
	r.incomingMu.Unlock()
	r.flushIncomingRender()
}

func formatIncomingRenderTypes(types map[uint64]uint64) string {
	if len(types) == 0 {
		return "none"
	}
	keys := make([]uint64, 0, len(types))
	for msgType := range types {
		keys = append(keys, msgType)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	parts := make([]string, 0, len(keys))
	for _, msgType := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", incomingRenderMessageName(msgType), types[msgType]))
	}
	return strings.Join(parts, ",")
}

func incomingRenderMessageName(msgType uint64) string {
	switch msgType {
	case protocol.MsgRelayoutBarrier:
		return "RelayoutBarrier"
	case protocol.MsgStyleInstall:
		return "StyleInstall"
	case protocol.MsgSetWritePosition:
		return "SetWritePosition"
	case protocol.MsgSetWriteStyle:
		return "SetWriteStyle"
	case protocol.MsgWriteText:
		return "WriteText"
	case protocol.MsgFill:
		return "Fill"
	case protocol.MsgCursorUpdate:
		return "CursorUpdate"
	case protocol.MsgScroll:
		return "Scroll"
	case protocol.MsgPresent:
		return "Present"
	default:
		return fmt.Sprintf("Message%d", msgType)
	}
}

func encodedFrameSize(frame protocol.Frame) int {
	var buf [binary.MaxVarintLen64]byte
	typeBytes := binary.PutUvarint(buf[:], frame.Type)
	payloadBytes := binary.PutUvarint(buf[:], uint64(len(frame.Payload)))
	return typeBytes + payloadBytes + len(frame.Payload)
}

func drawableRows(rows int) uint16 {
	if rows <= 1 {
		return 1
	}
	return uint16(rows - 1)
}
