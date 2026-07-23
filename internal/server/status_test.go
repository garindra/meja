package server

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func attachStatusTestClient(t *testing.T, s *SessionState, client *ClientInstance) {
	t.Helper()
	previous := clientForState(s)
	client.terminalCols.Store(previous.terminalCols.Load())
	client.terminalRows.Store(previous.terminalRows.Load())
	setLeasedTestClient(t, s, client, 1)
	if err := client.attachStatusOutput(client.StatusOutput); err != nil {
		t.Fatal(err)
	}
}

func TestRenameWindowPromptRendersEditsSubmitAndCancel(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	window, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash"})
	statusClient := newStatusTestClient()
	state := s
	attachStatusTestClient(t, state, testClientInstance(nil, nil, &statusClient.wire))

	clientForState(s).ConsumeInputByte(0x02)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(',')); err != nil {
		t.Fatal(err)
	}
	status := statusClient.read(t)
	assertStatusText(t, status, "(rename-window) bash")
	if got := status.Styles[statusNormalStyleID].FG; got != (protocol.Color{Mode: "indexed", Index: 15}) {
		t.Fatalf("normal status foreground = %#v, want white", got)
	}
	if got := status.Styles[statusNormalStyleID].BG; got != (protocol.Color{Mode: "rgb", R: 42, G: 88, B: 170}) {
		t.Fatalf("normal status background = %#v", got)
	}
	if got := status.Styles[statusPromptStyleID]; got.FG != (protocol.Color{Mode: "indexed", Index: 0}) || got.BG != (protocol.Color{Mode: "indexed", Index: 3}) {
		t.Fatalf("prompt style = %#v", got)
	}
	for i, cell := range status.Cells {
		if cell.StyleID != statusPromptStyleID || cell.Width != 1 {
			t.Fatalf("status cell %d = %#v, want prompt style width 1", i, cell)
		}
	}

	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte('x')); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(0x7f)); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)

	for _, b := range []byte("xy") {
		if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(b)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	consumed, events, terminated := clientForState(s).ConsumePromptInput([]byte("\x1b[3~"))
	if consumed != 4 || len(events) != 1 || terminated {
		t.Fatalf("delete sequence consumed=%d events=%#v", consumed, events)
	}
	if err := runStatusEvent(t, s, events[0]); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)

	for i := 0; i < len("bashx"); i++ {
		if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(0x7f)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	for _, b := range []byte("zsh") {
		if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(b)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte('\r')); err != nil {
		t.Fatal(err)
	}
	status = statusClient.read(t)
	assertStatusText(t, status, "[1] 0:zsh* ")
	if window.Name != "zsh" || clientForState(s).ActivePrompt() != nil {
		t.Fatalf("submitted window = %#v prompt=%#v", window, clientForState(s).ActivePrompt())
	}

	clientForState(s).ConsumeInputByte(0x02)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(',')); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	clientForState(s).ConsumeInputByte(0x1b)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte('x')); err != nil {
		t.Fatal(err)
	}
	status = statusClient.read(t)
	assertStatusText(t, status, "[1] 0:zsh* ")
	if window.Name != "zsh" {
		t.Fatalf("cancel changed window name to %q", window.Name)
	}

	clientForState(s).ConsumeInputByte(0x02)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(',')); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(0x03)); err != nil {
		t.Fatal(err)
	}
	status = statusClient.read(t)
	assertStatusText(t, status, "[1] 0:zsh* ")
}

func TestRenameSessionPromptUpdatesStatusName(t *testing.T) {
	s := NewSessionState(7)
	s.setSessionName("work")
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash"})
	statusClient := newStatusTestClient()
	state := s
	d := &Daemon{sessions: map[uint64]*SessionState{7: state}}
	state.daemon = d
	attachStatusTestClient(t, state, testClientInstance(nil, nil, &statusClient.wire))

	clientForState(s).ConsumeInputByte(0x02)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte('$')); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, statusClient.read(t), "(rename-session) work")
	for range "work" {
		if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(0x7f)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	for _, b := range []byte("dev") {
		if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(b)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte('\r')); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, statusClient.read(t), "[dev] 0:bash* ")
	if got := s.SessionName(); got != "dev" {
		t.Fatalf("session name = %q", got)
	}
}

func TestZoomedWindowStatusIncludesZFlag(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash", terminal: newTerminal(80, 23)})
	if _, _, err := splitTestFocusedPane(s, &Pane{ID: testAddPaneID(s), Title: "logs", terminal: newTerminal(80, 23)}, SplitVertical); err != nil {
		t.Fatal(err)
	}
	statusClient := newStatusTestClient()
	attachStatusTestClient(t, s, testClientInstance(nil, nil, &statusClient.wire))
	if _, err := executeTestClientCommand(clientForState(s), []string{"resize-pane", "-Z"}); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, statusClient.read(t), "[0] 0:bash*Z ")
}

func TestCommandErrorUsesPromptStyleThenRestoresNormalStatus(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash", terminal: newTerminal(80, 23)})
	statusClient := newStatusTestClient()
	attachStatusTestClient(t, s, testClientInstance(nil, nil, &statusClient.wire))
	clientForState(s).statusMessageDuration = 10 * time.Millisecond

	if _, err := clientForState(s).BeginCommandPrompt(); err != nil {
		t.Fatal(err)
	}
	if err := clientForState(s).publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	_, events, terminated := clientForState(s).ConsumePromptInput([]byte("send-keys\r"))
	if !terminated || len(events) == 0 {
		t.Fatalf("command prompt events=%#v terminated=%v", events, terminated)
	}
	if err := runStatusEvent(t, s, events[len(events)-1]); err != nil {
		t.Fatal(err)
	}
	errorStatus := statusClient.read(t)
	assertStatusText(t, errorStatus, `send-keys requires at least one key`)
	for i, cell := range errorStatus.Cells {
		if cell.StyleID != statusPromptStyleID {
			t.Fatalf("error status cell %d style=%d, want %d", i, cell.StyleID, statusPromptStyleID)
		}
	}

	deadline := time.Now().Add(time.Second)
	for {
		cleared := false
		if err := runStateOperation(s, func() error {
			message, _ := clientForState(s).statusMessage.Load().(string)
			cleared = message == ""
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command error did not clear")
		}
		time.Sleep(time.Millisecond)
	}
	normalStatus := statusClient.read(t)
	assertStatusText(t, normalStatus, "[1] 0:bash* ")
	for i, cell := range normalStatus.Cells {
		if cell.StyleID != statusNormalStyleID {
			t.Fatalf("restored status cell %d style=%d, want %d", i, cell.StyleID, statusNormalStyleID)
		}
	}
}

func TestSuccessfulSetRootPromptRestoresNormalStatus(t *testing.T) {
	s := NewSessionState(1)
	root := t.TempDir()
	s.rootDir = root
	s.daemon = testDaemonForState(s)
	s.daemon.processObserver = emptyProcessObserver{}
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	pane := &Pane{
		ID:       testAddPaneID(s),
		Title:    "bash",
		Launch:   PaneLaunch{Cwd: root},
		terminal: newTerminal(80, 23),
	}
	createTestWindow(s, pane)
	statusClient := newStatusTestClient()
	attachStatusTestClient(t, s, testClientInstance(nil, nil, &statusClient.wire))

	if _, err := clientForState(s).BeginCommandPrompt(); err != nil {
		t.Fatal(err)
	}
	if err := clientForState(s).publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	assertStatusTextWithLocation(t, statusClient.read(t), ":", currentStatusLocation(root))

	for _, b := range []byte("set-root .\r") {
		event := clientForState(s).ConsumeInputByte(b)
		if event.Command == serverCommandNone {
			continue
		}
		if err := runStatusEvent(t, s, event); err != nil {
			t.Fatal(err)
		}
		status := statusClient.read(t)
		if b == '\r' {
			assertStatusTextWithLocation(t, status, "[1] 0:bash* ", currentStatusLocation(root))
			for i, cell := range status.Cells {
				if cell.StyleID != statusNormalStyleID {
					t.Fatalf("submitted status cell %d style=%d, want %d", i, cell.StyleID, statusNormalStyleID)
				}
			}
		}
	}
}

func runStatusEvent(t *testing.T, s *SessionState, event serverInputEvent) error {
	t.Helper()
	_, err := clientForState(s).handleServerInputEvent(event)
	return err
}

type testStatusBar struct {
	Cells  []decodedTestCell
	Styles map[uint32]protocol.Style
}

type statusTestClient struct {
	wire    synchronizedBuffer
	decoder *protocol.DisplayDecoder
	status  testStatusBar
	row     int
	column  int
	styleID uint32
}

type synchronizedBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	data bytes.Buffer
}

func (b *synchronizedBuffer) init() {
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}
}

func (b *synchronizedBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	for b.data.Len() == 0 {
		b.cond.Wait()
	}
	return b.data.Read(p)
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	n, err := b.data.Write(p)
	b.cond.Broadcast()
	return n, err
}

func newStatusTestClient() *statusTestClient {
	c := &statusTestClient{status: testStatusBar{Styles: make(map[uint32]protocol.Style)}}
	c.decoder = protocol.NewDisplayDecoder(&c.wire)
	return c
}

func (c *statusTestClient) read(t *testing.T) testStatusBar {
	t.Helper()
	for {
		command, _, err := c.decoder.ReadCommand()
		if err != nil {
			t.Fatal(err)
		}
		switch command.Opcode {
		case protocol.DisplayOpcodeStyleInstall:
			c.status.Styles[command.StyleID] = command.Style
		case protocol.DisplayOpcodeSetWritePosition:
			c.row, c.column = command.Row, command.Column
		case protocol.DisplayOpcodeSetWriteStyle:
			c.styleID = command.StyleID
		case protocol.DisplayOpcodeFill:
			end := c.column + command.Fill.Columns
			if end > len(c.status.Cells) {
				c.status.Cells = append(c.status.Cells, make([]decodedTestCell, end-len(c.status.Cells))...)
			}
			for c.column < end {
				cluster := string(command.Fill.Rune)
				if command.Fill.Rune == ' ' {
					cluster = ""
				}
				c.status.Cells[c.column] = decodedTestCell{Cluster: cluster, StyleID: c.styleID, Width: command.Fill.Width}
				c.column++
			}
		case protocol.DisplayOpcodeWriteTextUTF8:
			for _, r := range string(command.Text) {
				c.status.Cells[c.column] = decodedTestCell{Cluster: string(r), StyleID: c.styleID, Width: 1}
				c.column++
			}
		case protocol.DisplayOpcodePresent:
			out := testStatusBar{Cells: append([]decodedTestCell(nil), c.status.Cells...), Styles: make(map[uint32]protocol.Style, len(c.status.Styles))}
			for id, style := range c.status.Styles {
				out.Styles[id] = style
			}
			return out
		default:
			t.Fatalf("unexpected status opcode 0x%02x", command.Opcode)
		}
	}
}

func assertStatusText(t *testing.T, status testStatusBar, want string) {
	t.Helper()
	assertStatusTextWithLocation(t, status, want, currentStatusLocation(""))
}

func assertStatusTextWithLocation(t *testing.T, status testStatusBar, want, location string) {
	t.Helper()
	var text strings.Builder
	for _, cell := range status.Cells {
		if cell.Cluster == "" {
			text.WriteByte(' ')
		} else {
			text.WriteString(cell.Cluster)
		}
	}
	got := strings.TrimRight(text.String(), " ")
	left, right := statusLineParts(len(status.Cells), want, location)
	wantCells := make([]rune, len(status.Cells))
	for i := range wantCells {
		wantCells[i] = ' '
	}
	copy(wantCells, left)
	copy(wantCells[len(wantCells)-len(right):], right)
	wantRendered := strings.TrimRight(string(wantCells), " ")
	if wantRendered != got {
		t.Fatalf("status text = %q, want %q", got, wantRendered)
	}
}

func TestStatusLocationNormalizesHome(t *testing.T) {
	tests := []struct {
		name string
		root string
		want string
	}{
		{name: "home", root: "/home/tester", want: "[host:~]"},
		{name: "under home", root: "/home/tester/projects/test", want: "[host:~/projects/test]"},
		{name: "outside home", root: "/srv/projects/test", want: "[host:/srv/projects/test]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := statusLocation("host", test.root, "/home/tester"); got != test.want {
				t.Fatalf("status location = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStatusLinePartsSharesOverflowAndKeepsLocationTail(t *testing.T) {
	left, right := statusLineParts(30, "left status is long", "[host:~/projects/a/last]")
	if got := string(left); got != "left status is…" {
		t.Fatalf("left status = %q, want %q", got, "left status is…")
	}
	if got := string(right); got != "[host:…/a/last]" {
		t.Fatalf("right status = %q, want %q", got, "[host:…/a/last]")
	}
}

func TestStatusOutputReconnectGetsBarrierlessFullRefresh(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(40, 3)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash"})

	first := newStatusTestClient()
	firstConnection := testClientInstance(nil, nil, &first.wire)
	attachStatusTestClient(t, s, firstConnection)
	if err := clientForState(s).publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, first.read(t), "[0] 0:bash* ")

	second := newStatusTestClient()
	secondConnection := testClientInstance(nil, nil, &second.wire)
	attachStatusTestClient(t, s, secondConnection)
	if err := clientForState(s).publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	status := second.read(t)
	assertStatusText(t, status, "[0] 0:bash* ")
	if _, ok := status.Styles[statusNormalStyleID]; !ok {
		t.Fatal("reconnected status stream did not reinstall normal style")
	}
	if _, ok := status.Styles[statusPromptStyleID]; !ok {
		t.Fatal("reconnected status stream did not reinstall prompt style")
	}

	firstConnection.detaching.Store(true)
	firstConnection.releaseFrontendResources(nil)
	s.Name = "live"
	if err := clientForState(s).publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, second.read(t), "[live] 0:bash* ")
}
