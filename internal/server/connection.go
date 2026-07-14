package server

import (
	"io"

	"github.com/quic-go/quic-go"

	"tali/internal/protocol"
)

// OutputLease is one enduring pane-output slot for the lifetime of a
// Connection. Its Stream is the physical QUIC stream in production. Exactly
// one pane actor or the session's unused pool holds a lease at a time.
type OutputLease struct {
	Slot   int
	Stream io.Writer
}

// Connection owns one live QUIC connection, its protocol streams, and its
// eight enduring output leases. It borrows a Session; sessions and panes can
// outlive any particular Connection.
type Connection struct {
	QUIC       quic.Connection
	Session    *Session
	Daemon     *Daemon
	Management quic.Stream
	Input      quic.Stream
	Output     [protocol.MaxRenderSlots]*OutputLease

	managementOut chan protocol.Frame
	shell         string
	defaultCwd    string
	defaultArgv   []string
}
