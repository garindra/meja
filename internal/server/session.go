package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/control"
	"github.com/garindra/meja/internal/protocol"
)

const (
	sessionID0 = 0
	clientID0  = 0
)

type sessionOperation struct {
	run  func() error
	done chan error
}

func (s *Session) coordinate(run func() error) error {
	if s.operations == nil {
		return run()
	}
	done := make(chan error, 1)
	select {
	case s.operations <- sessionOperation{run: run, done: done}:
	case <-s.operationsDone:
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-s.operationsDone:
		return nil
	}
}

func (s *Session) coordinateConnection(connection *Connection, run func() error) error {
	return s.coordinate(func() error {
		if s.connection != connection {
			return nil
		}
		return run()
	})
}

func (s *Session) runOperations() {
	for {
		select {
		case operation := <-s.operations:
			err := operation.run()
			if operation.done != nil {
				operation.done <- err
			}
			if s.stopping {
				s.stopOperations()
				return
			}
		case <-s.operationsDone:
			return
		}
	}
}

func (s *Session) post(run func() error) {
	if s.operations == nil {
		_ = run()
		return
	}
	select {
	case s.operations <- sessionOperation{run: run}:
	case <-s.operationsDone:
	}
}

func (s *Session) stopOperations() {
	s.stopOnce.Do(func() { close(s.operationsDone) })
}

// startQUIC is a daemon-to-session RPC over the Session actor's enduring
// operations channel. Completion means the listener is bound, the initial
// attach credential is installed, and the accept goroutine is running.
func (s *Session) startQUIC(parent context.Context, tlsConfig *tls.Config) (uint16, string, time.Time, error) {
	token, err := control.NewToken()
	if err != nil {
		return 0, "", time.Time{}, err
	}
	expiresAt := time.Now().Add(attachTTL)
	if parent == nil {
		parent = context.Background()
	}
	var port uint16
	started := false
	err = s.coordinate(func() error {
		if s.stopping {
			return control.ErrSessionUnavailable
		}
		if err := parent.Err(); err != nil {
			return err
		}
		listener, boundPort, err := listenQUICInRange(tlsConfig)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(parent)
		port = boundPort
		s.port = port
		s.quicListener = listener
		s.quicCancel = cancel
		s.attachToken = token
		s.attachExpires = expiresAt
		s.attachConsumed = false
		go s.runQUIC(ctx, listener)
		started = true
		return nil
	})
	if err == nil && !started {
		err = control.ErrSessionUnavailable
	}
	return port, control.EncodeToken(token), expiresAt, err
}

func (s *Session) runQUIC(ctx context.Context, listener *quic.Listener) {
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, quic.ErrServerClosed) {
				return
			}
			if s.daemon != nil {
				s.daemon.logf("meja session %d: accept QUIC connection: %v\n", s.ID, err)
			}
			_ = s.shutdown()
			return
		}
		go func() {
			if err := serveConnection(ctx, s.daemon, s, conn); err != nil && s.daemon != nil && !isSessionReplacedClose(err) {
				s.daemon.logf("meja session %d: %v\n", s.ID, err)
			}
		}()
	}
}

func isSessionReplacedClose(err error) bool {
	var applicationErr *quic.ApplicationError
	return errors.As(err, &applicationErr) && applicationErr.ErrorCode == protocol.SessionReplacedErrorCode
}

func listenQUICInRange(tlsConfig *tls.Config) (*quic.Listener, uint16, error) {
	for port := control.DefaultUDPMin; port <= control.DefaultUDPMax; port++ {
		listener, err := quic.ListenAddr(net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), tlsConfig, &quic.Config{
			MaxIdleTimeout:     quicMaxIdleTimeout,
			KeepAlivePeriod:    quicKeepAlivePeriod,
			MaxIncomingStreams: int64(protocol.MaxRenderSlots),
			InitialPacketSize:  protocol.QUICInitialPacketSize,
		})
		if err == nil {
			return listener, uint16(port), nil
		}
	}
	return nil, 0, errors.New("no UDP port available in 60000-61000")
}

// shutdownNow runs only on the session actor. The Session releases its own
// live resources, tells the Daemon to forget it, and lets runOperations exit
// after replying to the operation that caused the shutdown.
func (s *Session) shutdownNow() {
	if s.stopping {
		return
	}
	s.stopping = true
	s.ended = true
	s.attachToken = nil
	s.resumeTokens = nil
	connection := s.connection
	s.connection = nil
	// A ListenAddr listener closes established connections immediately. Send
	// the clean application close first so the client exits instead of treating
	// listener teardown as a transport failure and entering reconnect mode.
	if connection != nil && connection.QUIC != nil {
		_ = connection.QUIC.CloseWithError(0, "")
	}
	if s.quicCancel != nil {
		s.quicCancel()
		s.quicCancel = nil
	}
	if s.quicListener != nil {
		_ = s.quicListener.Close()
		s.quicListener = nil
	}
	if s.daemon != nil {
		s.daemon.sessionExited(s)
	}
}

func (s *Session) shutdown() error {
	return s.coordinate(func() error {
		s.shutdownNow()
		return nil
	})
}

func (s *Session) attachConnection(connection *Connection) {
	_ = s.coordinate(func() error {
		previous := s.connection
		if previous != nil && previous != connection && previous.QUIC != nil {
			_ = previous.QUIC.CloseWithError(protocol.SessionReplacedErrorCode, "session attached elsewhere")
		}
		s.connection = connection
		if connection != nil && connection.StatusOutput != nil {
			if err := s.attachStatusOutput(connection, connection.StatusOutput); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Session) detachConnection(connection *Connection) {
	_ = s.coordinate(func() error {
		if s.connection == connection {
			if err := s.detachStatusOutput(connection); err != nil {
				return err
			}
			s.connection = nil
		}
		return nil
	})
}

func (s *Session) currentManagementFrames() chan protocol.Frame {
	if s.connection == nil {
		return nil
	}
	return s.connection.managementOut
}

func (s *Session) currentOutputLease(slot int) *OutputLease {
	if s.connection == nil || slot < 0 || slot >= len(s.connection.Output) {
		return nil
	}
	return s.connection.Output[slot]
}

func (s *Session) isAttached() bool {
	attached := false
	_ = s.coordinate(func() error {
		attached = s.connection != nil
		return nil
	})
	return attached
}

func (s *Session) info() (name string, attached bool) {
	_ = s.coordinate(func() error {
		name = s.Name
		attached = s.connection != nil
		return nil
	})
	return name, attached
}

// issueBootstrap is a daemon-to-session RPC over the Session actor's enduring
// operations channel. Each call returns the stable listener port and installs
// a fresh, single-use attach credential for initial attachment or reconnect.
func (s *Session) issueBootstrap() (uint16, string, time.Time, error) {
	token, err := control.NewToken()
	if err != nil {
		return 0, "", time.Time{}, err
	}
	expires := time.Now().Add(attachTTL)
	var port uint16
	issued := false
	err = s.coordinate(func() error {
		if s.stopping {
			return control.ErrSessionUnavailable
		}
		port = s.port
		s.attachToken = token
		s.attachExpires = expires
		s.attachConsumed = false
		issued = true
		return nil
	})
	if err == nil && !issued {
		err = control.ErrSessionUnavailable
	}
	return port, control.EncodeToken(token), expires, err
}

func (s *Session) consumeAttachToken(encoded string) error {
	checked := false
	err := s.coordinate(func() error {
		checked = true
		if s.stopping {
			return fmt.Errorf("session attachment rejected")
		}
		if s.attachConsumed || time.Now().After(s.attachExpires) || !control.EqualToken(encoded, s.attachToken) {
			return fmt.Errorf("session attachment rejected")
		}
		s.attachConsumed = true
		return nil
	})
	if err == nil && !checked {
		return fmt.Errorf("session attachment rejected")
	}
	return err
}

func (s *Session) beginAttachment() (string, uint64, error) {
	token, err := control.NewToken()
	if err != nil {
		return "", 0, err
	}
	encoded := control.EncodeToken(token)
	var generation uint64
	issued := false
	err = s.coordinate(func() error {
		if s.stopping {
			return fmt.Errorf("session attachment rejected")
		}
		s.generation++
		generation = s.generation
		s.resumeTokens = map[string]uint64{encoded: generation}
		issued = true
		return nil
	})
	if err == nil && !issued {
		err = fmt.Errorf("session attachment rejected")
	}
	return encoded, generation, err
}

func (s *Session) resumeAttachment(encoded string, generation uint64) (string, uint64, error) {
	token, err := control.NewToken()
	if err != nil {
		return "", 0, err
	}
	nextToken := control.EncodeToken(token)
	var nextGeneration uint64
	issued := false
	err = s.coordinate(func() error {
		if s.stopping {
			return fmt.Errorf("session resume rejected")
		}
		if current, ok := s.resumeTokens[encoded]; !ok || current != generation || generation != s.generation {
			return fmt.Errorf("session resume rejected")
		}
		s.generation++
		nextGeneration = s.generation
		s.resumeTokens = map[string]uint64{nextToken: nextGeneration}
		issued = true
		return nil
	})
	if err == nil && !issued {
		err = fmt.Errorf("session resume rejected")
	}
	return nextToken, nextGeneration, err
}

// Session is the authority for one persistent terminal workspace. Its actor
// owns attachment credentials, the live Connection, clients, windows, and
// pane membership. Panes remain alive across Connection replacement.
type Session struct {
	ID      uint64
	Name    string
	Windows map[uint64]*Window
	Panes   map[uint64]*Pane
	Clients map[uint64]*ClientState

	NextWindowID       uint64
	NextWindowIndex    int
	NextPaneID         uint64
	NextLayoutRevision uint64

	attachToken         []byte
	attachExpires       time.Time
	attachConsumed      bool
	resumeTokens        map[string]uint64
	generation          uint64
	daemon              *Daemon
	port                uint16
	quicListener        *quic.Listener
	quicCancel          context.CancelFunc
	connection          *Connection
	defaultCwd          string
	operations          chan sessionOperation
	operationsDone      chan struct{}
	statusCommands      chan statusCommand
	stopOnce            sync.Once
	processNames        ProcessObserver
	nameMonitor         sync.Once
	autosave            sync.Once
	autosaveNow         chan struct{}
	promptContinuations map[uint64]promptContinuation
	stopping            bool
	ended               bool
}

type Window struct {
	ID             uint64
	DisplayIndex   int
	Name           string
	AutomaticName  bool
	ActivePaneID   uint64
	Layout         LayoutNode
	LayoutRevision uint64
}

type PromptKind uint8

const (
	PromptKindRenameWindow PromptKind = iota + 1
	PromptKindRenameSession
	PromptKindConfirm
)

type PromptAction uint8

const (
	PromptActionNone PromptAction = iota
	PromptActionChanged
	PromptActionSubmit
	PromptActionCancel
)

type PromptState struct {
	Kind           PromptKind
	Action         PromptAction
	TargetWindowID uint64
	Label          string
	Text           []rune
	Cursor         int
	pendingUTF8    []byte
	PendingEscape  []byte
}

type promptResult struct {
	Accepted bool
	Text     string
}

type promptContinuation func(promptResult) error

type RenderBinding struct {
	Slot   int
	PaneID uint64
}

type PaneSwapDirection int8

const (
	SwapPanePrevious PaneSwapDirection = -1
	SwapPaneNext     PaneSwapDirection = 1
)

type ClientState struct {
	ID             uint64
	SessionID      uint64
	ActiveWindowID uint64
	FocusedPaneID  uint64
	FocusX2        int
	FocusY2        int
	HasFocusPoint  bool
	TerminalCols   uint16
	TerminalRows   uint16

	RenderBindings    []RenderBinding
	HistoryViews      map[uint64]*HistoryView
	InputState        serverInputState
	PrefixEscape      []byte
	ResizeRepeatUntil time.Time
	Prompt            *PromptState
	LastWindowID      uint64
	HasLastWindow     bool
}

func (c *ClientState) setFocusedPane(paneID uint64) {
	c.FocusedPaneID = paneID
	c.HasFocusPoint = false
}

func NewSession(id uint64) *Session {
	session := &Session{
		ID:                  id,
		Windows:             map[uint64]*Window{},
		Panes:               map[uint64]*Pane{},
		Clients:             map[uint64]*ClientState{},
		NextWindowID:        1,
		resumeTokens:        map[string]uint64{},
		operations:          make(chan sessionOperation, 64),
		operationsDone:      make(chan struct{}),
		statusCommands:      make(chan statusCommand, 64),
		processNames:        NewProcessObserver(),
		autosaveNow:         make(chan struct{}, 1),
		promptContinuations: make(map[uint64]promptContinuation),
	}
	go session.runOperations()
	go session.runStatusOutput()
	return session
}

func (s *Session) nextLayoutRevisionLocked() uint64 {
	s.NextLayoutRevision++
	return s.NextLayoutRevision
}

func (s *Session) NewClient(id uint64) *ClientState {
	client := &ClientState{ID: id, SessionID: s.ID, HistoryViews: map[uint64]*HistoryView{}}
	s.Clients[id] = client
	return client
}

func (s *Session) EnsureClient(id uint64) *ClientState {
	client := s.ensureClientLocked(id)
	return cloneClientState(client)
}

func (s *Session) ensureClientLocked(id uint64) *ClientState {
	if client := s.Clients[id]; client != nil {
		if client.HistoryViews == nil {
			client.HistoryViews = map[uint64]*HistoryView{}
		}
		s.ensureClientFocusLocked(client)
		return client
	}
	client := &ClientState{ID: id, SessionID: s.ID, HistoryViews: map[uint64]*HistoryView{}}
	s.ensureClientFocusLocked(client)
	s.Clients[id] = client
	return client
}

func (s *Session) BeginPrompt(clientID uint64, kind PromptKind, label, initial string) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, fmt.Errorf("client %d has no active window", clientID)
	}
	text := []rune(initial)
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{
		Kind:           kind,
		TargetWindowID: window.ID,
		Label:          label,
		Text:           text,
		Cursor:         len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) BeginRenameWindowPrompt(clientID uint64) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, fmt.Errorf("client %d has no active window", clientID)
	}
	text := []rune(window.Name)
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{
		Kind:           PromptKindRenameWindow,
		TargetWindowID: window.ID,
		Label:          "(rename-window) ",
		Text:           text,
		Cursor:         len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) BeginRenameSessionPrompt(clientID uint64) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	text := []rune(s.Name)
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{
		Kind:   PromptKindRenameSession,
		Label:  "(rename-session) ",
		Text:   text,
		Cursor: len(text),
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) beginConfirmationPrompt(clientID uint64, label string, continuation promptContinuation) (*PromptState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, fmt.Errorf("unknown client %d", clientID)
	}
	client.InputState = serverInputNormal
	client.PrefixEscape = nil
	client.ResizeRepeatUntil = time.Time{}
	client.Prompt = &PromptState{Kind: PromptKindConfirm, Label: label}
	if continuation == nil {
		delete(s.promptContinuations, clientID)
	} else {
		s.promptContinuations[clientID] = continuation
	}
	return clonePromptState(client.Prompt), nil
}

func (s *Session) resolvePrompt(clientID uint64, result promptResult) error {
	continuation := s.promptContinuations[clientID]
	delete(s.promptContinuations, clientID)
	if continuation == nil {
		return s.publishStatusBar()
	}
	return continuation(result)
}

func (s *Session) finishSessionRename(name string, accepted bool) error {
	client := s.Clients[clientID0]
	if accepted {
		renamed := s.Name != name
		s.Name = name
		if renamed {
			s.requestAutosave()
		}
		if client != nil && client.Prompt != nil && client.Prompt.Kind == PromptKindRenameSession {
			client.Prompt = nil
		}
	} else if client != nil && client.Prompt != nil && client.Prompt.Kind == PromptKindRenameSession {
		client.Prompt.Action = PromptActionNone
	}
	return s.publishStatusBar()
}

func (s *Session) requestAutosave() {
	select {
	case s.autosaveNow <- struct{}{}:
	default:
	}
}

func (s *Session) SessionName() string {
	return s.Name
}

func (s *Session) setSessionName(name string) {
	s.Name = name
}

func (s *Session) ActivePrompt(clientID uint64) *PromptState {
	client := s.Clients[clientID]
	if client == nil {
		return nil
	}
	return clonePromptState(client.Prompt)
}

func (s *Session) ensureClientFocusLocked(client *ClientState) {
	if len(s.Windows) == 0 {
		return
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		ids := s.windowIDsLocked()
		window = s.Windows[ids[0]]
		client.ActiveWindowID = window.ID
	}
	window.ActivePaneID = windowActivePaneID(window)
	if !windowHasPane(window, client.FocusedPaneID) {
		client.setFocusedPane(window.ActivePaneID)
	}
}

func (s *Session) SetClientSize(clientID uint64, cols, rows uint16) *ClientState {
	client := s.ensureClientLocked(clientID)
	if client.TerminalCols != cols || client.TerminalRows != rows {
		for _, window := range s.Windows {
			window.LayoutRevision = s.nextLayoutRevisionLocked()
		}
		client.TerminalCols = cols
		client.TerminalRows = rows
	}
	return cloneClientState(client)
}

func (s *Session) CreateWindow(pane *Pane, activateFor uint64) (*Window, *ClientState) {
	if s.NextWindowID == 0 {
		s.NextWindowID = 1
	}
	windowID := s.NextWindowID
	s.NextWindowID++
	displayIndex := s.NextWindowIndex
	s.NextWindowIndex++
	window := &Window{
		ID:             windowID,
		DisplayIndex:   displayIndex,
		Name:           pane.Title,
		AutomaticName:  true,
		ActivePaneID:   pane.ID,
		Layout:         &PaneLayout{PaneID: pane.ID},
		LayoutRevision: s.nextLayoutRevisionLocked(),
	}
	s.Windows[windowID] = window
	s.Panes[pane.ID] = pane
	client := s.ensureClientLocked(activateFor)
	if client.ActiveWindowID != window.ID {
		client.LastWindowID = client.ActiveWindowID
		client.HasLastWindow = client.LastWindowID != 0
	}
	client.ActiveWindowID = windowID
	client.setFocusedPane(pane.ID)
	s.rebuildBindingsLocked(client, window)
	return window, cloneClientState(client)
}

func (s *Session) HasWindows() bool {
	return len(s.Windows) > 0
}

func (s *Session) PanesSnapshot() []*Pane {
	panes := make([]*Pane, 0, len(s.Panes))
	for _, pane := range s.Panes {
		panes = append(panes, pane)
	}
	return panes
}

func (s *Session) Pane(id uint64) *Pane {
	return s.Panes[id]
}

func (s *Session) ReattachClient(clientID uint64) (*Window, *Pane, *ClientState, error) {
	client := s.ensureClientLocked(clientID)
	if len(s.Windows) == 0 {
		return nil, nil, nil, fmt.Errorf("session has no windows")
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		ids := s.windowIDsLocked()
		window = s.Windows[ids[0]]
	}
	s.selectWindowLocked(client, window)
	pane := s.Panes[client.FocusedPaneID]
	return cloneWindow(window), pane, cloneClientState(client), nil
}

func (s *Session) AddPaneID() uint64 {
	id := s.NextPaneID
	s.NextPaneID++
	return id
}

func (s *Session) ActivePane(clientID uint64) (*Pane, *ClientState) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	return s.Panes[client.FocusedPaneID], cloneClientState(client)
}

func (s *Session) ActiveWindow(clientID uint64) (*Window, *ClientState) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	window := s.Windows[client.ActiveWindowID]
	return cloneWindow(window), cloneClientState(client)
}

func (s *Session) ResolveInputTarget(clientID, requestedPaneID uint64) (*Pane, *ClientState, bool) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false
	}
	pane := s.Panes[client.FocusedPaneID]
	if pane == nil {
		return nil, cloneClientState(client), false
	}
	return pane, cloneClientState(client), client.FocusedPaneID == requestedPaneID
}

func (s *Session) SelectWindow(clientID, windowID uint64) (*Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[windowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", windowID)
	}
	s.selectWindowLocked(client, window)
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) selectWindowLocked(client *ClientState, window *Window) {
	if client.ActiveWindowID != window.ID {
		if previous := s.Windows[client.ActiveWindowID]; previous != nil && windowHasPane(previous, client.FocusedPaneID) {
			previous.ActivePaneID = client.FocusedPaneID
		}
		client.LastWindowID = client.ActiveWindowID
		client.HasLastWindow = client.LastWindowID != 0
	}
	client.ActiveWindowID = window.ID
	window.ActivePaneID = windowActivePaneID(window)
	client.setFocusedPane(window.ActivePaneID)
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	s.rebuildBindingsLocked(client, window)
}

func (s *Session) RenameWindow(windowID uint64, name string) (*Window, error) {
	window := s.Windows[windowID]
	if window == nil {
		return nil, fmt.Errorf("unknown window %d", windowID)
	}
	// Empty names are valid; normal status projection remains well-formed.
	window.Name = name
	window.AutomaticName = false
	return cloneWindow(window), nil
}

func (s *Session) FocusPane(clientID, paneID uint64) (*Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if !windowHasPane(window, paneID) {
		return nil, nil, fmt.Errorf("pane %d not visible in window %d", paneID, window.ID)
	}
	window.ActivePaneID = paneID
	client.setFocusedPane(paneID)
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) ResizeFocusedPane(clientID uint64, direction PaneResizeDirection, amount int) (*Window, *ClientState, bool, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, false, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if direction > ResizePaneRight {
		return nil, nil, false, fmt.Errorf("invalid pane resize direction %d", direction)
	}
	if amount <= 0 {
		return nil, nil, false, fmt.Errorf("pane resize amount must be positive")
	}
	if !ResizePaneBoundary(window.Layout, client.FocusedPaneID, direction, amount, Rect{
		Width:  int(client.TerminalCols),
		Height: int(client.TerminalRows),
	}) {
		return cloneWindow(window), cloneClientState(client), false, nil
	}
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	s.rebuildBindingsLocked(client, window)
	return cloneWindow(window), cloneClientState(client), true, nil
}

func (s *Session) SplitFocusedPane(clientID uint64, pane *Pane, direction SplitDirection) (*Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if len(window.Layout.PaneIDs()) >= int(protocol.MaxVisiblePanes) {
		return nil, nil, fmt.Errorf("window %d has reached the %d-pane limit", window.ID, protocol.MaxVisiblePanes)
	}
	if !windowHasPane(window, client.FocusedPaneID) {
		return nil, nil, fmt.Errorf("focused pane %d not in window %d", client.FocusedPaneID, window.ID)
	}
	if direction != SplitVertical && direction != SplitHorizontal {
		return nil, nil, fmt.Errorf("invalid split direction %d", direction)
	}
	updated, replaced := replacePaneWithSplit(window.Layout, client.FocusedPaneID, pane.ID, direction)
	if !replaced {
		return nil, nil, fmt.Errorf("focused pane %d not found in layout", client.FocusedPaneID)
	}
	s.Panes[pane.ID] = pane
	window.Layout = updated
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	window.ActivePaneID = pane.ID
	client.setFocusedPane(pane.ID)
	s.rebuildBindingsLocked(client, window)
	return cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) SwapFocusedPane(clientID uint64, direction PaneSwapDirection) (*Window, *ClientState, bool, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, false, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if direction != SwapPanePrevious && direction != SwapPaneNext {
		return nil, nil, false, fmt.Errorf("invalid pane swap direction %d", direction)
	}

	s.rebuildBindingsLocked(client, window)
	if len(client.RenderBindings) < 2 {
		return cloneWindow(window), cloneClientState(client), false, nil
	}
	current := -1
	for i, binding := range client.RenderBindings {
		if binding.PaneID == client.FocusedPaneID {
			current = i
			break
		}
	}
	if current < 0 {
		return cloneWindow(window), cloneClientState(client), false, nil
	}
	target := (current + int(direction) + len(client.RenderBindings)) % len(client.RenderBindings)
	targetPaneID := client.RenderBindings[target].PaneID
	if !swapLayoutPanes(window.Layout, client.FocusedPaneID, targetPaneID) {
		return nil, nil, false, fmt.Errorf("swap panes %d and %d in window %d", client.FocusedPaneID, targetPaneID, window.ID)
	}
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	client.setFocusedPane(client.FocusedPaneID)
	s.rebuildBindingsLocked(client, window)
	return cloneWindow(window), cloneClientState(client), true, nil
}

func (s *Session) CanSplitFocusedPane(clientID uint64) error {
	client := s.Clients[clientID]
	if client == nil {
		return fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	if !windowHasPane(window, client.FocusedPaneID) {
		return fmt.Errorf("focused pane %d not in window %d", client.FocusedPaneID, window.ID)
	}
	if len(window.Layout.PaneIDs()) >= int(protocol.MaxVisiblePanes) {
		return fmt.Errorf("window %d has reached the %d-pane limit", window.ID, protocol.MaxVisiblePanes)
	}
	return nil
}

func (s *Session) CloseFocusedPane(clientID uint64) (closedPane *Pane, window *Window, client *ClientState, windowClosed bool, closedWindowID uint64, autoCreate bool, err error) {
	c := s.Clients[clientID]
	if c == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("unknown client %d", clientID)
	}
	window = s.Windows[c.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("unknown window %d", c.ActiveWindowID)
	}
	paneIDs := window.Layout.PaneIDs()
	for _, client := range s.Clients {
		delete(client.HistoryViews, c.FocusedPaneID)
	}
	if len(paneIDs) <= 1 {
		closedPane = s.Panes[c.FocusedPaneID]
		delete(s.Panes, c.FocusedPaneID)
		delete(s.Windows, window.ID)
		windowClosed = true
		closedWindowID = window.ID
		if len(s.Windows) == 0 {
			return closedPane, nil, cloneClientState(c), true, closedWindowID, true, nil
		}
		ids := s.windowIDsLocked()
		nextWindow := s.Windows[ids[0]]
		c.ActiveWindowID = nextWindow.ID
		nextWindow.ActivePaneID = windowActivePaneID(nextWindow)
		c.setFocusedPane(nextWindow.ActivePaneID)
		s.rebuildBindingsLocked(c, nextWindow)
		return closedPane, cloneWindow(nextWindow), cloneClientState(c), true, closedWindowID, false, nil
	}
	closedPane = s.Panes[c.FocusedPaneID]
	updated, nextFocusedPaneID, removed := removePaneFromLayout(window.Layout, c.FocusedPaneID)
	if !removed || updated == nil {
		return nil, nil, nil, false, 0, false, fmt.Errorf("focused pane %d not found in layout", c.FocusedPaneID)
	}
	delete(s.Panes, c.FocusedPaneID)
	window.Layout = updated
	window.LayoutRevision = s.nextLayoutRevisionLocked()
	window.ActivePaneID = nextFocusedPaneID
	c.setFocusedPane(nextFocusedPaneID)
	s.rebuildBindingsLocked(c, window)
	return closedPane, cloneWindow(window), cloneClientState(c), false, 0, false, nil
}

// RemovePane applies process exit to authoritative session state. It is a no-op
// when an explicit close already removed the pane before Process.Wait returned.
func (s *Session) RemovePane(paneID, clientID uint64) (window *Window, client *ClientState, finalPane, removed bool, err error) {
	c := s.Clients[clientID]
	if c == nil {
		return nil, nil, false, false, fmt.Errorf("unknown client %d", clientID)
	}
	var owner *Window
	for _, candidate := range s.Windows {
		if windowHasPane(candidate, paneID) {
			owner = candidate
			break
		}
	}
	if owner == nil || s.Panes[paneID] == nil {
		return nil, cloneClientState(c), false, false, nil
	}
	for _, state := range s.Clients {
		delete(state.HistoryViews, paneID)
	}
	delete(s.Panes, paneID)
	if len(owner.Layout.PaneIDs()) > 1 {
		updated, nextFocusedPaneID, ok := removePaneFromLayout(owner.Layout, paneID)
		if !ok || updated == nil {
			return nil, nil, false, false, fmt.Errorf("pane %d not found in window %d layout", paneID, owner.ID)
		}
		owner.Layout = updated
		owner.LayoutRevision = s.nextLayoutRevisionLocked()
		if owner.ActivePaneID == paneID {
			owner.ActivePaneID = nextFocusedPaneID
		}
		owner.ActivePaneID = windowActivePaneID(owner)
		if c.ActiveWindowID == owner.ID && c.FocusedPaneID == paneID {
			c.setFocusedPane(owner.ActivePaneID)
		}
	} else {
		delete(s.Windows, owner.ID)
		if len(s.Windows) == 0 {
			return nil, cloneClientState(c), true, true, nil
		}
		if c.ActiveWindowID == owner.ID {
			ids := s.windowIDsLocked()
			c.ActiveWindowID = ids[0]
			nextWindow := s.Windows[ids[0]]
			nextWindow.ActivePaneID = windowActivePaneID(nextWindow)
			c.setFocusedPane(nextWindow.ActivePaneID)
		}
	}
	active := s.Windows[c.ActiveWindowID]
	if active == nil {
		return nil, nil, false, false, fmt.Errorf("client %d has no active window after removing pane %d", clientID, paneID)
	}
	s.rebuildBindingsLocked(c, active)
	return cloneWindow(active), cloneClientState(c), false, true, nil
}

func (s *Session) CloseWindow(clientID, windowID uint64) (closed uint64, closedPanes []*Pane, replacement *Window, pane *Pane, client *ClientState, autoCreated bool, err error) {
	c := s.Clients[clientID]
	if c == nil {
		return 0, nil, nil, nil, nil, false, fmt.Errorf("unknown client %d", clientID)
	}
	w := s.Windows[windowID]
	if w == nil {
		return 0, nil, nil, nil, nil, false, fmt.Errorf("unknown window %d", windowID)
	}
	paneIDs := w.Layout.PaneIDs()
	if len(paneIDs) == 0 {
		return 0, nil, nil, nil, nil, false, fmt.Errorf("window %d has no panes", windowID)
	}
	closedPanes = make([]*Pane, 0, len(paneIDs))
	for _, paneID := range paneIDs {
		for _, client := range s.Clients {
			delete(client.HistoryViews, paneID)
		}
		if p := s.Panes[paneID]; p != nil {
			closedPanes = append(closedPanes, p)
		}
		delete(s.Panes, paneID)
	}
	delete(s.Windows, windowID)
	closed = windowID

	if len(s.Windows) == 0 {
		return closed, closedPanes, nil, nil, cloneClientState(c), true, nil
	}
	ids := s.windowIDsLocked()
	nextWindow := s.Windows[ids[0]]
	c.ActiveWindowID = nextWindow.ID
	nextWindow.ActivePaneID = windowActivePaneID(nextWindow)
	c.setFocusedPane(nextWindow.ActivePaneID)
	s.rebuildBindingsLocked(c, nextWindow)
	pane = s.Panes[c.FocusedPaneID]
	return closed, closedPanes, cloneWindow(nextWindow), pane, cloneClientState(c), false, nil
}

type WindowStatus struct {
	WindowID uint64
	Index    int
	Title    string
	Active   bool
}

func (s *Session) WindowStatuses(clientID uint64) []WindowStatus {
	client := s.Clients[clientID]
	active := uint64(0)
	if client != nil {
		active = client.ActiveWindowID
	}
	list := make([]WindowStatus, 0, len(s.Windows))
	for _, window := range s.Windows {
		list = append(list, WindowStatus{
			WindowID: window.ID,
			Index:    window.DisplayIndex,
			Title:    window.Name,
			Active:   window.ID == active,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	return list
}

func (s *Session) WindowLayout(clientID uint64) (protocol.WindowLayout, error) {
	client := s.Clients[clientID]
	if client == nil {
		return protocol.WindowLayout{}, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return protocol.WindowLayout{}, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	rect := Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)}
	placements := window.Layout.Compute(rect)
	out := make([]protocol.PanePlacement, 0, len(placements))
	for _, placement := range placements {
		slot := uint8(0)
		for _, binding := range client.RenderBindings {
			if binding.PaneID == placement.PaneID {
				slot = uint8(binding.Slot)
				break
			}
		}
		out = append(out, protocol.PanePlacement{
			PaneID: placement.PaneID,
			Slot:   slot,
			Rect: protocol.Rect{
				X:      placement.Rect.X,
				Y:      placement.Rect.Y,
				Width:  placement.Rect.Width,
				Height: placement.Rect.Height,
			},
		})
	}
	return protocol.WindowLayout{
		WindowID:       window.ID,
		FocusedPaneID:  client.FocusedPaneID,
		LayoutRevision: window.LayoutRevision,
		Panes:          out,
	}, nil
}

func (s *Session) VisiblePlacements(clientID uint64) ([]PanePlacement, *Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	placements := clonePlacements(window.Layout.Compute(Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)}))
	return placements, cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) BindingForPane(clientID, paneID uint64) (RenderBinding, bool) {
	client := s.Clients[clientID]
	if client == nil {
		return RenderBinding{}, false
	}
	for _, binding := range client.RenderBindings {
		if binding.PaneID == paneID {
			return binding, true
		}
	}
	return RenderBinding{}, false
}

func (s *Session) RenderBindings(clientID uint64) ([]RenderBinding, *ClientState) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil
	}
	bindings := append([]RenderBinding(nil), client.RenderBindings...)
	return bindings, cloneClientState(client)
}

func (s *Session) RebuildRenderBindings(clientID uint64) ([]RenderBinding, *Window, *ClientState, error) {
	client := s.Clients[clientID]
	if client == nil {
		return nil, nil, nil, fmt.Errorf("unknown client %d", clientID)
	}
	window := s.Windows[client.ActiveWindowID]
	if window == nil {
		return nil, nil, nil, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	s.rebuildBindingsLocked(client, window)
	return append([]RenderBinding(nil), client.RenderBindings...), cloneWindow(window), cloneClientState(client), nil
}

func (s *Session) rebuildBindingsLocked(client *ClientState, window *Window) {
	placements := window.Layout.Compute(Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)})
	sort.Slice(placements, func(i, j int) bool {
		if placements[i].Rect.Y != placements[j].Rect.Y {
			return placements[i].Rect.Y < placements[j].Rect.Y
		}
		if placements[i].Rect.X == placements[j].Rect.X {
			return placements[i].PaneID < placements[j].PaneID
		}
		return placements[i].Rect.X < placements[j].Rect.X
	})
	bindings := make([]RenderBinding, 0, len(placements))
	for slot, placement := range placements {
		bindings = append(bindings, RenderBinding{
			Slot:   slot,
			PaneID: placement.PaneID,
		})
	}
	client.RenderBindings = bindings
}

func (s *Session) UpdatePaneTitle(paneID uint64, title string) *Window {
	for _, window := range s.Windows {
		if window.AutomaticName && windowHasPane(window, paneID) {
			window.Name = title
			return cloneWindow(window)
		}
	}
	return nil
}

func (s *Session) IsFocusedPane(clientID, paneID uint64) bool {
	client := s.Clients[clientID]
	return client != nil && client.FocusedPaneID == paneID
}

func (s *Session) SnapshotClient(clientID uint64) *ClientState {
	return cloneClientState(s.Clients[clientID])
}

func (s *Session) ResizeAll(cols, rows uint16) {
	type resizeTarget struct {
		pane *Pane
		rect Rect
	}
	var targets []resizeTarget
	for _, client := range s.Clients {
		client.TerminalCols = cols
		client.TerminalRows = rows
		if window := s.Windows[client.ActiveWindowID]; window != nil {
			s.rebuildBindingsLocked(client, window)
		}
	}
	for _, window := range s.Windows {
		window.LayoutRevision = s.nextLayoutRevisionLocked()
		placements := window.Layout.Compute(Rect{Width: int(cols), Height: int(rows)})
		for _, placement := range placements {
			pane := s.Panes[placement.PaneID]
			if pane == nil {
				continue
			}
			targets = append(targets, resizeTarget{pane: pane, rect: placement.Rect})
		}
	}
	for _, target := range targets {
		_ = target.pane.resize(uint16(target.rect.Width), uint16(target.rect.Height))
	}
}

func (s *Session) windowIDsLocked() []uint64 {
	ids := make([]uint64, 0, len(s.Windows))
	for id := range s.Windows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := s.Windows[ids[i]], s.Windows[ids[j]]
		if left.DisplayIndex != right.DisplayIndex {
			return left.DisplayIndex < right.DisplayIndex
		}
		return ids[i] < ids[j]
	})
	return ids
}

func paneIDsFromLayout(layout LayoutNode) []uint64 {
	if layout == nil {
		return nil
	}
	return layout.PaneIDs()
}

func replacePaneWithSplit(layout LayoutNode, targetPaneID, newPaneID uint64, direction SplitDirection) (LayoutNode, bool) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID != targetPaneID {
			return layout, false
		}
		return &SplitLayout{
			Direction: direction,
			Ratio:     500,
			First:     node,
			Second:    &PaneLayout{PaneID: newPaneID},
		}, true
	case *SplitLayout:
		if updated, ok := replacePaneWithSplit(node.First, targetPaneID, newPaneID, direction); ok {
			node.First = updated
			return node, true
		}
		if updated, ok := replacePaneWithSplit(node.Second, targetPaneID, newPaneID, direction); ok {
			node.Second = updated
			return node, true
		}
	}
	return layout, false
}

func swapLayoutPanes(layout LayoutNode, firstPaneID, secondPaneID uint64) bool {
	first := paneLayoutByID(layout, firstPaneID)
	second := paneLayoutByID(layout, secondPaneID)
	if first == nil || second == nil || first == second {
		return false
	}
	first.PaneID, second.PaneID = second.PaneID, first.PaneID
	return true
}

func paneLayoutByID(layout LayoutNode, paneID uint64) *PaneLayout {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID == paneID {
			return node
		}
	case *SplitLayout:
		if pane := paneLayoutByID(node.First, paneID); pane != nil {
			return pane
		}
		return paneLayoutByID(node.Second, paneID)
	}
	return nil
}

func removePaneFromLayout(layout LayoutNode, targetPaneID uint64) (LayoutNode, uint64, bool) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID == targetPaneID {
			return nil, 0, true
		}
	case *SplitLayout:
		if updated, focusedPaneID, removed := removePaneFromLayout(node.First, targetPaneID); removed {
			if updated == nil {
				return node.Second, firstPaneID(node.Second), true
			}
			node.First = updated
			return node, focusedPaneID, true
		}
		if updated, focusedPaneID, removed := removePaneFromLayout(node.Second, targetPaneID); removed {
			if updated == nil {
				return node.First, firstPaneID(node.First), true
			}
			node.Second = updated
			return node, focusedPaneID, true
		}
	}
	return layout, 0, false
}

func firstPaneID(layout LayoutNode) uint64 {
	if layout == nil {
		return 0
	}
	ids := layout.PaneIDs()
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

func containsPane(ids []uint64, paneID uint64) bool {
	for _, id := range ids {
		if id == paneID {
			return true
		}
	}
	return false
}

func windowHasPane(window *Window, paneID uint64) bool {
	if window == nil {
		return false
	}
	return containsPane(window.Layout.PaneIDs(), paneID)
}

func windowPrimaryPaneID(window *Window) uint64 {
	if window == nil {
		return 0
	}
	ids := window.Layout.PaneIDs()
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

func clonePlacements(in []PanePlacement) []PanePlacement {
	out := make([]PanePlacement, len(in))
	copy(out, in)
	return out
}

func cloneWindow(window *Window) *Window {
	if window == nil {
		return nil
	}
	return &Window{
		ID:             window.ID,
		DisplayIndex:   window.DisplayIndex,
		Name:           window.Name,
		AutomaticName:  window.AutomaticName,
		ActivePaneID:   window.ActivePaneID,
		Layout:         window.Layout,
		LayoutRevision: window.LayoutRevision,
	}
}

const processNameRefreshInterval = time.Second

type processNameInput struct {
	windowID uint64
	paneID   uint64
	pane     *Pane
	anchor   Anchor
}

func (s *Session) startProcessNameMonitor() {
	s.nameMonitor.Do(func() {
		go s.runProcessNameMonitor()
	})
}

func (s *Session) runProcessNameMonitor() {
	refresh := func() {
		ctx, cancel := context.WithTimeout(context.Background(), processNameRefreshInterval)
		defer cancel()
		_ = s.refreshAutomaticWindowNames(ctx, s.processNames)
	}
	refresh()
	ticker := time.NewTicker(processNameRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			refresh()
		case <-s.operationsDone:
			return
		}
	}
}

// refreshAutomaticWindowNames observes processes outside the Session actor,
// then posts only validated name changes back through that actor. The status
// goroutine continues to receive ordinary immutable status models.
func (s *Session) refreshAutomaticWindowNames(ctx context.Context, observer ProcessObserver) error {
	if observer == nil {
		observer = NewProcessObserver()
	}
	var inputs []processNameInput
	if err := s.coordinate(func() error {
		inputs = make([]processNameInput, 0, len(s.Windows))
		for _, window := range s.Windows {
			if window == nil || !window.AutomaticName {
				continue
			}
			paneID := windowActivePaneID(window)
			pane := s.Panes[paneID]
			if pane == nil || pane.Root.PID <= 0 {
				continue
			}
			inputs = append(inputs, processNameInput{
				windowID: window.ID,
				paneID:   paneID,
				pane:     pane,
				anchor: Anchor{
					Key: PaneKey{
						SessionID: s.ID,
						PaneID:    paneID,
					},
					Root:        pane.Root,
					PTY:         pane.PTY,
					RootIsShell: len(pane.Launch.RequestedArgv) == 0,
				},
			})
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil || len(inputs) == 0 {
		return err
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].windowID < inputs[j].windowID })
	anchors := make([]Anchor, len(inputs))
	for index := range inputs {
		anchors[index] = inputs[index].anchor
	}
	observations := observer.Observe(ctx, anchors)
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.coordinate(func() error {
		changed := false
		for _, input := range inputs {
			window := s.Windows[input.windowID]
			if window == nil || !window.AutomaticName || windowActivePaneID(window) != input.paneID ||
				s.Panes[input.paneID] != input.pane || input.pane.Root != input.anchor.Root {
				continue
			}
			name := observationWindowName(observations[input.anchor.Key])
			if name != "" && name != window.Name {
				window.Name = name
				changed = true
			}
		}
		if changed {
			return s.publishStatusBar()
		}
		return nil
	})
}

func windowActivePaneID(window *Window) uint64 {
	if window == nil {
		return 0
	}
	if windowHasPane(window, window.ActivePaneID) {
		return window.ActivePaneID
	}
	return windowPrimaryPaneID(window)
}

func observationWindowName(observation ProcessObservation) string {
	var observed *ObservedProcess
	switch observation.Status {
	case StatusDetected:
		observed = observation.Candidate
	case StatusShellOwned:
		observed = observation.Root
	default:
		return ""
	}
	if observed == nil {
		return ""
	}
	name := observed.Name
	if name == "" && observed.Exe != "" {
		name = filepath.Base(strings.TrimSuffix(observed.Exe, " (deleted)"))
	}
	if name == "" && len(observed.Argv) > 0 {
		name = filepath.Base(observed.Argv[0])
	}
	return cleanProcessName(name)
}

func cleanProcessName(name string) string {
	runes := make([]rune, 0, len(name))
	for _, current := range strings.ToValidUTF8(name, "�") {
		if unicode.IsControl(current) {
			continue
		}
		runes = append(runes, current)
		if len(runes) == 64 {
			break
		}
	}
	return strings.TrimSpace(string(runes))
}

func clonePromptState(prompt *PromptState) *PromptState {
	if prompt == nil {
		return nil
	}
	out := *prompt
	out.Text = append([]rune(nil), prompt.Text...)
	out.pendingUTF8 = append([]byte(nil), prompt.pendingUTF8...)
	out.PendingEscape = append([]byte(nil), prompt.PendingEscape...)
	return &out
}

func cloneClientState(c *ClientState) *ClientState {
	if c == nil {
		return nil
	}
	out := *c
	out.RenderBindings = append([]RenderBinding(nil), c.RenderBindings...)
	out.PrefixEscape = append([]byte(nil), c.PrefixEscape...)
	out.Prompt = clonePromptState(c.Prompt)
	return &out
}
