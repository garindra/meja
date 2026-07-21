package server

import (
	"fmt"
	"io"
	"time"
)

const paneRenderBufferCapacity = 32 << 10

// paneRenderBuffer has exactly one owner at a time. It moves from an output
// worker to a pane actor and back through OutputLease channels.
type paneRenderBuffer struct {
	data []byte
}

type paneRenderBatch struct {
	buffer *paneRenderBuffer
	done   chan error
}

func newPaneRenderBuffer() *paneRenderBuffer {
	return &paneRenderBuffer{data: make([]byte, 0, paneRenderBufferCapacity)}
}

func (l *OutputLease) startWorker() {
	if l == nil {
		return
	}
	l.workerOnce.Do(func() {
		l.available = make(chan *paneRenderBuffer, 1)
		l.ready = make(chan paneRenderBatch, 1)
		l.failed = make(chan error, 1)
		go l.runWorker(newPaneRenderBuffer())
	})
}

func (l *OutputLease) runWorker(buffer *paneRenderBuffer) {
	returnBuffer := func() bool {
		buffer.data = buffer.data[:0]
		select {
		case l.available <- buffer:
			return true
		case <-l.done:
			return false
		}
	}
	if !returnBuffer() {
		return
	}
	for {
		select {
		case batch := <-l.ready:
			buffer = batch.buffer
			if deadlineWriter, ok := l.Stream.(interface{ SetWriteDeadline(time.Time) error }); ok {
				_ = deadlineWriter.SetWriteDeadline(time.Now().Add(quicMaxIdleTimeout))
			}
			err := writeAll(l.Stream, buffer.data)
			if deadlineWriter, ok := l.Stream.(interface{ SetWriteDeadline(time.Time) error }); ok {
				_ = deadlineWriter.SetWriteDeadline(time.Time{})
			}
			if err != nil {
				wrapped := fmt.Errorf("write pane output slot %d: %w", l.Slot, err)
				select {
				case l.failed <- wrapped:
				default:
				}
				if l.onFailure != nil {
					l.onFailure(wrapped)
				}
				if batch.done != nil {
					batch.done <- wrapped
				}
				return
			}
			if batch.done != nil {
				batch.done <- nil
			}
			if !returnBuffer() {
				return
			}
		case <-l.done:
			select {
			case abandoned := <-l.ready:
				if abandoned.done != nil {
					abandoned.done <- io.ErrClosedPipe
				}
			default:
			}
			return
		}
	}
}

func (l *OutputLease) availableBuffers() <-chan *paneRenderBuffer {
	if l == nil {
		return nil
	}
	l.startWorker()
	return l.available
}

func (l *OutputLease) failures() <-chan error {
	if l == nil {
		return nil
	}
	l.startWorker()
	return l.failed
}

func (l *OutputLease) submit(buffer *paneRenderBuffer) bool {
	return l.submitBatch(buffer, nil)
}

func (l *OutputLease) submitBatch(buffer *paneRenderBuffer, done chan error) bool {
	if l == nil || buffer == nil {
		return false
	}
	l.startWorker()
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.ready <- paneRenderBatch{buffer: buffer, done: done}:
		return true
	case <-l.done:
		return false
	default:
		return false
	}
}

func (l *OutputLease) recycle(buffer *paneRenderBuffer) {
	if buffer == nil {
		return
	}
	buffer.data = buffer.data[:0]
	_ = l.submit(buffer)
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
