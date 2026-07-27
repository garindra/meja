package server

import (
	"strings"
	"testing"
	"time"
)

func newBlockedDeliveryConnection() (*clientConnection, chan struct{}) {
	done := make(chan struct{})
	connection := newClientConnection()
	connection.commands = make(chan clientInstanceCommand)
	connection.done = done
	return connection, done
}

func TestRequiredClientDeliveriesPreserveFIFOOrder(t *testing.T) {
	connection := newClientConnection()
	done := make(chan struct{})
	connection.done = done
	connection.startDeliveryWorker()
	defer close(done)

	for sequence := uint64(1); sequence <= 32; sequence++ {
		if !connection.enqueueRequired(clientInstanceCommand{ClearStatusMessage: sequence}) {
			t.Fatalf("required delivery %d unexpectedly saturated", sequence)
		}
	}
	for sequence := uint64(1); sequence <= 32; sequence++ {
		select {
		case command := <-connection.commands:
			if command.ClearStatusMessage != sequence {
				t.Fatalf("delivery order = %d, want %d", command.ClearStatusMessage, sequence)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for required delivery %d", sequence)
		}
	}
}

func TestStatusClientDeliveriesCoalesceBehindRequiredWork(t *testing.T) {
	connection, done := newBlockedDeliveryConnection()
	connection.startDeliveryWorker()
	defer close(done)

	if !connection.enqueueRequired(clientInstanceCommand{ClearStatusMessage: 1}) {
		t.Fatal("initial required delivery saturated")
	}
	for range 100 {
		connection.enqueueStatusRefresh(clientStatusState{}, false)
	}
	if got := len(connection.refresh); got != 1 {
		t.Fatalf("coalesced refresh edges = %d, want 1", got)
	}

	first := <-connection.commands
	if first.ClearStatusMessage != 1 {
		t.Fatalf("first delivery = %#v, want required command", first)
	}
	select {
	case refresh := <-connection.commands:
		if !refresh.RefreshStatus {
			t.Fatalf("second delivery = %#v, want status refresh", refresh)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced status refresh")
	}
	select {
	case extra := <-connection.commands:
		t.Fatalf("status refresh did not coalesce: %#v", extra)
	case <-time.After(10 * time.Millisecond):
	}
}

func TestRequiredDeliverySaturationFencesExactConnection(t *testing.T) {
	connection, done := newBlockedDeliveryConnection()
	connection.startDeliveryWorker()
	defer close(done)

	saturated := false
	for sequence := uint64(1); sequence < 4*clientRequiredDeliveryCapacity; sequence++ {
		if !connection.enqueueRequired(clientInstanceCommand{ClearStatusMessage: sequence}) {
			saturated = true
			break
		}
	}
	if !saturated {
		t.Fatal("required delivery queue did not report saturation")
	}

	select {
	case command := <-connection.commands:
		if !command.Close || command.CloseCode == 0 ||
			!strings.Contains(command.CloseReason, "mailbox saturated") {
			t.Fatalf("saturation delivery = %#v, want explicit connection fence", command)
		}
	case <-time.After(time.Second):
		t.Fatal("saturated connection was not fenced")
	}
}

func TestFullClientMailboxDoesNotBlockDaemonWork(t *testing.T) {
	d := newCommandTestDaemon(t)
	connection, done := newBlockedDeliveryConnection()
	defer close(done)
	mutationDone := make(chan struct{})
	d.postAfter(func() {}, func() {
		for sequence := uint64(1); ; sequence++ {
			if !connection.enqueueRequired(clientInstanceCommand{ClearStatusMessage: sequence}) {
				break
			}
		}
	})
	d.call(func() {
		d.nextPaneID++
		close(mutationDone)
	})
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("full client mailbox blocked unrelated daemon mutation")
	}
}

func TestProcessObservationDoesNotBlockOnSaturatedClientDelivery(t *testing.T) {
	d := newCommandTestDaemon(t)
	state := NewSessionState(77)
	state.daemon = d
	connection, done := newBlockedDeliveryConnection()
	defer close(done)
	client := &ClientIdentity{
		ID:        9,
		SessionID: state.ID,
		State:     clientLifecycle{Phase: clientActive, Active: connection},
	}
	state.ClientID = client.ID
	d.sessions[state.ID] = state
	d.clients[client.ID] = client

	for sequence := uint64(1); ; sequence++ {
		if !connection.enqueueRequired(clientInstanceCommand{ClearStatusMessage: sequence}) {
			break
		}
	}
	d.processObservationDelivery(state.ID)(nil)

	queryDone := make(chan struct{})
	d.call(func() {
		_ = d.sessions[state.ID]
		close(queryDone)
	})
	select {
	case <-queryDone:
	case <-time.After(time.Second):
		t.Fatal("process observation blocked daemon behind saturated client delivery")
	}
}

func TestStoppedClientMailboxDoesNotBlockUnrelatedDaemonWork(t *testing.T) {
	d := newCommandTestDaemon(t)
	connection := newClientConnection()
	done := make(chan struct{})
	connection.done = done
	connection.startDeliveryWorker()
	close(done)
	postClientCommand(connection, clientInstanceCommand{RefreshStatus: true})

	queryDone := make(chan struct{})
	d.call(func() {
		d.nextWindowID++
		close(queryDone)
	})
	select {
	case <-queryDone:
	case <-time.After(time.Second):
		t.Fatal("stopped client mailbox blocked unrelated daemon work")
	}
}

func TestOldConnectionMailboxCannotDeliverIntoReplacement(t *testing.T) {
	oldConnection := newClientConnection()
	replacement := newClientConnection()
	oldDone := make(chan struct{})
	replacementDone := make(chan struct{})
	oldConnection.done = oldDone
	replacement.done = replacementDone
	oldConnection.startDeliveryWorker()
	replacement.startDeliveryWorker()
	defer close(oldDone)
	defer close(replacementDone)

	if !oldConnection.enqueueRequired(clientInstanceCommand{ClearStatusMessage: 41}) {
		t.Fatal("old connection delivery saturated")
	}
	select {
	case command := <-oldConnection.commands:
		if command.ClearStatusMessage != 41 {
			t.Fatalf("old connection received %#v", command)
		}
	case <-time.After(time.Second):
		t.Fatal("old connection did not receive its own delivery")
	}
	select {
	case command := <-replacement.commands:
		t.Fatalf("old mailbox delivered into replacement: %#v", command)
	case <-time.After(10 * time.Millisecond):
	}
}

func TestClosingConnectionPublishesRevocationBeforeDelivery(t *testing.T) {
	connection := newClientConnection()
	if connection.isRevoked() {
		t.Fatal("new connection started revoked")
	}
	if !connection.enqueueRequired(clientInstanceCommand{Close: true, CloseReason: "replaced"}) {
		t.Fatal("close delivery saturated")
	}
	if !connection.isRevoked() {
		t.Fatal("close did not synchronously revoke stale input")
	}
}

func TestConnectionStartsOnlyOneDeliveryWorker(t *testing.T) {
	started := make(chan struct{}, 2)
	connection := newClientConnection()
	done := make(chan struct{})
	connection.done = done
	connection.workerStarted = started
	defer close(done)

	for range 1000 {
		connection.enqueueStatusRefresh(clientStatusState{}, false)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery worker did not start")
	}
	select {
	case <-started:
		t.Fatal("more than one delivery worker started for one connection")
	case <-time.After(10 * time.Millisecond):
	}
}
