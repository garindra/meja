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

func (c *paneConfirmer) appendPosition(row, column int) error {
	return c.append(protocol.DisplayCommand{
		Opcode: protocol.DisplayOpcodeSetWritePosition,
		Row:    row,
		Column: column,
	})
}

func (c *paneConfirmer) compileRun(publication *viewPublication, run publishedRun) error {
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
		style, ok := semanticStyleAt(publication.Styles, cell.style)
		if !ok {
			return fmt.Errorf("publication style %d is unavailable", cell.style)
		}
		styleID, err := c.ensureStyle(style)
		if err != nil {
			return err
		}
		if err := c.appendPosition(int(run.Row), int(run.Column)+offset); err != nil {
			return err
		}
		if cell.kind == semanticCluster {
			text, _, err := semanticCellText(cell, publication.Clusters)
			if err != nil {
				return err
			}
			if err := c.append(protocol.DisplayCommand{
				Opcode:  protocol.DisplayOpcodeSetWriteStyle,
				StyleID: styleID,
			}); err != nil {
				return err
			}
			if err := c.append(protocol.DisplayCommand{
				Opcode: protocol.DisplayOpcodeWriteCluster,
				Width:  cell.width,
				Text:   text,
			}); err != nil {
				return err
			}
			offset += int(max(uint8(1), cell.width))
			continue
		}
		width := cell.width
		if width == 0 {
			width = 1
		}
		c.textScratch = c.textScratch[:0]
		groupStart := offset
		for start+offset < end {
			current := publication.Cells[start+offset]
			if current.kind == semanticContinuation {
				offset++
				continue
			}
			currentStyle, ok := semanticStyleAt(publication.Styles, current.style)
			if !ok || currentStyle != style || current.kind == semanticCluster || current.width != width {
				break
			}
			_, r, err := semanticCellText(current, publication.Clusters)
			if err != nil {
				return err
			}
			c.textScratch = utf8.AppendRune(c.textScratch, r)
			offset += int(max(uint8(1), current.width))
		}
		if offset == groupStart {
			return fmt.Errorf("publication compiler made no progress")
		}
		opcode := protocol.DisplayOpcodeWriteTextUTF8
		if width == 1 && styleID == protocol.CanonicalDefaultStyleID {
			opcode = protocol.DisplayOpcodeWriteTextUTF8Default
		} else if width != 1 {
			opcode = protocol.DisplayOpcodeWriteText
		}
		if opcode != protocol.DisplayOpcodeWriteTextUTF8Default {
			if err := c.append(protocol.DisplayCommand{
				Opcode:  protocol.DisplayOpcodeSetWriteStyle,
				StyleID: styleID,
			}); err != nil {
				return err
			}
		}
		if err := c.append(protocol.DisplayCommand{
			Opcode: opcode,
			Width:  width,
			Text:   c.textScratch,
		}); err != nil {
			return err
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
	for _, run := range publication.Runs {
		if err := c.compileRun(publication, run); err != nil {
			return nil, err
		}
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
	for len(frame) > 0 {
		chunk := frame[:min(len(frame), renderStreamChunkSize)]
		for len(chunk) > 0 {
			n, err := l.Stream.Write(chunk)
			if metrics != nil {
				metrics.physicalWrites.Add(1)
			}
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			chunk = chunk[n:]
			frame = frame[n:]
		}
	}
	return nil
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
