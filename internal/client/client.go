package client

import (
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/term"

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
	Original        string
	Username        string
	Hostname        string
	HasExplicitUser bool
}

type Config struct {
	Target             Target
	Port               int
	PortSet            bool
	IdentityFile       string
	DebugRender        bool
	DebugRenderLogPath string
	Cwd                string
	Argv               []string
	CtrlPath           string
	SessionID          uint64
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
	incomingTextBytes       uint64
	incomingCommandCount    uint64
	incomingMessageTypeHits map[protocol.DisplayOpcode]uint64
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
	commands       []protocol.DisplayCommand
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
		Original:        parsed.Original,
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

	bootstrap, err := fetchBootstrap(ctx, cfg)
	if err != nil {
		return err
	}
	resolved, err := sshconfig.Resolve(sshconfig.ParsedTarget{
		Host:            cfg.Target.Hostname,
		Username:        cfg.Target.Username,
		HasExplicitUser: cfg.Target.HasExplicitUser,
	}, sshconfig.ResolveOptions{
		LocalUsername: cfg.Target.Username,
	})
	if err != nil {
		return err
	}
	tlsConfig, err := loadTLSConfig(bootstrap.CertSPKISHA256)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(resolved.Hostname, fmt.Sprintf("%d", bootstrap.Port))
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
	if err := enqueueEncoded(mgmtFrames, protocol.MsgSessionAttach, protocol.SessionAttach{Version: protocol.ProtocolVersion, SessionID: bootstrap.SessionID, Token: bootstrap.AttachToken}, protocol.EncodeSessionAttach); err != nil {
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

	attachResult, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read session attachment result: %w", err)
	}
	switch attachResult.Type {
	case protocol.MsgSessionAttachOK:
		if _, err := protocol.DecodeSessionAttachOK(attachResult.Payload); err != nil {
			return err
		}
	case protocol.MsgSessionAttachFailed:
		return errors.New("session attachment rejected")
	default:
		return fmt.Errorf("unexpected session attachment result %d", attachResult.Type)
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

func acceptOutputStreams(ctx context.Context, conn quic.Connection, ui *runtimeState, outputReady chan<- struct{}, sessionDone chan<- error) {
	for i := 0; i < int(protocol.MaxRenderSlots); i++ {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			sessionDone <- fmt.Errorf("accept output stream: %w", err)
			return
		}
		frameDecoder := protocol.NewDecoder(stream, protocol.DefaultMaxFrameSize)
		openFrame, err := frameDecoder.ReadFrame()
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
		go readOutputStream(open.Slot, protocol.NewDisplayDecoderFromDecoder(frameDecoder), ui, sessionDone)
	}
	outputReady <- struct{}{}
}

func readOutputStream(slot uint8, decoder *protocol.DisplayDecoder, ui *runtimeState, sessionDone chan<- error) {
	var layoutRevision uint64
	var pending []protocol.DisplayCommand
	for {
		command, wireBytes, err := decoder.ReadCommand()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			sessionDone <- fmt.Errorf("read display stream on slot %d: %w", slot, err)
			return
		}
		ui.recordIncomingRenderCommand(slot, command, wireBytes)
		if command.Opcode == protocol.DisplayOpcodeRelayoutBarrier {
			pending = pending[:0]
			layoutRevision = command.LayoutRevision
			continue
		}
		if command.Opcode != protocol.DisplayOpcodePresent {
			if layoutRevision == 0 {
				sessionDone <- fmt.Errorf("display command 0x%02x on slot %d before RELAYOUT_BARRIER", byte(command.Opcode), slot)
				return
			}
			pending = append(pending, command)
			continue
		}
		if layoutRevision == 0 {
			sessionDone <- fmt.Errorf("PRESENT on slot %d before RELAYOUT_BARRIER", slot)
			return
		}
		batch := paneBatchEvent{slot: slot, layoutRevision: layoutRevision, commands: append([]protocol.DisplayCommand(nil), pending...)}
		pending = pending[:0]
		ui.emit(batch)
	}
}

func applyDisplayCommand(s *render.ClientState, slot uint8, command protocol.DisplayCommand) error {
	valid := false
	switch command.Opcode {
	case protocol.DisplayOpcodeStyleInstall:
		valid = s.InstallStyle(slot, protocol.StyleInstall{ID: command.StyleID, Style: command.Style})
	case protocol.DisplayOpcodeSetWritePosition:
		valid = s.SetWritePosition(slot, protocol.SetWritePosition{Row: command.Row, Column: command.Column})
	case protocol.DisplayOpcodeSetWriteStyle:
		valid = s.SetWriteStyle(slot, protocol.SetWriteStyle{StyleID: command.StyleID})
	case protocol.DisplayOpcodeWriteText:
		valid = s.WriteText(slot, protocol.WriteText{CellWidth: command.Width, Text: command.Text})
	case protocol.DisplayOpcodeWriteTextUTF8:
		valid = s.WriteText(slot, protocol.WriteText{CellWidth: 1, Text: command.Text})
	case protocol.DisplayOpcodeWriteTextUTF8Default:
		valid = s.WriteTextDefault(slot, command.Text)
	case protocol.DisplayOpcodeFill:
		valid = s.Fill(slot, command.Fill)
	case protocol.DisplayOpcodeCursorUpdate:
		valid = s.UpdateCursor(slot, command.Cursor)
	case protocol.DisplayOpcodeScroll:
		valid = s.ApplyScroll(slot, command.Delta)
	default:
		return fmt.Errorf("unexpected display opcode 0x%02x", byte(command.Opcode))
	}
	if !valid {
		return fmt.Errorf("invalid display command 0x%02x on slot %d", byte(command.Opcode), slot)
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

func (r *runtimeState) recordIncomingRenderCommand(slot uint8, command protocol.DisplayCommand, wireBytes uint64) {
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
		r.incomingMessageTypeHits = make(map[protocol.DisplayOpcode]uint64)
		r.incomingWriteStyleHits = make(map[renderStyleKey]uint64)
		r.incomingBurstTimer = time.AfterFunc(incomingBurstWindow, r.flushIncomingRender)
	}
	r.incomingWireBytes += wireBytes
	r.incomingTextBytes += uint64(len(command.Text))
	r.incomingCommandCount++
	r.incomingMessageTypeHits[command.Opcode]++
	key := renderStyleKey{slot: slot}
	if command.Opcode == protocol.DisplayOpcodeStyleInstall {
		key.id = command.StyleID
		if r.installedRenderStyles == nil {
			r.installedRenderStyles = make(map[renderStyleKey]protocol.Style)
		}
		r.installedRenderStyles[key] = command.Style
	}
	if command.Opcode == protocol.DisplayOpcodeSetWriteStyle {
		key.id = command.StyleID
		r.incomingWriteStyleHits[key]++
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
		"incoming burst at=%s window=%s elapsed=%s wire_bytes=%d text_bytes=%d commands=%d types=%s write_styles=%s",
		time.Now().Format(time.RFC3339Nano),
		incomingBurstWindow,
		time.Since(startedAt).Round(time.Millisecond),
		r.incomingWireBytes,
		r.incomingTextBytes,
		r.incomingCommandCount,
		types,
		writeStyles,
	)
	r.incomingBurstStarted = time.Time{}
	r.incomingWireBytes = 0
	r.incomingTextBytes = 0
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

func formatIncomingRenderTypes(types map[protocol.DisplayOpcode]uint64) string {
	if len(types) == 0 {
		return "none"
	}
	keys := make([]protocol.DisplayOpcode, 0, len(types))
	for msgType := range types {
		keys = append(keys, msgType)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	parts := make([]string, 0, len(keys))
	for _, msgType := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", incomingRenderOpcodeName(msgType), types[msgType]))
	}
	return strings.Join(parts, ",")
}

func incomingRenderOpcodeName(opcode protocol.DisplayOpcode) string {
	switch opcode {
	case protocol.DisplayOpcodeRelayoutBarrier:
		return "RelayoutBarrier"
	case protocol.DisplayOpcodeStyleInstall:
		return "StyleInstall"
	case protocol.DisplayOpcodeSetWritePosition:
		return "SetWritePosition"
	case protocol.DisplayOpcodeSetWriteStyle:
		return "SetWriteStyle"
	case protocol.DisplayOpcodeWriteText:
		return "WriteText"
	case protocol.DisplayOpcodeWriteTextUTF8:
		return "WriteTextUTF8"
	case protocol.DisplayOpcodeWriteTextUTF8Default:
		return "WriteTextUTF8Default"
	case protocol.DisplayOpcodeFill:
		return "Fill"
	case protocol.DisplayOpcodeCursorUpdate:
		return "CursorUpdate"
	case protocol.DisplayOpcodeScroll:
		return "Scroll"
	case protocol.DisplayOpcodePresent:
		return "Present"
	default:
		return fmt.Sprintf("Opcode0x%02x", byte(opcode))
	}
}

func drawableRows(rows int) uint16 {
	if rows <= 1 {
		return 1
	}
	return uint16(rows - 1)
}
