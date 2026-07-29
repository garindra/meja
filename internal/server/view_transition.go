package server

import (
	"errors"
	"fmt"

	"github.com/garindra/meja/internal/protocol"
)

// ViewTransitionReason identifies the operation which replaced a client's
// visible projection. It is diagnostic data, not branching control state.
type ViewTransitionReason string

const (
	viewTransitionAttach       ViewTransitionReason = "attach"
	viewTransitionCreateWindow ViewTransitionReason = "create-window"
	viewTransitionSelectWindow ViewTransitionReason = "select-window"
	viewTransitionSplitPane    ViewTransitionReason = "split-pane"
	viewTransitionLayout       ViewTransitionReason = "layout"
	viewTransitionResize       ViewTransitionReason = "resize"
	viewTransitionClosePane    ViewTransitionReason = "close-pane"
	viewTransitionPaneExit     ViewTransitionReason = "pane-exit"
	viewTransitionSession      ViewTransitionReason = "switch-session"
	viewTransitionFocus        ViewTransitionReason = "focus"
)

type ClientPanePlacement struct {
	Pane      *Pane
	Placement protocol.PanePlacement
}

// ClientView is the exact daemon-resolved view carried by a transition and,
// after successful application, retained by the ClientInstance.
type ClientView struct {
	Layout          protocol.ClientLayout
	Panes           []ClientPanePlacement
	NavigationPanes []protocol.PanePlacement
	Status          clientStatusState
	StatusValid     bool
	paneByID        map[uint64]*Pane
}

// Pane resolves an actor handle from the exact immutable view installed by the
// client actor. It never consults daemon-owned session maps.
func (v *ClientView) Pane(id uint64) *Pane {
	if v == nil {
		return nil
	}
	if v.paneByID != nil {
		return v.paneByID[id]
	}
	for _, resolved := range v.Panes {
		if resolved.Pane != nil && resolved.Pane.ID == id {
			return resolved.Pane
		}
	}
	return nil
}

func (v *ClientView) FocusedPane() *Pane {
	if v == nil {
		return nil
	}
	return v.Pane(v.Layout.FocusedPaneID)
}

func (v *ClientView) PaneActors() []*Pane {
	if v == nil {
		return nil
	}
	panes := make([]*Pane, 0, len(v.Panes))
	seen := make(map[uint64]struct{}, len(v.Panes))
	for _, resolved := range v.Panes {
		if resolved.Pane == nil {
			continue
		}
		if _, ok := seen[resolved.Pane.ID]; ok {
			continue
		}
		seen[resolved.Pane.ID] = struct{}{}
		panes = append(panes, resolved.Pane)
	}
	return panes
}

func (v *ClientView) Placements() []protocol.PanePlacement {
	if v == nil {
		return nil
	}
	return append([]protocol.PanePlacement(nil), v.Layout.Panes...)
}

// ClientProjectionPlan is the immutable daemon-to-client transition contract.
type ClientProjectionPlan struct {
	ClientID            ClientID
	SessionID           uint64
	ViewLeaseGeneration uint64
	ProjectionRevision  uint64
	View                ClientView
	FullSnapshot        bool
	Close               bool
	CloseReason         string
}

// ViewTransition is the daemon phase result. Canonical mutation and exact pane
// resolution are complete; transport and pane actors have not yet applied it.
type ViewTransition struct {
	Reason     ViewTransitionReason
	Projection ClientProjectionPlan

	delivery         *clientRequiredDelivery
	deliveryRejected bool
}

func (c *ClientInstance) applyViewTransition(transition ViewTransition) error {
	if c == nil {
		return errors.New("nil client view transition")
	}
	// Direct client-actor application consumes its daemon-order reservation as
	// a no-op. Mailbox-delivered transitions have already completed the same
	// reservation, so this is idempotent.
	transition.delivery.cancel()
	transition.delivery = nil
	if err := c.adoptTransitionSession(transition.Projection); err != nil {
		return err
	}
	if transition.Projection.Close {
		c.ended = true
		return nil
	}
	if err := c.validateProjectionPlan(transition.Projection); err != nil {
		return err
	}
	handoff := c.beginOutputHandoff()
	return c.installClientView(transition, handoff)
}

// adoptTransitionSession handles both an empty initial view and a daemon-
// authorized live session move. The daemon assignment is committed while the
// transition is prepared; the ClientInstance publishes that assignment to its
// own actor state immediately before installing the returned projection.
func (c *ClientInstance) adoptTransitionSession(plan ClientProjectionPlan) error {
	if plan.SessionID == 0 || plan.SessionID == c.sessionID {
		return nil
	}
	if c.Daemon == nil {
		return errors.New("view transition has no daemon")
	}
	authorized := false
	c.Daemon.call(func() {
		state := c.Daemon.sessions[plan.SessionID]
		identity := c.Daemon.clients[c.clientID]
		authorized = state != nil && state.ClientID == c.clientID &&
			identity != nil && identity.State.Active == c.connection
	})
	if !authorized {
		return errors.New("view transition session assignment is not authorized")
	}
	c.sessionID = plan.SessionID
	c.resetInputForSessionSwitch()
	return nil
}

func (c *ClientInstance) validateClientView(transition ViewTransition) error {
	reason, plan := transition.Reason, transition.Projection
	if err := c.validateProjectionPlan(plan); err != nil {
		return err
	}
	if !plan.FullSnapshot {
		return errors.New("visible transition requires a full projection")
	}
	if plan.View.Layout.LayoutRevision == 0 {
		return errors.New("visible transition has no client layout revision")
	}

	seenPanes := make(map[uint64]struct{}, len(plan.View.Panes))
	seenSlots := make(map[uint8]struct{}, len(plan.View.Panes))
	for _, resolved := range plan.View.Panes {
		placement := resolved.Placement
		if resolved.Pane == nil || resolved.Pane.ID != placement.PaneID {
			return fmt.Errorf("%s contains invalid resolved pane %d", reason, placement.PaneID)
		}
		if uint64(placement.Slot) >= protocol.MaxRenderSlots {
			return fmt.Errorf("pane %d has invalid render slot %d", placement.PaneID, placement.Slot)
		}
		if _, duplicate := seenPanes[placement.PaneID]; duplicate {
			return fmt.Errorf("pane %d has duplicate client placement", placement.PaneID)
		}
		if _, duplicate := seenSlots[placement.Slot]; duplicate {
			return fmt.Errorf("render slot %d has duplicate client placement", placement.Slot)
		}
		seenPanes[placement.PaneID] = struct{}{}
		seenSlots[placement.Slot] = struct{}{}
	}
	return nil
}

func (c *ClientInstance) installClientView(transition ViewTransition, handoff *outputHandoff) error {
	transition.delivery.cancel()
	transition.delivery = nil
	if err := c.validateClientView(transition); err != nil {
		return fmt.Errorf("%s projection: %w", transition.Reason, err)
	}
	plan := transition.Projection
	if err := c.commitProjectionPlan(plan); err != nil {
		return fmt.Errorf("%s install projection=%d: %w", transition.Reason, plan.ProjectionRevision, err)
	}
	if err := c.cancelFrontendPointerCapture(); err != nil {
		return fmt.Errorf("%s cancel pointer capture: %w", transition.Reason, err)
	}
	if err := c.finishOutputHandoff(handoff, plan); err != nil {
		return fmt.Errorf("%s bind outputs: %w", transition.Reason, err)
	}
	nextView := c.orderedViewStatus(plan.View, plan.SessionID)
	c.currentView.Status = nextView.Status
	c.currentView.StatusValid = nextView.StatusValid
	if err := c.sendClientLayout(nextView.Layout); err != nil {
		return fmt.Errorf("%s publish layout: %w", transition.Reason, err)
	}
	if err := c.publishClientStatus(); err != nil {
		return fmt.Errorf("%s publish status: %w", transition.Reason, err)
	}
	c.currentView = nextView
	c.appliedProjectionRevision = plan.ProjectionRevision
	return nil
}

func (c *ClientInstance) applyFocusTransition(transition ViewTransition) error {
	transition.delivery.cancel()
	transition.delivery = nil
	if transition.Projection.FullSnapshot {
		return errors.New("focus transition unexpectedly requires a full snapshot")
	}
	if err := c.commitProjectionPlan(transition.Projection); err != nil {
		return fmt.Errorf("%s install projection=%d: %w", transition.Reason, transition.Projection.ProjectionRevision, err)
	}
	if err := c.cancelFrontendPointerCapture(); err != nil {
		return fmt.Errorf("%s cancel pointer capture: %w", transition.Reason, err)
	}
	nextView := c.orderedViewStatus(transition.Projection.View, transition.Projection.SessionID)
	layout := nextView.Layout
	if err := c.sendClientLayout(layout); err != nil {
		return err
	}
	c.currentView = nextView
	c.appliedProjectionRevision = transition.Projection.ProjectionRevision
	return nil
}
