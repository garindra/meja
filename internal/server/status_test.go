package server

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func attachStatusTestClient(t *testing.T, s *Session, client *ClientInstance) {
	t.Helper()
	s.clientInstance = client
	if err := s.attachStatusOutput(client, client.StatusOutput); err != nil {
		t.Fatal(err)
	}
}

func TestRenameWindowPromptRendersEditsSubmitAndCancel(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	window, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash"}, 0)
	statusClient := newStatusTestClient()
	state := s
	handler := &ClientInstance{}
	attachStatusTestClient(t, state, testClientInstance(nil, nil, &statusClient.wire))

	s.ConsumeInputByte(0, 0x02)
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, ',')); err != nil {
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

	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, 'x')); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, 0x7f)); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)

	for _, b := range []byte("xy") {
		if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, b)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	consumed, events, terminated := s.ConsumePromptInput(0, []byte("\x1b[3~"))
	if consumed != 4 || len(events) != 1 || terminated {
		t.Fatalf("delete sequence consumed=%d events=%#v", consumed, events)
	}
	if err := runStatusEvent(t, s, handler, events[0]); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)

	for i := 0; i < len("bashx"); i++ {
		if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, 0x7f)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	for _, b := range []byte("zsh") {
		if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, b)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, '\r')); err != nil {
		t.Fatal(err)
	}
	status = statusClient.read(t)
	assertStatusText(t, status, "[0] 0:zsh* ")
	if window.Name != "zsh" || s.ActivePrompt(0) != nil {
		t.Fatalf("submitted window = %#v prompt=%#v", window, s.ActivePrompt(0))
	}

	s.ConsumeInputByte(0, 0x02)
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, ',')); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	s.ConsumeInputByte(0, 0x1b)
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, 'x')); err != nil {
		t.Fatal(err)
	}
	status = statusClient.read(t)
	assertStatusText(t, status, "[0] 0:zsh* ")
	if window.Name != "zsh" {
		t.Fatalf("cancel changed window name to %q", window.Name)
	}

	s.ConsumeInputByte(0, 0x02)
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, ',')); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(0, 0x03)); err != nil {
		t.Fatal(err)
	}
	status = statusClient.read(t)
	assertStatusText(t, status, "[0] 0:zsh* ")
}

func TestRenameSessionPromptUpdatesStatusName(t *testing.T) {
	s := NewSession(7)
	s.setSessionName("work")
	client := s.NewClient(clientID0)
	client.TerminalCols, client.TerminalRows = 80, 23
	s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash"}, clientID0)
	statusClient := newStatusTestClient()
	state := s
	d := &Daemon{sessions: map[uint64]*Session{7: state}}
	handler := &ClientInstance{Daemon: d}
	attachStatusTestClient(t, state, testClientInstance(nil, nil, &statusClient.wire))

	s.ConsumeInputByte(clientID0, 0x02)
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(clientID0, '$')); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, statusClient.read(t), "(rename-session) work")
	for range "work" {
		if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(clientID0, 0x7f)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	for _, b := range []byte("dev") {
		if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(clientID0, b)); err != nil {
			t.Fatal(err)
		}
		statusClient.read(t)
	}
	if err := runStatusEvent(t, s, handler, s.ConsumeInputByte(clientID0, '\r')); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, statusClient.read(t), "[dev] 0:bash* ")
	if got := s.SessionName(); got != "dev" {
		t.Fatalf("session name = %q", got)
	}
}

func TestZoomedWindowStatusIncludesZFlag(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 23
	s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash", terminal: newTerminal(80, 23)}, 0)
	if _, _, err := s.SplitFocusedPane(0, &Pane{ID: s.AddPaneID(), Title: "logs", terminal: newTerminal(80, 23)}, SplitVertical); err != nil {
		t.Fatal(err)
	}
	statusClient := newStatusTestClient()
	attachStatusTestClient(t, s, testClientInstance(nil, nil, &statusClient.wire))
	if err := s.commandToggleZoom(); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, statusClient.read(t), "[0] 0:bash*Z ")
}

func TestCommandErrorUsesPromptStyleThenRestoresNormalStatus(t *testing.T) {
	s := NewSession(0)
	s.statusMessageDuration = 10 * time.Millisecond
	client := s.NewClient(clientID0)
	client.TerminalCols, client.TerminalRows = 80, 23
	s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash", terminal: newTerminal(80, 23)}, clientID0)
	statusClient := newStatusTestClient()
	attachStatusTestClient(t, s, testClientInstance(nil, nil, &statusClient.wire))
	handler := &ClientInstance{}

	if _, err := s.BeginCommandPrompt(clientID0); err != nil {
		t.Fatal(err)
	}
	if err := s.publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	statusClient.read(t)
	_, events, terminated := s.ConsumePromptInput(clientID0, []byte("send-keys\r"))
	if !terminated || len(events) == 0 {
		t.Fatalf("command prompt events=%#v terminated=%v", events, terminated)
	}
	if err := runStatusEvent(t, s, handler, events[len(events)-1]); err != nil {
		t.Fatal(err)
	}
	errorStatus := statusClient.read(t)
	assertStatusText(t, errorStatus, `unknown command "send-keys"`)
	for i, cell := range errorStatus.Cells {
		if cell.StyleID != statusPromptStyleID {
			t.Fatalf("error status cell %d style=%d, want %d", i, cell.StyleID, statusPromptStyleID)
		}
	}

	deadline := time.Now().Add(time.Second)
	for {
		cleared := false
		if err := s.coordinate(func() error {
			cleared = s.Clients[clientID0].StatusMessage == ""
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
	assertStatusText(t, normalStatus, "[0] 0:bash* ")
	for i, cell := range normalStatus.Cells {
		if cell.StyleID != statusNormalStyleID {
			t.Fatalf("restored status cell %d style=%d, want %d", i, cell.StyleID, statusNormalStyleID)
		}
	}
}

func TestSuccessfulSetRootPromptRestoresNormalStatus(t *testing.T) {
	s := NewSession(0)
	root := t.TempDir()
	s.rootDir = root
	s.processNames = emptyProcessObserver{}
	client := s.NewClient(clientID0)
	client.TerminalCols, client.TerminalRows = 80, 23
	pane := &Pane{
		ID:       s.AddPaneID(),
		Title:    "bash",
		Launch:   PaneLaunch{Cwd: root},
		terminal: newTerminal(80, 23),
	}
	s.CreateWindow(pane, clientID0)
	statusClient := newStatusTestClient()
	handler := &ClientInstance{}
	attachStatusTestClient(t, s, testClientInstance(nil, nil, &statusClient.wire))

	if _, err := s.BeginCommandPrompt(clientID0); err != nil {
		t.Fatal(err)
	}
	if err := s.publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, statusClient.read(t), ":")

	for _, b := range []byte("set-root .\r") {
		event := s.ConsumeInputByte(clientID0, b)
		if event.Command == serverCommandNone {
			continue
		}
		if err := runStatusEvent(t, s, handler, event); err != nil {
			t.Fatal(err)
		}
		status := statusClient.read(t)
		if b == '\r' {
			assertStatusText(t, status, "[0] 0:bash* ")
			for i, cell := range status.Cells {
				if cell.StyleID != statusNormalStyleID {
					t.Fatalf("submitted status cell %d style=%d, want %d", i, cell.StyleID, statusNormalStyleID)
				}
			}
		}
	}
}

func runStatusEvent(t *testing.T, s *Session, handler *ClientInstance, event serverInputEvent) error {
	t.Helper()
	_, err := s.handleServerInputEvent(handler, event)
	return err
}

type testStatusBar struct {
	Cells  []protocol.Cell
	Styles map[uint32]protocol.Style
}

type statusTestClient struct {
	wire    bytes.Buffer
	decoder *protocol.DisplayDecoder
	status  testStatusBar
	row     int
	column  int
	styleID uint32
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
				c.status.Cells = append(c.status.Cells, make([]protocol.Cell, end-len(c.status.Cells))...)
			}
			for c.column < end {
				cluster := string(command.Fill.Rune)
				if command.Fill.Rune == ' ' {
					cluster = ""
				}
				c.status.Cells[c.column] = protocol.Cell{Cluster: cluster, StyleID: c.styleID, Width: command.Fill.Width}
				c.column++
			}
		case protocol.DisplayOpcodeWriteTextUTF8:
			for _, r := range string(command.Text) {
				c.status.Cells[c.column] = protocol.Cell{Cluster: string(r), StyleID: c.styleID, Width: 1}
				c.column++
			}
		case protocol.DisplayOpcodePresent:
			out := testStatusBar{Cells: append([]protocol.Cell(nil), c.status.Cells...), Styles: make(map[uint32]protocol.Style, len(c.status.Styles))}
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
	var text strings.Builder
	for _, cell := range status.Cells {
		if cell.Cluster == "" {
			text.WriteByte(' ')
		} else {
			text.WriteString(cell.Cluster)
		}
	}
	got := strings.TrimRight(text.String(), " ")
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		suffix := "[" + hostname + "]"
		if !strings.HasSuffix(got, suffix) {
			t.Fatalf("status text = %q, want hostname suffix %q", got, suffix)
		}
		got = strings.TrimRight(strings.TrimSuffix(got, suffix), " ")
	}
	if strings.TrimRight(want, " ") != got {
		t.Fatalf("status text = %q, want %q", got, strings.TrimRight(want, " "))
	}
}

func TestStatusOutputReconnectGetsBarrierlessFullRefresh(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(clientID0)
	client.TerminalCols, client.TerminalRows = 40, 3
	s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "bash"}, clientID0)

	first := newStatusTestClient()
	firstConnection := testClientInstance(nil, nil, &first.wire)
	attachStatusTestClient(t, s, firstConnection)
	if err := s.publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, first.read(t), "[0] 0:bash* ")

	second := newStatusTestClient()
	secondConnection := testClientInstance(nil, nil, &second.wire)
	attachStatusTestClient(t, s, secondConnection)
	status := second.read(t)
	assertStatusText(t, status, "[0] 0:bash* ")
	if _, ok := status.Styles[statusNormalStyleID]; !ok {
		t.Fatal("reconnected status stream did not reinstall normal style")
	}
	if _, ok := status.Styles[statusPromptStyleID]; !ok {
		t.Fatal("reconnected status stream did not reinstall prompt style")
	}

	firstConnection.detaching.Store(true)
	s.detachClientInstance()
	s.Name = "live"
	if err := s.publishStatusBar(); err != nil {
		t.Fatal(err)
	}
	assertStatusText(t, second.read(t), "[live] 0:bash* ")
}
