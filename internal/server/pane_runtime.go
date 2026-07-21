package server

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/garindra/meja/internal/protocol"
)

var ptyReadBuffers = sync.Pool{New: func() any { return make([]byte, 32*1024) }}

const (
	renderIdleFlush   = time.Millisecond
	renderMaxBatchAge = 16 * time.Millisecond
	// Finalized commands are streamed at this size while PRESENT remains the
	// client's atomic frame boundary.
	renderStreamChunkSize    = 8 << 10
	startupInputIdle         = 25 * time.Millisecond
	startupInputMaxWait      = 500 * time.Millisecond
	maxRetainedRenderBuffer  = 64 << 10
	paneOutputBytesPerSecond = 8 << 20
	paneOutputBurstBytes     = 1 << 20
)

var errRenderBufferFull = errors.New("pane render buffer is full")

type paneOutputRateLimiter struct {
	tokens float64
	last   time.Time
}

func newPaneOutputRateLimiter(now time.Time) paneOutputRateLimiter {
	return paneOutputRateLimiter{tokens: paneOutputBurstBytes, last: now}
}

func (l *paneOutputRateLimiter) reserve(now time.Time, bytes int) time.Duration {
	if bytes <= 0 {
		return 0
	}
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens = min(float64(paneOutputBurstBytes), l.tokens+elapsed.Seconds()*paneOutputBytesPerSecond)
	}
	l.last = now
	l.tokens -= float64(bytes)
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / paneOutputBytesPerSecond * float64(time.Second))
}

func (p *Pane) attachOutputStream(lease *OutputLease, layoutRevision uint64) error {
	attachment := &paneOutputAttach{Lease: lease, LayoutRevision: layoutRevision}
	if p.commands == nil {
		return p.renderAttachedView(newRenderOutput(lease.Stream), layoutRevision)
	}
	select {
	case p.commands <- paneCommand{attach: attachment}:
		return nil
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
}

func (p *Pane) renderAttachedView(output *renderOutput, layoutRevision uint64) error {
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeStartRender, LayoutRevision: layoutRevision, GridCols: p.terminal.Cols, GridRows: p.terminal.Rows}); err != nil {
		return err
	}
	if err := installStyle(output, protocol.CanonicalDefaultStyleID, protocol.CanonicalDefaultStyle()); err != nil {
		return err
	}
	switch p.currentViewMode() {
	case paneViewLive:
		return sendFullRender(output, p)
	case paneViewHistory:
		return fmt.Errorf("pane %d history output requires its actor", p.ID)
	default:
		return fmt.Errorf("pane %d has invalid view mode %d", p.ID, p.currentViewMode())
	}
}

// attachOutputWithRefresh is the low-level renderer hook used by pane tests.
// Session layout code attaches through attachOutputStream instead.
func (p *Pane) attachOutputWithRefresh(lease *OutputLease, refresh func(*renderOutput) error) error {
	attachment := &paneOutputAttach{Lease: lease, Refresh: refresh}
	if p.commands == nil {
		if refresh == nil {
			return nil
		}
		return refresh(newRenderOutput(lease.Stream))
	}
	select {
	case p.commands <- paneCommand{attach: attachment}:
		return nil
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
}

func (p *Pane) detachOutputStream(stream io.Writer) error {
	if p.commands == nil {
		return nil
	}
	return p.sendRenderCommand(paneCommand{detach: stream})
}

func (p *Pane) releaseOutputStream(done chan<- *OutputLease) {
	if p.commands == nil {
		done <- p.outputLease
		p.outputLease = nil
		return
	}
	release := &paneOutputRelease{done: done, acked: make(chan struct{})}
	select {
	case p.commands <- paneCommand{release: release}:
		go func() {
			select {
			case <-p.mainDone:
				release.acknowledge()
			case <-release.acked:
			}
		}()
	case <-p.mainDone:
		release.acknowledge()
	case <-p.done:
		release.acknowledge()
	}
}

func (p *Pane) applyRender(render func(*renderOutput) error) error {
	if p.commands == nil {
		return nil
	}
	return p.sendRenderCommand(paneCommand{apply: render})
}

func (p *Pane) sendRenderCommand(command paneCommand) error {
	done := make(chan error, 1)
	command.done = done
	select {
	case p.commands <- command:
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
	select {
	case err := <-done:
		return err
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
}

func relayPTYOutput(pane *Pane) {
	defer close(pane.ptyOutput)
	limiter := newPaneOutputRateLimiter(time.Now())
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		buf := ptyReadBuffers.Get().([]byte)
		n, err := pane.PTY.Read(buf)
		if n > 0 {
			pane.notifyProcessActivity()
			if delay := limiter.reserve(time.Now(), n); delay > 0 {
				if timer == nil {
					timer = time.NewTimer(delay)
				} else {
					timer.Reset(delay)
				}
				select {
				case <-timer.C:
				case <-pane.done:
					ptyReadBuffers.Put(buf[:cap(buf)])
					return
				}
			}
			select {
			case pane.ptyOutput <- buf[:n]:
			case <-pane.done:
				ptyReadBuffers.Put(buf[:cap(buf)])
				return
			}
		} else {
			ptyReadBuffers.Put(buf[:cap(buf)])
		}
		if err != nil {
			return
		}
	}
}

func runPTYWriter(pane *Pane, failed func(error)) {
	defer close(pane.writerDone)
	for {
		select {
		case data := <-pane.ptyInput:
			if err := writeAll(pane.PTY, data); err != nil {
				failed(err)
				return
			}
		case <-pane.done:
			return
		}
	}
}

func (pane *Pane) run() {
	defer func() {
		if pane.PTY != nil {
			_ = pane.PTY.Close()
		}
		close(pane.mainDone)
	}()
	renderer := newPaneRenderState(pane)
	var update Update
	var idle, maxAge *time.Timer
	var idleC, maxC <-chan time.Time
	var startupIdle *time.Timer
	var startupIdleC <-chan time.Time
	startupInput := pane.startupInput
	pane.startupInput = nil
	var startupMax *time.Timer
	var startupMaxC <-chan time.Time
	if len(startupInput) > 0 {
		startupMax = time.NewTimer(startupInputMaxWait)
		startupMaxC = startupMax.C
	}
	batching := false
	stop := func(timer *time.Timer) {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer func() {
		stop(idle)
		stop(maxAge)
		stop(startupIdle)
		stop(startupMax)
	}()
	arm := func(timer **time.Timer, channel *<-chan time.Time, duration time.Duration) {
		if *timer == nil {
			*timer = time.NewTimer(duration)
		} else {
			stop(*timer)
			(*timer).Reset(duration)
		}
		*channel = (*timer).C
	}
	disarm := func(timer *time.Timer, channel *<-chan time.Time) {
		stop(timer)
		*channel = nil
	}
	flushStartupInput := func() error {
		if len(startupInput) == 0 {
			return nil
		}
		disarm(startupIdle, &startupIdleC)
		disarm(startupMax, &startupMaxC)
		input := startupInput
		startupInput = nil
		return pane.sendOwnedInput(input)
	}
	flush := func() {
		if !batching {
			return
		}
		disarm(idle, &idleC)
		disarm(maxAge, &maxC)
		batching = false
		renderer.due = renderer.hasWork()
	}
	for {
		available := renderer.available()
		failures := renderer.failures()
		select {
		case buffer := <-available:
			lease := pane.outputLease
			if err := renderer.render(buffer); err != nil {
				if lease != nil {
					lease.recycle(buffer)
					lease.reportFailure(fmt.Errorf("render pane %d: %w", pane.ID, err))
				}
				pane.outputLease = nil
				renderer.detach()
			}
		case <-failures:
			pane.outputLease = nil
			renderer.detach()
		case command := <-pane.commands:
			if command.capture != nil {
				data, err := captureTerminalViewport(pane.terminal, command.capture.Options)
				command.capture.Result <- paneCaptureResult{Data: data, Err: err}
				continue
			}
			if command.release != nil {
				lease := pane.outputLease
				pane.outputLease = nil
				renderer.detach()
				command.release.returnLease(lease)
				continue
			}
			if command.detach != nil {
				if pane.outputLease != nil && pane.outputLease.Stream == command.detach {
					pane.outputLease = nil
					renderer.detach()
				}
				command.done <- nil
				continue
			}
			if command.attach != nil {
				pane.outputLease = command.attach.Lease
				renderer.attach(command.attach.Lease, command.attach.LayoutRevision, command.attach.Refresh)
				continue
			}
			if command.history != nil {
				result := pane.handleHistoryRequest(command.history)
				// History handlers historically rendered as a side effect even when
				// their Changed result only described mode transitions. Repaint from
				// the authoritative current view for every successful request.
				if result.Err == nil && pane.outputLease != nil {
					renderer.markFull()
					renderer.due = true
				}
				command.history.Result <- result
				continue
			}
			if command.apply != nil && pane.outputLease != nil {
				renderer.queued = append(renderer.queued, queuedPaneRender{render: command.apply, done: command.done})
				renderer.due = true
			} else if command.resize != nil {
				err := error(nil)
				if pane.PTY != nil {
					err = pty.Setsize(pane.PTY, &pty.Winsize{Cols: command.resize.cols, Rows: command.resize.rows})
				}
				pane.terminal.Resize(int(command.resize.cols), int(command.resize.rows))
				pane.publishTerminalMetadata()
				if pane.outputLease != nil {
					renderer.markFull()
					renderer.due = true
				}
				command.done <- err
			} else {
				command.done <- nil
			}
		case data, ok := <-pane.ptyOutput:
			if !ok {
				flush()
				return
			}
			trackDamage := pane.outputLease != nil && pane.currentViewMode() == paneViewLive
			update.ResetFor(pane.terminal.Rows, trackDamage)
			pane.terminal.ApplyInto(data, &update)
			if len(startupInput) > 0 {
				arm(&startupIdle, &startupIdleC, startupInputIdle)
			}
			ptyReadBuffers.Put(data[:cap(data)])
			for _, reply := range update.Replies {
				if err := pane.sendOwnedInput(reply); err != nil {
					return
				}
			}
			pane.publishTerminalMetadata()
			if !trackDamage {
				continue
			}
			renderer.merge(update)
			if !renderer.hasWork() {
				continue
			}
			if renderer.due {
				continue
			}
			if !batching {
				batching = true
				arm(&maxAge, &maxC, renderMaxBatchAge)
			}
			arm(&idle, &idleC, renderIdleFlush)
		case <-idleC:
			flush()
		case <-startupIdleC:
			if err := flushStartupInput(); err != nil {
				return
			}
		case <-startupMaxC:
			if err := flushStartupInput(); err != nil {
				return
			}
		case <-maxC:
			flush()
		case <-pane.done:
			return
		}
	}
}

const historySelectionStyleMask uint32 = 1 << 31

func historySelectionContains(selection *paneHistorySelection, row, column int) bool {
	if selection == nil {
		return false
	}
	start, end := normalizedHistorySelection(*selection)
	if row < start.Row || row > end.Row {
		return false
	}
	if start.Row == end.Row {
		return column >= start.Col && column <= end.Col
	}
	if row == start.Row {
		return column >= start.Col
	}
	if row == end.Row {
		return column <= end.Col
	}
	return true
}

func writeHistoryCounter(compiler *displayCompiler, view *paneHistoryView, label string) error {
	cols := view.Snapshot.Cols
	if len(label) > cols {
		label = label[len(label)-cols:]
	}
	if err := compiler.moveTo(0, max(0, cols-len(label))); err != nil {
		return err
	}
	if err := compiler.selectStyle(view.Snapshot.CounterStyle); err != nil {
		return err
	}
	return compiler.output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeWriteTextUTF8, Text: []byte(label)})
}

func emitTerminalUpdate(output *renderOutput, pane *Pane, update Update) error {
	if update.FullRedraw {
		return sendFullRender(output, pane)
	}
	if !update.HasDamage() && !update.CursorChanged && !update.VisibleChange {
		if update.ScrollDelta == 0 {
			return nil
		}
	}
	if update.ScrollDelta != 0 {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeScroll, Delta: update.ScrollDelta}); err != nil {
			return err
		}
	}
	compiler := newLiveDisplayCompiler(output, pane.terminal)
	for row := 0; row < pane.terminal.Rows; row++ {
		span := update.DirtySpans[row]
		if span.End == 0 {
			continue
		}
		cells := pane.terminal.gridRow(row)[span.Start:span.End]
		if err := compiler.writeCells(row, span.Start, cells); err != nil {
			return err
		}
	}
	if err := compiler.finish(); err != nil {
		return err
	}
	if update.CursorChanged || update.VisibleChange {
		if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.terminal.CursorX, Y: pane.terminal.CursorY}, Visible: pane.terminal.CursorVisible}}); err != nil {
			return err
		}
	}
	return output.present()
}

func sendFullRender(output *renderOutput, pane *Pane) error {
	compiler := newLiveDisplayCompiler(output, pane.terminal)
	for row := 0; row < pane.terminal.Rows; row++ {
		if err := compiler.writeCells(row, 0, pane.terminal.gridRow(row)); err != nil {
			return err
		}
	}
	if err := compiler.finish(); err != nil {
		return err
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeCursorUpdate, Cursor: protocol.CursorUpdate{Cursor: protocol.Cursor{X: pane.terminal.CursorX, Y: pane.terminal.CursorY}, Visible: pane.terminal.CursorVisible}}); err != nil {
		return err
	}
	return output.present()
}

func installStyle(output *renderOutput, id uint32, style protocol.Style) error {
	if id == protocol.CanonicalDefaultStyleID && !protocol.IsCanonicalDefaultStyle(style) {
		return fmt.Errorf("style %d must be canonical default", id)
	}
	if installed, ok := output.installedStyles[id]; ok && installed == style {
		return nil
	}
	if err := output.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodeStyleInstall, StyleID: id, Style: style}); err != nil {
		return err
	}
	output.installedStyles[id] = style
	return nil
}

type renderOutput struct {
	stream          io.Writer
	pending         []byte
	installedStyles map[uint32]protocol.Style
	limit           int
	bufferedOnly    bool
}

func newRenderOutput(stream ...io.Writer) *renderOutput {
	output := &renderOutput{stream: io.Discard, installedStyles: make(map[uint32]protocol.Style, 32)}
	if len(stream) > 0 {
		output.stream = stream[0]
	}
	return output
}

func newBoundedRenderOutput(buffer *paneRenderBuffer, installed map[uint32]protocol.Style, limit int) *renderOutput {
	if installed == nil {
		installed = make(map[uint32]protocol.Style, 32)
	}
	buffer.data = buffer.data[:0]
	return &renderOutput{
		stream:          io.Discard,
		pending:         buffer.data,
		installedStyles: installed,
		limit:           limit,
		bufferedOnly:    true,
	}
}

func (o *renderOutput) hasRoom(bytes int) bool {
	return o.limit <= 0 || bytes <= o.limit-len(o.pending)
}

func (o *renderOutput) append(command protocol.DisplayCommand) error {
	if o.pending == nil {
		o.pending = make([]byte, 0, 4096)
	}
	before := o.pending
	encoder := protocol.NewDisplayEncoder(before)
	if err := encoder.AppendCommand(command); err != nil {
		return err
	}
	encoded := encoder.Bytes()
	if o.limit > 0 && len(encoded) > o.limit {
		o.pending = before
		return errRenderBufferFull
	}
	o.pending = encoded
	if o.bufferedOnly {
		return nil
	}
	if len(o.pending) >= renderStreamChunkSize {
		return o.commit()
	}
	return nil
}

func (o *renderOutput) commit() error {
	if o.bufferedOnly {
		return nil
	}
	if len(o.pending) == 0 {
		return nil
	}
	data := o.pending
	err := writeAll(o.stream, data)
	if cap(data) <= maxRetainedRenderBuffer {
		o.pending = data[:0]
	} else {
		o.pending = nil
	}
	return err
}

func (o *renderOutput) present() error {
	if err := o.append(protocol.DisplayCommand{Opcode: protocol.DisplayOpcodePresent}); err != nil {
		return err
	}
	if o.bufferedOnly {
		return nil
	}
	return o.commit()
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
