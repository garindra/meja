package server

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/garindra/meja/internal/protocol"
	"github.com/garindra/meja/internal/theme"
)

const (
	statusNormalStyleID uint32 = 1
	statusPromptStyleID uint32 = 2
)

type statusModel struct {
	width    int
	text     string
	location string
	styleID  uint32
}

type statusCommand struct {
	attach io.Writer
	detach bool
	client *ClientInstance
	model  *statusModel
	done   chan error
}

func (s *Session) sendStatusCommand(command statusCommand) error {
	if s.statusCommands == nil {
		return nil
	}
	command.done = make(chan error, 1)
	select {
	case s.statusCommands <- command:
	case <-s.operationsDone:
		return nil
	}
	select {
	case err := <-command.done:
		return err
	case <-s.operationsDone:
		return nil
	}
}

func (s *Session) attachStatusOutput(client *ClientInstance, stream io.Writer) error {
	return s.sendStatusCommand(statusCommand{attach: stream, client: client})
}

func (s *Session) detachStatusOutput(client *ClientInstance) error {
	return s.sendStatusCommand(statusCommand{detach: true, client: client})
}

func (s *Session) runStatusOutput() {
	var client *ClientInstance
	var output *renderOutput
	initialized := false
	var latest statusModel
	var hasLatest bool
	for {
		select {
		case command := <-s.statusCommands:
			var err error
			switch {
			case command.attach != nil:
				client = command.client
				output = newRenderOutput(command.attach)
				initialized = false
				if hasLatest {
					err = renderStatusModel(output, latest, true)
					initialized = err == nil
				}
			case command.detach:
				if client == command.client {
					client = nil
					output = nil
					initialized = false
				}
			case command.model != nil:
				latest = *command.model
				hasLatest = true
				if output != nil {
					err = renderStatusModel(output, latest, !initialized)
					initialized = err == nil
				}
			}
			if err != nil {
				output = nil
				initialized = false
			}
			command.done <- err
		case <-s.operationsDone:
			return
		}
	}
}

func renderStatusModel(output *renderOutput, model statusModel, full bool) error {
	normal := protocol.Style{FG: protocol.Color{Mode: "indexed", Index: 15}, BG: theme.AccentColor()}
	prompt := protocol.Style{FG: protocol.Color{Mode: "indexed", Index: 0}, BG: protocol.Color{Mode: "indexed", Index: 3}}
	if full {
		if err := installStyle(output, statusNormalStyleID, normal); err != nil {
			return err
		}
		if err := installStyle(output, statusPromptStyleID, prompt); err != nil {
			return err
		}
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 0}); err != nil {
		return err
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWriteStyle, StyleID: model.styleID}); err != nil {
		return err
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeFill, Fill: protocol.Fill{Columns: model.width, Rune: ' ', Width: 1}}); err != nil {
		return err
	}
	left, right := statusLineParts(model.width, model.text, model.location)
	if len(left) > 0 {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: 0}); err != nil {
			return err
		}
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte(string(left))}); err != nil {
			return err
		}
	}
	if len(right) > 0 {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeSetWritePosition, Row: 0, Column: model.width - len(right)}); err != nil {
			return err
		}
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte(string(right))}); err != nil {
			return err
		}
	}
	return output.present()
}

func statusLineParts(width int, text, location string) ([]rune, []rune) {
	if width <= 0 {
		return nil, nil
	}

	left := []rune(text)
	right := []rune(location)
	if len(left)+len(right) <= width {
		return left, right
	}

	// When the two sides compete for space, give each side half the bar first.
	// Unused space from a short side is then made available to the other side.
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
		left = ellipsizePrefix(left, leftWidth)
	}
	if len(right) > rightWidth {
		right = ellipsizeLocation(right, rightWidth)
	}
	return left, right
}

func ellipsizePrefix(value []rune, width int) []rune {
	if width <= 0 {
		return nil
	}
	if width == 1 {
		return []rune{'…'}
	}
	result := make([]rune, width)
	copy(result, value[:width-1])
	result[width-1] = '…'
	return result
}

func ellipsizeLocation(value []rune, width int) []rune {
	if width <= 0 {
		return nil
	}
	if width == 1 {
		return []rune{'…'}
	}

	// Keep the host prefix and closing bracket when the location is wide
	// enough, while using the path suffix as the most valuable content.
	colon := -1
	for i, r := range value {
		if r == ':' {
			colon = i
			break
		}
	}
	if len(value) >= 3 && value[0] == '[' && value[len(value)-1] == ']' && colon > 0 {
		const ellipsisWidth = 1
		prefixWidth := colon + 1
		suffixWidth := 1
		// Avoid reducing the path to only a couple of characters just to
		// preserve the host; a useful path suffix wins in very narrow bars.
		tailWidth := width - prefixWidth - ellipsisWidth - suffixWidth
		if tailWidth >= 4 {
			result := make([]rune, 0, width)
			result = append(result, value[:prefixWidth]...)
			result = append(result, '…')
			result = append(result, value[len(value)-suffixWidth-tailWidth:len(value)-suffixWidth]...)
			result = append(result, value[len(value)-suffixWidth:]...)
			return result
		}
	}

	result := make([]rune, width)
	result[0] = '…'
	copy(result[1:], value[len(value)-width+1:])
	return result
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

func (s *Session) publishStatusBar() error {
	client := s.SnapshotClient(clientID0)
	if client == nil || client.TerminalCols == 0 {
		return nil
	}
	width := int(client.TerminalCols)
	styleID := statusNormalStyleID
	text := ""
	if prompt := s.ActivePrompt(clientID0); prompt != nil {
		styleID = statusPromptStyleID
		text = prompt.Label + string(prompt.Text)
	} else if client.StatusMessage != "" {
		styleID = statusPromptStyleID
		text = client.StatusMessage
	} else {
		list := s.WindowStatuses(clientID0)
		if name := s.SessionName(); name != "" {
			text = fmt.Sprintf("[%s] ", name)
		} else {
			text = fmt.Sprintf("[%d] ", s.ID)
		}
		for _, window := range list {
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
	return s.sendStatusCommand(statusCommand{model: &statusModel{
		width: width, text: text, location: currentStatusLocation(s.rootDir), styleID: styleID,
	}})
}

func (s *Session) publishWindowLayout() error {
	if err := s.cancelFrontendPointerCapture(s.clientInstance); err != nil {
		return err
	}
	layout, err := s.WindowLayout(clientID0)
	if err != nil {
		return err
	}
	client := s.clientInstance
	if client != nil {
		client.rememberLayout(layout)
		for previous := client.highestLayoutRevision.Load(); layout.LayoutRevision > previous; previous = client.highestLayoutRevision.Load() {
			if client.highestLayoutRevision.CompareAndSwap(previous, layout.LayoutRevision) {
				break
			}
		}
	}
	if client == nil || client.controlOut == nil {
		return nil
	}
	return sendEncoded(client.controlOut, protocol.MsgWindowLayout, layout, protocol.EncodeWindowLayout)
}

type outputHandoff struct {
	released chan *OutputLease
	pending  map[int]struct{}
}

func (s *Session) windowForPane(paneID uint64) *Window {
	for _, window := range s.Windows {
		if windowHasPane(window, paneID) {
			return cloneWindow(window)
		}
	}
	return nil
}

func (s *Session) beginOutputHandoff() *outputHandoff {
	bindings, _ := s.RenderBindings(clientID0)
	handoff := &outputHandoff{
		released: make(chan *OutputLease, len(bindings)),
		pending:  make(map[int]struct{}, len(bindings)),
	}
	for _, binding := range bindings {
		pane := s.Pane(binding.PaneID)
		if pane == nil {
			continue
		}
		handoff.pending[binding.Slot] = struct{}{}
		pane.releaseOutputStream(handoff.released)
	}
	return handoff
}

func (s *Session) rebindOutputsAndPublishLayout(handoff *outputHandoff) error {
	if err := s.cancelFrontendPointerCapture(s.clientInstance); err != nil {
		return err
	}
	bindings, _, _, err := s.RebuildRenderBindings(clientID0)
	if err != nil {
		return err
	}
	if err := s.finishOutputHandoff(handoff, bindings); err != nil {
		return err
	}
	if err := s.publishStatusBar(); err != nil {
		return err
	}
	return s.publishWindowLayout()
}

func (s *Session) finishOutputHandoff(handoff *outputHandoff, bindings []RenderBinding) error {
	bySlot := make(map[int]RenderBinding, len(bindings))
	for _, binding := range bindings {
		bySlot[binding.Slot] = binding
	}
	if handoff == nil {
		for _, binding := range bindings {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
		return nil
	}
	for _, binding := range bindings {
		if _, waiting := handoff.pending[binding.Slot]; !waiting {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
	}
	stillPending := make(map[int]struct{}, len(handoff.pending))
	for slot := range handoff.pending {
		stillPending[slot] = struct{}{}
	}
	for i := 0; i < len(handoff.pending); i++ {
		lease := <-handoff.released
		if lease == nil {
			continue
		}
		delete(stillPending, lease.Slot)
		if binding, ok := bySlot[lease.Slot]; ok {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
	}
	// A pane can already have lost its old transport's lease before a
	// reconnect begins. Its release then returns nil, but the handoff still
	// completed and the replacement output for that logical slot must be
	// attached. Wait for every release above, then attach those nil slots.
	for slot := range stillPending {
		if binding, ok := bySlot[slot]; ok {
			if err := s.attachBinding(binding); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Session) attachBinding(binding RenderBinding) error {
	pane := s.Pane(binding.PaneID)
	window := s.windowForPane(binding.PaneID)
	if pane == nil || window == nil {
		return nil
	}
	lease := s.currentOutputLease(binding.Slot)
	if lease == nil {
		return nil
	}
	return pane.attachOutputStream(lease, window.LayoutRevision)
}

func (s *Session) publishVisibleSnapshots(handoff *outputHandoff) error {
	bindings, _ := s.RenderBindings(clientID0)
	return s.finishOutputHandoff(handoff, bindings)
}

func (s *Session) detachLeases(leases map[int]*OutputLease) error {
	for _, pane := range s.PanesSnapshot() {
		for _, lease := range leases {
			if err := pane.detachOutputStream(lease.Stream); err != nil {
				return err
			}
		}
	}
	return nil
}
