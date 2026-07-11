package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"sort"
	"syscall"

	"github.com/quic-go/quic-go"

	"tali/internal/auth"
	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

const (
	sessionID0 = 0
	windowID0  = 0
	paneID0    = 0
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

type session struct {
	conn        quic.Connection
	verifier    *auth.Verifier
	mgmtStream  quic.Stream
	inputStream quic.Stream
	unixUser    *user.User
	fingerprint string
	pane        *Pane
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

	listener, err := quic.ListenAddr(cfg.ListenAddr, tlsConfig, nil)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}

		go func(conn quic.Connection) {
			if err := handleSession(ctx, conn); err != nil && cfg.Stderr != nil {
				fmt.Fprintf(cfg.Stderr, "session error: %v\n", err)
			}
		}(conn)
	}
}

func handleSession(ctx context.Context, conn quic.Connection) error {
	s := &session{conn: conn, verifier: auth.NewVerifier()}
	defer conn.CloseWithError(0, "")

	var err error
	s.mgmtStream, err = conn.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("accept management stream: %w", err)
	}
	s.inputStream, err = conn.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("accept input stream: %w", err)
	}

	mgmtDecoder := protocol.NewDecoder(s.mgmtStream, protocol.DefaultMaxFrameSize)
	inputDecoder := protocol.NewDecoder(s.inputStream, protocol.DefaultMaxFrameSize)
	if err := expectStreamOpen(mgmtDecoder, protocol.MsgOpenManagementStream, protocol.StreamTypeManagement); err != nil {
		return err
	}
	if err := expectStreamOpen(inputDecoder, protocol.MsgOpenInputStream, protocol.StreamTypeInput); err != nil {
		return err
	}

	var hello protocol.ClientHello
	if err := expectMessage(mgmtDecoder, protocol.MsgClientHello, &hello); err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if hello.Version != 1 {
		return errors.New("unsupported client protocol version")
	}

	mgmtFrames := make(chan protocol.Frame, 32)
	writerErrs := make(chan error, 4)
	go writeStream(s.mgmtStream, mgmtFrames, writerErrs)
	defer close(mgmtFrames)

	var authBegin protocol.AuthBegin
	if err := expectMessage(mgmtDecoder, protocol.MsgAuthBegin, &authBegin); err != nil {
		return fmt.Errorf("read auth begin: %w", err)
	}

	beginResult, err := s.verifier.Begin(authBegin.Username, authBegin.PublicKey)
	if err != nil {
		_ = sendFrame(mgmtFrames, protocol.MsgAuthFailed, protocol.AuthFailed{Reason: err.Error()})
		return fmt.Errorf("begin auth: %w", err)
	}
	s.unixUser = beginResult.User
	s.fingerprint = beginResult.Fingerprint

	if err := sendFrame(mgmtFrames, protocol.MsgAuthChallenge, protocol.AuthChallenge{
		ChallengeID: beginResult.Challenge.ID,
		Nonce:       beginResult.Challenge.Nonce,
		ExpiresAt:   beginResult.Challenge.ExpiresAt,
	}); err != nil {
		return err
	}

	var authResponse protocol.AuthResponse
	if err := expectMessage(mgmtDecoder, protocol.MsgAuthResponse, &authResponse); err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	if err := s.verifier.Verify(authBegin.Username, s.fingerprint, authResponse.ChallengeID, authResponse.Signature); err != nil {
		_ = sendFrame(mgmtFrames, protocol.MsgAuthFailed, protocol.AuthFailed{Reason: err.Error()})
		return fmt.Errorf("verify auth response: %w", err)
	}

	shell := loginShellForUser(s.unixUser)
	if err := sendFrame(mgmtFrames, protocol.MsgAuthOK, protocol.AuthOK{
		Username: s.unixUser.Username,
		HomeDir:  s.unixUser.HomeDir,
		Shell:    shell,
	}); err != nil {
		return err
	}

	var createPane protocol.CreatePane
	if err := expectMessage(mgmtDecoder, protocol.MsgCreatePane, &createPane); err != nil {
		return fmt.Errorf("read create pane: %w", err)
	}

	s.pane, err = StartPane(s.unixUser, paneRequest{
		Cwd:     createPane.Cwd,
		Command: createPane.Argv,
		Cols:    createPane.Cols,
		Rows:    createPane.Rows,
		Shell:   shell,
	})
	if err != nil {
		return fmt.Errorf("start pane: %w", err)
	}
	defer s.pane.PTY.Close()

	outputStream, err := s.conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open output stream: %w", err)
	}
	outputFrames := make(chan protocol.Frame, 128)
	go writeStream(outputStream, outputFrames, writerErrs)
	defer close(outputFrames)

	if err := sendFrame(outputFrames, protocol.MsgOpenPaneOutputStream, protocol.StreamOpen{
		StreamType: protocol.StreamTypePaneOutput,
		PaneID:     s.pane.ID,
	}); err != nil {
		return err
	}
	if err := sendReplaceSnapshot(outputFrames, s.pane); err != nil {
		return err
	}
	if err := sendFrame(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: s.pane.ID}); err != nil {
		return err
	}

	paneIOErrs := make(chan error, 2)
	go relayPTYToTerminal(s.pane, outputFrames, paneIOErrs)
	go handleInput(s.pane, inputDecoder, outputFrames, paneIOErrs)

	mgmtErrs := make(chan error, 1)
	go handleManagementRequests(mgmtDecoder, s.pane, outputFrames, mgmtErrs)

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- paneExitMessage(s.pane, mgmtFrames, outputFrames)
	}()

	select {
	case err := <-paneIOErrs:
		return err
	case err := <-writerErrs:
		return err
	case err := <-waitDone:
		return err
	case err := <-mgmtErrs:
		return err
	case <-ctx.Done():
		if s.pane != nil && s.pane.Process.Process != nil {
			_ = s.pane.Process.Process.Signal(syscall.SIGHUP)
		}
		return ctx.Err()
	}
}

func expectStreamOpen(decoder *protocol.Decoder, opener uint64, streamType string) error {
	frame, err := decoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read stream opener: %w", err)
	}
	if frame.Type != opener {
		return fmt.Errorf("unexpected stream opener %d", frame.Type)
	}

	var open protocol.StreamOpen
	if err := protocol.DecodeMessage(frame, &open); err != nil {
		return err
	}
	if open.StreamType != streamType {
		return fmt.Errorf("unexpected stream type %q", open.StreamType)
	}
	return nil
}

func expectMessage(decoder *protocol.Decoder, msgType uint64, v any) error {
	frame, err := decoder.ReadFrame()
	if err != nil {
		return err
	}
	if frame.Type != msgType {
		return fmt.Errorf("unexpected message type %d", frame.Type)
	}
	return protocol.DecodeMessage(frame, v)
}

func relayPTYToTerminal(pane *Pane, outputFrames chan<- protocol.Frame, done chan<- error) {
	buf := make([]byte, 32*1024)
	for {
		n, err := pane.PTY.Read(buf)
		if n > 0 {
			update := pane.Terminal.Apply(buf[:n])
			if sendErr := emitTerminalUpdate(pane, outputFrames, update); sendErr != nil {
				done <- sendErr
				return
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrClosed) || errors.Is(err, io.EOF) {
				return
			}
			done <- fmt.Errorf("read pty: %w", err)
			return
		}
	}
}

func emitTerminalUpdate(pane *Pane, outputFrames chan<- protocol.Frame, update terminal.Update) error {
	if update.FullRedraw {
		return sendReplaceSnapshot(outputFrames, pane)
	}

	if len(update.DefinedStyles) > 0 {
		ids := make([]int, 0, len(update.DefinedStyles))
		for id := range update.DefinedStyles {
			ids = append(ids, int(id))
		}
		sort.Ints(ids)
		for _, rawID := range ids {
			id := uint32(rawID)
			if err := sendFrame(outputFrames, protocol.MsgDefineStyle, protocol.DefineStyle{
				PaneID: pane.ID,
				ID:     id,
				Style:  update.DefinedStyles[id],
			}); err != nil {
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
		if err := sendFrame(outputFrames, protocol.MsgSetRun, protocol.SetRun{
			SessionID:      sessionID0,
			WindowID:       windowID0,
			PaneID:         pane.ID,
			BaseGeneration: base,
			Generation:     pane.Generation,
			Row:            row,
			Column:         0,
			Cells:          append([]protocol.Cell(nil), pane.Terminal.Cells[start:end]...),
		}); err != nil {
			return err
		}
	}
	if update.CursorChanged {
		base := pane.Generation
		pane.Generation++
		if err := sendFrame(outputFrames, protocol.MsgSetCursor, protocol.SetCursor{
			SessionID:      sessionID0,
			WindowID:       windowID0,
			PaneID:         pane.ID,
			BaseGeneration: base,
			Generation:     pane.Generation,
			Cursor: protocol.Cursor{
				X: pane.Terminal.CursorX,
				Y: pane.Terminal.CursorY,
			},
		}); err != nil {
			return err
		}
	}
	if update.VisibleChange {
		base := pane.Generation
		pane.Generation++
		if err := sendFrame(outputFrames, protocol.MsgSetCursorVisible, protocol.SetCursorVisible{
			SessionID:      sessionID0,
			WindowID:       windowID0,
			PaneID:         pane.ID,
			BaseGeneration: base,
			Generation:     pane.Generation,
			Visible:        pane.Terminal.CursorVisible,
		}); err != nil {
			return err
		}
	}
	return nil
}

func sendReplaceSnapshot(outputFrames chan<- protocol.Frame, pane *Pane) error {
	pane.Generation++
	styleDefs := pane.Terminal.SnapshotStyles()
	styles := make([]protocol.StyleDefinition, 0, len(styleDefs))
	for _, def := range styleDefs {
		styles = append(styles, protocol.StyleDefinition{ID: def.ID, Style: def.Style})
	}
	return sendFrame(outputFrames, protocol.MsgReplacePane, protocol.ReplacePane{
		SessionID:     sessionID0,
		WindowID:      windowID0,
		PaneID:        pane.ID,
		Generation:    pane.Generation,
		Cols:          pane.Terminal.Cols,
		Rows:          pane.Terminal.Rows,
		Cells:         append([]protocol.Cell(nil), pane.Terminal.Cells...),
		Styles:        styles,
		Cursor:        protocol.Cursor{X: pane.Terminal.CursorX, Y: pane.Terminal.CursorY},
		CursorVisible: pane.Terminal.CursorVisible,
	})
}

func handleInput(pane *Pane, inputDecoder *protocol.Decoder, outputFrames chan<- protocol.Frame, done chan<- error) {
	for {
		frame, err := inputDecoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if pane.Process.Process != nil {
					_ = pane.Process.Process.Signal(syscall.SIGHUP)
				}
				return
			}
			done <- fmt.Errorf("read input frame: %w", err)
			return
		}

		switch frame.Type {
		case protocol.MsgInputBytes:
			var msg protocol.InputBytes
			if err := protocol.DecodeMessage(frame, &msg); err != nil {
				done <- err
				return
			}
			if msg.PaneID != pane.ID {
				done <- fmt.Errorf("unknown pane id %d", msg.PaneID)
				return
			}
			if _, err := pane.PTY.Write(msg.Data); err != nil {
				done <- fmt.Errorf("write pty: %w", err)
				return
			}
		case protocol.MsgResizePane:
			var msg protocol.ResizePane
			if err := protocol.DecodeMessage(frame, &msg); err != nil {
				done <- err
				return
			}
			if msg.PaneID != pane.ID {
				done <- fmt.Errorf("unknown pane id %d", msg.PaneID)
				return
			}
			if err := pane.Resize(msg.Cols, msg.Rows); err != nil {
				done <- fmt.Errorf("resize pty: %w", err)
				return
			}
			pane.Terminal.Resize(int(msg.Cols), int(msg.Rows))
			if err := sendReplaceSnapshot(outputFrames, pane); err != nil {
				done <- err
				return
			}
		default:
			done <- fmt.Errorf("unexpected input frame %d", frame.Type)
			return
		}
	}
}

func handleManagementRequests(decoder *protocol.Decoder, pane *Pane, outputFrames chan<- protocol.Frame, done chan<- error) {
	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			done <- fmt.Errorf("read management frame: %w", err)
			return
		}
		switch frame.Type {
		case protocol.MsgRequestPaneSnapshot:
			var req protocol.RequestPaneSnapshot
			if err := protocol.DecodeMessage(frame, &req); err != nil {
				done <- err
				return
			}
			if req.PaneID != pane.ID {
				done <- fmt.Errorf("unknown pane id %d in snapshot request", req.PaneID)
				return
			}
			if err := sendReplaceSnapshot(outputFrames, pane); err != nil {
				done <- err
				return
			}
		case protocol.MsgPaneExited:
			return
		default:
			// Ignore messages already handled during setup.
		}
	}
}

func paneExitMessage(pane *Pane, mgmtFrames, outputFrames chan<- protocol.Frame) error {
	err := pane.Process.Wait()
	exitCode := 0
	signalName := ""
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signalName = status.Signal().String()
			}
		} else {
			return fmt.Errorf("wait for pane: %w", err)
		}
	}

	msg := protocol.PaneExited{
		PaneID:   pane.ID,
		ExitCode: exitCode,
		Signal:   signalName,
	}
	if sendErr := sendFrame(mgmtFrames, protocol.MsgPaneExited, msg); sendErr != nil {
		return sendErr
	}
	if sendErr := sendFrame(outputFrames, protocol.MsgPaneExited, msg); sendErr != nil {
		return sendErr
	}
	if exitCode != 0 {
		return fmt.Errorf("remote pane exited with code %d signal %s", exitCode, signalName)
	}
	return nil
}

func sendFrame(ch chan<- protocol.Frame, msgType uint64, v any) error {
	frame, err := protocol.EncodeMessage(msgType, v)
	if err != nil {
		return err
	}
	defer func() { recover() }()
	ch <- frame
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
