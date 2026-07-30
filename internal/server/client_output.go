package server

import "fmt"

type outputHandoff struct {
	released chan *OutputLease
	pending  map[int]struct{}
	waited   bool
}

func (c *ClientInstance) beginOutputHandoff() *outputHandoff {
	placements := c.currentView.Panes
	handoff := &outputHandoff{
		released: make(chan *OutputLease, len(placements)),
		pending:  make(map[int]struct{}, len(placements)),
	}
	for _, resolved := range placements {
		if resolved.Pane == nil {
			continue
		}
		handoff.pending[int(resolved.Placement.Slot)] = struct{}{}
		resolved.Pane.releaseOutputStream(handoff.released)
	}
	return handoff
}

func (c *ClientInstance) finishOutputHandoff(handoff *outputHandoff, plan ClientProjectionPlan) error {
	bySlot := make(map[int]ClientPanePlacement, len(plan.View.Panes))
	for _, pane := range plan.View.Panes {
		bySlot[int(pane.Placement.Slot)] = pane
	}
	install := func(resolved ClientPanePlacement) error {
		placement := resolved.Placement
		lease := c.currentOutputLease(int(placement.Slot))
		if resolved.Pane == nil {
			return fmt.Errorf("pane %d has no resolved actor", placement.PaneID)
		}
		c.Daemon.logf("meja projection: bind client=%d session=%d window=%d pane=%d slot=%d revision=%d grid=%dx%d\n",
			c.clientID, plan.SessionID, plan.View.Layout.WindowID, resolved.Pane.ID, placement.Slot,
			plan.View.Layout.LayoutRevision, placement.Rect.Width, placement.Rect.Height)
		return resolved.Pane.installOutputLease(
			lease,
			plan.View.Layout.LayoutRevision,
			uint16(placement.Rect.Width),
			uint16(placement.Rect.Height),
		)
	}
	if handoff == nil || handoff.waited {
		for _, pane := range plan.View.Panes {
			if err := install(pane); err != nil {
				return err
			}
		}
		return nil
	}
	for _, pane := range plan.View.Panes {
		if _, waiting := handoff.pending[int(pane.Placement.Slot)]; !waiting {
			if err := install(pane); err != nil {
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
			if err := install(binding); err != nil {
				return err
			}
		}
	}
	for slot := range stillPending {
		if binding, ok := bySlot[slot]; ok {
			if err := install(binding); err != nil {
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
			if err := pane.detachOutputLease(lease); err != nil {
				return err
			}
		}
	}
	return nil
}
