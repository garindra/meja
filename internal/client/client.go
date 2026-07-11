package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	osuser "os/user"
	"strings"
	"sync"
	"syscall"

	"github.com/quic-go/quic-go"
	"golang.org/x/term"

	"tali/internal/auth"
	"tali/internal/client/render"
	"tali/internal/protocol"
	"tali/internal/sshconfig"
)

type Target struct {
	Username        string
	Hostname        string
	HasExplicitUser bool
}

type Config struct {
	Target       Target
	Port         int
	PortSet      bool
	CAFile       string
	IdentityFile string
	Cwd          string
	Argv         []string
	Stdin        *os.File
	Stdout       io.Writer
	Stderr       io.Writer
}

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
	conn, err := quic.DialAddr(ctx, addr, tlsConfig, nil)
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

	mgmtFrames := make(chan protocol.Frame, 32)
	inputFrames := make(chan protocol.Frame, 64)
	streamErrs := make(chan error, 8)
	go writeFrames(mgmtStream, mgmtFrames, streamErrs)
	go writeFrames(inputStream, inputFrames, streamErrs)
	defer close(mgmtFrames)
	defer close(inputFrames)

	if err := enqueueMessage(mgmtFrames, protocol.MsgOpenManagementStream, protocol.StreamOpen{StreamType: protocol.StreamTypeManagement}); err != nil {
		return err
	}
	if err := enqueueMessage(mgmtFrames, protocol.MsgClientHello, protocol.ClientHello{Version: 1}); err != nil {
		return err
	}
	if err := enqueueMessage(inputFrames, protocol.MsgOpenInputStream, protocol.StreamOpen{StreamType: protocol.StreamTypeInput}); err != nil {
		return err
	}

	mgmtDecoder := protocol.NewDecoder(mgmtStream, protocol.DefaultMaxFrameSize)
	outputReady := make(chan uint64, 1)
	sessionDone := make(chan error, 1)
	go acceptOutputStream(ctx, conn, cfg.Stdout, mgmtFrames, outputReady, sessionDone)

	if err := enqueueMessage(mgmtFrames, protocol.MsgAuthBegin, protocol.AuthBegin{
		Username:  resolved.Username,
		PublicKey: identity.AuthorizedKey(),
	}); err != nil {
		return err
	}

	challengeFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth challenge: %w", err)
	}
	if challengeFrame.Type == protocol.MsgAuthFailed {
		var failed protocol.AuthFailed
		if err := protocol.DecodeMessage(challengeFrame, &failed); err != nil {
			return err
		}
		return fmt.Errorf("authentication failed: %s", failed.Reason)
	}
	if challengeFrame.Type != protocol.MsgAuthChallenge {
		return fmt.Errorf("unexpected auth message type %d", challengeFrame.Type)
	}

	var challenge protocol.AuthChallenge
	if err := protocol.DecodeMessage(challengeFrame, &challenge); err != nil {
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
	if err := enqueueMessage(mgmtFrames, protocol.MsgAuthResponse, protocol.AuthResponse{
		ChallengeID: challenge.ChallengeID,
		Signature:   signature,
	}); err != nil {
		return err
	}

	authResult, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read auth result: %w", err)
	}
	switch authResult.Type {
	case protocol.MsgAuthOK:
		var ok protocol.AuthOK
		if err := protocol.DecodeMessage(authResult, &ok); err != nil {
			return err
		}
	case protocol.MsgAuthFailed:
		var failed protocol.AuthFailed
		if err := protocol.DecodeMessage(authResult, &failed); err != nil {
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
	rawState, err := term.MakeRaw(int(cfg.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	var restoreOnce sync.Once
	restoreTerminal := func() {
		restoreOnce.Do(func() {
			_, _ = io.WriteString(cfg.Stdout, "\x1b[?25h\x1b[0m")
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

	if err := enqueueMessage(mgmtFrames, protocol.MsgCreatePane, protocol.CreatePane{
		Cwd:  cfg.Cwd,
		Argv: cfg.Argv,
		Cols: cols,
		Rows: rows,
	}); err != nil {
		return err
	}

	createdFrame, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read pane created: %w", err)
	}
	if createdFrame.Type != protocol.MsgPaneCreated {
		return fmt.Errorf("unexpected pane message type %d", createdFrame.Type)
	}
	var paneCreated protocol.PaneCreated
	if err := protocol.DecodeMessage(createdFrame, &paneCreated); err != nil {
		return err
	}

	select {
	case paneID := <-outputReady:
		if paneID != paneCreated.PaneID {
			return fmt.Errorf("pane output stream mismatch: %d != %d", paneID, paneCreated.PaneID)
		}
	case err := <-sessionDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go forwardInput(copyCtx, cfg.Stdin, inputFrames, paneCreated.PaneID, streamErrs)
	go forwardResize(copyCtx, cfg.Stdin, inputFrames, paneCreated.PaneID, streamErrs)

	for {
		select {
		case err := <-streamErrs:
			if err != nil {
				return err
			}
		case err := <-sessionDone:
			return err
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		frame, err := mgmtDecoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read management frame: %w", err)
		}
		if frame.Type != protocol.MsgPaneExited {
			continue
		}
		var exited protocol.PaneExited
		if err := protocol.DecodeMessage(frame, &exited); err != nil {
			return err
		}
		if exited.ExitCode != 0 {
			return fmt.Errorf("remote pane exited with code %d signal %s", exited.ExitCode, exited.Signal)
		}
		return nil
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

func acceptOutputStream(ctx context.Context, conn quic.Connection, stdout io.Writer, mgmtFrames chan<- protocol.Frame, outputReady chan<- uint64, sessionDone chan<- error) {
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

	var open protocol.StreamOpen
	if err := protocol.DecodeMessage(openFrame, &open); err != nil {
		sessionDone <- err
		return
	}
	outputReady <- open.PaneID
	state := render.NewClientState()

	for {
		frame, err := decoder.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				sessionDone <- nil
				return
			}
			sessionDone <- fmt.Errorf("read output frame: %w", err)
			return
		}
		needsRedraw := false
		switch frame.Type {
		case protocol.MsgReplacePane:
			var msg protocol.ReplacePane
			if err := protocol.DecodeMessage(frame, &msg); err != nil {
				sessionDone <- err
				return
			}
			state.ApplyReplace(msg)
			needsRedraw = true
		case protocol.MsgDefineStyle:
			var msg protocol.DefineStyle
			if err := protocol.DecodeMessage(frame, &msg); err != nil {
				sessionDone <- err
				return
			}
			state.DefineStyle(msg)
		case protocol.MsgSetRun:
			var msg protocol.SetRun
			if err := protocol.DecodeMessage(frame, &msg); err != nil {
				sessionDone <- err
				return
			}
			if !state.ApplySetRun(msg) {
				_ = enqueueMessage(mgmtFrames, protocol.MsgRequestPaneSnapshot, protocol.RequestPaneSnapshot{PaneID: msg.PaneID})
				continue
			}
			needsRedraw = true
		case protocol.MsgSetCursor:
			var msg protocol.SetCursor
			if err := protocol.DecodeMessage(frame, &msg); err != nil {
				sessionDone <- err
				return
			}
			if !state.ApplySetCursor(msg) {
				_ = enqueueMessage(mgmtFrames, protocol.MsgRequestPaneSnapshot, protocol.RequestPaneSnapshot{PaneID: msg.PaneID})
				continue
			}
			needsRedraw = true
		case protocol.MsgSetCursorVisible:
			var msg protocol.SetCursorVisible
			if err := protocol.DecodeMessage(frame, &msg); err != nil {
				sessionDone <- err
				return
			}
			if !state.ApplySetCursorVisible(msg) {
				_ = enqueueMessage(mgmtFrames, protocol.MsgRequestPaneSnapshot, protocol.RequestPaneSnapshot{PaneID: msg.PaneID})
				continue
			}
			needsRedraw = true
		case protocol.MsgPaneExited:
			var exited protocol.PaneExited
			if err := protocol.DecodeMessage(frame, &exited); err != nil {
				sessionDone <- err
				return
			}
			if exited.ExitCode == 0 {
				sessionDone <- nil
				return
			}
			sessionDone <- fmt.Errorf("remote pane exited with code %d signal %s", exited.ExitCode, exited.Signal)
			return
		default:
			sessionDone <- fmt.Errorf("unexpected output message %d", frame.Type)
			return
		}
		if needsRedraw {
			if _, err := stdout.Write(render.RenderANSI(state)); err != nil {
				sessionDone <- fmt.Errorf("write stdout: %w", err)
				return
			}
		}
	}
}

func forwardInput(ctx context.Context, stdin *os.File, inputFrames chan<- protocol.Frame, paneID uint64, errs chan<- error) {
	buf := make([]byte, 32*1024)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if sendErr := enqueueMessage(inputFrames, protocol.MsgInputBytes, protocol.InputBytes{
				PaneID: paneID,
				Data:   append([]byte(nil), buf[:n]...),
			}); sendErr != nil {
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

func forwardResize(ctx context.Context, tty *os.File, inputFrames chan<- protocol.Frame, paneID uint64, errs chan<- error) {
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
			if err := enqueueMessage(inputFrames, protocol.MsgResizePane, protocol.ResizePane{PaneID: paneID, Cols: cols, Rows: rows}); err != nil {
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

func enqueueMessage(ch chan<- protocol.Frame, msgType uint64, v any) error {
	frame, err := protocol.EncodeMessage(msgType, v)
	if err != nil {
		return err
	}
	defer func() { recover() }()
	ch <- frame
	return nil
}

func currentUsername() (string, error) {
	current, err := osuser.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current username: %w", err)
	}
	return strings.TrimSpace(current.Username), nil
}
