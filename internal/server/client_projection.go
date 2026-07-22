package server

import (
	"errors"
	"fmt"

	"github.com/garindra/meja/internal/protocol"
)

func (c *ClientInstance) activePane() *Pane {
	if c == nil || c.clientState == nil || c.Daemon == nil {
		return nil
	}
	value, ok := c.Daemon.paneIndex.Load(c.clientState.FocusedPaneID)
	if !ok || value == nil {
		return nil
	}
	return value.(*Pane)
}

func (c *ClientInstance) activeWindow() *Window {
	if c == nil || c.clientState == nil {
		return nil
	}
	state := c.sessionState()
	if state == nil {
		return nil
	}
	return cloneWindow(state.Windows[c.clientState.ActiveWindowID])
}

func (c *ClientInstance) snapshotClient() *ClientState {
	if c == nil {
		return nil
	}
	return cloneClientState(c.clientState)
}

func (c *ClientInstance) bindingForPane(paneID uint64) (RenderBinding, bool) {
	if c == nil || c.clientState == nil {
		return RenderBinding{}, false
	}
	for _, binding := range c.clientState.RenderBindings {
		if binding.PaneID == paneID {
			return binding, true
		}
	}
	return RenderBinding{}, false
}

func (c *ClientInstance) renderBindings() []RenderBinding {
	if c == nil || c.clientState == nil {
		return nil
	}
	return append([]RenderBinding(nil), c.clientState.RenderBindings...)
}

func (c *ClientInstance) windowLayout() (protocol.WindowLayout, error) {
	state := c.sessionState()
	client := c.clientState
	if state == nil || client == nil {
		return protocol.WindowLayout{}, errors.New("client is unavailable")
	}
	window := state.Windows[client.ActiveWindowID]
	if window == nil {
		return protocol.WindowLayout{}, fmt.Errorf("unknown window %d", client.ActiveWindowID)
	}
	placements := visibleWindowPlacementsForSession(state, window, Rect{Width: int(client.TerminalCols), Height: int(client.TerminalRows)})
	out := make([]protocol.PanePlacement, 0, len(placements))
	for _, placement := range placements {
		slot := uint8(0)
		if binding, ok := c.bindingForPane(placement.PaneID); ok {
			slot = uint8(binding.Slot)
		}
		out = append(out, protocol.PanePlacement{PaneID: placement.PaneID, Slot: slot, Rect: protocol.Rect{X: placement.Rect.X, Y: placement.Rect.Y, Width: placement.Rect.Width, Height: placement.Rect.Height}})
	}
	layoutRevision := c.highestLayoutRevision.Load()
	if layoutRevision == 0 {
		layoutRevision = window.LayoutRevision
	}
	return protocol.WindowLayout{WindowID: window.ID, FocusedPaneID: client.FocusedPaneID, LayoutRevision: layoutRevision, Panes: out}, nil
}

func (c *ClientInstance) isFocusedPane(paneID uint64) bool {
	return c != nil && c.clientState != nil && c.clientState.FocusedPaneID == paneID
}
