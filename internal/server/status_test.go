package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func attachStatusTestClient(t *testing.T, s *SessionState, client *ClientInstance) {
	t.Helper()
	previous := clientForState(s)
	client.terminalCols = previous.terminalCols
	client.terminalRows = previous.terminalRows
	if client.controlOut == nil {
		t.Fatal("status test client requires a control output")
	}
	setLeasedTestClient(t, s, client, 1)
}

func TestRenameWindowPromptRendersEditsSubmitAndCancel(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	window, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash"})
	statusClient := newStatusTestClient()
	state := s
	statusConnection := testClientInstance(nil, nil)
	statusConnection.controlOut = statusClient.frames
	attachStatusTestClient(t, state, statusConnection)

	clientForState(s).ConsumeInputByte(0x02)
	if err := runStatusEvent(t, s, clientForState(s).ConsumeInputByte(',')); err != nil {
		t.Fatal(err)
	}
	status := statusClient.read(t)
	assertStatusText(t, status, "(rename-window) bash")
	if status.Status.Kind != protocol.ClientStatusPrompt ||
		status.Status.Prompt.Mode != protocol.ClientStatusPromptText ||
		status.Status.Prompt.Label != "(rename-window) " ||
		status.Status.Prompt.Text != "bash" ||
		status.Status.Prompt.Cursor != len([]rune("bash")) {
		t.Fatalf("rename prompt snapshot = %#v", status.Status)
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
	statusConnection := testClientInstance(nil, nil)
	statusConnection.controlOut = statusClient.frames
	attachStatusTestClient(t, state, statusConnection)
	syncTestProjection(t, state)

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
	window, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash", terminal: newTerminal(80, 23)})
	if _, _, err := splitTestFocusedPane(s, &Pane{ID: testAddPaneID(s), Title: "logs", terminal: newTerminal(80, 23)}, SplitVertical); err != nil {
		t.Fatal(err)
	}
	statusClient := newStatusTestClient()
	statusConnection := testClientInstance(nil, nil)
	statusConnection.controlOut = statusClient.frames
	attachStatusTestClient(t, s, statusConnection)
	if _, err := executeTestClientCommand(clientForState(s), []string{"resize-pane", "-Z"}); err != nil {
		t.Fatal(err)
	}
	status := statusClient.read(t)
	assertStatusText(t, status, "[0] 0:bash*Z ")
	if len(status.Status.Windows) != 1 ||
		status.Status.Windows[0].WindowID != window.ID ||
		status.Status.Windows[0].Index != 0 ||
		!status.Status.Windows[0].Active ||
		!status.Status.Windows[0].Zoomed {
		t.Fatalf("zoomed window snapshot = %#v", status.Status.Windows)
	}
}

func TestCommandErrorUsesPromptStyleThenRestoresNormalStatus(t *testing.T) {
	s := NewSessionState(1)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash", terminal: newTerminal(80, 23)})
	statusClient := newStatusTestClient()
	statusConnection := testClientInstance(nil, nil)
	statusConnection.controlOut = statusClient.frames
	attachStatusTestClient(t, s, statusConnection)
	clientForState(s).statusMessageDuration = 10 * time.Millisecond

	if _, err := clientForState(s).BeginCommandPrompt(); err != nil {
		t.Fatal(err)
	}
	if err := clientForState(s).publishClientStatus(); err != nil {
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
	if got := snapshotTestClientActor(clientForState(s)).StatusMessage; got == "" {
		t.Fatal("command error did not install a status message")
	}
	errorStatus := statusClient.read(t)
	assertStatusText(t, errorStatus, `send-keys requires at least one key`)
	if errorStatus.Status.Kind != protocol.ClientStatusMessage ||
		errorStatus.Status.Message.ID == 0 ||
		errorStatus.Status.Message.Text != "send-keys requires at least one key" {
		t.Fatalf("temporary message snapshot = %#v", errorStatus.Status)
	}
	deadline := time.Now().Add(time.Second)
	for {
		cleared := snapshotTestClientActor(clientForState(s)).StatusMessage == ""
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
	if normalStatus.Status.Kind != protocol.ClientStatusNormal ||
		normalStatus.Status.Message != (protocol.ClientStatusMessageState{}) ||
		normalStatus.Status.Revision <= errorStatus.Status.Revision {
		t.Fatalf("cleared message snapshot = %#v, previous revision=%d", normalStatus.Status, errorStatus.Status.Revision)
	}
}

func TestSuccessfulSetRootPromptRestoresNormalStatus(t *testing.T) {
	s := NewSessionState(1)
	root := t.TempDir()
	nextRoot := t.TempDir()
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
	statusConnection := testClientInstance(nil, nil)
	statusConnection.controlOut = statusClient.frames
	attachStatusTestClient(t, s, statusConnection)

	if _, err := clientForState(s).BeginCommandPrompt(); err != nil {
		t.Fatal(err)
	}
	if err := clientForState(s).publishClientStatus(); err != nil {
		t.Fatal(err)
	}
	assertStatusTextWithLocation(t, statusClient.read(t), ":", currentStatusLocation(root))

	command := []byte("set-root " + nextRoot + "\r")
	for _, b := range command {
		event := clientForState(s).ConsumeInputByte(b)
		if event.Command == serverCommandNone {
			continue
		}
		if err := runStatusEvent(t, s, event); err != nil {
			t.Fatal(err)
		}
		status := statusClient.read(t)
		if b == '\r' {
			assertStatusTextWithLocation(t, status, "[1] 0:bash* ", currentStatusLocation(nextRoot))
		}
	}
}

func runStatusEvent(t *testing.T, s *SessionState, event serverInputEvent) error {
	t.Helper()
	_, err := clientForState(s).handleServerInputEvent(event)
	return err
}

type testStatusBar struct {
	Rendered string
	Width    int
	Status   protocol.ClientStatus
}

type statusTestClient struct {
	frames chan protocol.Frame
	width  int
}

func newStatusTestClient() *statusTestClient {
	return &statusTestClient{frames: make(chan protocol.Frame, 64), width: 80}
}

func (c *statusTestClient) read(t *testing.T) testStatusBar {
	t.Helper()
	var frame protocol.Frame
	for {
		frame = <-c.frames
		if frame.Type == protocol.MsgClientStatus {
			break
		}
	}
	status, err := protocol.DecodeClientStatus(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	switch status.Kind {
	case protocol.ClientStatusPrompt:
		text = status.Prompt.Label + status.Prompt.Text
	case protocol.ClientStatusMessage:
		text = status.Message.Text
	default:
		if status.SessionName != "" {
			text = fmt.Sprintf("[%s] ", status.SessionName)
		} else {
			text = fmt.Sprintf("[%d] ", status.SessionID)
		}
		for _, window := range status.Windows {
			flags := ""
			if window.Active {
				flags += "*"
			}
			if window.Zoomed {
				flags += "Z"
			}
			if flags == "" {
				flags = " "
			}
			text += fmt.Sprintf("%d:%s%s ", window.Index, window.Title, flags)
		}
	}
	location := statusLocation(status.ServerHostname, status.Root, status.ServerHome)
	left, right := statusLineParts(c.width, text, location)
	cells := make([]rune, c.width)
	for index := range cells {
		cells[index] = ' '
	}
	copy(cells, left)
	copy(cells[len(cells)-len(right):], right)
	return testStatusBar{
		Rendered: strings.TrimRight(string(cells), " "),
		Width:    c.width,
		Status:   status,
	}
}

func assertStatusText(t *testing.T, status testStatusBar, want string) {
	t.Helper()
	assertStatusTextWithLocation(t, status, want, currentStatusLocation(""))
}

func assertStatusTextWithLocation(t *testing.T, status testStatusBar, want, location string) {
	t.Helper()
	got := status.Rendered
	left, right := statusLineParts(status.Width, want, location)
	wantCells := make([]rune, status.Width)
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

func currentStatusLocation(root string) string {
	hostname, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	return statusLocation(hostname, root, home)
}

func statusLocation(hostname, root, home string) string {
	if root != "" {
		root = filepath.Clean(root)
	}
	if home != "" {
		home = filepath.Clean(home)
	}
	if root == "." {
		root = ""
	}
	if root != "" && home != "" {
		if relative, err := filepath.Rel(home, root); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			if relative == "." {
				root = "~"
			} else {
				root = "~/" + filepath.ToSlash(relative)
			}
		}
	}
	if hostname == "" {
		hostname = "?"
	}
	if root == "" {
		return "[" + hostname + "]"
	}
	return "[" + hostname + ":" + filepath.ToSlash(root) + "]"
}

func statusLineParts(width int, text, location string) ([]rune, []rune) {
	if width <= 0 {
		return nil, nil
	}
	left, right := []rune(text), []rune(location)
	if len(left)+len(right) <= width {
		return left, right
	}
	leftWidth := width / 2
	rightWidth := width - leftWidth
	if len(left) < leftWidth {
		rightWidth += leftWidth - len(left)
		leftWidth = len(left)
	}
	if len(right) < rightWidth {
		leftWidth += rightWidth - len(right)
		rightWidth = len(right)
	}
	if len(left) > leftWidth {
		left = append(append([]rune(nil), left[:leftWidth-1]...), '…')
	}
	if len(right) > rightWidth {
		colon := -1
		for i, r := range right {
			if r == ':' {
				colon = i
				break
			}
		}
		if len(right) >= 3 && right[0] == '[' && right[len(right)-1] == ']' && colon > 0 {
			prefixWidth := colon + 1
			tailWidth := rightWidth - prefixWidth - 2
			if tailWidth >= 4 {
				result := make([]rune, 0, rightWidth)
				result = append(result, right[:prefixWidth]...)
				result = append(result, '…')
				result = append(result, right[len(right)-1-tailWidth:len(right)-1]...)
				result = append(result, ']')
				right = result
				return left, right
			}
		}
		result := make([]rune, rightWidth)
		result[0] = '…'
		copy(result[1:], right[len(right)-rightWidth+1:])
		right = result
	}
	return left, right
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

func TestStatusReconnectGetsCompleteSnapshotWithNewerRevision(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(40, 3)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "bash"})

	first := newStatusTestClient()
	firstConnection := testClientInstance(nil, nil)
	firstConnection.controlOut = first.frames
	attachStatusTestClient(t, s, firstConnection)
	if err := clientForState(s).publishClientStatus(); err != nil {
		t.Fatal(err)
	}
	firstStatus := first.read(t)
	assertStatusText(t, firstStatus, "[0] 0:bash* ")

	second := newStatusTestClient()
	secondConnection := newClientInstance(s.daemon, testClientIdentity(firstConnection))
	secondConnection.controlOut = second.frames
	attachStatusTestClient(t, s, secondConnection)
	if err := clientForState(s).publishClientStatus(); err != nil {
		t.Fatal(err)
	}
	status := second.read(t)
	assertStatusText(t, status, "[0] 0:bash* ")
	if status.Status.Revision <= firstStatus.Status.Revision {
		t.Fatalf("reconnected status revision = %d, want newer than %d", status.Status.Revision, firstStatus.Status.Revision)
	}
	if len(status.Status.Windows) != 1 || status.Status.Windows[0].WindowID == 0 ||
		status.Status.Windows[0].Title != "bash" || !status.Status.Windows[0].Active {
		t.Fatalf("reconnected status snapshot = %#v, want complete window state", status.Status)
	}

	firstConnection.releaseFrontendResources()
	s.Name = "live"
	syncTestStatus(t, s)
	assertStatusText(t, second.read(t), "[live] 0:bash* ")
}
