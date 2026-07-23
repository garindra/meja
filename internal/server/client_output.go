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
	model  *statusModel
	done   chan error
}

func (c *ClientInstance) sendStatusCommand(command statusCommand) error {
	if c.statusCommands == nil {
		return nil
	}
	command.done = make(chan error, 1)
	select {
	case c.statusCommands <- command:
	case <-c.lifetimeDone:
		return nil
	}
	select {
	case err := <-command.done:
		return err
	case <-c.lifetimeDone:
		return nil
	}
}

func (c *ClientInstance) attachStatusOutput(stream io.Writer) error {
	return c.sendStatusCommand(statusCommand{attach: stream})
}

func (c *ClientInstance) detachStatusOutput() error {
	return c.sendStatusCommand(statusCommand{detach: true})
}

func (c *ClientInstance) runStatusOutput() {
	var output *renderOutput
	initialized := false
	var latest statusModel
	var hasLatest bool
	for {
		select {
		case command := <-c.statusCommands:
			var err error
			switch {
			case command.attach != nil:
				output = newRenderOutput(command.attach)
				initialized = false
				if hasLatest {
					err = renderStatusModel(output, latest, true)
					initialized = err == nil
				}
			case command.detach:
				output = nil
				initialized = false
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
		case <-c.lifetimeDone:
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

func (c *ClientInstance) publishStatusBar() error {
	width := int(c.terminalCols.Load())
	if width == 0 {
		return nil
	}
	status, ok := c.Daemon.clientStatusSnapshot(c.sessionID)
	if !ok {
		return nil
	}
	styleID := statusNormalStyleID
	text := ""
	if prompt := c.ActivePrompt(); prompt != nil {
		styleID = statusPromptStyleID
		text = prompt.Label + string(prompt.Text)
	} else if statusMessage, _ := c.statusMessage.Load().(string); statusMessage != "" {
		styleID = statusPromptStyleID
		text = statusMessage
	} else {
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
	return c.sendStatusCommand(statusCommand{model: &statusModel{
		width: width, text: text, location: currentStatusLocation(status.Root), styleID: styleID,
	}})
}

func (c *ClientInstance) sendPreparedClientLayout(layout protocol.ClientLayout) error {
	if c.controlOut == nil {
		return nil
	}
	return sendEncoded(c.controlOut, protocol.MsgClientLayout, layout, protocol.EncodeClientLayout)
}

type outputHandoff struct {
	released chan *OutputLease
	pending  map[int]struct{}
	waited   bool
}

func (c *ClientInstance) beginOutputHandoffWithRemovedPanes(removedPanes []*Pane) *outputHandoff {
	placements := c.currentPanePlacements()
	handoff := &outputHandoff{
		released: make(chan *OutputLease, len(placements)),
		pending:  make(map[int]struct{}, len(placements)),
	}
	for _, placement := range placements {
		var pane *Pane
		if c.Daemon != nil {
			if value, ok := c.Daemon.paneIndex.Load(placement.PaneID); ok && value != nil {
				pane = value.(*Pane)
			}
		}
		if pane == nil {
			for _, removed := range removedPanes {
				if removed != nil && removed.ID == placement.PaneID {
					pane = removed
					break
				}
			}
		}
		if pane == nil {
			continue
		}
		handoff.pending[int(placement.Slot)] = struct{}{}
		pane.releaseOutputStream(handoff.released)
	}
	return handoff
}

func (c *ClientInstance) finishPreparedOutputHandoff(handoff *outputHandoff, prepared PreparedProjection) error {
	bySlot := make(map[int]PreparedRenderPane, len(prepared.Panes))
	for _, pane := range prepared.Panes {
		bySlot[int(pane.Placement.Slot)] = pane
	}
	attach := func(preparedPane PreparedRenderPane) error {
		placement := preparedPane.Placement
		lease := c.currentOutputLease(int(placement.Slot))
		if lease == nil || preparedPane.Pane == nil {
			return nil
		}
		cols, rows := preparedPane.Pane.TerminalSize()
		if cols != placement.Rect.Width || rows != placement.Rect.Height {
			return fmt.Errorf("pane %d grid changed from prepared layout %dx%d to %dx%d", preparedPane.Pane.ID, placement.Rect.Width, placement.Rect.Height, cols, rows)
		}
		c.Daemon.logf("meja projection: bind attachment=%d session=%d window=%d pane=%d slot=%d revision=%d grid=%dx%d\n",
			c.AttachmentID, prepared.Plan.SessionID, prepared.Plan.Layout.WindowID, preparedPane.Pane.ID, placement.Slot,
			prepared.Plan.Layout.LayoutRevision, cols, rows)
		return preparedPane.Pane.attachOutputStream(lease, prepared.Plan.Layout.LayoutRevision)
	}
	if handoff == nil || handoff.waited {
		for _, pane := range prepared.Panes {
			if err := attach(pane); err != nil {
				return err
			}
		}
		return nil
	}
	for _, pane := range prepared.Panes {
		if _, waiting := handoff.pending[int(pane.Placement.Slot)]; !waiting {
			if err := attach(pane); err != nil {
				return err
			}
		}
	}
	stillPending := make(map[int]struct{}, len(handoff.pending))
	for slot := range handoff.pending {
		stillPending[slot] = struct{}{}
	}
	for range handoff.pending {
		lease := <-handoff.released
		if lease == nil {
			continue
		}
		delete(stillPending, lease.Slot)
		if binding, ok := bySlot[lease.Slot]; ok {
			if err := attach(binding); err != nil {
				return err
			}
		}
	}
	for slot := range stillPending {
		if binding, ok := bySlot[slot]; ok {
			if err := attach(binding); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *ClientInstance) waitOutputHandoff(handoff *outputHandoff) error {
	if handoff == nil || handoff.waited {
		return nil
	}
	for range handoff.pending {
		<-handoff.released
	}
	handoff.waited = true
	return nil
}

func (c *ClientInstance) detachLeases(panes []*Pane, leases map[int]*OutputLease) error {
	for _, pane := range panes {
		for _, lease := range leases {
			if err := pane.detachOutputStream(lease.Stream); err != nil {
				return err
			}
		}
	}
	return nil
}
