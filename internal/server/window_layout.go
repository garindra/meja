package server

import "sort"

type Rect struct {
	X      int
	Y      int
	Width  int
	Height int
}

type PanePlacement struct {
	PaneID uint64
	Rect   Rect
}

type LayoutNode interface {
	Compute(rect Rect) []PanePlacement
	PaneIDs() []uint64
}

type PaneLayout struct {
	PaneID uint64
}

type SplitDirection uint8

const (
	SplitVertical SplitDirection = iota
	SplitHorizontal
)

type SplitLayout struct {
	Direction SplitDirection
	Ratio     uint16
	First     LayoutNode
	Second    LayoutNode
}

type PaneResizeDirection uint8

const (
	ResizePaneUp PaneResizeDirection = iota
	ResizePaneDown
	ResizePaneLeft
	ResizePaneRight
)

const (
	layoutPresetEvenHorizontal = iota
	layoutPresetEvenVertical
	layoutPresetMainHorizontal
	layoutPresetMainVertical
	layoutPresetTiled
	layoutPresetCount
	layoutPresetCustom = -1
)

const mainLayoutRatio uint16 = 600

func (p *PaneLayout) Compute(rect Rect) []PanePlacement {
	return []PanePlacement{{PaneID: p.PaneID, Rect: rect}}
}

func (p *PaneLayout) PaneIDs() []uint64 {
	return []uint64{p.PaneID}
}

func (s *SplitLayout) Compute(rect Rect) []PanePlacement {
	if s == nil || rect.Width <= 0 || rect.Height <= 0 {
		return nil
	}
	if s.Direction != SplitVertical && s.Direction != SplitHorizontal {
		return nil
	}
	firstRect, secondRect, ok := s.rects(rect)
	if !ok {
		return append(s.First.Compute(rect), s.Second.Compute(rect)...)
	}
	out := append(s.First.Compute(firstRect), s.Second.Compute(secondRect)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rect.Y != out[j].Rect.Y {
			return out[i].Rect.Y < out[j].Rect.Y
		}
		if out[i].Rect.X == out[j].Rect.X {
			return out[i].PaneID < out[j].PaneID
		}
		return out[i].Rect.X < out[j].Rect.X
	})
	return out
}

func (s *SplitLayout) rects(rect Rect) (Rect, Rect, bool) {
	axisSize := rect.Width
	if s.Direction == SplitHorizontal {
		axisSize = rect.Height
	}
	if axisSize <= 1 {
		return Rect{}, Rect{}, false
	}
	available := axisSize - 1
	firstSize := splitFirstSize(available, s.Ratio)
	secondSize := available - firstSize
	firstRect := rect
	secondRect := rect
	if s.Direction == SplitVertical {
		firstRect.Width = firstSize
		secondRect.X += firstSize + 1
		secondRect.Width = secondSize
	} else {
		firstRect.Height = firstSize
		secondRect.Y += firstSize + 1
		secondRect.Height = secondSize
	}
	return firstRect, secondRect, true
}

func splitFirstSize(available int, ratio uint16) int {
	value := int(ratio)
	if value <= 0 || value >= 1000 {
		value = 500
	}
	firstSize := (available * value) / 1000
	if firstSize < 1 {
		firstSize = 1
	}
	secondSize := available - firstSize
	if secondSize < 1 {
		secondSize = 1
		firstSize = available - secondSize
	}
	return firstSize
}

func (s *SplitLayout) PaneIDs() []uint64 {
	if s == nil {
		return nil
	}
	out := append([]uint64{}, s.First.PaneIDs()...)
	out = append(out, s.Second.PaneIDs()...)
	return out
}

func buildPresetLayout(paneIDs []uint64, focusedPaneID uint64, preset int) LayoutNode {
	if len(paneIDs) == 0 {
		return nil
	}
	if len(paneIDs) == 1 {
		return &PaneLayout{PaneID: paneIDs[0]}
	}

	switch preset {
	case layoutPresetEvenVertical:
		return balancedPaneLayout(paneIDs, SplitHorizontal)
	case layoutPresetMainHorizontal:
		main, rest := mainAndRestPaneIDs(paneIDs, focusedPaneID)
		return &SplitLayout{
			Direction: SplitHorizontal,
			Ratio:     mainLayoutRatio,
			First:     &PaneLayout{PaneID: main},
			Second:    balancedPaneLayout(rest, SplitVertical),
		}
	case layoutPresetMainVertical:
		main, rest := mainAndRestPaneIDs(paneIDs, focusedPaneID)
		return &SplitLayout{
			Direction: SplitVertical,
			Ratio:     mainLayoutRatio,
			First:     &PaneLayout{PaneID: main},
			Second:    balancedPaneLayout(rest, SplitHorizontal),
		}
	case layoutPresetTiled:
		return tiledPaneLayout(paneIDs)
	default:
		return balancedPaneLayout(paneIDs, SplitVertical)
	}
}

func balancedPaneLayout(paneIDs []uint64, direction SplitDirection) LayoutNode {
	nodes := make([]LayoutNode, len(paneIDs))
	for i, paneID := range paneIDs {
		nodes[i] = &PaneLayout{PaneID: paneID}
	}
	return balancedLayout(nodes, direction)
}

func balancedLayout(nodes []LayoutNode, direction SplitDirection) LayoutNode {
	if len(nodes) == 0 {
		return nil
	}
	if len(nodes) == 1 {
		return nodes[0]
	}
	middle := len(nodes) / 2
	return &SplitLayout{
		Direction: direction,
		Ratio:     uint16(middle * 1000 / len(nodes)),
		First:     balancedLayout(nodes[:middle], direction),
		Second:    balancedLayout(nodes[middle:], direction),
	}
}

func mainAndRestPaneIDs(paneIDs []uint64, focusedPaneID uint64) (uint64, []uint64) {
	main := paneIDs[0]
	mainIndex := 0
	for i, paneID := range paneIDs {
		if paneID == focusedPaneID {
			main = paneID
			mainIndex = i
			break
		}
	}
	ordered := make([]uint64, 0, len(paneIDs)-1)
	ordered = append(ordered, paneIDs[:mainIndex]...)
	ordered = append(ordered, paneIDs[mainIndex+1:]...)
	return main, ordered
}

func tiledPaneLayout(paneIDs []uint64) LayoutNode {
	columns := 1
	for columns*columns < len(paneIDs) {
		columns++
	}
	rows := make([]LayoutNode, 0, (len(paneIDs)+columns-1)/columns)
	for start := 0; start < len(paneIDs); start += columns {
		end := min(start+columns, len(paneIDs))
		rows = append(rows, balancedPaneLayout(paneIDs[start:end], SplitVertical))
	}
	return balancedLayout(rows, SplitHorizontal)
}

type resizeCandidate struct {
	split     *SplitLayout
	rect      Rect
	preferred bool
}

// ResizePaneBoundary moves the closest split boundary for paneID in the
// requested screen direction. A boundary on that side is preferred; when the
// pane is at the outside edge, the closest split on the same axis is moved
// instead, matching tmux resize-pane behavior.
func ResizePaneBoundary(layout LayoutNode, paneID uint64, direction PaneResizeDirection, amount int, rect Rect) bool {
	if layout == nil || direction > ResizePaneRight || amount <= 0 || !containsPane(layout.PaneIDs(), paneID) {
		return false
	}
	var candidates []resizeCandidate
	collectResizeCandidates(layout, paneID, direction, rect, &candidates)
	if len(candidates) == 0 {
		return false
	}
	target := candidates[0]
	for _, candidate := range candidates {
		if candidate.preferred {
			target = candidate
			break
		}
	}
	return resizeSplitBoundary(target.split, target.rect, direction, amount)
}

func collectResizeCandidates(layout LayoutNode, paneID uint64, direction PaneResizeDirection, rect Rect, out *[]resizeCandidate) {
	split, ok := layout.(*SplitLayout)
	if !ok || split == nil {
		return
	}
	firstRect, secondRect, valid := split.rects(rect)
	if !valid {
		return
	}
	inFirst := containsPane(split.First.PaneIDs(), paneID)
	if inFirst {
		collectResizeCandidates(split.First, paneID, direction, firstRect, out)
	} else if containsPane(split.Second.PaneIDs(), paneID) {
		collectResizeCandidates(split.Second, paneID, direction, secondRect, out)
	} else {
		return
	}
	vertical := direction == ResizePaneLeft || direction == ResizePaneRight
	if vertical != (split.Direction == SplitVertical) {
		return
	}
	preferred := (inFirst && (direction == ResizePaneRight || direction == ResizePaneDown)) ||
		(!inFirst && (direction == ResizePaneLeft || direction == ResizePaneUp))
	*out = append(*out, resizeCandidate{split: split, rect: rect, preferred: preferred})
}

func resizeSplitBoundary(split *SplitLayout, rect Rect, direction PaneResizeDirection, amount int) bool {
	firstRect, _, ok := split.rects(rect)
	if !ok {
		return false
	}
	available := rect.Width - 1
	current := firstRect.Width
	minimumFirst := layoutMinimumSize(split.First, SplitVertical)
	minimumSecond := layoutMinimumSize(split.Second, SplitVertical)
	if split.Direction == SplitHorizontal {
		available = rect.Height - 1
		current = firstRect.Height
		minimumFirst = layoutMinimumSize(split.First, SplitHorizontal)
		minimumSecond = layoutMinimumSize(split.Second, SplitHorizontal)
	}
	minimum, maximum := minimumFirst, available-minimumSecond
	if minimum > maximum {
		return false
	}
	desired := current + amount
	if direction == ResizePaneLeft || direction == ResizePaneUp {
		desired = current - amount
	}
	if desired < minimum {
		desired = minimum
	}
	if desired > maximum {
		desired = maximum
	}
	if desired == current {
		return false
	}

	bestRatio, bestSize, bestDistance := uint16(0), current, int(^uint(0)>>1)
	for ratio := 1; ratio < 1000; ratio++ {
		size := splitFirstSize(available, uint16(ratio))
		if size < minimum || size > maximum {
			continue
		}
		if desired < current && size >= current || desired > current && size <= current {
			continue
		}
		distance := size - desired
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			bestRatio, bestSize, bestDistance = uint16(ratio), size, distance
		}
	}
	if bestRatio == 0 || bestSize == current {
		return false
	}
	split.Ratio = bestRatio
	return true
}

func layoutMinimumSize(layout LayoutNode, axis SplitDirection) int {
	switch node := layout.(type) {
	case *PaneLayout:
		return 1
	case *SplitLayout:
		first := layoutMinimumSize(node.First, axis)
		second := layoutMinimumSize(node.Second, axis)
		if node.Direction == axis {
			return first + 1 + second
		}
		if second > first {
			return second
		}
		return first
	default:
		return 1
	}
}
