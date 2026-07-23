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

// ClientProjectionPlan is the immutable daemon-to-client transition contract.
// Layout is its single pane/slot/geometry representation.
type ClientProjectionPlan struct {
	AttachmentID        uint64
	SessionID           uint64
	ViewLeaseGeneration uint64
	ProjectionRevision  uint64
	Layout              protocol.ClientLayout
	FullSnapshot        bool
	Close               bool
	CloseReason         string
}

type PreparedPaneResize struct {
	Pane *Pane
	Cols uint16
	Rows uint16
}

// PreparedViewTransition is the daemon phase result. Graph mutation, lease
// transfer, and target geometry have been decided, but visible pane grids and
// transport state have not changed. The ClientInstance applies these exact
// pane resizes only after reclaiming its currently installed output leases.
type PreparedViewTransition struct {
	Reason       ViewTransitionReason
	Projection   ClientProjectionPlan
	PaneResizes  []PreparedPaneResize
	RemovedPanes []*Pane
}

// PreparedRenderPane is a resolved, immutable output destination. Keeping the
// pane together with its published placement prevents the application phase
// from inventing a second answer by walking the live graph after the daemon
// transaction.
type PreparedRenderPane struct {
	Placement protocol.PanePlacement
	Pane      *Pane
}

// PreparedProjection is the client-resolved application input for one daemon
// transition. ClientLayoutRevision is allocated before the actor boundary, and
// installation must publish the prepared value unchanged.
type PreparedProjection struct {
	Reason ViewTransitionReason
	Plan   ClientProjectionPlan
	Panes  []PreparedRenderPane
}

func (c *ClientInstance) applyViewTransition(transition PreparedViewTransition) error {
	if c == nil {
		return errors.New("nil client view transition")
	}
	if err := c.adoptTransitionSession(transition.Projection); err != nil {
		return err
	}
	if transition.Projection.Close {
		c.ended.Store(true)
		if c.QUIC != nil {
			return c.QUIC.CloseWithError(0, transition.Projection.CloseReason)
		}
		return nil
	}
	if err := c.validateProjectionPlan(transition.Projection); err != nil {
		return err
	}
	handoff := c.beginOutputHandoffWithRemovedPanes(transition.RemovedPanes)
	if len(transition.PaneResizes) > 0 {
		if err := c.waitOutputHandoff(handoff); err != nil {
			return fmt.Errorf("%s release outputs: %w", transition.Reason, err)
		}
		for _, resize := range transition.PaneResizes {
			if resize.Pane == nil {
				return fmt.Errorf("%s contains nil pane resize", transition.Reason)
			}
			if err := resize.Pane.resize(resize.Cols, resize.Rows); err != nil {
				return fmt.Errorf("%s resize pane %d to %dx%d: %w", transition.Reason, resize.Pane.ID, resize.Cols, resize.Rows, err)
			}
		}
	}
	prepared, err := c.prepareProjection(transition)
	if err != nil {
		return fmt.Errorf("%s projection: %w", transition.Reason, err)
	}
	return c.installPreparedProjection(prepared, handoff)
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
		authorized = c.Daemon.clients[plan.SessionID] == c
	})
	if !authorized {
		return errors.New("view transition session assignment is not authorized")
	}
	c.sessionID = plan.SessionID
	c.resetInputForSessionSwitch()
	return nil
}

func (c *ClientInstance) prepareProjection(transition PreparedViewTransition) (PreparedProjection, error) {
	reason, plan := transition.Reason, transition.Projection
	if err := c.validateProjectionPlan(plan); err != nil {
		return PreparedProjection{}, err
	}
	if !plan.FullSnapshot {
		return PreparedProjection{}, errors.New("visible transition requires a full projection")
	}
	if plan.Layout.LayoutRevision == 0 {
		return PreparedProjection{}, errors.New("visible transition has no client layout revision")
	}
	// The daemon hands slices across an actor boundary. Own their backing arrays
	// here so neither a caller nor later graph work can mutate an in-flight
	// projection after validation.
	plan.Layout.Panes = append([]protocol.PanePlacement(nil), plan.Layout.Panes...)

	seenPanes := make(map[uint64]struct{}, len(plan.Layout.Panes))
	seenSlots := make(map[uint8]struct{}, len(plan.Layout.Panes))
	for _, placement := range plan.Layout.Panes {
		if uint64(placement.Slot) >= protocol.MaxRenderSlots {
			return PreparedProjection{}, fmt.Errorf("pane %d has invalid render slot %d", placement.PaneID, placement.Slot)
		}
		if _, duplicate := seenPanes[placement.PaneID]; duplicate {
			return PreparedProjection{}, fmt.Errorf("pane %d has duplicate client placement", placement.PaneID)
		}
		if _, duplicate := seenSlots[placement.Slot]; duplicate {
			return PreparedProjection{}, fmt.Errorf("render slot %d has duplicate client placement", placement.Slot)
		}
		seenPanes[placement.PaneID] = struct{}{}
		seenSlots[placement.Slot] = struct{}{}
	}

	prepared := PreparedProjection{Reason: reason, Plan: plan}
	prepared.Panes = make([]PreparedRenderPane, 0, len(plan.Layout.Panes))
	for _, placement := range plan.Layout.Panes {
		value, ok := c.Daemon.paneIndex.Load(placement.PaneID)
		if !ok || value == nil {
			return PreparedProjection{}, fmt.Errorf("pane %d disappeared while preparing projection", placement.PaneID)
		}
		pane := value.(*Pane)
		cols, rows := pane.TerminalSize()
		if cols != placement.Rect.Width || rows != placement.Rect.Height {
			return PreparedProjection{}, fmt.Errorf("pane %d grid %dx%d does not match layout %dx%d", pane.ID, cols, rows, placement.Rect.Width, placement.Rect.Height)
		}
		prepared.Panes = append(prepared.Panes, PreparedRenderPane{Placement: placement, Pane: pane})
	}
	return prepared, nil
}

func (c *ClientInstance) installPreparedProjection(prepared PreparedProjection, handoff *outputHandoff) error {
	if err := c.commitProjectionPlan(prepared.Plan); err != nil {
		return fmt.Errorf("%s install projection=%d: %w", prepared.Reason, prepared.Plan.ProjectionRevision, err)
	}
	if err := c.cancelFrontendPointerCapture(); err != nil {
		return fmt.Errorf("%s cancel pointer capture: %w", prepared.Reason, err)
	}
	if err := c.finishPreparedOutputHandoff(handoff, prepared); err != nil {
		return fmt.Errorf("%s bind outputs: %w", prepared.Reason, err)
	}
	if err := c.publishStatusBar(); err != nil {
		return fmt.Errorf("%s publish status: %w", prepared.Reason, err)
	}
	if err := c.sendPreparedClientLayout(prepared.Plan.Layout); err != nil {
		return fmt.Errorf("%s publish layout: %w", prepared.Reason, err)
	}
	c.currentLayout = prepared.Plan.Layout
	c.appliedProjectionRevision.Store(prepared.Plan.ProjectionRevision)
	return nil
}

func (c *ClientInstance) applyFocusTransition(transition PreparedViewTransition) error {
	if transition.Projection.FullSnapshot {
		return errors.New("focus transition unexpectedly requires a full snapshot")
	}
	if err := c.commitProjectionPlan(transition.Projection); err != nil {
		return fmt.Errorf("%s install projection=%d: %w", transition.Reason, transition.Projection.ProjectionRevision, err)
	}
	if err := c.cancelFrontendPointerCapture(); err != nil {
		return fmt.Errorf("%s cancel pointer capture: %w", transition.Reason, err)
	}
	layout := transition.Projection.Layout
	if err := c.sendPreparedClientLayout(layout); err != nil {
		return err
	}
	c.currentLayout = layout
	c.appliedProjectionRevision.Store(transition.Projection.ProjectionRevision)
	return nil
}
