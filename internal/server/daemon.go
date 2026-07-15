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
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/garindra/meja/internal/control"
	"github.com/garindra/meja/internal/protocol"
	"github.com/garindra/meja/internal/server/terminal"
)

const attachTTL = 2 * time.Minute

type Config struct {
	ControlPath      string
	Stdout           io.Writer
	Stderr           io.Writer
	TerminalDebugLog io.Writer
}

// Daemon owns the server-wide session registry and name reservations. All
// registry access is serialized by requests; control handlers query it and
// then query Sessions separately, so neither actor waits on the other.
type Daemon struct {
	logMu     sync.Mutex
	requests  chan daemonRequest
	nextID    uint64
	sessions  map[uint64]*Session
	names     map[string]*Session
	tlsConfig *tls.Config
	certHash  string
	serverCtx context.Context
	stop      context.CancelFunc
	stderr    io.Writer
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
	terminal.SetDebugLogger(cfg.TerminalDebugLog)
	socket := cfg.ControlPath
	if socket == "" {
		var err error
		socket, err = control.ControlPath()
		if err != nil {
			return err
		}
	}
	lock, err := control.AcquireServerLock(socket)
	if err != nil {
		return err
	}
	defer lock.Close()
	cert, hash, err := daemonCertificate()
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{protocol.ALPN}, MinVersion: tls.VersionTLS13}
	d := &Daemon{requests: make(chan daemonRequest, 64), nextID: 1, sessions: make(map[uint64]*Session), names: make(map[string]*Session), tlsConfig: tlsConfig, certHash: hash, serverCtx: serverCtx, stop: stop, stderr: cfg.Stderr}
	actorCtx, stopActor := context.WithCancel(context.Background())
	go d.runRequests(actorCtx)
	defer func() {
		d.disconnectActiveClients()
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
	err = control.Serve(serverCtx, socket, d.handleControl)
	if err != nil {
		stop()
	}
	return err
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

func (d *Daemon) handleControl(operation string, target control.SessionTarget) (control.Bootstrap, []control.SessionInfo, int, error) {
	if operation == "stop-server" {
		var stop context.CancelFunc
		d.call(func() { stop = d.stop })
		if stop == nil {
			return control.Bootstrap{}, nil, 0, errors.New("server stop unavailable")
		}
		pid := os.Getpid()
		d.disconnectActiveClients()
		stop()
		return control.Bootstrap{}, nil, pid, nil
	}
	if operation == "list-sessions" {
		type listedSession struct {
			id    uint64
			state *Session
		}
		var states []listedSession
		d.call(func() {
			states = make([]listedSession, 0, len(d.sessions))
			for id, state := range d.sessions {
				states = append(states, listedSession{id: id, state: state})
			}
		})

		sessions := make([]control.SessionInfo, 0, len(states))
		for _, listed := range states {
			if listed.state != nil {
				name, attached := listed.state.info()
				sessions = append(sessions, control.SessionInfo{ID: listed.id, Name: name, Attached: attached})
			}
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
		return control.Bootstrap{}, sessions, 0, nil
	}
	var s *Session
	var operationErr error
	created := false
	var port uint16
	var encodedToken string
	var expires time.Time
	d.call(func() {
		switch operation {
		case "create-session":
			if target.Name != "" {
				if err := control.ValidateSessionName(target.Name); err != nil {
					operationErr = err
					return
				}
				if d.sessionByName(target.Name) != nil {
					operationErr = fmt.Errorf("session %q already exists", target.Name)
					return
				}
			}
			if d.nextID == 0 {
				operationErr = errors.New("session ID exhausted")
				return
			}
			s = newSession(d.nextID, target.Name)
			s.daemon = d
			port, encodedToken, expires, operationErr = s.startQUIC(d.serverCtx, d.tlsConfig)
			if operationErr != nil {
				_ = s.shutdown()
				return
			}
			d.sessions[d.nextID] = s
			d.reserveSessionName(s, target.Name)
			d.nextID++
			created = true
		case "connect-session":
			if target.Name != "" {
				s = d.sessionByName(target.Name)
			} else {
				s = d.sessions[target.ID]
			}
			if s == nil {
				operationErr = control.ErrSessionUnavailable
			}
		default:
			operationErr = errors.New("unsupported control operation")
		}
	})
	if operationErr != nil {
		return control.Bootstrap{}, nil, 0, operationErr
	}

	if !created {
		var err error
		port, encodedToken, expires, err = s.issueBootstrap()
		if err != nil {
			return control.Bootstrap{}, nil, 0, err
		}
	}
	return control.Bootstrap{Version: control.ProtocolVersion, SessionID: s.ID, Port: port, AttachToken: encodedToken, ExpiresAt: expires, CertSPKISHA256: d.certHash}, nil, 0, nil
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
	if err := control.ValidateSessionName(name); err != nil {
		return err
	}
	if d.sessions[state.ID] != state {
		return control.ErrSessionUnavailable
	}
	if existing := d.sessionByName(name); existing != nil && existing != state {
		return fmt.Errorf("session %q already exists", name)
	}
	return nil
}

// requestSessionRename is deliberately one-way. The Session actor never waits
// on the Daemon actor; the Daemon posts the accepted/rejected result back.
func (d *Daemon) requestSessionRename(state *Session, name string) {
	if d.requests == nil {
		err := d.validateSessionRename(state, name)
		if err == nil {
			d.reserveSessionName(state, name)
		}
		_ = state.finishSessionRename(name, err == nil)
		return
	}
	d.post(func() {
		err := d.validateSessionRename(state, name)
		if err == nil {
			d.reserveSessionName(state, name)
		}
		state.post(func() error { return state.finishSessionRename(name, err == nil) })
	})
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
// released its Connection; the Daemon only removes matching registry refs.
func (d *Daemon) sessionExited(state *Session) {
	remove := func() {
		if d.sessions[state.ID] != state {
			return
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

func (d *Daemon) activate(s *Session, connection *Connection) {
	s.attachConnection(connection)
}

func (d *Daemon) deactivate(s *Session, connection *Connection) {
	s.detachConnection(connection)
}
