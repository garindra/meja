package server

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/creack/pty"

	"github.com/garindra/meja/internal/protocol"
)

const (
	ptyReadBufferSize  = 32 * 1024
	ptyReadBufferCount = 2
	ptyTurnInterval    = 50 * time.Millisecond
)

type ptyReadBuffer [ptyReadBufferSize]byte

type ptyAdmissionState struct {
	ready      bool
	immediate  bool
	bypassUsed bool
	deadline   time.Time
	interval   time.Duration
}

func newPTYAdmissionState(interval time.Duration) ptyAdmissionState {
	return ptyAdmissionState{ready: true, interval: interval}
}

func (s *ptyAdmissionState) canAdmit() bool {
	return s.ready || s.immediate
}

func (s *ptyAdmissionState) admit(now time.Time) {
	// A full extra interval with no waiting PTY turn marks a new interactive
	// burst. Merely reaching each deadline during sustained output does not.
	if s.ready && !s.deadline.IsZero() && !now.Before(s.deadline.Add(s.interval)) {
		s.bypassUsed = false
	}
	s.ready = false
	s.immediate = false
	s.deadline = now.Add(s.interval)
}

func (s *ptyAdmissionState) timerFired() {
	s.ready = true
}

func (s *ptyAdmissionState) grantKeyboardOpportunity() {
	s.grantImmediateOpportunity()
}

func (s *ptyAdmissionState) grantStructuralOpportunity() {
	s.grantImmediateOpportunity()
}

func (s *ptyAdmissionState) grantImmediateOpportunity() {
	if !s.ready && !s.bypassUsed {
		s.immediate = true
		s.bypassUsed = true
	}
}

const (
	// Finalized commands are streamed at this size while PRESENT remains the
	// client's atomic frame boundary.
	renderStreamChunkSize   = 8 << 10
	startupInputIdle        = 25 * time.Millisecond
	startupInputMaxWait     = 500 * time.Millisecond
	maxRetainedRenderBuffer = 64 << 10
)

var errRenderBufferFull = errors.New("pane render buffer is full")

func (p *Pane) installOutputLease(lease *OutputLease, layoutRevision protocol.ClientLayoutRevision, cols, rows uint16) error {
	installation := &paneOutputInstall{
		Lease: lease, LayoutRevision: layoutRevision, Cols: cols, Rows: rows,
	}
	if p.commands == nil {
		if p.terminal == nil {
			p.outputLease = lease
			return nil
		}
		if p.terminal.Cols != int(cols) || p.terminal.Rows != int(rows) {
			if p.PTY != nil {
				if err := pty.Setsize(p.PTY, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
					return err
				}
			}
			p.terminal.Resize(int(cols), int(rows))
			p.publishTerminalMetadata()
		}
		p.outputLease = lease
		if lease == nil {
			return nil
		}
		return p.renderAttachedView(newRenderOutput(lease.Stream), layoutRevision)
	}
	return p.sendRenderCommand(paneCommand{install: installation})
}

func (p *Pane) renderAttachedView(output *renderOutput, layoutRevision protocol.ClientLayoutRevision) error {
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

func (p *Pane) detachOutputLease(lease *OutputLease) error {
	if p.commands == nil {
		if p.outputLease == lease {
			p.outputLease = nil
		}
		return nil
	}
	return p.sendRenderCommand(paneCommand{detach: &paneOutputDetach{Lease: lease}})
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
	relayPTYOutputFrom(pane, pane.PTY)
}

func relayPTYOutputFrom(pane *Pane, reader io.Reader) {
	defer close(pane.ptyOutput)
	for {
		var buf []byte
		select {
		case buf = <-pane.ptyFree:
		case <-pane.done:
			return
		}
		n, err := reader.Read(buf)
		if n > 0 {
			pane.notifyProcessActivity()
			select {
			case pane.ptyOutput <- buf[:n]:
			case <-pane.done:
				pane.ptyFree <- buf
				return
			}
		} else {
			pane.ptyFree <- buf
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

func (p *Pane) run() {
	defer func() {
		if p.PTY != nil {
			_ = p.PTY.Close()
		}
		close(p.mainDone)
	}()
	renderer := newPaneRenderState(p)
	var update Update
	pacingInterval := p.ptyPacingInterval
	if pacingInterval <= 0 {
		pacingInterval = ptyTurnInterval
	}
	admission := newPTYAdmissionState(pacingInterval)
	newTimer := time.NewTimer
	if p.newTimer != nil {
		newTimer = p.newTimer
	}
	var ptyTimer *time.Timer
	var ptyTimerC <-chan time.Time
	var startupIdle *time.Timer
	var startupIdleC <-chan time.Time
	startupInput := p.startupInput
	p.startupInput = nil
	var startupMax *time.Timer
	var startupMaxC <-chan time.Time
	if len(startupInput) > 0 {
		startupMax = time.NewTimer(startupInputMaxWait)
		startupMaxC = startupMax.C
	}
	stop := func(timer *time.Timer) {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer func() {
		stop(ptyTimer)
		stop(startupIdle)
		stop(startupMax)
	}()
	arm := func(timer **time.Timer, channel *<-chan time.Time, duration time.Duration) {
		if *timer == nil {
			*timer = newTimer(duration)
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
		return p.sendOwnedInput(input)
	}
	for {
		available := renderer.available()
		failures := renderer.failures()
		var ptyOutput <-chan []byte
		// Detached panes continue advancing their canonical terminal state at
		// the same paced cadence. Attached panes additionally stop admission
		// while visual work is waiting for their bounded output path.
		if admission.canAdmit() && (p.outputLease == nil || !renderer.hasWork()) {
			ptyOutput = p.ptyOutput
		}
		select {
		case buffer := <-available:
			lease := p.outputLease
			if err := renderer.render(buffer); err != nil {
				if lease != nil {
					lease.recycle(buffer)
					lease.reportFailure(fmt.Errorf("render pane %d: %w", p.ID, err))
				}
				p.outputLease = nil
				renderer.detach()
			}
		case <-failures:
			p.outputLease = nil
			renderer.detach()
		case command := <-p.commands:
			if command.capture != nil {
				data, err := captureTerminalViewport(p.terminal, command.capture.Options)
				command.capture.Result <- paneCaptureResult{Data: data, Err: err}
				continue
			}
			if command.release != nil {
				lease := p.outputLease
				p.outputLease = nil
				renderer.detach()
				command.release.returnLease(lease)
				continue
			}
			if command.detach != nil {
				if p.outputLease == command.detach.Lease {
					p.outputLease = nil
					renderer.detach()
				}
				command.done <- nil
				continue
			}
			if command.install != nil {
				installation := command.install
				p.outputLease = nil
				renderer.detach()
				if p.terminal.Cols != int(installation.Cols) || p.terminal.Rows != int(installation.Rows) {
					if p.PTY != nil {
						if err := pty.Setsize(p.PTY, &pty.Winsize{Cols: installation.Cols, Rows: installation.Rows}); err != nil {
							command.done <- err
							continue
						}
					}
					p.terminal.Resize(int(installation.Cols), int(installation.Rows))
					p.publishTerminalMetadata()
				}
				p.outputLease = installation.Lease
				if installation.Lease == nil {
					command.done <- nil
					continue
				}
				renderer.attach(installation.Lease, installation.LayoutRevision, installation.Refresh)
				admission.grantStructuralOpportunity()
				command.done <- nil
				continue
			}
			if command.history != nil {
				result := p.handleHistoryRequest(command.history)
				if result.Render.HasRenderChange() && p.outputLease != nil {
					renderer.mergeViewMutation(result.Render)
					renderer.due = true
				}
				command.history.Result <- result
				continue
			}
			if command.apply != nil && p.outputLease != nil {
				renderer.queued = append(renderer.queued, queuedPaneRender{render: command.apply, done: command.done})
				renderer.due = true
			} else if command.resize != nil {
				err := error(nil)
				if p.outputLease != nil {
					command.done <- fmt.Errorf("resize pane %d while its output grid is still attached", p.ID)
					continue
				}
				if p.PTY != nil {
					err = pty.Setsize(p.PTY, &pty.Winsize{Cols: command.resize.cols, Rows: command.resize.rows})
				}
				p.terminal.Resize(int(command.resize.cols), int(command.resize.rows))
				p.publishTerminalMetadata()
				admission.grantStructuralOpportunity()
				if p.outputLease != nil {
					renderer.markFull()
					renderer.due = true
				}
				command.done <- err
			} else {
				command.done <- nil
			}
		case data, ok := <-ptyOutput:
			if !ok {
				return
			}
			admission.admit(time.Now())
			arm(&ptyTimer, &ptyTimerC, pacingInterval)
			select {
			case <-p.ptyInteractive:
			default:
			}
			trackDamage := p.outputLease != nil && p.currentViewMode() == paneViewLive
			var currentFrontendWrite func([]byte) error
			if p.outputLease != nil {
				currentFrontendWrite = p.outputLease.frontendTerminalWrite
			}
			update.ResetFor(p.terminal.Rows, trackDamage)
			p.terminal.ApplyInto(data, &update)
			if len(startupInput) > 0 {
				arm(&startupIdle, &startupIdleC, startupInputIdle)
			}
			p.ptyFree <- data[:ptyReadBufferSize]
			for _, reply := range update.Replies {
				if err := p.sendOwnedInput(reply); err != nil {
					return
				}
			}
			if err := p.routeFrontendWrites(&update, currentFrontendWrite); err != nil {
				return
			}
			p.publishTerminalMetadata()
			if !trackDamage {
				continue
			}
			renderer.merge(update)
			if !renderer.hasWork() {
				continue
			}
			renderer.due = true
		case <-ptyTimerC:
			ptyTimerC = nil
			admission.timerFired()
		case <-p.ptyInteractive:
			admission.grantKeyboardOpportunity()
		case <-startupIdleC:
			if err := flushStartupInput(); err != nil {
				return
			}
		case <-startupMaxC:
			if err := flushStartupInput(); err != nil {
				return
			}
		case <-p.done:
			return
		}
	}
}

func (p *Pane) routeFrontendWrites(update *Update, current func([]byte) error) error {
	for _, write := range update.FrontendWrites {
		destination := current
		if write.OSC52Sequence == p.pendingOSC52Sequence {
			destination = p.pendingOSC52FrontendWrite
		}
		if destination != nil {
			if err := destination(write.Data); err != nil {
				return err
			}
		}
	}
	if !p.terminal.Parser.oscCandidate {
		p.pendingOSC52FrontendWrite = nil
		p.pendingOSC52Sequence = 0
	} else if p.pendingOSC52Sequence != p.terminal.Parser.oscSequence {
		p.pendingOSC52FrontendWrite = current
		p.pendingOSC52Sequence = p.terminal.Parser.oscSequence
	}
	return nil
}

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
