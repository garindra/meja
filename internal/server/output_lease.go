package server

import (
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/garindra/meja/internal/protocol"
)

type confirmerMessage struct {
	publication *viewPublicationBuffer
	sync        chan error
}

type paneConfirmer struct {
	frame       []byte
	styleIDs    map[protocol.Style]uint32
	nextStyleID uint32
	textScratch []byte
	epoch       RenderEpoch
	version     RenderVersion
	hasBase     bool
}

func newPaneConfirmer() *paneConfirmer {
	return &paneConfirmer{
		frame:    make([]byte, 0, initialReliableFrameCap),
		styleIDs: make(map[protocol.Style]uint32, maxTerminalStyles),
	}
}

func (c *paneConfirmer) append(command protocol.DisplayCommand) error {
	encoder := protocol.NewDisplayEncoder(c.frame)
	if err := encoder.AppendCommand(command); err != nil {
		return err
	}
	c.frame = encoder.Bytes()
	return nil
}

func (c *paneConfirmer) resetWireStyles() {
	clear(c.styleIDs)
	c.styleIDs[protocol.CanonicalDefaultStyle()] = protocol.CanonicalDefaultStyleID
	c.nextStyleID = 1
}

func (c *paneConfirmer) ensureStyle(style protocol.Style) (uint32, error) {
	if id, ok := c.styleIDs[style]; ok {
		return id, nil
	}
	if c.nextStyleID >= maxTerminalStyles {
		return 0, fmt.Errorf("wire style capacity exceeded")
	}
	id := c.nextStyleID
	c.nextStyleID++
	if err := c.append(protocol.DisplayCommand{
		Opcode:  protocol.DisplayOpcodeStyleInstall,
		StyleID: id,
		Style:   style,
	}); err != nil {
		return 0, err
	}
	c.styleIDs[style] = id
	return id, nil
}

type frameCompiler struct {
	confirmer   *paneConfirmer
	publication *viewPublication
	cols        int

	positionValid bool
	row, column   int
	styleValid    bool
	styleID       uint32

	pendingOpcode protocol.DisplayOpcode
	pendingWidth  uint8
	pendingStyle  uint32
	pendingSource protocol.Style
	pendingFill   protocol.Fill
}

func newFrameCompiler(confirmer *paneConfirmer, publication *viewPublication) frameCompiler {
	confirmer.textScratch = confirmer.textScratch[:0]
	return frameCompiler{confirmer: confirmer, publication: publication, cols: int(publication.Cols)}
}

func (c *frameCompiler) finishPending() error {
	opcode := c.pendingOpcode
	if opcode == 0 {
		return nil
	}
	c.pendingOpcode = 0
	if opcode == protocol.DisplayOpcodeFill {
		fill := c.pendingFill
		c.pendingFill = protocol.Fill{}
		return c.confirmer.append(protocol.DisplayCommand{Opcode: opcode, Fill: fill})
	}
	text := c.confirmer.textScratch
	err := c.confirmer.append(protocol.DisplayCommand{Opcode: opcode, Width: c.pendingWidth, Text: text})
	c.confirmer.textScratch = c.confirmer.textScratch[:0]
	return err
}

func (c *frameCompiler) moveTo(row, column int) error {
	if c.positionValid && c.row == row && c.column == column {
		return nil
	}
	if err := c.finishPending(); err != nil {
		return err
	}
	if err := c.confirmer.append(protocol.DisplayCommand{
		Opcode: protocol.DisplayOpcodeSetWritePosition,
		Row:    row,
		Column: column,
	}); err != nil {
		return err
	}
	c.positionValid = true
	c.row, c.column = row, column
	return nil
}

func (c *frameCompiler) advance(columns int) {
	c.column += columns
	if c.column == c.cols {
		c.row++
		c.column = 0
	}
}

func (c *frameCompiler) selectStyle(styleID uint32) error {
	if c.styleValid && c.styleID == styleID {
		return nil
	}
	if err := c.finishPending(); err != nil {
		return err
	}
	if err := c.confirmer.append(protocol.DisplayCommand{
		Opcode:  protocol.DisplayOpcodeSetWriteStyle,
		StyleID: styleID,
	}); err != nil {
		return err
	}
	c.styleValid = true
	c.styleID = styleID
	return nil
}

func (c *frameCompiler) ensureStyle(style protocol.Style) (uint32, error) {
	if id, ok := c.confirmer.styleIDs[style]; ok {
		return id, nil
	}
	if err := c.finishPending(); err != nil {
		return 0, err
	}
	return c.confirmer.ensureStyle(style)
}

func compilerOpcode(width uint8, styleID uint32, styleValid bool, selectedStyle uint32) protocol.DisplayOpcode {
	if width != 1 {
		return protocol.DisplayOpcodeWriteText
	}
	if styleID == protocol.CanonicalDefaultStyleID && (!styleValid || selectedStyle != styleID) {
		return protocol.DisplayOpcodeWriteTextUTF8Default
	}
	return protocol.DisplayOpcodeWriteTextUTF8
}

func (c *frameCompiler) queueRune(r rune, width uint8, style protocol.Style, styleID uint32) error {
	opcode := compilerOpcode(width, styleID, c.styleValid, c.styleID)
	if c.pendingOpcode != opcode || c.pendingWidth != width || c.pendingStyle != styleID {
		if err := c.finishPending(); err != nil {
			return err
		}
		if opcode != protocol.DisplayOpcodeWriteTextUTF8Default {
			if err := c.selectStyle(styleID); err != nil {
				return err
			}
		}
		c.pendingOpcode = opcode
		c.pendingWidth = width
		c.pendingStyle = styleID
		c.pendingSource = style
	}
	c.confirmer.textScratch = utf8.AppendRune(c.confirmer.textScratch, r)
	c.advance(int(width))
	return nil
}

func uvarintSize(value uint64) int {
	size := 1
	for value >= 0x80 {
		value >>= 7
		size++
	}
	return size
}

func textCommandSize(opcode protocol.DisplayOpcode, payloadBytes int) int {
	size := 1 + uvarintSize(uint64(payloadBytes)) + payloadBytes
	if opcode == protocol.DisplayOpcodeWriteText {
		size++
	}
	return size
}

func fillCommandSize(fill protocol.Fill) int {
	return 2 + uvarintSize(uint64(fill.Columns)) + uvarintSize(uint64(fill.Rune))
}

func (c *frameCompiler) styleSelectionSize(styleID uint32) int {
	if c.styleValid && c.styleID == styleID {
		return 0
	}
	return 1 + uvarintSize(uint64(styleID))
}

func (c *frameCompiler) fillIsSmaller(r rune, count int, styleID uint32) bool {
	fill := protocol.Fill{Columns: count, Rune: r, Width: 1}
	if c.pendingOpcode == protocol.DisplayOpcodeFill && c.pendingStyle == styleID &&
		c.pendingFill.Rune == r && c.pendingFill.Width == 1 {
		return true
	}
	opcode := compilerOpcode(1, styleID, c.styleValid, c.styleID)
	payloadBytes := utf8.RuneLen(r) * count
	textSize := textCommandSize(opcode, payloadBytes)
	if c.pendingOpcode == opcode && c.pendingStyle == styleID {
		oldBytes := len(c.confirmer.textScratch)
		textSize = payloadBytes + uvarintSize(uint64(oldBytes+payloadBytes)) - uvarintSize(uint64(oldBytes))
	} else if opcode != protocol.DisplayOpcodeWriteTextUTF8Default {
		textSize += c.styleSelectionSize(styleID)
	}
	return fillCommandSize(fill)+c.styleSelectionSize(styleID) < textSize
}

func (c *frameCompiler) queueFill(r rune, count int, styleID uint32) error {
	if c.pendingOpcode == protocol.DisplayOpcodeFill && c.pendingStyle == styleID &&
		c.pendingFill.Rune == r && c.pendingFill.Width == 1 {
		c.pendingFill.Columns += count
		c.advance(count)
		return nil
	}
	if err := c.finishPending(); err != nil {
		return err
	}
	if err := c.selectStyle(styleID); err != nil {
		return err
	}
	c.pendingOpcode = protocol.DisplayOpcodeFill
	c.pendingStyle = styleID
	c.pendingFill = protocol.Fill{Columns: count, Rune: r, Width: 1}
	c.advance(count)
	return nil
}

func effectiveBlankBackground(style protocol.Style) protocol.Color {
	if style.Reverse {
		return style.FG
	}
	return style.BG
}

func normalizedColor(color protocol.Color) protocol.Color {
	if color.Mode == "" {
		color.Mode = "default"
	}
	return color
}

func blankStylesEquivalent(left, right protocol.Style) bool {
	if left == right {
		return true
	}
	if left.Underline || right.Underline {
		return false
	}
	return normalizedColor(effectiveBlankBackground(left)) == normalizedColor(effectiveBlankBackground(right))
}

func (c *frameCompiler) blankBridgeEnd(cells []semanticCell, start int) (int, bool) {
	if (c.pendingOpcode != protocol.DisplayOpcodeWriteText &&
		c.pendingOpcode != protocol.DisplayOpcodeWriteTextUTF8 &&
		c.pendingOpcode != protocol.DisplayOpcodeWriteTextUTF8Default) || c.pendingWidth != 1 {
		return start, false
	}
	end := start
	for end < len(cells) {
		cell := cells[end]
		if cell.kind != semanticBlank || cell.width != 1 {
			break
		}
		style, ok := semanticStyleAt(c.publication.Styles, cell.style)
		if !ok || !blankStylesEquivalent(style, c.pendingSource) {
			break
		}
		end++
	}
	if end == start || end >= len(cells) {
		return start, false
	}
	next := cells[end]
	style, ok := semanticStyleAt(c.publication.Styles, next.style)
	return end, ok && style == c.pendingSource && next.kind == semanticScalar && next.width == 1
}

func (c *frameCompiler) compileRun(run publishedRun) error {
	publication := c.publication
	start := int(run.CellStart)
	end := start + int(run.Columns)
	if start < 0 || end > len(publication.Cells) {
		return fmt.Errorf("publication run cell range is invalid")
	}
	for offset := 0; start+offset < end; {
		cell := publication.Cells[start+offset]
		if cell.kind == semanticContinuation {
			offset++
			continue
		}
		if err := c.moveTo(int(run.Row), int(run.Column)+offset); err != nil {
			return err
		}
		style, ok := semanticStyleAt(publication.Styles, cell.style)
		if !ok {
			return fmt.Errorf("publication style %d is unavailable", cell.style)
		}
		if bridgeEnd, bridge := c.blankBridgeEnd(publication.Cells[start:end], offset); bridge {
			for offset < bridgeEnd {
				if err := c.queueRune(' ', 1, c.pendingSource, c.pendingStyle); err != nil {
					return err
				}
				offset++
			}
			continue
		}
		styleID, err := c.ensureStyle(style)
		if err != nil {
			return err
		}
		if cell.kind == semanticCluster {
			text, _, err := semanticCellText(cell, publication.Clusters)
			if err != nil {
				return err
			}
			if err := c.finishPending(); err != nil {
				return err
			}
			if err := c.selectStyle(styleID); err != nil {
				return err
			}
			if err := c.confirmer.append(protocol.DisplayCommand{
				Opcode: protocol.DisplayOpcodeWriteCluster,
				Width:  cell.width,
				Text:   text,
			}); err != nil {
				return err
			}
			columns := int(max(uint8(1), cell.width))
			c.advance(columns)
			offset += columns
			continue
		}
		width := cell.width
		styleIndex := cell.style
		for start+offset < end {
			current := publication.Cells[start+offset]
			if current.kind == semanticCluster || current.kind == semanticContinuation || current.width != width {
				break
			}
			if current.style != styleIndex {
				if bridgeEnd, bridge := c.blankBridgeEnd(publication.Cells[start:end], offset); bridge {
					for offset < bridgeEnd {
						if err := c.queueRune(' ', 1, style, styleID); err != nil {
							return err
						}
						offset++
					}
					continue
				}
				break
			}
			var r rune
			if current.kind == semanticBlank {
				r = ' '
			} else {
				r = rune(current.payload)
			}
			if width == 1 {
				count := 1
				pendingFill := c.pendingOpcode == protocol.DisplayOpcodeFill && c.pendingStyle == styleID &&
					c.pendingFill.Rune == r && c.pendingFill.Width == 1
				if pendingFill {
					for start+offset+count < end && publication.Cells[start+offset+count] == current {
						count++
					}
				} else if start+offset+2 < end && publication.Cells[start+offset+1] == current && publication.Cells[start+offset+2] == current {
					count = 3
					for start+offset+count < end && publication.Cells[start+offset+count] == current {
						count++
					}
				}
				if (pendingFill || count > 1) && c.fillIsSmaller(r, count, styleID) {
					if err := c.queueFill(r, count, styleID); err != nil {
						return err
					}
					offset += count
					continue
				}
			}
			if err := c.queueRune(r, width, style, styleID); err != nil {
				return err
			}
			offset += int(width)
		}
	}
	return nil
}

func (c *paneConfirmer) compile(publication *viewPublication) ([]byte, error) {
	if publication == nil || publication.TargetVersion == 0 {
		return nil, fmt.Errorf("publication has an invalid target version")
	}
	switch publication.Kind {
	case PublicationKeyframe:
		if publication.BaseVersion != 0 {
			return nil, fmt.Errorf("keyframe base version is %d, want 0", publication.BaseVersion)
		}
		newBase := !c.hasBase || publication.Barrier || publication.Epoch != c.epoch
		if newBase && publication.TargetVersion != 1 {
			return nil, fmt.Errorf("epoch keyframe target version is %d, want 1", publication.TargetVersion)
		}
		if !newBase && publication.TargetVersion != c.version+1 {
			return nil, fmt.Errorf("keyframe target version is %d, want %d", publication.TargetVersion, c.version+1)
		}
	case PublicationDelta:
		if publication.Barrier || !c.hasBase || publication.Epoch != c.epoch || publication.BaseVersion != c.version ||
			publication.TargetVersion != publication.BaseVersion+1 {
			return nil, fmt.Errorf("delta version chain %d/%d -> %d does not follow %d/%d",
				publication.Epoch, publication.BaseVersion, publication.TargetVersion, c.epoch, c.version)
		}
	default:
		return nil, fmt.Errorf("publication kind %d is invalid", publication.Kind)
	}
	c.frame = c.frame[:0]
	if publication.Barrier {
		c.resetWireStyles()
		if err := c.append(protocol.DisplayCommand{
			Opcode:         protocol.DisplayOpcodeStartRender,
			LayoutRevision: publication.LayoutRevision,
			GridCols:       int(publication.Cols),
			GridRows:       int(publication.Rows),
		}); err != nil {
			return nil, err
		}
		if err := c.append(protocol.DisplayCommand{
			Opcode:  protocol.DisplayOpcodeStyleInstall,
			StyleID: protocol.CanonicalDefaultStyleID,
			Style:   protocol.CanonicalDefaultStyle(),
		}); err != nil {
			return nil, err
		}
	} else if len(c.styleIDs) == 0 {
		return nil, fmt.Errorf("publication has no START_RENDER wire barrier")
	}
	if publication.HasScroll {
		if err := c.append(protocol.DisplayCommand{
			Opcode: protocol.DisplayOpcodeScrollRegion,
			ScrollRegion: protocol.ScrollRegion{
				Top: int(publication.Scroll.Top), Bottom: int(publication.Scroll.Bottom), Delta: int(publication.Scroll.Delta),
			},
		}); err != nil {
			return nil, err
		}
	}
	compiler := newFrameCompiler(c, publication)
	for _, run := range publication.Runs {
		if err := compiler.compileRun(run); err != nil {
			return nil, err
		}
	}
	if err := compiler.finishPending(); err != nil {
		return nil, err
	}
	if publication.CursorChanged {
		if err := c.append(protocol.DisplayCommand{
			Opcode: protocol.DisplayOpcodeCursorUpdate,
			Cursor: protocol.CursorUpdate{
				Cursor:  protocol.Cursor{X: int(publication.Cursor.X), Y: int(publication.Cursor.Y)},
				Visible: publication.Cursor.Visible,
			},
		}); err != nil {
			return nil, err
		}
	}
	if err := c.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
		return nil, err
	}
	c.epoch = publication.Epoch
	c.version = publication.TargetVersion
	c.hasBase = true
	return c.frame, nil
}

func (l *OutputLease) startWorker() {
	if l == nil {
		return
	}
	l.workerOnce.Do(func() {
		l.ready = make(chan confirmerMessage, 1)
		l.failed = make(chan error, 1)
		l.workerDone = make(chan struct{})
		go l.runConfirmer()
	})
}

func (l *OutputLease) submissions() chan<- confirmerMessage {
	l.startWorker()
	return l.ready
}

func (l *OutputLease) runConfirmer() {
	defer func() {
		for {
			select {
			case abandoned := <-l.ready:
				if abandoned.publication != nil {
					abandoned.publication.release()
				}
				if abandoned.sync != nil {
					abandoned.sync <- io.ErrClosedPipe
				}
			default:
				close(l.workerDone)
				return
			}
		}
	}()
	confirmer := newPaneConfirmer()
	var terminalError error
	for {
		select {
		case message := <-l.ready:
			if terminalError != nil {
				if message.publication != nil {
					message.publication.release()
				}
				if message.sync != nil {
					message.sync <- terminalError
				}
				continue
			}
			if message.sync != nil {
				message.sync <- nil
				continue
			}
			buffer := message.publication
			if buffer == nil {
				continue
			}
			metrics := buffer.metrics
			fromPTYDrain := buffer.fromPTYDrain
			frame, err := confirmer.compile(&buffer.publication)
			buffer.release()
			if err == nil && metrics != nil {
				metrics.recordCompiledFrame(len(frame), fromPTYDrain)
			}
			if err == nil {
				started := time.Now()
				err = l.writeFrame(frame, metrics)
				if metrics != nil {
					metrics.confirmerWriteBlockedNanos.Add(uint64(time.Since(started)))
				}
			}
			if err != nil {
				terminalError = fmt.Errorf("write pane output slot %d: %w", l.Slot, err)
				l.reportFailure(terminalError)
			}
		case <-l.done:
			return
		}
	}
}

func (l *OutputLease) writeFrame(frame []byte, metrics *paneRenderMetrics) error {
	if deadlineWriter, ok := l.Stream.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = deadlineWriter.SetWriteDeadline(time.Now().Add(quicMaxIdleTimeout))
		defer deadlineWriter.SetWriteDeadline(time.Time{})
	}
	writes, err := writeAll(l.Stream, frame)
	if metrics != nil {
		metrics.physicalWrites.Add(writes)
	}
	return err
}

func (l *OutputLease) failures() <-chan error {
	if l == nil {
		return nil
	}
	l.startWorker()
	return l.failed
}

func (l *OutputLease) sync() error {
	if l == nil {
		return nil
	}
	done := make(chan error, 1)
	message := confirmerMessage{sync: done}
	select {
	case l.submissions() <- message:
	case <-l.done:
		return io.ErrClosedPipe
	case <-l.workerDone:
		return io.ErrClosedPipe
	}
	select {
	case err := <-done:
		return err
	case <-l.done:
		return io.ErrClosedPipe
	case <-l.workerDone:
		return io.ErrClosedPipe
	}
}

func (l *OutputLease) reportFailure(err error) {
	if l == nil || err == nil {
		return
	}
	select {
	case l.failed <- err:
	default:
	}
	if l.onFailure != nil {
		l.onFailure(err)
	}
}
