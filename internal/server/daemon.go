package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

const attachTTL = 2 * time.Minute

type Config struct {
	ControlPath      string
	Stdout           io.Writer
	Stderr           io.Writer
	TerminalDebugLog io.Writer
}

// Daemon owns the server-wide session registry, client-instance registry,
// one-to-one assignments, attach grants, and shared QUIC listener. All
// registry access is serialized by requests; control handlers query it and
// then query Sessions separately, so neither actor waits on the other.
type Daemon struct {
	logMu                sync.Mutex
	requests             chan daemonRequest
	nextID               uint64
	sessions             map[uint64]*Session
	names                map[string]*Session
	reconnectCredentials map[string]*reconnectCredential
	// clientSessions is separate from reconnectCredentials: it is only the
	// target-session hint consulted when rebuilding a client after reconnect.
	clientSessions        map[*reconnectCredential]uint64
	attachments           map[uint64]*reconnectCredential
	attachGrants          []attachGrant
	tlsConfig             *tls.Config
	certHash              string
	quicMu                sync.Mutex
	quicListener          *quic.Listener
	quicPort              uint16
	quicCancel            context.CancelFunc
	serverCtx             context.Context
	stop                  context.CancelFunc
	stderr                io.Writer
	controlPath           string
	sessionPersistenceDir string
}

type daemonRequest struct {
	run  func()
	done chan struct{}
}

func (d *Daemon) runRequests(ctx context.Context) {
	for {
		select {
		case request := <-d.requests:
			request.run()
			if request.done != nil {
				close(request.done)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *Daemon) call(run func()) {
	if d.requests == nil {
		run()
		return
	}
	done := make(chan struct{})
	d.requests <- daemonRequest{run: run, done: done}
	<-done
}

func (d *Daemon) post(run func()) {
	if d.requests == nil {
		run()
		return
	}
	d.requests <- daemonRequest{run: run}
}

func Run(ctx context.Context, cfg Config) error {
	serverCtx, stop := context.WithCancel(context.Background())
	defer stop()
	setTerminalDebugLogger(cfg.TerminalDebugLog)
	socket := cfg.ControlPath
	if socket == "" {
		var err error
		socket, err = defaultCommandSocketPath()
		if err != nil {
			return err
		}
	}
	lock, err := acquireCommandServerLock(socket)
	if err != nil {
		return err
	}
	defer lock.Close()
	cert, hash, err := daemonCertificate()
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	d := &Daemon{
		requests:              make(chan daemonRequest, 64),
		nextID:                1,
		sessions:              make(map[uint64]*Session),
		names:                 make(map[string]*Session),
		reconnectCredentials:  make(map[string]*reconnectCredential),
		clientSessions:        make(map[*reconnectCredential]uint64),
		attachments:           make(map[uint64]*reconnectCredential),
		tlsConfig:             tlsConfig,
		certHash:              hash,
		serverCtx:             serverCtx,
		stop:                  stop,
		stderr:                cfg.Stderr,
		controlPath:           socket,
		sessionPersistenceDir: sessionPersistenceDirectory(socket),
	}
	actorCtx, stopActor := context.WithCancel(context.Background())
	go d.runRequests(actorCtx)
	defer func() {
		d.disconnectActiveClients()
		d.closeQUIC()
		stopActor()
	}()
	go func() {
		select {
		case <-ctx.Done():
			d.disconnectActiveClients()
			stop()
		case <-serverCtx.Done():
		}
	}()
	err = serveCommandSocket(serverCtx, socket, d)
	if err != nil {
		stop()
	}
	return err
}

func sessionPersistenceDirectory(controlPath string) string {
	return filepath.Join(filepath.Dir(controlPath), "sessions")
}

func (d *Daemon) logSessionAttached(sessionID uint64) {
	d.logf("meja server: session %d attached\n", sessionID)
}

func (d *Daemon) logf(format string, args ...any) {
	if d.stderr == nil {
		return
	}
	d.logMu.Lock()
	defer d.logMu.Unlock()
	fmt.Fprintf(d.stderr, format, args...)
}

func daemonCertificate() (tls.Certificate, string, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "meja-daemon"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"meja-daemon"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, publicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	hash := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	return cert, hex.EncodeToString(hash[:]), nil
}

// disconnectActiveClients uses the same clean QUIC close path as an explicit
// client detach (Ctrl-B, d). The client restores its terminal and does not
// receive a protocol error or a synthetic input event.
func (d *Daemon) disconnectActiveClients() {
	var sessions []*Session
	d.call(func() {
		sessions = make([]*Session, 0, len(d.sessions))
		for _, session := range d.sessions {
			sessions = append(sessions, session)
		}
	})
	for _, session := range sessions {
		_ = session.shutdown()
	}
}

func newSession(id uint64, name string) *Session {
	session := NewSession(id)
	session.setSessionName(name)
	return session
}

// sessionByName runs only on the daemon actor.
func (d *Daemon) sessionByName(name string) *Session {
	if state := d.names[name]; state != nil {
		return state
	}
	for _, state := range d.sessions {
		if state != nil && state.SessionName() == name {
			return state
		}
	}
	return nil
}

func (d *Daemon) renameSession(state *Session, name string) error {
	var renameErr error
	d.call(func() {
		renameErr = d.validateSessionRename(state, name)
		if renameErr == nil {
			d.reserveSessionName(state, name)
		}
	})
	if renameErr == nil {
		state.setSessionName(name)
	}
	return renameErr
}

func (d *Daemon) validateSessionRename(state *Session, name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	if d.sessions[state.ID] != state {
		return errSessionUnavailable
	}
	if existing := d.sessionByName(name); existing != nil && existing != state {
		return fmt.Errorf("session %q already exists", name)
	}
	return nil
}

// requestSessionRename is deliberately one-way. The Session actor never waits
// on the Daemon actor; the Daemon posts the accepted/rejected result back.
func (d *Daemon) requestSessionRename(state *Session, currentName, name string) {
	run := func() { d.prepareSessionRename(state, currentName, name) }
	if d.requests == nil {
		run()
		return
	}
	d.post(run)
}

func (d *Daemon) prepareSessionRename(state *Session, currentName, name string) {
	if err := d.validateSessionRename(state, name); err != nil {
		d.deliverSessionResult(state, func() error { return state.finishSessionRename(name, false) })
		return
	}
	if currentName != name {
		exists, err := sessionPersistenceFileExists(d.sessionPersistenceDir, name)
		if err != nil {
			d.logf("meja server: check persistence before renaming session %d: %v\n", state.ID, err)
			d.deliverSessionResult(state, func() error { return state.finishSessionRename(name, false) })
			return
		}
		if exists {
			label := fmt.Sprintf("persisted session %q exists; overwrite? (y/N) ", name)
			d.deliverSessionResult(state, func() error {
				_, err := state.beginConfirmationPrompt(clientID0, label, func(result promptResult) error {
					if !result.Accepted {
						return state.publishStatusBar()
					}
					d.confirmSessionRename(state, name)
					return state.publishStatusBar()
				})
				if err != nil {
					return err
				}
				return state.publishStatusBar()
			})
			return
		}
	}
	d.reserveSessionName(state, name)
	d.deliverSessionResult(state, func() error { return state.finishSessionRename(name, true) })
}

func (d *Daemon) confirmSessionRename(state *Session, name string) {
	run := func() {
		err := d.validateSessionRename(state, name)
		if err == nil {
			d.reserveSessionName(state, name)
		}
		d.deliverSessionResult(state, func() error { return state.finishSessionRename(name, err == nil) })
	}
	if d.requests == nil {
		run()
		return
	}
	d.post(run)
}

func (d *Daemon) deliverSessionResult(state *Session, run func() error) {
	if d.requests == nil {
		_ = run()
		return
	}
	state.post(run)
}

func sessionPersistenceFileExists(sessionPersistenceDir, name string) (bool, error) {
	_, err := os.Stat(filepath.Join(sessionPersistenceDir, name+".session.meja"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (d *Daemon) reserveSessionName(state *Session, name string) {
	if d.names == nil {
		d.names = make(map[string]*Session)
	}
	for existingName, existing := range d.names {
		if existing == state {
			delete(d.names, existingName)
		}
	}
	if name != "" {
		d.names[name] = state
	}
}

// sessionExited is a one-way death notification. The Session has already
// released its client instance; the Daemon only removes matching registry refs.
func (d *Daemon) sessionExited(state *Session) {
	remove := func() {
		if d.sessions[state.ID] != state {
			return
		}
		if credential := d.attachments[state.ID]; credential != nil {
			delete(d.attachments, state.ID)
			delete(d.clientSessions, credential)
			credential.TerminalReason = "session is no longer available"
		}
		delete(d.sessions, state.ID)
		d.reserveSessionName(state, "")
	}
	if d.requests == nil {
		remove()
		return
	}
	d.post(remove)
}

func (d *Daemon) session(id uint64) *Session {
	var session *Session
	d.call(func() { session = d.sessions[id] })
	return session
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
