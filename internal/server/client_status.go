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
		Windows:        make([]protocol.ClientStatusWindow, 0, len(status.Windows)),
	}
	for _, window := range status.Windows {
		msg.Windows = append(msg.Windows, protocol.ClientStatusWindow{
			WindowID: window.WindowID,
			Index:    window.Index,
			Title:    window.Title,
			Active:   window.Active,
			Zoomed:   window.Zoomed,
		})
	}
	if prompt := c.ActivePrompt(); prompt != nil {
		msg.Kind = protocol.ClientStatusPrompt
		mode := protocol.ClientStatusPromptText
		if prompt.Mode == PromptModeConfirm {
			mode = protocol.ClientStatusPromptConfirm
		}
		msg.Prompt = protocol.ClientStatusPromptState{
			Mode: mode, Label: prompt.Label, Text: string(prompt.Text), Cursor: prompt.Cursor,
		}
	} else if c.statusMessage != "" {
		msg.Kind = protocol.ClientStatusMessage
		msg.Message = protocol.ClientStatusMessageState{ID: c.statusMessageID, Text: c.statusMessage}
	}
	return sendEncoded(c.controlOut, protocol.MsgClientStatus, msg, protocol.EncodeClientStatus)
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
