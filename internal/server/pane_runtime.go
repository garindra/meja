package server

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/garindra/meja/internal/protocol"
)

const (
	ptyReadBufferSize  = 32 * 1024
	ptyReadBufferCount = 2
	ptyDrainByteBudget = 64 * 1024
	ptyDrainTimeBudget = 2 * time.Millisecond
	ptyTurnInterval    = 50 * time.Millisecond
)

type ptyReadBuffer [ptyReadBufferSize]byte

type ptyDrainStopReason uint8

const (
	ptyDrainStoppedEmpty ptyDrainStopReason = iota + 1
	ptyDrainStoppedByteBudget
	ptyDrainStoppedTimeBudget
	ptyDrainStoppedEOF
	ptyDrainStoppedError
	ptyDrainStoppedCancelled
)

type ptyDrainRequest struct {
	maxBytes    int
	maxDuration time.Duration
}

type ptyDrainEvent struct {
	buffer   []byte
	reason   ptyDrainStopReason
	reads    int
	duration time.Duration
}

// ptyReadiness is the deterministic test seam for readers without a pollable
// descriptor. Production PTYs use poll(2) with a zero timeout.
type ptyReadiness interface {
	ptyReadReady() (bool, error)
}

type ptyAdmissionState struct {
	credit     bool
	bypassUsed bool
	deadline   time.Time
	interval   time.Duration
}

func newPTYAdmissionState(interval time.Duration) ptyAdmissionState {
	return ptyAdmissionState{credit: true, interval: interval}
}

func (s *ptyAdmissionState) canAdmit() bool {
	return s.credit
}

func (s *ptyAdmissionState) admit(now time.Time) {
	// A full extra interval with no waiting PTY turn marks a new interactive
	// burst. Merely reaching each deadline during sustained output does not.
	if s.credit && !s.deadline.IsZero() && !now.Before(s.deadline.Add(s.interval)) {
		s.bypassUsed = false
	}
	s.credit = false
	s.deadline = now.Add(s.interval)
}

func (s *ptyAdmissionState) timerFired() {
	s.credit = true
}

func (s *ptyAdmissionState) grantImmediateOpportunity() {
	if !s.credit && !s.bypassUsed {
		s.credit = true
		s.bypassUsed = true
	}
}

const (
	startupInputIdle        = 25 * time.Millisecond
	startupInputMaxWait     = 500 * time.Millisecond
	initialReliableFrameCap = 64 << 10
)

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
		return p.publishAttachedViewSynchronously(lease, layoutRevision)
	}
	return p.sendRenderCommand(paneCommand{install: installation})
}

func (p *Pane) publishAttachedViewSynchronously(lease *OutputLease, layoutRevision protocol.ClientLayoutRevision) error {
	state := newPanePublicationState(p)
	state.attach(lease, layoutRevision)
	buffer := <-state.free
	if err := state.prepare(buffer); err != nil {
		return err
	}
	if state.pending == nil {
		return nil
	}
	lease.submissions() <- confirmerMessage{publication: state.pending}
	state.handedOff()
	return lease.sync()
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

func (p *Pane) syncOutput() error {
	if p.commands == nil {
		if p.outputLease == nil {
			return nil
		}
		return p.outputLease.sync()
	}
	ready := make(chan *OutputLease, 1)
	select {
	case p.commands <- paneCommand{sync: ready}:
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
	var lease *OutputLease
	select {
	case lease = <-ready:
	case <-p.mainDone:
		return nil
	case <-p.done:
		return nil
	}
	if lease == nil {
		return nil
	}
	return lease.sync()
}

func relayPTYOutput(pane *Pane) {
	relayPTYOutputFrom(pane, pane.PTY)
}

func relayPTYOutputFrom(pane *Pane, reader io.Reader) {
	defer func() {
		close(pane.ptyDrainEvents)
		close(pane.ptyReaderDone)
	}()
	for {
		var request ptyDrainRequest
		select {
		case request = <-pane.ptyDrainRequests:
		case <-pane.done:
			return
		}
		if !drainPTYOpportunity(pane, reader, request) {
			return
		}
	}
}

func drainPTYOpportunity(pane *Pane, reader io.Reader, request ptyDrainRequest) bool {
	now := time.Now
	if pane.ptyDrainNow != nil {
		now = pane.ptyDrainNow
	}
	if request.maxBytes <= 0 {
		request.maxBytes = ptyDrainByteBudget
	}
	if request.maxDuration <= 0 {
		request.maxDuration = ptyDrainTimeBudget
	}
	started := now()
	bytesRead, reads := 0, 0
	cancel := func() bool {
		pane.renderMetrics.recordPTYDrain(ptyDrainEvent{
			reason: ptyDrainStoppedCancelled, reads: reads, duration: now().Sub(started),
		})
		return false
	}
	complete := func(reason ptyDrainStopReason) bool {
		event := ptyDrainEvent{
			reason: reason, reads: reads, duration: now().Sub(started),
		}
		select {
		case pane.ptyDrainEvents <- event:
			return reason != ptyDrainStoppedEOF && reason != ptyDrainStoppedError
		case <-pane.done:
			return cancel()
		}
	}

	for {
		ready, err := ptyReadImmediatelyAvailable(reader)
		if err != nil {
			return complete(ptyDrainStoppedError)
		}
		if !ready {
			return complete(ptyDrainStoppedEmpty)
		}
		if bytesRead >= request.maxBytes {
			return complete(ptyDrainStoppedByteBudget)
		}
		if reads > 0 && now().Sub(started) >= request.maxDuration {
			return complete(ptyDrainStoppedTimeBudget)
		}

		var buffer []byte
		select {
		case buffer = <-pane.ptyFree:
		case <-pane.done:
			return cancel()
		}
		remaining := request.maxBytes - bytesRead
		if remaining < len(buffer) {
			buffer = buffer[:remaining]
		}
		n, readErr := reader.Read(buffer)
		reads++
		if n > 0 {
			bytesRead += n
			pane.notifyProcessActivity()
			select {
			case pane.ptyDrainEvents <- ptyDrainEvent{buffer: buffer[:n]}:
			case <-pane.done:
				pane.ptyFree <- buffer[:ptyReadBufferSize]
				return cancel()
			}
		} else {
			pane.ptyFree <- buffer[:ptyReadBufferSize]
		}
		if readErr != nil {
			if errors.Is(readErr, unix.EAGAIN) || errors.Is(readErr, unix.EWOULDBLOCK) {
				return complete(ptyDrainStoppedEmpty)
			}
			// A signal such as SIGCHLD from a short-lived foreground command
			// may interrupt the readiness/read syscall. It says nothing about
			// PTY lifetime; end this opportunity and retry on the next credit.
			if errors.Is(readErr, unix.EINTR) {
				return complete(ptyDrainStoppedEmpty)
			}
			if errors.Is(readErr, io.EOF) {
				return complete(ptyDrainStoppedEOF)
			}
			return complete(ptyDrainStoppedError)
		}
	}
}

func ptyReadImmediatelyAvailable(reader io.Reader) (bool, error) {
	if readiness, ok := reader.(ptyReadiness); ok {
		return readiness.ptyReadReady()
	}
	fdReader, ok := reader.(interface{ Fd() uintptr })
	if !ok {
		// Non-file readers are used only by tests and adapters which guarantee
		// that Read will not block.
		return true, nil
	}
	pollFDs := [1]unix.PollFd{{
		Fd:     int32(fdReader.Fd()),
		Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
	}}
	n, err := unix.Poll(pollFDs[:], 0)
	return ptyPollReadinessResult(n, err)
}

func ptyPollReadinessResult(n int, err error) (bool, error) {
	if errors.Is(err, unix.EINTR) {
		// The next 50 ms or bounded immediate opportunity will retry. Treating
		// EINTR as a terminal PTY error would close the master and SIGHUP the
		// otherwise healthy shell that just reaped a child process.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func runPTYWriter(pane *Pane, failed func(error)) {
	defer close(pane.writerDone)
	for {
		select {
		case data := <-pane.ptyInput:
			if _, err := writeAll(pane.PTY, data); err != nil {
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
		// Stop the reader and wait until it no longer accesses the PTY before
		// closing the file. os.File.Close racing a readiness Fd call is unsafe.
		p.stop()
		if p.ptyReaderDone != nil {
			<-p.ptyReaderDone
		}
		if p.PTY != nil {
			_ = p.PTY.Close()
		}
		close(p.mainDone)
	}()
	renderer := newPanePublicationState(p)
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
	drainActive := false
	ptyDrainEvents := (<-chan ptyDrainEvent)(p.ptyDrainEvents)
	applyPTYChunk := func(data []byte) error {
		p.renderMetrics.ptyBytes.Add(uint64(len(data)))
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
				return err
			}
		}
		if err := p.routeFrontendWrites(&update, currentFrontendWrite); err != nil {
			return err
		}
		p.publishTerminalMetadata()
		if trackDamage {
			renderer.merge(update)
		}
		return nil
	}
	for {
		renderer.flushSyncWaiters()
		available := renderer.available()
		if drainActive {
			// Commands and protocol side effects remain live during a drain,
			// but visual publication is gated on its explicit completion.
			// Commands interleave at chunk boundaries: capture observes the
			// canonical state parsed so far, while structural changes update
			// the epoch/keyframe state that the completed drain will publish.
			available = nil
		}
		failures := renderer.failures()
		submission, publication := renderer.submission()
		var drainRequests chan<- ptyDrainRequest
		// Detached panes continue advancing their canonical terminal state at
		// the same paced cadence. Attached panes additionally stop admission
		// while visual work is waiting for their bounded output path.
		if !drainActive && admission.canAdmit() && !renderer.blocksPTY() {
			drainRequests = p.ptyDrainRequests
		}
		select {
		case drainRequests <- ptyDrainRequest{maxBytes: ptyDrainByteBudget, maxDuration: ptyDrainTimeBudget}:
			drainActive = true
			admission.admit(time.Now())
			p.renderMetrics.ptyDrainOpportunities.Add(1)
			arm(&ptyTimer, &ptyTimerC, pacingInterval)
		case buffer := <-available:
			lease := p.outputLease
			if err := renderer.prepare(buffer); err != nil {
				if lease != nil {
					lease.reportFailure(fmt.Errorf("render pane %d: %w", p.ID, err))
				}
				p.outputLease = nil
				renderer.detach()
			}
		case submission <- publication:
			renderer.handedOff()
		case <-failures:
			p.outputLease = nil
			renderer.detach()
		case command := <-p.commands:
			if command.sync != nil {
				renderer.requestSync(command.sync)
				continue
			}
			if command.republish {
				if p.outputLease != nil {
					renderer.invalidateEpoch(false)
				}
				command.done <- nil
				continue
			}
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
				renderer.attach(installation.Lease, installation.LayoutRevision)
				admission.grantImmediateOpportunity()
				command.done <- nil
				continue
			}
			if command.history != nil {
				beforeMode := p.currentViewMode()
				result := p.handleHistoryRequest(command.history)
				if p.currentViewMode() != beforeMode && p.outputLease != nil {
					renderer.invalidateEpoch(false)
				}
				if result.Render.HasRenderChange() && p.outputLease != nil {
					renderer.mergeViewMutation(result.Render)
				}
				command.history.Result <- result
				continue
			}
			if command.resize != nil {
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
				renderer.invalidateEpoch(false)
				admission.grantImmediateOpportunity()
				command.done <- err
			} else {
				command.done <- nil
			}
		case event, ok := <-ptyDrainEvents:
			if !ok {
				ptyDrainEvents = nil
				continue
			}
			if event.buffer != nil {
				if err := applyPTYChunk(event.buffer); err != nil {
					return
				}
				continue
			}
			if event.reason == 0 {
				continue
			}
			drainActive = false
			p.renderMetrics.recordPTYDrain(event)
			if renderer.hasMutation() {
				renderer.attributeNextPreparationToDrain()
			}
			if event.reason == ptyDrainStoppedError {
				return
			}
		case <-ptyTimerC:
			ptyTimerC = nil
			admission.timerFired()
		case <-p.ptyInteractive:
			admission.grantImmediateOpportunity()
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

func writeAll(w io.Writer, data []byte) (writes uint64, err error) {
	for len(data) > 0 {
		n, writeErr := w.Write(data)
		writes++
		if writeErr != nil {
			return writes, writeErr
		}
		if n == 0 {
			return writes, io.ErrShortWrite
		}
		data = data[n:]
	}
	return writes, nil
}
