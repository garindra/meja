package server

func replacePaneWithSplit(layout LayoutNode, targetPaneID, newPaneID uint64, direction SplitDirection) (LayoutNode, bool) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID != targetPaneID {
			return layout, false
		}
		return &SplitLayout{Direction: direction, Ratio: 500, First: node, Second: &PaneLayout{PaneID: newPaneID}}, true
	case *SplitLayout:
		if updated, ok := replacePaneWithSplit(node.First, targetPaneID, newPaneID, direction); ok {
			node.First = updated
			return node, true
		}
		if updated, ok := replacePaneWithSplit(node.Second, targetPaneID, newPaneID, direction); ok {
			node.Second = updated
			return node, true
		}
	}
	return layout, false
}

func swapLayoutPanes(layout LayoutNode, firstPaneID, secondPaneID uint64) bool {
	first := paneLayoutByID(layout, firstPaneID)
	second := paneLayoutByID(layout, secondPaneID)
	if first == nil || second == nil || first == second {
		return false
	}
	first.PaneID, second.PaneID = second.PaneID, first.PaneID
	return true
}

func paneLayoutByID(layout LayoutNode, paneID uint64) *PaneLayout {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID == paneID {
			return node
		}
	case *SplitLayout:
		if pane := paneLayoutByID(node.First, paneID); pane != nil {
			return pane
		}
		return paneLayoutByID(node.Second, paneID)
	}
	return nil
}

func removePaneFromLayout(layout LayoutNode, targetPaneID uint64) (LayoutNode, uint64, bool) {
	switch node := layout.(type) {
	case *PaneLayout:
		if node.PaneID == targetPaneID {
			return nil, 0, true
		}
	case *SplitLayout:
		if updated, focusedPaneID, removed := removePaneFromLayout(node.First, targetPaneID); removed {
			if updated == nil {
				return node.Second, firstPaneID(node.Second), true
			}
			node.First = updated
			return node, focusedPaneID, true
		}
		if updated, focusedPaneID, removed := removePaneFromLayout(node.Second, targetPaneID); removed {
			if updated == nil {
				return node.First, firstPaneID(node.First), true
			}
			node.Second = updated
			return node, focusedPaneID, true
		}
	}
	return layout, 0, false
}

func firstPaneID(layout LayoutNode) uint64 {
	if layout == nil {
		return 0
	}
	ids := layout.PaneIDs()
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

func containsPane(ids []uint64, paneID uint64) bool {
	for _, id := range ids {
		if id == paneID {
			return true
		}
	}
	return false
}

func windowHasPane(window *Window, paneID uint64) bool {
	return window != nil && containsPane(window.Layout.PaneIDs(), paneID)
}

func windowPrimaryPaneID(window *Window) uint64 {
	if window == nil {
		return 0
	}
	return firstPaneID(window.Layout)
}

func windowActivePaneID(window *Window) uint64 {
	if window == nil {
		return 0
	}
	if windowHasPane(window, window.ActivePaneID) {
		return window.ActivePaneID
	}
	return windowPrimaryPaneID(window)
}

func cloneWindow(window *Window) *Window {
	if window == nil {
		return nil
	}
	return &Window{
		ID: window.ID, GroupID: window.GroupID, DisplayIndex: window.DisplayIndex,
		Name: window.Name, AutomaticName: window.AutomaticName,
		ActivePaneID: window.ActivePaneID, Zoomed: window.Zoomed, ZoomedPaneID: window.ZoomedPaneID,
		Layout: window.Layout, LayoutRevision: window.LayoutRevision, Cols: window.Cols, Rows: window.Rows,
		layoutCycleIndex: window.layoutCycleIndex,
	}
}
