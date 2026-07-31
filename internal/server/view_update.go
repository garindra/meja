package server

// ViewUpdate describes only the visual consequences of a state change.
// Terminal-parser side effects such as replies and frontend writes do not cross
// this renderer boundary.
type ViewUpdate struct {
	DirtySpans    []DirtySpan
	ScrollRegion  *ScrollRegion
	FullRedraw    bool
	CursorChanged bool
}

func (u *ViewUpdate) reset(rows int) {
	if rows < 0 {
		rows = 0
	}
	if cap(u.DirtySpans) < rows {
		u.DirtySpans = make([]DirtySpan, rows)
	} else {
		u.DirtySpans = u.DirtySpans[:rows]
		clear(u.DirtySpans)
	}
	u.ScrollRegion = nil
	u.FullRedraw = false
	u.CursorChanged = false
}

func (u *ViewUpdate) markDirty(row, start, end, cols int) {
	if u == nil || u.FullRedraw || row < 0 || row >= len(u.DirtySpans) || start >= end || cols <= 0 {
		return
	}
	if start < 0 {
		start = 0
	}
	if end > cols {
		end = cols
	}
	if start >= end {
		return
	}
	span := u.DirtySpans[row]
	if span.End == 0 {
		u.DirtySpans[row] = DirtySpan{Start: start, End: end}
		return
	}
	if start < span.Start {
		span.Start = start
	}
	if end > span.End {
		span.End = end
	}
	u.DirtySpans[row] = span
}

func (u *ViewUpdate) recordScrollRegion(top, bottom, delta int) {
	if u == nil {
		return
	}
	if top < 0 || top >= bottom || bottom > len(u.DirtySpans) || delta == 0 || delta < -(bottom-top) || delta > bottom-top {
		u.forceFullRedraw()
		return
	}
	if u.FullRedraw {
		return
	}
	if pending := u.ScrollRegion; pending != nil &&
		(pending.Top != top || pending.Bottom != bottom || (pending.Delta < 0) != (delta < 0)) {
		u.forceFullRedraw()
		return
	}
	spans := u.DirtySpans[top:bottom]
	if delta < 0 {
		shift := -delta
		copy(spans[:len(spans)-shift], spans[shift:])
		clear(spans[len(spans)-shift:])
	} else {
		shift := delta
		copy(spans[shift:], spans[:len(spans)-shift])
		clear(spans[:shift])
	}
	if u.ScrollRegion == nil {
		u.ScrollRegion = &ScrollRegion{Top: top, Bottom: bottom, Delta: delta}
		return
	}
	u.ScrollRegion.Delta += delta
	height := bottom - top
	if u.ScrollRegion.Delta < -height {
		u.ScrollRegion.Delta = -height
	} else if u.ScrollRegion.Delta > height {
		u.ScrollRegion.Delta = height
	}
}

func (u *ViewUpdate) forceFullRedraw() {
	u.FullRedraw = true
	u.ScrollRegion = nil
	clear(u.DirtySpans)
}

func (u ViewUpdate) HasDamage() bool {
	for _, span := range u.DirtySpans {
		if span.End != 0 {
			return true
		}
	}
	return false
}

func (u ViewUpdate) HasRenderChange() bool {
	return u.FullRedraw || u.ScrollRegion != nil || u.CursorChanged || u.HasDamage()
}
