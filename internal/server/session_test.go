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

func TestSessionSplitCreatesNewPaneAndPlacements(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)

	pane0 := &Pane{ID: testAddPaneID(s), Title: "bash"}
	window, inputState := createTestWindow(s, pane0)
	if window.ID != 1 || windowPrimaryPaneID(window) != pane0.ID || inputState.FocusedPaneID != pane0.ID {
		t.Fatalf("initial window = %#v client=%#v", window, inputState)
	}

	pane1 := &Pane{ID: testAddPaneID(s), Title: "logs"}
	window, _, err := splitTestFocusedPane(s, pane1, SplitVertical)
	if err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if _, ok := window.Layout.(*SplitLayout); !ok {
		t.Fatalf("window layout = %#v, want split", window.Layout)
	}
	syncTestProjection(t, s)
	inputState = client.testLayout()
	placements, _ := testClientLayoutPanes(s)
	if inputState.FocusedPaneID != pane1.ID || len(placements) != 2 {
		t.Fatalf("client after split = %#v", inputState)
	}
	if placements[1].PaneID != pane1.ID {
		t.Fatalf("second render slot = %#v", placements)
	}
}

func TestResizeFocusedPaneAdvancesRevisionAndPersistsRatio(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	left := &Pane{ID: testAddPaneID(s)}
	createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)
	before := s.Windows[client.testLayout().WindowID].LayoutRevision
	window, _, changed, err := resizeTestFocusedPane(s, ResizePaneRight, 5)
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
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	left := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	instance := clientForState(s)
	if _, err := instance.executeAttachedCommand([]string{"resize-pane", "-R", "5"}); err != nil {
		t.Fatal(err)
	}
	leftCols, leftRows := left.TerminalSize()
	rightCols, rightRows := right.TerminalSize()
	if leftCols != 44 || rightCols != 35 || leftRows != 24 || rightRows != 24 {
		t.Fatalf("terminal sizes after resize: left=%dx%d right=%dx%d", leftCols, leftRows, rightCols, rightRows)
	}
}

func TestToggleZoomProjectsFocusedPaneWithoutChangingLayout(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	left := &Pane{ID: testAddPaneID(s)}
	createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	root := s.Windows[client.testLayout().WindowID].Layout.(*SplitLayout)
	ratio := root.Ratio
	window, _, changed, err := toggleTestZoom(s)
	if err != nil || !changed {
		t.Fatalf("ToggleZoom() changed=%v err=%v", changed, err)
	}
	if !window.Zoomed || window.ZoomedPaneID != left.ID || root.Ratio != ratio {
		t.Fatalf("zoom changed underlying layout: window=%#v ratio=%d want=%d", window, root.Ratio, ratio)
	}
	syncTestProjection(t, s)
	placements, _ := testClientLayoutPanes(s)
	if len(placements) != 1 || placements[0].PaneID != left.ID || placements[0].Slot != 0 {
		t.Fatalf("zoomed placements = %#v", placements)
	}
	layout, err := testClientLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 1 || layout.Panes[0].PaneID != left.ID || layout.Panes[0].Rect != (protocol.Rect{Width: 80, Height: 24}) {
		t.Fatalf("zoomed protocol layout = %#v", layout)
	}

	window, _, changed, err = toggleTestZoom(s)
	if err != nil || !changed || window.Zoomed {
		t.Fatalf("unzoom changed=%v window=%#v err=%v", changed, window, err)
	}
	syncTestProjection(t, s)
	placements, _ = testClientLayoutPanes(s)
	if len(placements) != 2 || root.Ratio != ratio {
		t.Fatalf("unzoomed placements=%#v ratio=%d want=%d", placements, root.Ratio, ratio)
	}
}

func TestToggleZoomSinglePaneIsNoOp(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	createTestWindow(s, &Pane{ID: testAddPaneID(s)})
	window, _, changed, err := toggleTestZoom(s)
	if err != nil || changed || window.Zoomed {
		t.Fatalf("single pane ToggleZoom() changed=%v window=%#v err=%v", changed, window, err)
	}
}

func TestZoomCommandResizesOnlyVisiblePaneToFullWindow(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	left := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	if err := resizeTestSessionActiveWindow(s, 80, 24); err != nil {
		t.Fatal(err)
	}
	instance := clientForState(s)
	if _, err := instance.executeAttachedCommand([]string{"resize-pane", "-Z"}); err != nil {
		t.Fatal(err)
	}
	leftCols, leftRows := left.TerminalSize()
	rightCols, rightRows := right.TerminalSize()
	if leftCols != 80 || leftRows != 24 || rightCols != 40 || rightRows != 24 {
		t.Fatalf("zoomed sizes: left=%dx%d right=%dx%d", leftCols, leftRows, rightCols, rightRows)
	}
	if err := resizeTestSessionActiveWindow(s, 100, 30); err != nil {
		t.Fatal(err)
	}
	leftCols, leftRows = left.TerminalSize()
	rightCols, rightRows = right.TerminalSize()
	if leftCols != 100 || leftRows != 30 || rightCols != 40 || rightRows != 24 {
		t.Fatalf("resized zoomed sizes: left=%dx%d right=%dx%d", leftCols, leftRows, rightCols, rightRows)
	}
	if _, err := instance.executeAttachedCommand([]string{"resize-pane", "-Z"}); err != nil {
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
		if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := toggleTestZoom(s); err != nil {
			t.Fatal(err)
		}
		syncTestProjection(t, s)
		window, state, err := clientForState(s).FocusPaneDirection('C')
		placements, _ := testClientLayoutPanes(s)
		if err != nil || window.Zoomed || state.FocusedPaneID != right.ID || len(placements) != 2 {
			t.Fatalf("focus after zoom: window=%#v state=%#v err=%v", window, state, err)
		}
	})
	t.Run("resize", func(t *testing.T) {
		s, left, _ := newZoomTestSession(t)
		if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := toggleTestZoom(s); err != nil {
			t.Fatal(err)
		}
		syncTestProjection(t, s)
		window, _, changed, err := resizeTestFocusedPane(s, ResizePaneRight, 1)
		syncTestProjection(t, s)
		state := snapshotTestClient(s)
		placements, _ := testClientLayoutPanes(s)
		if err != nil || !changed || window.Zoomed || len(placements) != 2 {
			t.Fatalf("resize after zoom: changed=%v window=%#v state=%#v err=%v", changed, window, state, err)
		}
	})
	t.Run("swap", func(t *testing.T) {
		s, _, _ := newZoomTestSession(t)
		if _, _, _, err := toggleTestZoom(s); err != nil {
			t.Fatal(err)
		}
		syncTestProjection(t, s)
		window, _, changed, err := swapTestFocusedPane(s, SwapPanePrevious)
		syncTestProjection(t, s)
		state := snapshotTestClient(s)
		placements, _ := testClientLayoutPanes(s)
		if err != nil || !changed || window.Zoomed || len(placements) != 2 {
			t.Fatalf("swap after zoom: changed=%v window=%#v state=%#v err=%v", changed, window, state, err)
		}
	})
	t.Run("split", func(t *testing.T) {
		s, _, _ := newZoomTestSession(t)
		if _, _, _, err := toggleTestZoom(s); err != nil {
			t.Fatal(err)
		}
		window, _, err := splitTestFocusedPane(s, &Pane{ID: testAddPaneID(s)}, SplitHorizontal)
		syncTestProjection(t, s)
		state := snapshotTestClient(s)
		placements, _ := testClientLayoutPanes(s)
		if err != nil || window.Zoomed || len(placements) != 3 {
			t.Fatalf("split after zoom: window=%#v state=%#v err=%v", window, state, err)
		}
	})
}

func TestZoomStateSurvivesWindowSwitch(t *testing.T) {
	s, left, _ := newZoomTestSession(t)
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	zoomed, _, _, err := toggleTestZoom(s)
	if err != nil {
		t.Fatal(err)
	}
	zoomedWindowID := zoomed.ID
	createTestWindow(s, &Pane{ID: testAddPaneID(s)})
	window, state, err := selectTestSessionWindow(s, zoomedWindowID)
	if err != nil {
		t.Fatal(err)
	}
	placements, _ := testClientLayoutPanes(s)
	if !window.Zoomed || window.ZoomedPaneID != left.ID || len(placements) != 1 || placements[0].PaneID != left.ID {
		t.Fatalf("restored zoomed window=%#v state=%#v", window, state)
	}
}

func TestPaneExitMaintainsOrClearsZoomAsLayoutAllows(t *testing.T) {
	s, left, right := newZoomTestSession(t)
	third := &Pane{ID: testAddPaneID(s)}
	if _, _, err := splitTestFocusedPane(s, third, SplitHorizontal); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := toggleTestZoom(s); err != nil {
		t.Fatal(err)
	}
	window, _, final, removed, err := removeTestPane(s, right.ID)
	if err != nil || final || !removed || !window.Zoomed || window.ZoomedPaneID != left.ID {
		t.Fatalf("hidden pane exit: window=%#v final=%v removed=%v err=%v", window, final, removed, err)
	}
	window, state, final, removed, err := removeTestPane(s, third.ID)
	placements, _ := testClientLayoutPanes(s)
	if err != nil || final || !removed || window.Zoomed || len(placements) != 1 || placements[0].PaneID != left.ID {
		t.Fatalf("last hidden pane exit: window=%#v state=%#v final=%v removed=%v err=%v", window, state, final, removed, err)
	}
}

func TestClosingZoomedPaneClearsZoom(t *testing.T) {
	s, left, _ := newZoomTestSession(t)
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := toggleTestZoom(s); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)
	_, window, state, windowClosed, _, _, err := closeTestFocusedPane(s)
	placements, _ := testClientLayoutPanes(s)
	if err != nil || windowClosed || window.Zoomed || len(placements) != 1 {
		t.Fatalf("close zoomed pane: window=%#v state=%#v windowClosed=%v err=%v", window, state, windowClosed, err)
	}
}

func newZoomTestSession(t *testing.T) (*SessionState, *Pane, *Pane) {
	t.Helper()
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	left := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)
	return s, left, right
}

func TestResizePreservesVisualClientPaneOrder(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(16, 4)
	first := &Pane{ID: 2, terminal: newTerminal(16, 4)}
	second := &Pane{ID: 1, terminal: newTerminal(16, 4)}
	s.daemon.nextPaneID = 3
	createTestWindow(s, first)
	window := s.Windows[client.testLayout().WindowID]
	s.Panes[second.ID] = second
	window.Layout = &SplitLayout{
		Direction: SplitHorizontal,
		Ratio:     500,
		First:     &PaneLayout{PaneID: first.ID},
		Second:    &PaneLayout{PaneID: second.ID},
	}
	clientInstance := clientForState(s)
	clientInstance.currentView.Layout.Panes = []protocol.PanePlacement{{PaneID: first.ID, Slot: 0}, {PaneID: second.ID, Slot: 1}}
	if got := clientInstance.currentPanePlacements()[0].PaneID; got != first.ID {
		t.Fatalf("initial slot 0 pane = %d, want %d", got, first.ID)
	}

	if err := resizeTestSessionActiveWindow(s, 16, 1); err != nil {
		t.Fatal(err)
	}
	if got := clientInstance.currentPanePlacements()[0].PaneID; got != first.ID {
		t.Fatalf("resized slot 0 pane = %d, want %d", got, first.ID)
	}
}

func TestSwapFocusedPaneUsesVisualOrderAndKeepsFocus(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)
	left := &Pane{ID: testAddPaneID(s)}
	createTestWindow(s, left)
	topRight := &Pane{ID: testAddPaneID(s)}
	if _, _, err := splitTestFocusedPane(s, topRight, SplitVertical); err != nil {
		t.Fatal(err)
	}
	bottomRight := &Pane{ID: testAddPaneID(s)}
	window, _, err := splitTestFocusedPane(s, bottomRight, SplitHorizontal)
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision := window.LayoutRevision
	syncTestProjection(t, s)

	window, state, changed, err := swapTestFocusedPane(s, SwapPanePrevious)
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

	window, state, changed, err = swapTestFocusedPane(s, SwapPaneNext)
	if err != nil || !changed {
		t.Fatalf("SwapFocusedPane(next) changed=%v err=%v", changed, err)
	}
	placements = window.Layout.Compute(Rect{Width: 120, Height: 39})
	if placements[1].PaneID != topRight.ID || placements[2].PaneID != bottomRight.ID || state.FocusedPaneID != bottomRight.ID {
		t.Fatalf("placements after next swap = %#v state=%#v", placements, state)
	}
}

func TestSwapFocusedPaneWrapsAtVisualEdges(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 24)
	first := &Pane{ID: testAddPaneID(s)}
	createTestWindow(s, first)
	second := &Pane{ID: testAddPaneID(s)}
	if _, _, err := splitTestFocusedPane(s, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, first.ID); err != nil {
		t.Fatal(err)
	}

	window, state, changed, err := swapTestFocusedPane(s, SwapPanePrevious)
	if err != nil || !changed {
		t.Fatalf("SwapFocusedPane(previous) changed=%v err=%v", changed, err)
	}
	placements := window.Layout.Compute(Rect{Width: 80, Height: 24})
	if placements[1].PaneID != first.ID || state.FocusedPaneID != first.ID {
		t.Fatalf("wrapped placements=%#v state=%#v", placements, state)
	}
}

func TestRecursiveMixedSplitsAndCloseCollapseOnlyParent(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)

	pane0 := &Pane{ID: testAddPaneID(s), Title: "root"}
	createTestWindow(s, pane0)
	pane1 := &Pane{ID: testAddPaneID(s), Title: "right"}
	if _, _, err := splitTestFocusedPane(s, pane1, SplitVertical); err != nil {
		t.Fatalf("vertical SplitFocusedPane() error = %v", err)
	}
	pane2 := &Pane{ID: testAddPaneID(s), Title: "bottom-right"}
	window, _, err := splitTestFocusedPane(s, pane2, SplitHorizontal)
	if err != nil {
		t.Fatalf("nested horizontal SplitFocusedPane() error = %v", err)
	}
	syncTestProjection(t, s)
	clientPlacements, _ := testClientLayoutPanes(s)
	layoutPlacements := window.Layout.Compute(Rect{Width: 120, Height: 39})
	if len(layoutPlacements) != 3 || len(clientPlacements) != 3 {
		t.Fatalf("nested layout = %#v client placements=%#v", layoutPlacements, clientPlacements)
	}
	if layoutPlacements[0].PaneID != pane0.ID || layoutPlacements[0].Rect.Width != 59 ||
		layoutPlacements[1].PaneID != pane1.ID || layoutPlacements[1].Rect.Height != 19 ||
		layoutPlacements[2].PaneID != pane2.ID || layoutPlacements[2].Rect.Y != 20 {
		t.Fatalf("nested placements = %#v", layoutPlacements)
	}

	closed, window, inputState, windowClosed, _, _, err := closeTestFocusedPane(s)
	if err != nil || windowClosed || closed != pane2 {
		t.Fatalf("CloseFocusedPane() closed=%#v windowClosed=%v err=%v", closed, windowClosed, err)
	}
	layoutPlacements = window.Layout.Compute(Rect{Width: 120, Height: 39})
	if len(layoutPlacements) != 2 || layoutPlacements[0].PaneID != pane0.ID || layoutPlacements[1].PaneID != pane1.ID || inputState.FocusedPaneID != pane1.ID {
		t.Fatalf("collapsed nested layout = %#v client=%#v", layoutPlacements, inputState)
	}
}

func TestWindowRejectsNinthPane(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(240, 80)
	pane := &Pane{ID: testAddPaneID(s)}
	createTestWindow(s, pane)
	for count := 2; count <= int(protocol.MaxVisiblePanes); count++ {
		pane = &Pane{ID: testAddPaneID(s)}
		if _, _, err := splitTestFocusedPane(s, pane, SplitDirection(count%2)); err != nil {
			t.Fatalf("split %d error = %v", count, err)
		}
	}
	if err := s.CanSplitFocusedPane(); err == nil {
		t.Fatal("CanSplitFocusedPane() allowed ninth pane")
	}
	extra := &Pane{ID: testAddPaneID(s)}
	if _, _, err := splitTestFocusedPane(s, extra, SplitVertical); err == nil {
		t.Fatal("SplitFocusedPane() allowed ninth pane")
	}
}

func TestClientLayoutAndFocusReuseVisiblePanes(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)

	pane0 := &Pane{ID: testAddPaneID(s), Title: "bash"}
	createTestWindow(s, pane0)
	pane1 := &Pane{ID: testAddPaneID(s), Title: "logs"}
	if _, _, err := splitTestFocusedPane(s, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	syncTestProjection(t, s)

	layout, err := testClientLayout(s)
	if err != nil {
		t.Fatalf("ClientLayout() error = %v", err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Rect.Width != 59 || layout.Panes[1].Rect.Width != 60 {
		t.Fatalf("ClientLayout() = %#v", layout)
	}

	if _, inputState, err := focusTestSessionPane(s, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	} else if inputState.FocusedPaneID != pane0.ID {
		t.Fatalf("FocusPane() client = %#v", inputState)
	}
}

func TestResolveInputTargetUsesFocusedPaneWithinSplit(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)

	pane0 := &Pane{ID: testAddPaneID(s), Title: "bash"}
	createTestWindow(s, pane0)
	pane1 := &Pane{ID: testAddPaneID(s), Title: "logs"}
	if _, _, err := splitTestFocusedPane(s, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	if _, _, err := focusTestSessionPane(s, pane0.ID); err != nil {
		t.Fatalf("FocusPane() error = %v", err)
	}

	pane, inputState, exact := resolveTestInputTarget(s, pane1.ID)
	if pane == nil || inputState.LayoutRevision == 0 || pane.ID != pane0.ID || exact {
		t.Fatalf("ResolveInputTarget() = pane %#v client %#v exact=%v", pane, inputState, exact)
	}
}

func TestCloseFocusedPaneCollapsesSplit(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)

	pane0 := &Pane{ID: testAddPaneID(s), Title: "bash"}
	createTestWindow(s, pane0)
	pane1 := &Pane{ID: testAddPaneID(s), Title: "logs"}
	if _, _, err := splitTestFocusedPane(s, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}
	syncTestProjection(t, s)

	closedPane, window, inputState, windowClosed, _, autoCreate, err := closeTestFocusedPane(s)
	if err != nil {
		t.Fatalf("CloseFocusedPane() error = %v", err)
	}
	if windowClosed || autoCreate || closedPane == nil || inputState.FocusedPaneID != pane0.ID {
		t.Fatalf("CloseFocusedPane() = pane %#v window %#v client %#v windowClosed=%v autoCreate=%v", closedPane, window, inputState, windowClosed, autoCreate)
	}
	if _, ok := window.Layout.(*PaneLayout); !ok {
		t.Fatalf("collapsed layout = %#v, want single pane", window.Layout)
	}
}

func TestCloseFocusedPaneRestoresMostRecentlyFocusedSurvivor(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)

	first := &Pane{ID: testAddPaneID(s), Title: "first"}
	second := &Pane{ID: testAddPaneID(s), Title: "second"}
	third := &Pane{ID: testAddPaneID(s), Title: "third"}
	createTestWindow(s, first)
	if _, _, err := splitTestFocusedPane(s, second, SplitVertical); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := splitTestFocusedPane(s, third, SplitHorizontal); err != nil {
		t.Fatal(err)
	}
	// The third pane's structural sibling is first. Make second the most
	// recently focused survivor so an MRU fallback is distinguishable from a
	// layout-neighbor fallback.
	if _, _, err := focusTestSessionPane(s, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := focusTestSessionPane(s, third.ID); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)

	closed, _, state, _, _, _, err := closeTestFocusedPane(s)
	if err != nil {
		t.Fatal(err)
	}
	if closed != third || state.FocusedPaneID != second.ID {
		t.Fatalf("first close = pane %#v focus %d, want pane %d then focus %d", closed, state.FocusedPaneID, third.ID, second.ID)
	}
	closed, _, state, _, _, _, err = closeTestFocusedPane(s)
	if err != nil {
		t.Fatal(err)
	}
	if closed != second || state.FocusedPaneID != first.ID {
		t.Fatalf("second close = pane %#v focus %d, want pane %d then focus %d", closed, state.FocusedPaneID, second.ID, first.ID)
	}
}

func TestCloseFocusedPaneSelectsLatestSurvivingWindow(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	first, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "first"})
	second, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "second"})
	third, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "third"})
	if _, _, err := selectTestSessionWindow(s, third.ID); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.daemon.windowSelectionTarget(s.ID, 0, true); !ok || got != second.ID {
		t.Fatalf("last window before close = %d, %v; want %d, true", got, ok, second.ID)
	}

	_, replacement, client, closed, _, _, err := closeTestFocusedPane(s)
	if err != nil || !closed {
		t.Fatalf("CloseFocusedPane() replacement=%#v client=%#v closed=%v err=%v", replacement, client, closed, err)
	}
	if replacement == nil || replacement.ID != second.ID || client.WindowID != second.ID {
		t.Fatalf("replacement window=%#v client=%#v; want window %d active", replacement, client, second.ID)
	}
	if client.WindowID == first.ID {
		t.Fatalf("close selected window fell back to first window %d", first.ID)
	}
}

func TestRemovePaneCollapsesSplitAndMovesFocus(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	pane0 := &Pane{ID: testAddPaneID(s), Title: "bash"}
	createTestWindow(s, pane0)
	pane1 := &Pane{ID: testAddPaneID(s), Title: "logs"}
	if _, _, err := splitTestFocusedPane(s, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	window, client, finalPane, removed, err := removeTestPane(s, pane1.ID)
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
	s := NewSessionState(0)
	newTestClient(s)
	pane := &Pane{ID: testAddPaneID(s), Title: "bash"}
	createTestWindow(s, pane)

	window, client, finalPane, removed, err := removeTestPane(s, pane.ID)
	if err != nil || !removed || !finalPane || window != nil || client.LayoutRevision == 0 {
		t.Fatalf("RemovePane() window=%#v client=%#v removed=%v final=%v err=%v", window, client, removed, finalPane, err)
	}
	if s.HasWindows() {
		t.Fatal("session retained a window after its final pane exited")
	}
}

func TestLayoutRevisionsAreUniqueAcrossWindows(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	first := &Pane{ID: testAddPaneID(s), Title: "one"}
	w1, _ := createTestWindow(s, first)
	second := &Pane{ID: testAddPaneID(s), Title: "two"}
	w2, _ := createTestWindow(s, second)
	if w1.LayoutRevision == 0 || w2.LayoutRevision <= w1.LayoutRevision {
		t.Fatalf("layout revisions first=%d second=%d", w1.LayoutRevision, w2.LayoutRevision)
	}
}

func TestReattachPreservesActiveClientLayoutRevision(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	pane := &Pane{ID: testAddPaneID(s), Title: "one"}
	window, _ := createTestWindow(s, pane)
	previousRevision := window.LayoutRevision

	client := clientForState(s).currentView.Layout
	reattached := cloneWindow(s.Windows[client.WindowID])
	activePane := s.Panes[client.FocusedPaneID]
	if activePane != pane {
		t.Fatalf("active pane = %#v, want %#v", activePane, pane)
	}
	if reattached.LayoutRevision != previousRevision {
		t.Fatalf("reattach revision = %d, want unchanged at %d", reattached.LayoutRevision, previousRevision)
	}
}

func TestSelectingWindowAdvancesClientProjectionWithoutChangingCanonicalLayout(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	first, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "one"})
	second, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "two"})
	firstRevision, secondRevision := first.LayoutRevision, second.LayoutRevision
	client := clientForState(s)
	clientLayoutRevision := client.currentView.Layout.LayoutRevision

	selected, _, err := selectTestSessionWindow(s, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if selected.LayoutRevision != firstRevision || second.LayoutRevision != secondRevision {
		t.Fatalf("selection changed canonical revisions: first=%d want=%d second=%d want=%d", selected.LayoutRevision, firstRevision, second.LayoutRevision, secondRevision)
	}
	if client.currentView.Layout.LayoutRevision <= clientLayoutRevision {
		t.Fatalf("selected client layout revision = %d, want newer than %d", client.currentView.Layout.LayoutRevision, clientLayoutRevision)
	}
}

func TestSelectingWindowRestoresItsLastActivePane(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	left := &Pane{ID: testAddPaneID(s), Title: "left"}
	firstWindow, _ := createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s), Title: "right"}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	secondWindow, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "second"})

	_, client, err := selectTestSessionWindow(s, firstWindow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view := s.WindowViews[firstWindow.ID]; client.FocusedPaneID != right.ID || view.FocusedPaneID != right.ID {
		t.Fatalf("first selection client focus=%d session-view focus=%d, want %d", client.FocusedPaneID, view.FocusedPaneID, right.ID)
	}
	if _, _, err := focusTestSessionPane(s, left.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectTestSessionWindow(s, secondWindow.ID); err != nil {
		t.Fatal(err)
	}
	_, client, err = selectTestSessionWindow(s, firstWindow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view := s.WindowViews[firstWindow.ID]; client.FocusedPaneID != left.ID || view.FocusedPaneID != left.ID {
		t.Fatalf("second selection client focus=%d session-view focus=%d, want %d", client.FocusedPaneID, view.FocusedPaneID, left.ID)
	}
}

func TestClientLayoutCarriesRenderSlots(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(80, 23)
	left := &Pane{ID: testAddPaneID(s)}
	createTestWindow(s, left)
	right := &Pane{ID: testAddPaneID(s)}
	if _, _, err := splitTestFocusedPane(s, right, SplitVertical); err != nil {
		t.Fatal(err)
	}
	syncTestProjection(t, s)
	layout, err := testClientLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Panes) != 2 || layout.Panes[0].Slot == layout.Panes[1].Slot {
		t.Fatalf("layout slots=%#v", layout.Panes)
	}
}

func TestCreateWindowSizePrefersClientDimensionsOverSplitPane(t *testing.T) {
	s := NewSessionState(0)
	client := newTestClient(s)
	client.setTestTerminalSize(120, 39)

	pane0 := &Pane{ID: testAddPaneID(s), Title: "bash", terminal: newTerminal(120, 39)}
	createTestWindow(s, pane0)
	pane1 := &Pane{ID: testAddPaneID(s), Title: "logs", terminal: newTerminal(59, 39)}
	if _, _, err := splitTestFocusedPane(s, pane1, SplitVertical); err != nil {
		t.Fatalf("SplitFocusedPane() error = %v", err)
	}

	cols, rows, err := clientForState(s).createWindowSize()
	if err != nil {
		t.Fatalf("createWindowSize() error = %v", err)
	}
	if cols != 120 || rows != 39 {
		t.Fatalf("createWindowSize() = %dx%d, want 120x39", cols, rows)
	}
}

func TestWindowDisplayIndicesSurviveDeletionAndNewCreation(t *testing.T) {
	s := NewSessionState(0)
	newTestClient(s)
	first, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "one"})
	second, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "two"})
	third, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "three"})
	fourth, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "four"})
	if first.DisplayIndex != 0 || second.DisplayIndex != 1 || third.DisplayIndex != 2 || fourth.DisplayIndex != 3 {
		t.Fatalf("initial display indices = %d, %d, %d, %d", first.DisplayIndex, second.DisplayIndex, third.DisplayIndex, fourth.DisplayIndex)
	}

	client := clientForState(s)
	result, err := s.daemon.removeClientPane(client.identity, second.ActivePaneID)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitTestProjection(client, result.Transition); err != nil {
		t.Fatal(err)
	}
	statuses := s.WindowStatuses()
	if len(statuses) != 3 || statuses[0].Index != 0 || statuses[1].Index != 2 || statuses[2].Index != 3 {
		t.Fatalf("statuses after deleting display index 1 = %#v", statuses)
	}
	if got, ok := clientForState(s).WindowIDByIndex(1); ok || got != 0 {
		t.Fatalf("deleted display index lookup = %d, %v", got, ok)
	}
	if got, ok := clientForState(s).WindowIDByIndex(3); !ok || got != fourth.ID {
		t.Fatalf("display index 3 lookup = %d, %v; want %d, true", got, ok, fourth.ID)
	}
	clientForState(s).ConsumeInputByte(0x02)
	event := clientForState(s).ConsumeInputByte('2')
	if !isCommandInput(event, "select-window", "-t", ":2") {
		t.Fatalf("numeric selection event = %#v", event)
	}

	fifth, _ := createTestWindow(s, &Pane{ID: testAddPaneID(s), Title: "five"})
	if fifth.DisplayIndex != 1 {
		t.Fatalf("new window display index = %d, want 1", fifth.DisplayIndex)
	}
	if got, ok := clientForState(s).WindowIDByIndex(1); !ok || got != fifth.ID {
		t.Fatalf("display index 1 lookup = %d, %v; want %d, true", got, ok, fifth.ID)
	}
	if _, _, err := selectTestSessionWindow(s, third.ID); err != nil {
		t.Fatal(err)
	}
	if layout := clientForState(s).currentView.Layout; layout.WindowID != third.ID {
		t.Fatalf("numeric-selection target = %d, want %d", layout.WindowID, third.ID)
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
	s := NewSessionState(1)
	d := newCommandTestDaemon(t)
	s.daemon = d
	d.sessions[s.ID] = s
	setTestClient(s, &ClientInstance{QUIC: connection})

	if err := d.shutdownSession(s); err != nil {
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
	s := NewSessionState(1)
	d := newCommandTestDaemon(t)
	s.daemon = d
	d.sessions[s.ID] = s
	timeouts := paneTerminationTimeouts{
		hangup:    25 * time.Millisecond,
		terminate: 25 * time.Millisecond,
		kill:      time.Second,
	}
	var pane *Pane
	if err := runStateOperation(s, func() error {
		setTestClientSize(s, 80, 24)
		var createErr error
		pane, _, createErr = s.daemon.startSessionWindow(s, directory, []string{
			shell,
			"-c",
			`trap '' HUP TERM; : > "$1"; while :; do sleep 1; done`,
			"meja-pane-test",
			ready,
		}, 80, 24, shell)
		return createErr
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.shutdownSessionWithTimeouts(s, timeouts) }()

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
	if err := d.shutdownSessionWithTimeouts(s, timeouts); err != nil {
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

func TestInitializeAttachedViewDoesNotReplaceDaemonClient(t *testing.T) {
	s := NewSessionState(1)
	fixtureClient := newTestClient(s)
	fixtureClient.setTestTerminalSize(80, 24)
	createTestWindow(s, &Pane{ID: testAddPaneID(s), terminal: newTerminal(80, 24)})
	existing := newClientInstance(s.daemon, nil)
	setTestClient(s, existing)
	replacement := newClientInstance(s.daemon, &ClientIdentity{SessionID: s.ID, ID: 999})

	if _, err := s.daemon.initializeClient(ClientInitialized{ClientID: replacement.identity.ID, Cols: 80, Rows: 24}); err == nil {
		t.Fatal("unregistered replacement initialized a view")
	}
	if testClientOf(s) != existing {
		t.Fatal("client-side view initialization changed daemon ownership")
	}
}

func TestStaleTransportCleanupDoesNotDetachReconnectedClientInstance(t *testing.T) {
	s := NewSessionState(1)
	stale := newClientInstance(s.daemon, nil)
	stale.detaching.Store(true)
	current := newClientInstance(s.daemon, nil)
	setTestClient(s, current)

	stale.releaseFrontendResources()

	if testClientOf(s) != current {
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
	session := NewSessionState(12)
	t.Cleanup(func() { stopState(session) })
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
	if err := runStateOperation(session, func() error {
		firstWindow, _ = createTestWindow(session, firstPane)
		secondWindow, _ = createTestWindow(session, secondPane)
		setTestClientSize(session, 80, 23)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	firstRoot := ObservedProcess{Identity: firstPane.Root, Name: "bash"}
	secondRoot := ObservedProcess{Identity: secondPane.Root, Name: "nvim"}
	batch := monitoredProcessBatch{
		{
			anchor: Anchor{Key: PaneKey{PaneID: firstPane.ID}, Root: firstPane.Root, PTY: firstPane.PTY},
			observation: ProcessObservation{
				Key:    PaneKey{PaneID: firstPane.ID},
				Status: StatusShellOwned,
				Root:   &firstRoot,
			},
		},
		{
			anchor: Anchor{Key: PaneKey{PaneID: secondPane.ID}, Root: secondPane.Root, PTY: secondPane.PTY},
			observation: ProcessObservation{
				Key:       PaneKey{PaneID: secondPane.ID},
				Status:    StatusDetected,
				Root:      &secondRoot,
				Candidate: &secondRoot,
			},
		},
	}
	if err := runStateOperation(session, func() error { return session.daemon.applyMonitoredProcessObservations(session, batch) }); err != nil {
		t.Fatal(err)
	}

	var firstCurrent, secondCurrent *Window
	if err := runStateOperation(session, func() error {
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
	session := NewSessionState(13)
	t.Cleanup(func() { stopState(session) })
	pane := &Pane{
		ID:     1,
		Title:  "bash",
		Root:   Identity{PID: 101, BirthToken: 1001},
		Launch: PaneLaunch{Shell: "/bin/bash"},
	}
	var window *Window
	if err := runStateOperation(session, func() error {
		window, _ = createTestWindow(session, pane)
		setTestClientSize(session, 80, 23)
		_, err := session.RenameWindow(window.ID, "work")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	process := ObservedProcess{Identity: pane.Root, Name: "top"}
	batch := monitoredProcessBatch{{
		anchor: Anchor{Key: PaneKey{PaneID: pane.ID}, Root: pane.Root, PTY: pane.PTY},
		observation: ProcessObservation{
			Status:    StatusDetected,
			Candidate: &process,
		},
	}}
	if err := runStateOperation(session, func() error { return session.daemon.applyMonitoredProcessObservations(session, batch) }); err != nil {
		t.Fatal(err)
	}
	var current *Window
	if err := runStateOperation(session, func() error {
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
