package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"tali/internal/protocol"
)

const (
	quicMaxIdleTimeout  = 60 * time.Second
	quicKeepAlivePeriod = 10 * time.Second
)

func serveConnection(ctx context.Context, d *Daemon, conn quic.Connection) error {
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

	first, err := mgmtDecoder.ReadFrame()
	if err != nil {
		return fmt.Errorf("read session attachment: %w", err)
	}
	var s *Session
	var resumeEncoded string
	var generation uint64
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
		s, err = d.attach(attach.SessionID, attach.Token)
		if err == nil {
			resumeEncoded, generation, err = s.beginAttachment()
		}
	case protocol.MsgSessionResume:
		resume, decodeErr := protocol.DecodeSessionResume(first.Payload)
		if decodeErr != nil {
			return decodeErr
		}
		if resume.Version != protocol.ProtocolVersion {
			return errors.New("unsupported session protocol version")
		}
		s, resumeEncoded, generation, err = d.resume(resume.SessionID, resume.ResumeToken, resume.Generation)
		responseType = protocol.MsgSessionResumeOK
	default:
		return fmt.Errorf("expected session attachment, got message type %d", first.Type)
	}
	if err != nil {
		_ = sendEncodedDirect(mgmtStream, protocol.MsgSessionAttachFailed, protocol.SessionAttachFailed{Reason: "session attachment rejected"}, protocol.EncodeSessionAttachFailed)
		return err
	}
	mgmtFrames := make(chan protocol.Frame, 64)
	writerErrs := make(chan error, 4)
	go writeStream(mgmtStream, mgmtFrames, writerErrs)
	defer close(mgmtFrames)
	if responseType == protocol.MsgSessionResumeOK {
		if err := sendEncoded(mgmtFrames, protocol.MsgSessionResumeOK, protocol.SessionResumeOK{Version: protocol.ProtocolVersion, SessionID: s.ID, ResumeToken: resumeEncoded, Generation: generation}, protocol.EncodeSessionResumeOK); err != nil {
			return err
		}
	} else if err := sendEncoded(mgmtFrames, protocol.MsgSessionAttachOK, protocol.SessionAttachOK{Version: protocol.ProtocolVersion, SessionID: s.ID, ResumeToken: resumeEncoded, Generation: generation}, protocol.EncodeSessionAttachOK); err != nil {
		return err
	}
	d.logSessionAttached(s.ID)
	shell := defaultShell()

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

	handler := &Connection{
		QUIC:          conn,
		Session:       s,
		Daemon:        d,
		Management:    mgmtStream,
		Input:         inputStream,
		StatusOutput:  statusOutput,
		shell:         shell,
		managementOut: mgmtFrames,
	}
	for slot, lease := range outputLeases {
		handler.Output[slot] = lease
	}
	d.activate(s, handler)
	defer func() {
		_ = conn.CloseWithError(0, "")
		_ = s.coordinate(func() error { return s.detachLeases(outputLeases) })
		d.deactivate(s, handler)
	}()

	createPane, err := expectDecoded(mgmtDecoder, protocol.MsgCreatePane, protocol.DecodeCreatePane)
	if err != nil {
		return fmt.Errorf("read create pane: %w", err)
	}
	if err := s.coordinate(func() error {
		s.EnsureClient(clientID0)
		s.SetClientSize(clientID0, createPane.Cols, createPane.Rows)
		if !s.HasWindows() {
			cwd, err := resolveStartingDirectory(createPane.Cwd)
			if err != nil {
				s.shutdownNow()
				return err
			}
			s.defaultCwd = cwd
			initialPane, _, _, err := s.createWindow(handler, s.defaultCwd, createPane.Argv, createPane.Cols, createPane.Rows)
			if err != nil {
				s.shutdownNow()
				return err
			}
			if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: initialPane.ID}, protocol.EncodePaneCreated); err != nil {
				return err
			}
			if err := handler.Session.publishStatusBar(); err != nil {
				return err
			}
			if err := handler.Session.publishWindowLayout(); err != nil {
				return err
			}
			s.startPane(initialPane)
			return handler.Session.publishBindingsAndSnapshots(nil)
		}

		handoff := handler.Session.beginOutputHandoff()
		s.ResizeAll(createPane.Cols, createPane.Rows)
		_, pane, _, err := s.ReattachClient(clientID0)
		if err != nil {
			return err
		}
		if err := sendEncoded(mgmtFrames, protocol.MsgPaneCreated, protocol.PaneCreated{PaneID: pane.ID}, protocol.EncodePaneCreated); err != nil {
			return err
		}
		if err := handler.Session.publishStatusBar(); err != nil {
			return err
		}
		if err := handler.Session.publishWindowLayout(); err != nil {
			return err
		}
		return handler.Session.publishBindingsAndSnapshots(handoff)
	}); err != nil {
		return err
	}
	mgmtErrs := make(chan error, 1)
	inputErrs := make(chan error, 1)
	go handler.handleManagement(mgmtDecoder, mgmtErrs)
	go s.readInput(handler, inputErrs)

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

func (c *Connection) handleManagement(decoder *protocol.Decoder, done chan<- error) {
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
		case protocol.MsgPing:
			msg, err := protocol.DecodePing(frame.Payload)
			if err != nil {
				done <- err
				return
			}
			if err := sendEncoded(c.managementOut, protocol.MsgPong, protocol.Pong{
				Seq:           msg.Seq,
				SentUnixMilli: msg.SentUnixMilli,
			}, protocol.EncodePong); err != nil {
				done <- err
				return
			}
		}
	}
}

func terminatePane(pane *Pane) error {
	if pane == nil {
		return nil
	}
	if pane.Process != nil && pane.Process.Process != nil {
		_ = pane.Process.Process.Signal(syscall.SIGHUP)
	}
	pane.stop()
	return nil
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

func sendEncodedDirect[T any](w io.Writer, msgType uint64, msg T, encode func([]byte, T) ([]byte, error)) error {
	// Kept separate from the asynchronous stream writer for pre-attachment
	// failures, where no session state may be touched.
	payload, err := encode(nil, msg)
	if err != nil {
		return err
	}
	return protocol.NewEncoder(w).WriteFrame(protocol.Frame{Type: msgType, Payload: payload})
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
