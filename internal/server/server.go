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
	"syscall"

	"github.com/quic-go/quic-go"

	"tali/internal/auth"
	"tali/internal/protocol"
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
	s := &session{
		conn:     conn,
		verifier: auth.NewVerifier(),
	}
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

	helloFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if helloFrame.Type != protocol.MsgClientHello {
		return fmt.Errorf("unexpected management message %d", helloFrame.Type)
	}

	var hello protocol.ClientHello
	if err := protocol.DecodeMessage(helloFrame, &hello); err != nil {
		return err
	}
	if hello.Version != 1 {
		return errors.New("unsupported client protocol version")
	}

	mgmtFrames := make(chan protocol.Frame, 16)
	writerErrs := make(chan error, 4)
	go writeStream(s.mgmtStream, mgmtFrames, writerErrs)
	defer close(mgmtFrames)

	authBeginFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth begin: %w", err)
	}
	if authBeginFrame.Type != protocol.MsgAuthBegin {
		return fmt.Errorf("unexpected auth begin type %d", authBeginFrame.Type)
	}

	var authBegin protocol.AuthBegin
	if err := protocol.DecodeMessage(authBeginFrame, &authBegin); err != nil {
		return err
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

	authResponseFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth response: %w", err)
	}
	if authResponseFrame.Type != protocol.MsgAuthResponse {
		return fmt.Errorf("unexpected auth response type %d", authResponseFrame.Type)
	}

	var authResponse protocol.AuthResponse
	if err := protocol.DecodeMessage(authResponseFrame, &authResponse); err != nil {
		return err
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

	createPaneFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read create pane: %w", err)
	}
	if createPaneFrame.Type != protocol.MsgCreatePane {
		return fmt.Errorf("unexpected create pane type %d", createPaneFrame.Type)
	}

	var createPane protocol.CreatePane
	if err := protocol.DecodeMessage(createPaneFrame, &createPane); err != nil {
		return err
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

	outputFrames := make(chan protocol.Frame, 64)
	go writeStream(outputStream, outputFrames, writerErrs)
	defer close(outputFrames)

	if err := sendFrame(outputFrames, protocol.MsgOpenPaneOutputStream, protocol.StreamOpen{
		StreamType: protocol.StreamTypePaneOutput,
		PaneID:     s.pane.ID,
	}); err != nil {
		return err
	}
	if err := sendFrame(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: s.pane.ID}); err != nil {
		return err
	}

	paneIOErrs := make(chan error, 2)
	go relayPTYOutput(s.pane, outputFrames, paneIOErrs)
	go handleInput(s.pane, inputDecoder, paneIOErrs)

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
	case <-ctx.Done():
		if s.pane != nil && s.pane.Cmd.Process != nil {
			_ = s.pane.Cmd.Process.Signal(syscall.SIGHUP)
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

func relayPTYOutput(pane *Pane, outputFrames chan<- protocol.Frame, done chan<- error) {
	buf := make([]byte, 32*1024)
	for {
		n, err := pane.PTY.Read(buf)
		if n > 0 {
			if sendErr := sendFrame(outputFrames, protocol.MsgPTYOutput, protocol.PTYOutput{
				Data: append([]byte(nil), buf[:n]...),
			}); sendErr != nil {
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

func handleInput(pane *Pane, inputDecoder *protocol.Decoder, done chan<- error) {
	for {
		frame, err := inputDecoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if pane.Cmd.Process != nil {
					_ = pane.Cmd.Process.Signal(syscall.SIGHUP)
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
				done <- fmt.Errorf("unknown pane id %q", msg.PaneID)
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
				done <- fmt.Errorf("unknown pane id %q", msg.PaneID)
				return
			}
			if err := pane.Resize(msg.Cols, msg.Rows); err != nil {
				done <- fmt.Errorf("resize pty: %w", err)
				return
			}
		default:
			done <- fmt.Errorf("unexpected input frame %d", frame.Type)
			return
		}
	}
}

func paneExitMessage(pane *Pane, mgmtFrames, outputFrames chan<- protocol.Frame) error {
	err := pane.Cmd.Wait()
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
	defer func() {
		recover()
	}()
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
