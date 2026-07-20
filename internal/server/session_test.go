package server

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/garindra/meja/internal/protocol"
)

type recordingQUICConnection struct {
	quic.Connection
	closeWithError func(quic.ApplicationErrorCode, string) error
}

func (c *recordingQUICConnection) CloseWithError(code quic.ApplicationErrorCode, message string) error {
	return c.closeWithError(code, message)
}

func TestPaneAndSplitLayoutsComputeExpectedRects(t *testing.T) {
	single := (&PaneLayout{PaneID: 1}).Compute(Rect{X: 0, Y: 0, Width: 120, Height: 39})
	if len(single) != 1 || single[0].Rect.Width != 120 || single[0].Rect.Height != 39 {
		t.Fatalf("single pane layout = %#v", single)
	}

	split := (&SplitLayout{
		Direction: SplitVertical,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second:    &PaneLayout{PaneID: 2},
	}).Compute(Rect{X: 0, Y: 0, Width: 120, Height: 39})
	if len(split) != 2 || split[0].Rect.Width != 59 || split[1].Rect.X != 60 || split[1].Rect.Width != 60 {
		t.Fatalf("vertical split layout = %#v", split)
	}

	split = (&SplitLayout{
		Direction: SplitHorizontal,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second:    &PaneLayout{PaneID: 2},
	}).Compute(Rect{X: 0, Y: 0, Width: 120, Height: 39})
	if len(split) != 2 || split[0].Rect.Height != 19 || split[1].Rect.Y != 20 || split[1].Rect.Height != 19 {
		t.Fatalf("horizontal split layout = %#v", split)
	}
}

func TestResizePaneBoundaryMovesInScreenDirection(t *testing.T) {
	layout := &SplitLayout{
		Direction: SplitVertical,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second:    &PaneLayout{PaneID: 2},
	}
	rect := Rect{Width: 80, Height: 24}
	if !ResizePaneBoundary(layout, 1, ResizePaneLeft, 5, rect) {
		t.Fatal("left resize did not change layout")
	}
	placements := layout.Compute(rect)
	if placements[0].Rect.Width != 34 || placements[1].Rect.Width != 45 {
		t.Fatalf("left resize placements = %#v", placements)
	}
	if !ResizePaneBoundary(layout, 1, ResizePaneRight, 5, rect) {
		t.Fatal("right resize did not change layout")
	}
	placements = layout.Compute(rect)
	if placements[0].Rect.Width != 39 || placements[1].Rect.Width != 40 {
		t.Fatalf("right resize placements = %#v", placements)
	}
	if !ResizePaneBoundary(layout, 2, ResizePaneLeft, 5, rect) {
		t.Fatal("left resize of right pane did not change layout")
	}
	placements = layout.Compute(rect)
	if placements[0].Rect.Width != 34 || placements[1].Rect.Width != 45 {
		t.Fatalf("right pane left resize placements = %#v", placements)
	}
}

func TestResizePaneBoundaryMovesHorizontalBoundary(t *testing.T) {
	layout := &SplitLayout{
		Direction: SplitHorizontal,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second:    &PaneLayout{PaneID: 2},
	}
	rect := Rect{Width: 80, Height: 24}
	if !ResizePaneBoundary(layout, 1, ResizePaneUp, 3, rect) {
		t.Fatal("up resize did not change layout")
	}
	placements := layout.Compute(rect)
	if placements[0].Rect.Height != 8 || placements[1].Rect.Height != 15 {
		t.Fatalf("up resize placements = %#v", placements)
	}
	if !ResizePaneBoundary(layout, 1, ResizePaneDown, 3, rect) {
		t.Fatal("down resize did not change layout")
	}
	placements = layout.Compute(rect)
	if placements[0].Rect.Height != 11 || placements[1].Rect.Height != 12 {
		t.Fatalf("down resize placements = %#v", placements)
	}
}

func TestResizePaneBoundarySelectsNearestBoundaryOnRequestedSide(t *testing.T) {
	inner := &SplitLayout{
		Direction: SplitVertical,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 2},
		Second:    &PaneLayout{PaneID: 3},
	}
	layout := &SplitLayout{
		Direction: SplitVertical,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second:    inner,
	}
	rect := Rect{Width: 120, Height: 24}
	before := layout.Compute(rect)
	if !ResizePaneBoundary(layout, 2, ResizePaneLeft, 5, rect) {
		t.Fatal("middle pane left resize did not change layout")
	}
	afterLeft := layout.Compute(rect)
	if afterLeft[0].Rect.Width != before[0].Rect.Width-5 {
		t.Fatalf("left resize moved wrong boundary: before=%#v after=%#v", before, afterLeft)
	}
	leftWidth := afterLeft[0].Rect.Width
	beforeMiddleWidth := afterLeft[1].Rect.Width
	if !ResizePaneBoundary(layout, 2, ResizePaneRight, 5, rect) {
		t.Fatal("middle pane right resize did not change layout")
	}
	afterRight := layout.Compute(rect)
	if afterRight[0].Rect.Width != leftWidth || afterRight[1].Rect.Width != beforeMiddleWidth+5 {
		t.Fatalf("right resize moved wrong boundary: before=%#v after=%#v", afterLeft, afterRight)
	}
}

func TestResizePaneBoundaryClampsForNestedPaneMinimums(t *testing.T) {
	layout := &SplitLayout{
		Direction: SplitVertical,
		Ratio:     500,
		First:     &PaneLayout{PaneID: 1},
		Second: &SplitLayout{
			Direction: SplitVertical,
			Ratio:     500,
			First:     &PaneLayout{PaneID: 2},
			Second:    &PaneLayout{PaneID: 3},
		},
	}
	rect := Rect{Width: 8, Height: 4}
	if !ResizePaneBoundary(layout, 1, ResizePaneRight, 100, rect) {
		t.Fatal("clamped resize did not change layout")
	}
	placements := layout.Compute(rect)
	if placements[0].Rect.Width != 4 || placements[1].Rect.Width != 1 || placements[2].Rect.Width != 1 {
		t.Fatalf("clamped placements = %#v", placements)
	}
	if ResizePaneBoundary(layout, 1, ResizePaneRight, 1, rect) {
		t.Fatal("resize moved boundary past nested minimum")
	}
}

func TestSessionSplitCreatesNewPaneAndBindings(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	window, clientState := s.CreateWindow(pane0, 0)
	if window.ID != 1 || windowPrimaryPaneID(window) != pane0.ID || clientState.FocusedPaneID != pane0.ID {
		t.Fatalf("initial window = %#v client=%#v", window, clientState)
	}

	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	window, clientState, err := s.SplitFocusedPane(0, pane1, SplitVertical)
	if err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if _, ok := window.Layout.(*SplitLayout); !ok {
		t.Fatalf("window layout = %#v, want split", window.Layout)
	}
	if clientState.FocusedPaneID != pane1.ID || len(clientState.RenderBindings) != 2 {
		t.Fatalf("client after split = %#v", clientState)
	}
	if clientState.RenderBindings[1].PaneID != pane1.ID {
		t.Fatalf("second render slot = %#v", clientState.RenderBindings)
	}
}

func TestResizeFocusedPaneAdvancesRevisionAndPersistsRatio(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	left := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	before := s.Windows[client.ActiveWindowID].LayoutRevision
	window, _, changed, err := s.ResizeFocusedPane(0, ResizePaneRight, 5)
	if err != nil || !changed {
		t.Fatalf("ResizeFocusedPane() changed=%v err=%v", changed, err)
	}
	if window.LayoutRevision <= before {
		t.Fatalf("layout revision = %d, want > %d", window.LayoutRevision, before)
	}
	persisted, err := planLayout(window.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Ratio <= 0.5 {
		t.Fatalf("persisted resized ratio = %f, want > 0.5", persisted.Ratio)
	}
}

func TestResizePaneCommandRelayoutsPaneTerminals(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	left := &Pane{ID: s.AddPaneID(), terminal: newTerminal(80, 24)}
	s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID(), terminal: newTerminal(80, 24)}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.commandResizePane(ResizePaneRight, 5); err != nil {
		t.Fatal(err)
	}
	leftCols, leftRows := left.TerminalSize()
	rightCols, rightRows := right.TerminalSize()
	if leftCols != 44 || rightCols != 35 || leftRows != 24 || rightRows != 24 {
		t.Fatalf("terminal sizes after resize: left=%dx%d right=%dx%d", leftCols, leftRows, rightCols, rightRows)
	}
}

func TestToggleZoomProjectsFocusedPaneWithoutChangingLayout(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	left := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	root := s.Windows[client.ActiveWindowID].Layout.(*SplitLayout)
	ratio := root.Ratio
	window, state, changed, err := s.ToggleZoom(0)
	if err != nil || !changed {
		t.Fatalf("ToggleZoom() changed=%v err=%v", changed, err)
	}
	if !window.Zoomed || window.ZoomedPaneID != left.ID || root.Ratio != ratio {
		t.Fatalf("zoom changed underlying layout: window=%#v ratio=%d want=%d", window, root.Ratio, ratio)
	}
	if len(state.RenderBindings) != 1 || state.RenderBindings[0].PaneID != left.ID || state.RenderBindings[0].Slot != 0 {
		t.Fatalf("zoomed bindings = %#v", state.RenderBindings)
	}
	layout, err := s.WindowLayout(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 1 || layout.Panes[0].PaneID != left.ID || layout.Panes[0].Rect != (protocol.Rect{Width: 80, Height: 24}) {
		t.Fatalf("zoomed protocol layout = %#v", layout)
	}

	window, state, changed, err = s.ToggleZoom(0)
	if err != nil || !changed || window.Zoomed {
		t.Fatalf("unzoom changed=%v window=%#v err=%v", changed, window, err)
	}
	if len(state.RenderBindings) != 2 || root.Ratio != ratio {
		t.Fatalf("unzoomed bindings=%#v ratio=%d want=%d", state.RenderBindings, root.Ratio, ratio)
	}
}

func TestToggleZoomSinglePaneIsNoOp(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	s.CreateWindow(&Pane{ID: s.AddPaneID()}, 0)
	window, _, changed, err := s.ToggleZoom(0)
	if err != nil || changed || window.Zoomed {
		t.Fatalf("single pane ToggleZoom() changed=%v window=%#v err=%v", changed, window, err)
	}
}

func TestZoomCommandResizesOnlyVisiblePaneToFullWindow(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	left := &Pane{ID: s.AddPaneID(), terminal: newTerminal(80, 24)}
	s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID(), terminal: newTerminal(80, 24)}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	s.ResizeAll(80, 24)
	if err := s.commandToggleZoom(); err != nil {
		t.Fatal(err)
	}
	leftCols, leftRows := left.TerminalSize()
	rightCols, rightRows := right.TerminalSize()
	if leftCols != 80 || leftRows != 24 || rightCols != 40 || rightRows != 24 {
		t.Fatalf("zoomed sizes: left=%dx%d right=%dx%d", leftCols, leftRows, rightCols, rightRows)
	}
	s.ResizeAll(100, 30)
	leftCols, leftRows = left.TerminalSize()
	rightCols, rightRows = right.TerminalSize()
	if leftCols != 100 || leftRows != 30 || rightCols != 50 || rightRows != 30 {
		t.Fatalf("resized zoomed sizes: left=%dx%d right=%dx%d", leftCols, leftRows, rightCols, rightRows)
	}
	if err := s.commandToggleZoom(); err != nil {
		t.Fatal(err)
	}
	leftCols, leftRows = left.TerminalSize()
	rightCols, rightRows = right.TerminalSize()
	if leftCols != 49 || leftRows != 30 || rightCols != 50 || rightRows != 30 {
		t.Fatalf("unzoomed sizes: left=%dx%d right=%dx%d", leftCols, leftRows, rightCols, rightRows)
	}
}

func TestLayoutOperationsUnzoomWindow(t *testing.T) {
	t.Run("focus direction", func(t *testing.T) {
		s, left, right := newZoomTestSession(t)
		if _, _, err := s.FocusPane(0, left.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.ToggleZoom(0); err != nil {
			t.Fatal(err)
		}
		window, state, err := s.FocusPaneDirection(0, 'C')
		if err != nil || window.Zoomed || state.FocusedPaneID != right.ID || len(state.RenderBindings) != 2 {
			t.Fatalf("focus after zoom: window=%#v state=%#v err=%v", window, state, err)
		}
	})
	t.Run("resize", func(t *testing.T) {
		s, left, _ := newZoomTestSession(t)
		if _, _, err := s.FocusPane(0, left.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.ToggleZoom(0); err != nil {
			t.Fatal(err)
		}
		window, state, changed, err := s.ResizeFocusedPane(0, ResizePaneRight, 1)
		if err != nil || !changed || window.Zoomed || len(state.RenderBindings) != 2 {
			t.Fatalf("resize after zoom: changed=%v window=%#v state=%#v err=%v", changed, window, state, err)
		}
	})
	t.Run("swap", func(t *testing.T) {
		s, _, _ := newZoomTestSession(t)
		if _, _, _, err := s.ToggleZoom(0); err != nil {
			t.Fatal(err)
		}
		window, state, changed, err := s.SwapFocusedPane(0, SwapPanePrevious)
		if err != nil || !changed || window.Zoomed || len(state.RenderBindings) != 2 {
			t.Fatalf("swap after zoom: changed=%v window=%#v state=%#v err=%v", changed, window, state, err)
		}
	})
	t.Run("split", func(t *testing.T) {
		s, _, _ := newZoomTestSession(t)
		if _, _, _, err := s.ToggleZoom(0); err != nil {
			t.Fatal(err)
		}
		window, state, err := s.SplitFocusedPane(0, &Pane{ID: s.AddPaneID()}, SplitHorizontal)
		if err != nil || window.Zoomed || len(state.RenderBindings) != 3 {
			t.Fatalf("split after zoom: window=%#v state=%#v err=%v", window, state, err)
		}
	})
}

func TestZoomStateSurvivesWindowSwitch(t *testing.T) {
	s, left, _ := newZoomTestSession(t)
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	zoomed, _, _, err := s.ToggleZoom(0)
	if err != nil {
		t.Fatal(err)
	}
	zoomedWindowID := zoomed.ID
	s.CreateWindow(&Pane{ID: s.AddPaneID()}, 0)
	window, state, err := s.SelectWindow(0, zoomedWindowID)
	if err != nil {
		t.Fatal(err)
	}
	if !window.Zoomed || window.ZoomedPaneID != left.ID || len(state.RenderBindings) != 1 || state.RenderBindings[0].PaneID != left.ID {
		t.Fatalf("restored zoomed window=%#v state=%#v", window, state)
	}
}

func TestPaneExitMaintainsOrClearsZoomAsLayoutAllows(t *testing.T) {
	s, left, right := newZoomTestSession(t)
	third := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, third, SplitHorizontal); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ToggleZoom(0); err != nil {
		t.Fatal(err)
	}
	window, _, final, removed, err := s.RemovePane(right.ID, 0)
	if err != nil || final || !removed || !window.Zoomed || window.ZoomedPaneID != left.ID {
		t.Fatalf("hidden pane exit: window=%#v final=%v removed=%v err=%v", window, final, removed, err)
	}
	window, state, final, removed, err := s.RemovePane(third.ID, 0)
	if err != nil || final || !removed || window.Zoomed || len(state.RenderBindings) != 1 || state.RenderBindings[0].PaneID != left.ID {
		t.Fatalf("last hidden pane exit: window=%#v state=%#v final=%v removed=%v err=%v", window, state, final, removed, err)
	}
}

func TestClosingZoomedPaneClearsZoom(t *testing.T) {
	s, left, _ := newZoomTestSession(t)
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.ToggleZoom(0); err != nil {
		t.Fatal(err)
	}
	_, window, state, windowClosed, _, _, err := s.CloseFocusedPane(0)
	if err != nil || windowClosed || window.Zoomed || len(state.RenderBindings) != 1 {
		t.Fatalf("close zoomed pane: window=%#v state=%#v windowClosed=%v err=%v", window, state, windowClosed, err)
	}
}

func newZoomTestSession(t *testing.T) (*Session, *Pane, *Pane) {
	t.Helper()
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	left := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	return s, left, right
}

func TestResizeRebuildsVisualRenderBindings(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 16, 4
	first := &Pane{ID: 2, terminal: newTerminal(16, 4)}
	second := &Pane{ID: 1, terminal: newTerminal(16, 4)}
	s.NextPaneID = 3
	s.CreateWindow(first, 0)
	window := s.Windows[client.ActiveWindowID]
	s.Panes[second.ID] = second
	window.Layout = &SplitLayout{
		Direction: SplitHorizontal,
		Ratio:     500,
		First:     &PaneLayout{PaneID: first.ID},
		Second:    &PaneLayout{PaneID: second.ID},
	}
	s.rebuildBindingsLocked(client, window)
	if got := client.RenderBindings[0].PaneID; got != first.ID {
		t.Fatalf("initial slot 0 pane = %d, want %d", got, first.ID)
	}

	s.ResizeAll(16, 1)
	if got := client.RenderBindings[0].PaneID; got != second.ID {
		t.Fatalf("resized slot 0 pane = %d, want %d", got, second.ID)
	}
}

func TestSwapFocusedPaneUsesVisualOrderAndKeepsFocus(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 120, 39
	left := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(left, 0)
	topRight := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, topRight, SplitVertical); err != nil {
		t.Fatal(err)
	}
	bottomRight := &Pane{ID: s.AddPaneID()}
	window, _, err := s.SplitFocusedPane(0, bottomRight, SplitHorizontal)
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision := window.LayoutRevision

	window, state, changed, err := s.SwapFocusedPane(0, SwapPanePrevious)
	if err != nil || !changed {
		t.Fatalf("SwapFocusedPane(previous) changed=%v err=%v", changed, err)
	}
	placements := window.Layout.Compute(Rect{Width: 120, Height: 39})
	if len(placements) != 3 || placements[0].PaneID != left.ID || placements[1].PaneID != bottomRight.ID || placements[2].PaneID != topRight.ID {
		t.Fatalf("placements after previous swap = %#v", placements)
	}
	if state.FocusedPaneID != bottomRight.ID || window.LayoutRevision <= beforeRevision {
		t.Fatalf("swap state=%#v revision=%d before=%d", state, window.LayoutRevision, beforeRevision)
	}

	window, state, changed, err = s.SwapFocusedPane(0, SwapPaneNext)
	if err != nil || !changed {
		t.Fatalf("SwapFocusedPane(next) changed=%v err=%v", changed, err)
	}
	placements = window.Layout.Compute(Rect{Width: 120, Height: 39})
	if placements[1].PaneID != topRight.ID || placements[2].PaneID != bottomRight.ID || state.FocusedPaneID != bottomRight.ID {
		t.Fatalf("placements after next swap = %#v state=%#v", placements, state)
	}
}

func TestSwapFocusedPaneWrapsAtVisualEdges(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols, client.TerminalRows = 80, 24
	first := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(first, 0)
	second := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FocusPane(0, first.ID); err != nil {
		t.Fatal(err)
	}

	window, state, changed, err := s.SwapFocusedPane(0, SwapPanePrevious)
	if err != nil || !changed {
		t.Fatalf("SwapFocusedPane(previous) changed=%v err=%v", changed, err)
	}
	placements := window.Layout.Compute(Rect{Width: 80, Height: 24})
	if placements[1].PaneID != first.ID || state.FocusedPaneID != first.ID {
		t.Fatalf("wrapped placements=%#v state=%#v", placements, state)
	}
}

func TestRecursiveMixedSplitsAndCloseCollapseOnlyParent(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "root"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "right"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("vertical SplitFocusedPane() error = %v", err)
	}
	pane2 := &Pane{ID: s.AddPaneID(), Title: "bottom-right"}
	window, clientState, err := s.SplitFocusedPane(0, pane2, SplitHorizontal)
	if err != nil {
		t.Fatalf("nested horizontal SplitFocusedPane() error = %v", err)
	}
	placements := window.Layout.Compute(Rect{Width: 120, Height: 39})
	if len(placements) != 3 || len(clientState.RenderBindings) != 3 {
		t.Fatalf("nested layout = %#v bindings=%#v", placements, clientState.RenderBindings)
	}
	if placements[0].PaneID != pane0.ID || placements[0].Rect.Width != 59 ||
		placements[1].PaneID != pane1.ID || placements[1].Rect.Height != 19 ||
		placements[2].PaneID != pane2.ID || placements[2].Rect.Y != 20 {
		t.Fatalf("nested placements = %#v", placements)
	}

	closed, window, clientState, windowClosed, _, _, err := s.CloseFocusedPane(0)
	if err != nil || windowClosed || closed != pane2 {
		t.Fatalf("CloseFocusedPane() closed=%#v windowClosed=%v err=%v", closed, windowClosed, err)
	}
	placements = window.Layout.Compute(Rect{Width: 120, Height: 39})
	if len(placements) != 2 || placements[0].PaneID != pane0.ID || placements[1].PaneID != pane1.ID || clientState.FocusedPaneID != pane1.ID {
		t.Fatalf("collapsed nested layout = %#v client=%#v", placements, clientState)
	}
}

func TestWindowRejectsNinthPane(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 240
	client.TerminalRows = 80
	pane := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(pane, 0)
	for count := 2; count <= int(protocol.MaxVisiblePanes); count++ {
		pane = &Pane{ID: s.AddPaneID()}
		if _, _, err := s.SplitFocusedPane(0, pane, SplitDirection(count%2)); err != nil {
			t.Fatalf("split %d error = %v", count, err)
		}
	}
	if err := s.CanSplitFocusedPane(0); err == nil {
		t.Fatal("CanSplitFocusedPane() allowed ninth pane")
	}
	extra := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, extra, SplitVertical); err == nil {
		t.Fatal("SplitFocusedPane() allowed ninth pane")
	}
}

func TestWindowLayoutAndFocusReuseVisiblePanes(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	layout, err := s.WindowLayout(0)
	if err != nil {
		t.Fatalf("WindowLayout() error = %v", err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Rect.Width != 59 || layout.Panes[1].Rect.Width != 60 {
		t.Fatalf("WindowLayout() = %#v", layout)
	}

	if _, clientState, err := s.FocusPane(0, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	} else if clientState.FocusedPaneID != pane0.ID {
		t.Fatalf("FocusPane() client = %#v", clientState)
	}
}

func TestResolveInputTargetUsesFocusedPaneWithinSplit(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if _, _, err := s.FocusPane(0, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	}

	pane, clientState, exact := s.ResolveInputTarget(0, pane1.ID)
	if pane == nil || clientState == nil || pane.ID != pane0.ID || exact {
		t.Fatalf("ResolveInputTarget() = pane %#v client %#v exact=%v", pane, clientState, exact)
	}
}

func TestCloseFocusedPaneCollapsesSplit(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	closedPane, window, clientState, windowClosed, _, autoCreate, err := s.CloseFocusedPane(0)
	if err != nil {
		t.Fatalf("CloseFocusedPane() error = %v", err)
	}
	if windowClosed || autoCreate || closedPane == nil || clientState.FocusedPaneID != pane0.ID {
		t.Fatalf("CloseFocusedPane() = pane %#v window %#v client %#v windowClosed=%v autoCreate=%v", closedPane, window, clientState, windowClosed, autoCreate)
	}
	if _, ok := window.Layout.(*PaneLayout); !ok {
		t.Fatalf("collapsed layout = %#v, want single pane", window.Layout)
	}
}

func TestRemovePaneCollapsesSplitAndMovesFocus(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	window, client, finalPane, removed, err := s.RemovePane(pane1.ID, 0)
	if err != nil || !removed || finalPane {
		t.Fatalf("RemovePane() removed=%v final=%v err=%v", removed, finalPane, err)
	}
	if client.FocusedPaneID != pane0.ID {
		t.Fatalf("focused pane = %d, want %d", client.FocusedPaneID, pane0.ID)
	}
	if _, ok := window.Layout.(*PaneLayout); !ok {
		t.Fatalf("collapsed layout = %#v, want single pane", window.Layout)
	}
}

func TestRemoveFinalPaneRequestsReplacement(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	pane := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane, 0)

	window, client, finalPane, removed, err := s.RemovePane(pane.ID, 0)
	if err != nil || !removed || !finalPane || window != nil || client == nil {
		t.Fatalf("RemovePane() window=%#v client=%#v removed=%v final=%v err=%v", window, client, removed, finalPane, err)
	}
	if s.HasWindows() {
		t.Fatal("session retained a window after its final pane exited")
	}
}

func TestLayoutRevisionsAreUniqueAcrossWindows(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	first := &Pane{ID: s.AddPaneID(), Title: "one"}
	w1, _ := s.CreateWindow(first, 0)
	second := &Pane{ID: s.AddPaneID(), Title: "two"}
	w2, _ := s.CreateWindow(second, 0)
	if w1.LayoutRevision == 0 || w2.LayoutRevision <= w1.LayoutRevision {
		t.Fatalf("layout revisions first=%d second=%d", w1.LayoutRevision, w2.LayoutRevision)
	}
}

func TestReattachAdvancesActiveWindowLayoutRevision(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	pane := &Pane{ID: s.AddPaneID(), Title: "one"}
	window, _ := s.CreateWindow(pane, 0)
	previousRevision := window.LayoutRevision

	reattached, activePane, _, err := s.ReattachClient(0)
	if err != nil {
		t.Fatal(err)
	}
	if activePane != pane {
		t.Fatalf("active pane = %#v, want %#v", activePane, pane)
	}
	if reattached.LayoutRevision <= previousRevision {
		t.Fatalf("reattach revision = %d, want greater than %d", reattached.LayoutRevision, previousRevision)
	}
}

func TestSelectingWindowAdvancesItsLayoutRevision(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	first, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "one"}, 0)
	second, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "two"}, 0)
	firstRevision, secondRevision := first.LayoutRevision, second.LayoutRevision

	selected, _, err := s.SelectWindow(0, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if selected.LayoutRevision <= secondRevision || selected.LayoutRevision <= firstRevision {
		t.Fatalf("selected revision = %d, prior revisions = %d and %d", selected.LayoutRevision, firstRevision, secondRevision)
	}
}

func TestSelectingWindowRestoresItsLastActivePane(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	left := &Pane{ID: s.AddPaneID(), Title: "left"}
	firstWindow, _ := s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID(), Title: "right"}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	secondWindow, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "second"}, 0)

	selected, client, err := s.SelectWindow(0, firstWindow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if client.FocusedPaneID != right.ID || selected.ActivePaneID != right.ID {
		t.Fatalf("first selection focused=%d active=%d, want %d", client.FocusedPaneID, selected.ActivePaneID, right.ID)
	}
	if _, _, err := s.FocusPane(0, left.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SelectWindow(0, secondWindow.ID); err != nil {
		t.Fatal(err)
	}
	selected, client, err = s.SelectWindow(0, firstWindow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if client.FocusedPaneID != left.ID || selected.ActivePaneID != left.ID {
		t.Fatalf("second selection focused=%d active=%d, want %d", client.FocusedPaneID, selected.ActivePaneID, left.ID)
	}
}

func TestWindowLayoutCarriesRenderSlots(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 80
	client.TerminalRows = 23
	left := &Pane{ID: s.AddPaneID()}
	s.CreateWindow(left, 0)
	right := &Pane{ID: s.AddPaneID()}
	if _, _, err := s.SplitFocusedPane(0, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	layout, err := s.WindowLayout(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Slot == layout.Panes[1].Slot {
		t.Fatalf("layout slots=%#v", layout.Panes)
	}
}

func TestCreateWindowSizePrefersClientDimensionsOverSplitPane(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	client.TerminalCols = 120
	client.TerminalRows = 39

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash", terminal: newTerminal(120, 39)}
	s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs", terminal: newTerminal(59, 39)}
	if _, _, err := s.SplitFocusedPane(0, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	cols, rows, err := s.createWindowSize()
	if err != nil {
		t.Fatalf("createWindowSize() error = %v", err)
	}
	if cols != 120 || rows != 39 {
		t.Fatalf("createWindowSize() = %dx%d, want 120x39", cols, rows)
	}
}

func TestWindowDisplayIndicesSurviveDeletionAndNewCreation(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	first, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "one"}, 0)
	second, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "two"}, 0)
	third, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "three"}, 0)
	fourth, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "four"}, 0)
	if first.DisplayIndex != 0 || second.DisplayIndex != 1 || third.DisplayIndex != 2 || fourth.DisplayIndex != 3 {
		t.Fatalf("initial display indices = %d, %d, %d, %d", first.DisplayIndex, second.DisplayIndex, third.DisplayIndex, fourth.DisplayIndex)
	}

	if _, _, _, _, _, _, err := s.CloseWindow(0, second.ID); err != nil {
		t.Fatal(err)
	}
	statuses := s.WindowStatuses(0)
	if len(statuses) != 3 || statuses[0].Index != 0 || statuses[1].Index != 2 || statuses[2].Index != 3 {
		t.Fatalf("statuses after deleting display index 1 = %#v", statuses)
	}
	if got, ok := s.WindowIDByIndex(1); ok || got != 0 {
		t.Fatalf("deleted display index lookup = %d, %v", got, ok)
	}
	if got, ok := s.WindowIDByIndex(3); !ok || got != fourth.ID {
		t.Fatalf("display index 3 lookup = %d, %v; want %d, true", got, ok, fourth.ID)
	}
	s.ConsumeInputByte(0, 0x02)
	event := s.ConsumeInputByte(0, '2')
	if !isCommandInput(event, "select-window", "-t", ":2") {
		t.Fatalf("numeric selection event = %#v", event)
	}

	fifth, _ := s.CreateWindow(&Pane{ID: s.AddPaneID(), Title: "five"}, 0)
	if fifth.DisplayIndex != 1 {
		t.Fatalf("new window display index = %d, want 1", fifth.DisplayIndex)
	}
	if got, ok := s.WindowIDByIndex(1); !ok || got != fifth.ID {
		t.Fatalf("display index 1 lookup = %d, %v; want %d, true", got, ok, fifth.ID)
	}
	if _, _, err := s.SelectWindow(0, third.ID); err != nil {
		t.Fatal(err)
	}
	if state := s.SnapshotClient(0); state.ActiveWindowID != third.ID {
		t.Fatalf("numeric-selection target = %d, want %d", state.ActiveWindowID, third.ID)
	}
}

func TestSessionShutdownCleanlyClosesConnection(t *testing.T) {
	closed := false
	var closeErr error
	connection := &recordingQUICConnection{closeWithError: func(code quic.ApplicationErrorCode, message string) error {
		if code != 0 || message != "" {
			closeErr = fmt.Errorf("CloseWithError(%d, %q), want clean application close", code, message)
		}
		closed = true
		return nil
	}}
	s := NewSession(1)
	s.clientInstance = &ClientInstance{QUIC: connection}

	if err := s.shutdown(); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("active QUIC connection was not closed")
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestSessionShutdownEscalatesAndReapsPaneProcess(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	s := NewSession(1)
	timeouts := paneTerminationTimeouts{
		hangup:    25 * time.Millisecond,
		terminate: 25 * time.Millisecond,
		kill:      time.Second,
	}
	var pane *Pane
	if err := s.coordinate(func() error {
		s.EnsureClient(clientID0)
		s.SetClientSize(clientID0, 80, 24)
		var createErr error
		pane, _, _, createErr = s.createWindow(directory, []string{
			shell,
			"-c",
			`trap '' HUP TERM; : > "$1"; while :; do sleep 1; done`,
			"meja-pane-test",
			ready,
		}, 80, 24, shell)
		if createErr == nil {
			s.startPane(pane)
		}
		return createErr
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.shutdownWithTimeouts(timeouts) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("pane process did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}

	started := time.Now()
	if err := s.shutdownWithTimeouts(timeouts); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < timeouts.hangup+timeouts.terminate {
		t.Fatalf("shutdown completed in %v before signal escalation deadlines elapsed", elapsed)
	}
	if elapsed >= time.Second {
		t.Fatalf("shutdown took %v after SIGKILL", elapsed)
	}
	if pane.Process.ProcessState == nil {
		t.Fatalf("pane process was not reaped: %#v", pane.Process.ProcessState)
	}
	select {
	case <-pane.processDone:
	default:
		t.Fatal("pane process waiter did not complete")
	}
}

func TestSessionClosesExistingConnectionBeforeAttachingReplacement(t *testing.T) {
	s := NewSession(1)
	clientState := s.NewClient(clientID0)
	clientState.TerminalCols, clientState.TerminalRows = 80, 24
	s.CreateWindow(&Pane{ID: s.AddPaneID(), terminal: newTerminal(80, 24)}, clientID0)
	replacement := &ClientInstance{}
	closed := false
	var closeErr error
	existing := &ClientInstance{QUIC: &recordingQUICConnection{closeWithError: func(code quic.ApplicationErrorCode, message string) error {
		if s.clientInstance == replacement {
			closeErr = fmt.Errorf("replacement was attached before existing connection was closed")
		}
		if code != protocol.SessionReplacedErrorCode || message != "session attached elsewhere" {
			closeErr = fmt.Errorf("CloseWithError(%d, %q), want replacement close", code, message)
		}
		closed = true
		return nil
	}}}
	s.clientInstance = existing

	if err := s.attachClientInstance(replacement, 80, 24); err != nil {
		t.Fatal(err)
	}

	if !closed {
		t.Fatal("existing QUIC connection was not closed")
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if s.clientInstance != replacement {
		t.Fatal("replacement connection was not attached")
	}
}

func TestStaleTransportCleanupDoesNotDetachReconnectedClientInstance(t *testing.T) {
	s := NewSession(1)
	stale := &ClientInstance{}
	stale.detaching.Store(true)
	current := &ClientInstance{}
	s.clientInstance = current

	s.detachClientInstance()

	if s.clientInstance != current {
		t.Fatal("stale transport cleanup detached the replacement transport")
	}
}

func TestSessionReplacementCloseIsNotLoggable(t *testing.T) {
	err := fmt.Errorf("read control frame: %w", &quic.ApplicationError{
		ErrorCode:    protocol.SessionReplacedErrorCode,
		ErrorMessage: "session attached elsewhere",
	})
	if !isSessionReplacedClose(err) {
		t.Fatal("wrapped session replacement close was not recognized")
	}
	if isSessionReplacedClose(errors.New("read control frame: connection lost")) {
		t.Fatal("ordinary connection error was recognized as session replacement")
	}
}

func TestMonitoredObservationsNameEachWindowFromItsActivePane(t *testing.T) {
	session := NewSession(12)
	t.Cleanup(session.stopOperations)
	firstPane := &Pane{
		ID:     1,
		Title:  "bash",
		Root:   Identity{PID: 101, BirthToken: 1001},
		Launch: PaneLaunch{Shell: "/bin/bash"},
	}
	secondPane := &Pane{
		ID:    2,
		Title: "bash",
		Root:  Identity{PID: 102, BirthToken: 1002},
		Launch: PaneLaunch{
			Shell:         "/bin/bash",
			RequestedArgv: []string{"nvim", "notes.txt"},
		},
	}
	var firstWindow, secondWindow *Window
	if err := session.coordinate(func() error {
		firstWindow, _ = session.CreateWindow(firstPane, clientID0)
		secondWindow, _ = session.CreateWindow(secondPane, clientID0)
		session.SetClientSize(clientID0, 80, 23)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	firstRoot := ObservedProcess{Identity: firstPane.Root, Name: "bash"}
	secondRoot := ObservedProcess{Identity: secondPane.Root, Name: "nvim"}
	batch := monitoredProcessBatch{
		{
			anchor: Anchor{Key: PaneKey{SessionID: session.ID, PaneID: firstPane.ID}, Root: firstPane.Root, PTY: firstPane.PTY},
			observation: ProcessObservation{
				Key:    PaneKey{SessionID: session.ID, PaneID: firstPane.ID},
				Status: StatusShellOwned,
				Root:   &firstRoot,
			},
		},
		{
			anchor: Anchor{Key: PaneKey{SessionID: session.ID, PaneID: secondPane.ID}, Root: secondPane.Root, PTY: secondPane.PTY},
			observation: ProcessObservation{
				Key:       PaneKey{SessionID: session.ID, PaneID: secondPane.ID},
				Status:    StatusDetected,
				Root:      &secondRoot,
				Candidate: &secondRoot,
			},
		},
	}
	if err := session.coordinate(func() error { return session.applyMonitoredProcessObservations(batch) }); err != nil {
		t.Fatal(err)
	}

	var firstCurrent, secondCurrent *Window
	if err := session.coordinate(func() error {
		firstCurrent = cloneWindow(session.Windows[firstWindow.ID])
		secondCurrent = cloneWindow(session.Windows[secondWindow.ID])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if firstCurrent.Name != "bash" || !firstCurrent.AutomaticName {
		t.Fatalf("first window=%#v", firstCurrent)
	}
	if secondCurrent.Name != "nvim" || !secondCurrent.AutomaticName {
		t.Fatalf("second window=%#v", secondCurrent)
	}
}

func TestMonitoredObservationDoesNotOverwriteManualWindowName(t *testing.T) {
	session := NewSession(13)
	t.Cleanup(session.stopOperations)
	pane := &Pane{
		ID:     1,
		Title:  "bash",
		Root:   Identity{PID: 101, BirthToken: 1001},
		Launch: PaneLaunch{Shell: "/bin/bash"},
	}
	var window *Window
	if err := session.coordinate(func() error {
		window, _ = session.CreateWindow(pane, clientID0)
		session.SetClientSize(clientID0, 80, 23)
		_, err := session.RenameWindow(window.ID, "work")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	process := ObservedProcess{Identity: pane.Root, Name: "top"}
	batch := monitoredProcessBatch{{
		anchor: Anchor{Key: PaneKey{SessionID: session.ID, PaneID: pane.ID}, Root: pane.Root, PTY: pane.PTY},
		observation: ProcessObservation{
			Status:    StatusDetected,
			Candidate: &process,
		},
	}}
	if err := session.coordinate(func() error { return session.applyMonitoredProcessObservations(batch) }); err != nil {
		t.Fatal(err)
	}
	var current *Window
	if err := session.coordinate(func() error {
		current = cloneWindow(session.Windows[window.ID])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if current.Name != "work" || current.AutomaticName {
		t.Fatalf("manual window name changed: %#v", current)
	}
}

func TestObservationWindowNameIgnoresAmbiguousJobsAndSanitizesNames(t *testing.T) {
	if got := observationWindowName(ProcessObservation{Status: StatusAmbiguous}); got != "" {
		t.Fatalf("ambiguous name=%q", got)
	}
	process := ObservedProcess{Name: "\n nv\tim\x00"}
	if got := observationWindowName(ProcessObservation{Status: StatusDetected, Candidate: &process}); got != "nvim" {
		t.Fatalf("sanitized name=%q", got)
	}
}
