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
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"tali/internal/control"
	"tali/internal/protocol"
	"tali/internal/server/terminal"
)

const attachTTL = 2 * time.Minute

type daemon struct {
	mu       sync.Mutex
	logMu    sync.Mutex
	nextID   uint64
	sessions map[uint64]*sessionState
	certHash string
	port     uint16
	stop     context.CancelFunc
	stderr   io.Writer
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
	listener, port, err := listenQUICInRange(tlsConfig)
	if err != nil {
		return err
	}
	defer listener.Close()
	d := &daemon{nextID: 1, sessions: make(map[uint64]*sessionState), certHash: hash, port: port, stop: stop, stderr: cfg.Stderr}
	defer d.disconnectActiveClients()
	go func() {
		select {
		case <-ctx.Done():
			d.disconnectActiveClients()
			stop()
		case <-serverCtx.Done():
		}
	}()
	controlErr := make(chan error, 1)
	go func() {
		err := control.Serve(serverCtx, socket, d.handleControl)
		controlErr <- err
		if err != nil {
			stop()
		}
	}()
	for {
		conn, acceptErr := listener.Accept(serverCtx)
		if acceptErr != nil {
			if errors.Is(acceptErr, context.Canceled) {
				select {
				case err := <-controlErr:
					return err
				default:
					return nil
				}
			}
			return fmt.Errorf("accept QUIC connection: %w", acceptErr)
		}
		go func() {
			if err := handleSession(serverCtx, d, conn); err != nil {
				d.logf("tali session: %v\n", err)
			}
		}()
		select {
		case err := <-controlErr:
			if err != nil {
				return err
			}
		default:
		}
	}
}

func (d *daemon) logSessionAttached(sessionID uint64) {
	d.logf("tali server: session %d attached\n", sessionID)
}

func (d *daemon) logf(format string, args ...any) {
	if d.stderr == nil {
		return
	}
	d.logMu.Lock()
	defer d.logMu.Unlock()
	fmt.Fprintf(d.stderr, format, args...)
}

func listenQUICInRange(tlsConfig *tls.Config) (*quic.Listener, uint16, error) {
	for port := control.DefaultUDPMin; port <= control.DefaultUDPMax; port++ {
		listener, err := quic.ListenAddr(net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), tlsConfig, &quic.Config{MaxIdleTimeout: quicMaxIdleTimeout, KeepAlivePeriod: quicKeepAlivePeriod, MaxIncomingStreams: int64(protocol.MaxRenderSlots)})
		if err == nil {
			return listener, uint16(port), nil
		}
	}
	return nil, 0, errors.New("no UDP port available in 60000-61000")
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
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "tali-daemon"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"tali-daemon"}}
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

func (d *daemon) handleControl(operation string, id uint64) (control.Bootstrap, []uint64, int, error) {
	d.mu.Lock()
	if operation == "stop-server" {
		if d.stop == nil {
			d.mu.Unlock()
			return control.Bootstrap{}, nil, 0, errors.New("server stop unavailable")
		}
		pid := os.Getpid()
		d.mu.Unlock()
		d.disconnectActiveClients()
		d.stop()
		return control.Bootstrap{}, nil, pid, nil
	}
	defer d.mu.Unlock()
	if operation == "list-sessions" {
		ids := make([]uint64, 0, len(d.sessions))
		for sessionID := range d.sessions {
			ids = append(ids, sessionID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return control.Bootstrap{}, ids, 0, nil
	}
	var s *sessionState
	switch operation {
	case "create-session":
		if d.nextID == 0 {
			return control.Bootstrap{}, nil, 0, errors.New("session ID exhausted")
		}
		s = newSessionState(d.nextID)
		d.sessions[d.nextID] = s
		d.nextID++
	case "connect-session":
		s = d.sessions[id]
		if s == nil {
			return control.Bootstrap{}, nil, 0, control.ErrSessionUnavailable
		}
	default:
		return control.Bootstrap{}, nil, 0, errors.New("unsupported control operation")
	}
	token, err := control.NewToken()
	if err != nil {
		return control.Bootstrap{}, nil, 0, err
	}
	s.attachMu.Lock()
	s.attachToken = token
	s.attachExpires = time.Now().Add(attachTTL)
	s.attachConsumed = false
	s.attachMu.Unlock()
	return control.Bootstrap{Version: control.ProtocolVersion, SessionID: s.sessionID, Port: d.port, AttachToken: control.EncodeToken(token), ExpiresAt: time.Now().Add(attachTTL), CertSPKISHA256: d.certHash}, nil, 0, nil
}

// disconnectActiveClients uses the same clean QUIC close path as an explicit
// client detach (Ctrl-B, d). The client restores its terminal and does not
// receive a protocol error or a synthetic input event.
func (d *daemon) disconnectActiveClients() {
	d.mu.Lock()
	sessions := make([]*sessionState, 0, len(d.sessions))
	for _, session := range d.sessions {
		sessions = append(sessions, session)
	}
	d.mu.Unlock()
	for _, session := range sessions {
		session.attachMu.Lock()
		conn := session.activeConn
		session.activeConn = nil
		session.attachMu.Unlock()
		if conn != nil {
			_ = conn.CloseWithError(0, "")
		}
	}
}

func newSessionState(id uint64) *sessionState {
	state := &sessionState{
		sessionID:    id,
		session:      NewSession(id),
		resumeTokens: map[string]uint64{},
		operations:   make(chan sessionOperation),
	}
	go state.runOperations()
	return state
}

func (d *daemon) session(id uint64) *sessionState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[id]
}

func (d *daemon) attach(id uint64, encoded string) (*sessionState, error) {
	s := d.session(id)
	if s == nil {
		return nil, control.ErrSessionUnavailable
	}
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if s.attachConsumed || time.Now().After(s.attachExpires) || !control.EqualToken(encoded, s.attachToken) {
		return nil, errors.New("session attachment rejected")
	}
	s.attachConsumed = true
	return s, nil
}

func (d *daemon) resume(id uint64, encoded string, generation uint64) (*sessionState, string, uint64, error) {
	s := d.session(id)
	if s == nil {
		return nil, "", 0, control.ErrSessionUnavailable
	}
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if current, ok := s.resumeTokens[encoded]; !ok || current != generation || generation != s.generation {
		return nil, "", 0, errors.New("session resume rejected")
	}
	token, err := control.NewToken()
	if err != nil {
		return nil, "", 0, err
	}
	s.generation++
	encodedToken := control.EncodeToken(token)
	s.resumeTokens = map[string]uint64{encodedToken: s.generation}
	return s, encodedToken, s.generation, nil
}

func (d *daemon) activate(s *sessionState, conn quic.Connection) {
	s.attachMu.Lock()
	old := s.activeConn
	s.activeConn = conn
	s.attachMu.Unlock()
	if old != nil && old != conn {
		_ = old.CloseWithError(0x54414c49, "session resumed")
	}
}
