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
)

type PreparedPaneResize struct {
	Pane *Pane
	Rect Rect
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

// PreparedRenderBinding is a resolved, immutable output destination. Keeping
// the pane and rectangle here prevents the application phase from inventing a
// second answer by walking the live graph after the daemon transaction.
type PreparedRenderBinding struct {
	Binding RenderBinding
	Pane    *Pane
	Rect    Rect
}

// PreparedProjection is the complete visible result of a daemon transaction.
// LayoutRevision is materialized when it is installed because that revision is
// transport-local; all other layout and binding data comes directly from Plan.
type PreparedProjection struct {
	Reason   ViewTransitionReason
	Plan     ClientProjectionPlan
	Layout   protocol.WindowLayout
	Bindings []PreparedRenderBinding
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
			if err := resize.Pane.resize(uint16(resize.Rect.Width), uint16(resize.Rect.Height)); err != nil {
				return fmt.Errorf("%s resize pane %d to %dx%d: %w", transition.Reason, resize.Pane.ID, resize.Rect.Width, resize.Rect.Height, err)
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
	if len(plan.Panes) != len(plan.Bindings) {
		return PreparedProjection{}, fmt.Errorf("pane count %d does not match binding count %d", len(plan.Panes), len(plan.Bindings))
	}
	// The daemon hands slices across an actor boundary. Own their backing arrays
	// here so neither a caller nor later graph work can mutate an in-flight
	// projection after validation.
	plan.Panes = append([]RenderPane(nil), plan.Panes...)
	plan.Bindings = append([]RenderBinding(nil), plan.Bindings...)

	byPane := make(map[uint64]RenderBinding, len(plan.Bindings))
	for _, binding := range plan.Bindings {
		if binding.Slot < 0 || uint64(binding.Slot) >= protocol.MaxRenderSlots {
			return PreparedProjection{}, fmt.Errorf("pane %d has invalid render slot %d", binding.PaneID, binding.Slot)
		}
		if _, duplicate := byPane[binding.PaneID]; duplicate {
			return PreparedProjection{}, fmt.Errorf("pane %d has duplicate render binding", binding.PaneID)
		}
		byPane[binding.PaneID] = binding
	}

	prepared := PreparedProjection{Reason: reason, Plan: plan}
	prepared.Layout = protocol.WindowLayout{
		WindowID:       plan.WindowID,
		FocusedPaneID:  plan.FocusedPaneID,
		LayoutRevision: plan.LayoutRevision,
		Panes:          make([]protocol.PanePlacement, 0, len(plan.Panes)),
	}
	prepared.Bindings = make([]PreparedRenderBinding, 0, len(plan.Panes))
	for _, renderPane := range plan.Panes {
		binding, ok := byPane[renderPane.PaneID]
		if !ok {
			return PreparedProjection{}, fmt.Errorf("pane %d has no render binding", renderPane.PaneID)
		}
		value, ok := c.Daemon.paneIndex.Load(renderPane.PaneID)
		if !ok || value == nil {
			return PreparedProjection{}, fmt.Errorf("pane %d disappeared while preparing projection", renderPane.PaneID)
		}
		pane := value.(*Pane)
		cols, rows := pane.TerminalSize()
		if cols != renderPane.Rect.Width || rows != renderPane.Rect.Height {
			return PreparedProjection{}, fmt.Errorf("pane %d grid %dx%d does not match layout %dx%d", pane.ID, cols, rows, renderPane.Rect.Width, renderPane.Rect.Height)
		}
		prepared.Bindings = append(prepared.Bindings, PreparedRenderBinding{Binding: binding, Pane: pane, Rect: renderPane.Rect})
		prepared.Layout.Panes = append(prepared.Layout.Panes, protocol.PanePlacement{
			PaneID: renderPane.PaneID,
			Slot:   uint8(binding.Slot),
			Rect: protocol.Rect{X: renderPane.Rect.X, Y: renderPane.Rect.Y,
				Width: renderPane.Rect.Width, Height: renderPane.Rect.Height},
		})
	}
	return prepared, nil
}

func (c *ClientInstance) installPreparedProjection(prepared PreparedProjection, handoff *outputHandoff) error {
	if err := c.commitProjectionPlan(prepared.Plan); err != nil {
		return fmt.Errorf("%s install projection=%d: %w", prepared.Reason, prepared.Plan.ProjectionRevision, err)
	}
	prepared.Layout.LayoutRevision = c.highestLayoutRevision.Load()
	if err := c.cancelFrontendPointerCapture(); err != nil {
		return fmt.Errorf("%s cancel pointer capture: %w", prepared.Reason, err)
	}
	if err := c.finishPreparedOutputHandoff(handoff, prepared); err != nil {
		return fmt.Errorf("%s bind outputs: %w", prepared.Reason, err)
	}
	if err := c.publishStatusBar(); err != nil {
		return fmt.Errorf("%s publish status: %w", prepared.Reason, err)
	}
	if err := c.publishPreparedWindowLayout(prepared.Layout); err != nil {
		return fmt.Errorf("%s publish layout: %w", prepared.Reason, err)
	}
	return nil
}
