package server

import (
	"os"

	"github.com/garindra/meja/internal/protocol"
)

func (c *ClientInstance) nextStatusPublicationRevision() uint64 {
	if c == nil {
		return 0
	}
	var revision uint64
	if c.Daemon != nil && c.clientID != 0 {
		c.Daemon.call(func() {
			if identity := c.Daemon.clients[c.clientID]; identity != nil {
				if identity.statusRevision != ^uint64(0) {
					identity.statusRevision++
				}
				revision = identity.statusRevision
			}
		})
	}
	if revision > c.statusPublicationRevision {
		c.statusPublicationRevision = revision
	} else if c.statusPublicationRevision != ^uint64(0) {
		c.statusPublicationRevision++
	}
	return c.statusPublicationRevision
}

func (c *ClientInstance) publishClientStatus() error {
	if c == nil || c.controlOut == nil || !c.currentView.StatusValid {
		return nil
	}
	status := c.currentView.Status
	msg := protocol.ClientStatus{
		Revision:       c.nextStatusPublicationRevision(),
		SessionID:      status.SessionID,
		SessionName:    status.SessionName,
		ServerHostname: serverHostname(),
		ServerHome:     serverHome(),
		Root:           status.Root,
		Kind:           protocol.ClientStatusNormal,
		Windows:        statusWindows(status.Windows),
	}
	if prompt := c.ActivePrompt(); prompt != nil {
		msg.Kind = protocol.ClientStatusPrompt
		mode := protocol.ClientStatusPromptText
		if prompt.Mode == PromptModeConfirm {
			mode = protocol.ClientStatusPromptConfirm
		}
		msg.Prompt = protocol.ClientStatusPromptState{
			PromptID: prompt.ID, Mode: mode, Label: prompt.Label, Initial: prompt.Initial,
		}
	} else if c.statusMessage != "" {
		msg.Kind = protocol.ClientStatusMessage
		msg.Message = protocol.ClientStatusMessageState{ID: c.statusMessageID, Text: c.statusMessage}
	}
	return sendEncoded(c.controlOut, protocol.MsgClientStatus, msg, protocol.EncodeClientStatus)
}

func statusWindows(windows []WindowStatus) []protocol.ClientStatusWindow {
	out := make([]protocol.ClientStatusWindow, 0, len(windows))
	for _, window := range windows {
		out = append(out, protocol.ClientStatusWindow{
			WindowID: window.WindowID,
			Index:    window.Index,
			Title:    window.Title,
			Active:   window.Active,
			Zoomed:   window.Zoomed,
		})
	}
	return out
}

// statusTabWindowAt maps a pointer cell on the status row to a window index.
// Clicks are ignored while a prompt or message replaces the tab list.
func (c *ClientInstance) statusTabWindowAt(x, y int) (int, bool) {
	if c == nil || !c.currentView.StatusValid || c.terminalRows == 0 || y != int(c.terminalRows)-1 {
		return 0, false
	}
	if c.ActivePrompt() != nil || c.statusMessage != "" {
		return 0, false
	}
	status := c.currentView.Status
	text, tabs := protocol.StatusText(status.SessionName, status.SessionID, statusWindows(status.Windows))
	location := protocol.StatusLocation(serverHostname(), status.Root, serverHome())
	return protocol.StatusTabAt(int(c.terminalCols), x, text, location, tabs)
}

func serverHostname() string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		return "?"
	}
	return hostname
}

func serverHome() string {
	home, _ := os.UserHomeDir()
	return home
}

func (c *ClientInstance) sendClientLayout(layout protocol.ClientLayout) error {
	if c.controlOut == nil {
		return nil
	}
	return sendEncoded(c.controlOut, protocol.MsgClientLayout, layout, protocol.EncodeClientLayout)
}
