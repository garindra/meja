package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os/user"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"tali/internal/auth"
	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

const (
	sessionID0          = 0
	clientID0           = 0
	quicMaxIdleTimeout  = 60 * time.Second
	quicKeepAlivePeriod = 10 * time.Second
)

type Config struct {
	ListenAddr string
	CertFile   string
	KeyFile    string
	Stdout     io.Writer
	Stderr     io.Writer
}

type paneRequest struct {
	Cwd     string
	Command []string
	Cols    uint16
	Rows    uint16
	Shell   string
}

type sessionState struct {
	verifier *auth.Verifier
	session  *Session

	mu           sync.RWMutex
	outputFrames chan protocol.Frame
}

type controller struct {
	ctx          context.Context
	state        *sessionState
	unixUser     *user.User
	shell        string
	mgmtFrames   chan protocol.Frame
	outputFrames chan protocol.Frame
}

func Run(ctx context.Context, cfg Config) error {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{protocol.ALPN},
		MinVersion:   tls.VersionTLS13,
	}
	listener, err := quic.ListenAddr(cfg.ListenAddr, tlsConfig, &quic.Config{
		MaxIdleTimeout:  quicMaxIdleTimeout,
		KeepAlivePeriod: quicKeepAlivePeriod,
	})
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	defer listener.Close()

	shared := &sessionState{
		verifier: auth.NewVerifier(),
		session:  NewSession(sessionID0),
	}

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		go func(conn quic.Connection) {
			if err := handleSession(ctx, shared, conn); err != nil && cfg.Stderr != nil {
				fmt.Fprintf(cfg.Stderr, "session error: %v\n", err)
			}
		}(conn)
	}
}

func handleSession(ctx context.Context, s *sessionState, conn quic.Connection) error {
	defer conn.CloseWithError(0, "")

	var err error
	mgmtStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("accept management stream: %w", err)
	}
	inputStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("accept input stream: %w", err)
	}
	mgmtDecoder := protocol.NewDecoder(mgmtStream, protocol.DefaultMaxFrameSize)
	inputDecoder := protocol.NewDecoder(inputStream, protocol.DefaultMaxFrameSize)
	if err := expectStreamOpen(mgmtDecoder, protocol.MsgOpenManagementStream, protocol.StreamTypeManagement); err != nil {
		return err
	}
	if err := expectStreamOpen(inputDecoder, protocol.MsgOpenInputStream, protocol.StreamTypeInput); err != nil {
		return err
	}

	hello, err := expectDecoded(mgmtDecoder, protocol.MsgClientHello, protocol.DecodeClientHello)
	if err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if hello.Version != 1 {
		return errors.New("unsupported client protocol version")
	}

	mgmtFrames := make(chan protocol.Frame, 64)
	writerErrs := make(chan error, 4)
	go writeStream(mgmtStream, mgmtFrames, writerErrs)
	defer close(mgmtFrames)

	authBegin, err := expectDecoded(mgmtDecoder, protocol.MsgAuthBegin, protocol.DecodeAuthBegin)
	if err != nil {
		return fmt.Errorf("read auth begin: %w", err)
	}
	beginResult, err := s.verifier.Begin(authBegin.Username, authBegin.PublicKey)
	if err != nil {
		_ = sendEncoded(mgmtFrames, protocol.MsgAuthFailed, protocol.AuthFailed{Reason: err.Error()}, protocol.EncodeAuthFailed)
		return fmt.Errorf("begin auth: %w", err)
	}
	unixUser := beginResult.User
	fingerprint := beginResult.Fingerprint
	if err := sendEncoded(mgmtFrames, protocol.MsgAuthChallenge, protocol.AuthChallenge{
		ChallengeID: beginResult.Challenge.ID,
		Nonce:       beginResult.Challenge.Nonce,
		ExpiresAt:   beginResult.Challenge.ExpiresAt,
	}, protocol.EncodeAuthChallenge); err != nil {
		return err
	}
	authResponse, err := expectDecoded(mgmtDecoder, protocol.MsgAuthResponse, protocol.DecodeAuthResponse)
	if err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	if err := s.verifier.Verify(authBegin.Username, fingerprint, authResponse.ChallengeID, authResponse.Signature); err != nil {
		_ = sendEncoded(mgmtFrames, protocol.MsgAuthFailed, protocol.AuthFailed{Reason: err.Error()}, protocol.EncodeAuthFailed)
		return fmt.Errorf("verify auth response: %w", err)
	}

	shell := loginShellForUser(unixUser)
	if err := sendEncoded(mgmtFrames, protocol.MsgAuthOK, protocol.AuthOK{
		Username: unixUser.Username,
		HomeDir:  unixUser.HomeDir,
		Shell:    shell,
	}, protocol.EncodeAuthOK); err != nil {
		return err
	}

	outputStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open output stream: %w", err)
	}
	outputFrames := make(chan protocol.Frame, 256)
	go writeStream(outputStream, outputFrames, writerErrs)
	defer close(outputFrames)
	s.setOutputFrames(outputFrames)
	defer s.clearOutputFrames(outputFrames)
	if err := sendEncoded(outputFrames, protocol.MsgOpenPaneOutputStream, protocol.StreamOpen{StreamType: protocol.StreamTypePaneOutput}, protocol.EncodeStreamOpen); err != nil {
		return err
	}

	ctrl := &controller{
		ctx:          ctx,
		state:        s,
		unixUser:     unixUser,
		shell:        shell,
		mgmtFrames:   mgmtFrames,
		outputFrames: outputFrames,
	}
	s.session.EnsureClient(clientID0)

	createPane, err := expectDecoded(mgmtDecoder, protocol.MsgCreatePane, protocol.DecodeCreatePane)
	if err != nil {
		return fmt.Errorf("read create pane: %w", err)
	}
	if !s.session.HasWindows() {
		initialPane, window, clientState, err := ctrl.createWindow(createPane.Cwd, createPane.Argv, createPane.Cols, createPane.Rows)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: initialPane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := ctrl.publishWindowList(); err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: initialPane.ID}, protocol.EncodeWindowSelected); err != nil {
			return err
		}
		if err := ctrl.bindAndSnapshot(clientState, window, initialPane); err != nil {
			return err
		}
		ctrl.startPane(initialPane)
	} else {
		s.session.ResizeAll(createPane.Cols, createPane.Rows)
		window, pane, clientState, err := s.session.ReattachClient(clientID0)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: pane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := ctrl.publishWindowList(); err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
			return err
		}
		if err := ctrl.bindAndSnapshot(clientState, window, pane); err != nil {
			return err
		}
	}

	mgmtErrs := make(chan error, 1)
	inputErrs := make(chan error, 1)
	go ctrl.handleManagement(mgmtDecoder, mgmtErrs)
	go ctrl.handleInput(inputDecoder, inputErrs)

	select {
	case err := <-writerErrs:
		return err
	case err := <-mgmtErrs:
		return err
	case err := <-inputErrs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *controller) startPane(pane *Pane) {
	go c.relayPTYToTerminal(pane)
	go func() {
		_ = pane.Process.Wait()
		_ = pane.PTY.Close()
	}()
}

func (c *controller) createWindow(cwd string, argv []string, cols, rows uint16) (*Pane, *Window, *ClientState, error) {
	paneID := c.state.session.AddPaneID()
	pane, err := StartPane(c.unixUser, paneID, paneRequest{
		Cwd:     cwd,
		Command: argv,
		Cols:    cols,
		Rows:    rows,
		Shell:   c.shell,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("start pane: %w", err)
	}
	window, clientState := c.state.session.CreateWindow(pane, clientID0)
	return pane, window, clientState, nil
}

func (c *controller) handleManagement(decoder *protocol.Decoder, done chan<- error) {
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
		case protocol.MsgCreateWindow:
			msg, err := protocol.DecodeCreateWindow(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			activePane, _ := c.state.session.ActivePane(clientID0)
			cols, rows := uint16(activePane.Terminal.Cols), uint16(activePane.Terminal.Rows)
			pane, window, clientState, err := c.createWindow(msg.Cwd, msg.Argv, cols, rows)
			if err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowCreated, protocol.WindowCreated{Window: c.windowInfo(window, clientState.ActiveWindowID == window.ID)}, protocol.EncodeWindowCreated); err != nil {
				done <- err
				return
			}
			if err := c.publishWindowList(); err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
				done <- err
				return
			}
			if err := c.bindAndSnapshot(clientState, window, pane); err != nil {
				done <- err
				return
			}
			c.startPane(pane)
		case protocol.MsgSelectWindow:
			msg, err := protocol.DecodeSelectWindow(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			window, pane, clientState, err := c.state.session.SelectWindow(clientID0, msg.WindowID)
			if err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: window.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
				done <- err
				return
			}
			if err := c.bindAndSnapshot(clientState, window, pane); err != nil {
				done <- err
				return
			}
		case protocol.MsgCloseWindow:
			msg, err := protocol.DecodeCloseWindow(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			targetWindowID := msg.WindowID
			if targetWindowID == 0 {
				clientState := c.state.session.SnapshotClient(clientID0)
				if clientState != nil {
					targetWindowID = clientState.ActiveWindowID
				}
			}
			closed, closedPane, replacement, pane, clientState, autoCreate, err := c.state.session.CloseWindow(clientID0, targetWindowID)
			if err != nil {
				done <- err
				return
			}
			_ = terminatePane(closedPane)
			if autoCreate {
				cols, rows := uint16(80), uint16(24)
				if clientState != nil && clientState.FocusedPaneID != 0 {
					if activePane, _ := c.state.session.ActivePane(clientID0); activePane != nil {
						cols, rows = uint16(activePane.Terminal.Cols), uint16(activePane.Terminal.Rows)
					}
				}
				var err error
				pane, replacement, clientState, err = c.createWindow("", nil, cols, rows)
				if err != nil {
					done <- err
					return
				}
				c.startPane(pane)
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowClosed, protocol.WindowClosed{WindowID: closed}, protocol.EncodeWindowClosed); err != nil {
				done <- err
				return
			}
			if err := c.publishWindowList(); err != nil {
				done <- err
				return
			}
			if replacement != nil && pane != nil && clientState != nil {
				if err := sendEncoded(c.mgmtFrames, protocol.MsgWindowSelected, protocol.WindowSelected{WindowID: replacement.ID, PaneID: pane.ID}, protocol.EncodeWindowSelected); err != nil {
					done <- err
					return
				}
				if err := c.bindAndSnapshot(clientState, replacement, pane); err != nil {
					done <- err
					return
				}
			}
		case protocol.MsgListWindows:
			if err := c.publishWindowList(); err != nil {
				done <- err
				return
			}
		case protocol.MsgRequestPaneSnapshot:
			msg, err := protocol.DecodeRequestPaneSnapshot(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane, clientState := c.state.session.ActivePane(clientID0)
			if pane == nil || clientState == nil || pane.ID != msg.PaneID {
				continue
			}
			window := c.windowForPane(pane.ID)
			if window == nil {
				continue
			}
			if err := c.sendReplaceSnapshot(clientState, window, pane); err != nil {
				done <- err
				return
			}
		case protocol.MsgPing:
			msg, err := protocol.DecodePing(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.mgmtFrames, protocol.MsgPong, protocol.Pong{
				Seq:           msg.Seq,
				SentUnixMilli: msg.SentUnixMilli,
			}, protocol.EncodePong); err != nil {
				done <- err
				return
			}
		case protocol.MsgPaneExited:
			done <- nil
			return
		}
	}
}

func (c *controller) handleInput(decoder *protocol.Decoder, done chan<- error) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			done <- fmt.Errorf("read input frame: %w", err)
			return
		}
		switch frame.Type {
		case protocol.MsgInputBytes:
			msg, err := protocol.DecodeInputBytes(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane, _, _ := c.state.session.ResolveInputTarget(clientID0, msg.PaneID)
			if pane == nil {
				continue
			}
			if _, err := pane.PTY.Write(msg.Data); err != nil {
				done <- fmt.Errorf("write pty: %w", err)
				return
			}
		case protocol.MsgResizePane:
			msg, err := protocol.DecodeResizePane(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			pane, clientState, _ := c.state.session.ResolveInputTarget(clientID0, msg.PaneID)
			if pane == nil || clientState == nil {
				continue
			}
			c.state.session.ResizeAll(msg.Cols, msg.Rows)
			window := c.windowForPane(pane.ID)
			if err := c.sendReplaceSnapshot(clientState, window, pane); err != nil {
				done <- err
				return
			}
		default:
			done <- fmt.Errorf("unexpected input frame %d", frame.Type)
			return
		}
	}
}

func (c *controller) relayPTYToTerminal(pane *Pane) {
	buf := make([]byte, 32*1024)
	for {
		n, err := pane.PTY.Read(buf)
		if n > 0 {
			update := pane.Terminal.Apply(buf[:n])
			if sendErr := c.emitTerminalUpdate(pane, update); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *controller) emitTerminalUpdate(pane *Pane, update terminal.Update) error {
	clientState := c.state.session.SnapshotClient(clientID0)
	if clientState == nil || clientState.FocusedPaneID != pane.ID {
		return nil
	}
	outputFrames := c.state.currentOutputFrames()
	if outputFrames == nil {
		return nil
	}
	window := c.windowForPane(pane.ID)
	if window == nil {
		return nil
	}
	if update.FullRedraw {
		return c.sendReplaceSnapshot(clientState, window, pane)
	}

	if len(update.DefinedStyles) > 0 {
		ids := make([]int, 0, len(update.DefinedStyles))
		for id := range update.DefinedStyles {
			ids = append(ids, int(id))
		}
		sort.Ints(ids)
		for _, rawID := range ids {
			id := uint32(rawID)
			if err := sendEncoded(outputFrames, protocol.MsgDefineStyle, protocol.DefineStyle{
				PaneID:            pane.ID,
				BindingGeneration: clientState.BindingGeneration,
				ID:                id,
				Style:             update.DefinedStyles[id],
			}, protocol.EncodeDefineStyle); err != nil {
				return err
			}
		}
	}

	rows := make([]int, 0, len(update.DirtyRows))
	for row := range update.DirtyRows {
		rows = append(rows, row)
	}
	sort.Ints(rows)
	for _, row := range rows {
		base := pane.Generation
		pane.Generation++
		start := row * pane.Terminal.Cols
		end := start + pane.Terminal.Cols
		if err := sendEncoded(outputFrames, protocol.MsgSetRun, protocol.SetRun{
			SessionID:         c.state.session.ID,
			WindowID:          window.ID,
			PaneID:            pane.ID,
			BindingGeneration: clientState.BindingGeneration,
			BaseGeneration:    base,
			Generation:        pane.Generation,
			Row:               row,
			Column:            0,
			Cells:             append([]protocol.Cell(nil), pane.Terminal.Cells[start:end]...),
		}, protocol.EncodeSetRun); err != nil {
			return err
		}
	}
	if update.CursorChanged {
		base := pane.Generation
		pane.Generation++
		if err := sendEncoded(outputFrames, protocol.MsgSetCursor, protocol.SetCursor{
			SessionID:         c.state.session.ID,
			WindowID:          window.ID,
			PaneID:            pane.ID,
			BindingGeneration: clientState.BindingGeneration,
			BaseGeneration:    base,
			Generation:        pane.Generation,
			Cursor:            protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY},
		}, protocol.EncodeSetCursor); err != nil {
			return err
		}
	}
	if update.VisibleChange {
		base := pane.Generation
		pane.Generation++
		if err := sendEncoded(outputFrames, protocol.MsgSetCursorVisible, protocol.SetCursorVisible{
			SessionID:         c.state.session.ID,
			WindowID:          window.ID,
			PaneID:            pane.ID,
			BindingGeneration: clientState.BindingGeneration,
			BaseGeneration:    base,
			Generation:        pane.Generation,
			Visible:           pane.Terminal.CursorVisible,
		}, protocol.EncodeSetCursorVisible); err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) bindAndSnapshot(clientState *ClientState, window *Window, pane *Pane) error {
	if err := sendEncoded(c.outputFrames, protocol.MsgBindRenderStream, protocol.BindRenderStream{
		SessionID:         c.state.session.ID,
		WindowID:          window.ID,
		PaneID:            pane.ID,
		BindingGeneration: clientState.BindingGeneration,
	}, protocol.EncodeBindRenderStream); err != nil {
		return err
	}
	return c.sendReplaceSnapshot(clientState, window, pane)
}

func (c *controller) sendReplaceSnapshot(clientState *ClientState, window *Window, pane *Pane) error {
	pane.Generation++
	styleDefs := pane.Terminal.SnapshotStyles()
	styles := make([]protocol.StyleDefinition, 0, len(styleDefs))
	for _, def := range styleDefs {
		styles = append(styles, protocol.StyleDefinition{ID: def.ID, Style: def.Style})
	}
	return sendEncoded(c.outputFrames, protocol.MsgReplacePane, protocol.ReplacePane{
		SessionID:         c.state.session.ID,
		WindowID:          window.ID,
		PaneID:            pane.ID,
		BindingGeneration: clientState.BindingGeneration,
		Generation:        pane.Generation,
		Cols:              pane.Terminal.Cols,
		Rows:              pane.Terminal.Rows,
		Cells:             append([]protocol.Cell(nil), pane.Terminal.Cells...),
		Styles:            styles,
		Cursor:            protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY},
		CursorVisible:     pane.Terminal.CursorVisible,
	}, protocol.EncodeReplacePane)
}

func (c *controller) publishWindowList() error {
	return sendEncoded(c.mgmtFrames, protocol.MsgWindowList, c.state.session.WindowList(clientID0), protocol.EncodeWindowList)
}

func (c *controller) windowForPane(paneID uint64) *Window {
	c.state.session.mu.RLock()
	defer c.state.session.mu.RUnlock()
	for _, window := range c.state.session.Windows {
		if window.PaneID == paneID {
			cp := *window
			return &cp
		}
	}
	return nil
}

func terminatePane(pane *Pane) error {
	if pane == nil {
		return nil
	}
	if pane.Process.Process != nil {
		_ = pane.Process.Process.Signal(syscall.SIGHUP)
	}
	_ = pane.PTY.Close()
	return nil
}

func (c *controller) windowInfo(window *Window, active bool) protocol.WindowInfo {
	list := c.state.session.WindowList(clientID0)
	for _, w := range list.Windows {
		if w.WindowID == window.ID {
			w.Active = active
			return w
		}
	}
	return protocol.WindowInfo{WindowID: window.ID, PaneID: window.PaneID, Title: window.Name, Active: active}
}

func expectStreamOpen(decoder *protocol.Decoder, opener uint64, streamType string) error {
	frame, err := decoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read stream opener: %w", err)
	}
	if frame.Type != opener {
		return fmt.Errorf("unexpected stream opener %d", frame.Type)
	}
	open, err := protocol.DecodeStreamOpen(frame.Payload)
	if err != nil {
		return err
	}
	if open.StreamType != streamType {
		return fmt.Errorf("unexpected stream type %q", open.StreamType)
	}
	return nil
}

func expectDecoded[T any](decoder *protocol.Decoder, msgType uint64, decode func([]byte) (T, error)) (T, error) {
	frame, err := decoder.ReadFrame()
	if err != nil {
		var zero T
		return zero, err
	}
	if frame.Type != msgType {
		var zero T
		return zero, fmt.Errorf("unexpected message type %d", frame.Type)
	}
	return decode(frame.Payload)
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

func writeStream(stream io.Writer, frames <-chan protocol.Frame, errs chan<- error) {
	enc := protocol.NewEncoder(stream)
	for frame := range frames {
		if err := enc.WriteFrame(frame); err != nil {
			errs <- fmt.Errorf("write frame type %d: %w", frame.Type, err)
			return
		}
	}
}

func (s *sessionState) setOutputFrames(outputFrames chan protocol.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputFrames = outputFrames
}

func (s *sessionState) clearOutputFrames(outputFrames chan protocol.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputFrames == outputFrames {
		s.outputFrames = nil
	}
}

func (s *sessionState) currentOutputFrames() chan protocol.Frame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outputFrames
}
