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
	axisSize := rect.Width
	if s.Direction == SplitHorizontal {
		axisSize = rect.Height
	}
	if axisSize <= 1 {
		return append(s.First.Compute(rect), s.Second.Compute(rect)...)
	}
	ratio := int(s.Ratio)
	if ratio <= 0 || ratio >= 1000 {
		ratio = 500
	}
	available := axisSize - 1
	firstSize := (available * ratio) / 1000
	if firstSize < 1 {
		firstSize = 1
	}
	secondSize := available - firstSize
	if secondSize < 1 {
		secondSize = 1
		firstSize = available - secondSize
	}
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

func (s *SplitLayout) PaneIDs() []uint64 {
	if s == nil {
		return nil
	}
	out := append([]uint64{}, s.First.PaneIDs()...)
	out = append(out, s.Second.PaneIDs()...)
	return out
}
