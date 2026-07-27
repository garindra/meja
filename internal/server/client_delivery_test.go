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

func newPausedDeliveryConnection() (*clientConnection, chan struct{}) {
	connection, done := newBlockedDeliveryConnection()
	connection.workerOnce.Do(func() {})
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

func TestRequiredDeliveryBacklogHasOneDocumentedBound(t *testing.T) {
	connection := newClientConnection()
	if got, want := cap(connection.required), clientRequiredDeliveryCapacity-1; got != want {
		t.Fatalf("queued required capacity = %d, want %d", got, want)
	}
	if got := cap(connection.commands); got != 0 {
		t.Fatalf("client actor handoff capacity = %d, want unbuffered", got)
	}
	if got := cap(connection.required) + 1; got != clientRequiredDeliveryCapacity {
		t.Fatalf("total required backlog = %d, want %d", got, clientRequiredDeliveryCapacity)
	}
}

func TestRequiredReservationsPreserveDaemonOrderAcrossReversedProducers(t *testing.T) {
	connection := newClientConnection()
	done := make(chan struct{})
	connection.done = done
	connection.startDeliveryWorker()
	defer close(done)

	first, ok := connection.reserveRequired()
	if !ok {
		t.Fatal("first daemon-turn reservation saturated")
	}
	second, ok := connection.reserveRequired()
	if !ok {
		t.Fatal("second daemon-turn reservation saturated")
	}

	results := make(chan uint64, 2)
	attempt := func(delivery *clientRequiredDelivery, sequence uint64) {
		commandDone := make(chan error, 1)
		delivery.finish(clientInstanceCommand{
			ClearStatusMessage: sequence,
			Done:               commandDone,
		}, false)
		if err := <-commandDone; err != nil {
			t.Errorf("delivery %d result: %v", sequence, err)
		}
		results <- sequence
	}
	go attempt(second, 2)
	select {
	case command := <-connection.commands:
		t.Fatalf("second producer overtook first reservation: %#v", command)
	case <-time.After(10 * time.Millisecond):
	}
	go attempt(first, 1)

	for want := uint64(1); want <= 2; want++ {
		select {
		case command := <-connection.commands:
			if command.ClearStatusMessage != want {
				t.Fatalf("applied delivery = %d, want %d", command.ClearStatusMessage, want)
			}
			command.Done <- nil
		case <-time.After(time.Second):
			t.Fatalf("timed out applying delivery %d", want)
		}
		select {
		case got := <-results:
			if got != want {
				t.Fatalf("command result order = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for command result %d", want)
		}
	}
}

func TestRequiredReservationOrdersTransitionWithNonTransitionCommand(t *testing.T) {
	connection := newClientConnection()
	done := make(chan struct{})
	connection.done = done
	connection.startDeliveryWorker()
	defer close(done)

	transitionDelivery, ok := connection.reserveRequired()
	if !ok {
		t.Fatal("transition reservation saturated")
	}
	commandDelivery, ok := connection.reserveRequired()
	if !ok {
		t.Fatal("command reservation saturated")
	}
	commandDelivery.finish(clientInstanceCommand{EnterHistory: true}, false)
	select {
	case command := <-connection.commands:
		t.Fatalf("non-transition command overtook transition: %#v", command)
	case <-time.After(10 * time.Millisecond):
	}
	transition := ViewTransition{Reason: viewTransitionLayout}
	transitionDelivery.finish(clientInstanceCommand{Transition: &transition}, false)

	first := <-connection.commands
	if first.Transition == nil {
		t.Fatalf("first interleaved delivery = %#v, want transition", first)
	}
	second := <-connection.commands
	if !second.EnterHistory {
		t.Fatalf("second interleaved delivery = %#v, want history command", second)
	}
}

func TestClientStatusSnapshotsRejectOldSessionAndRevision(t *testing.T) {
	client := &ClientInstance{
		sessionID: 2,
		currentView: ClientView{
			Status: clientStatusState{
				Revision: 3, SessionID: 2, SessionName: "new",
				Windows: []WindowStatus{{Title: "new-window"}},
			},
			StatusValid: true,
		},
	}
	client.runClientCommand(clientInstanceCommand{
		RefreshStatus: true, HasStatus: true,
		Status: clientStatusState{Revision: 2, SessionID: 1, SessionName: "old"},
	})
	if got := client.currentView.Status.SessionName; got != "new" {
		t.Fatalf("old-session status replaced transition status: %q", got)
	}

	client.runClientCommand(clientInstanceCommand{
		RefreshStatus: true, HasStatus: true,
		Status: clientStatusState{
			Revision: 2, SessionID: 2,
			Windows: []WindowStatus{{Title: "old-window"}},
		},
	})
	if got := client.currentView.Status.Windows[0].Title; got != "new-window" {
		t.Fatalf("old window status replaced newer layout status: %q", got)
	}
}

func TestOldSessionStatusPendingBeforeSessionSwitchIsIgnored(t *testing.T) {
	connection, done := newPausedDeliveryConnection()
	defer close(done)
	connection.enqueueStatusRefresh(clientStatusState{
		Revision: 1, SessionID: 1, SessionName: "source",
	}, true)
	delivery, ok := connection.reserveRequired()
	if !ok {
		t.Fatal("session-switch reservation saturated")
	}
	transition := ViewTransition{
		Reason: viewTransitionSession,
		Projection: ClientProjectionPlan{
			SessionID: 2,
			View: ClientView{
				Status: clientStatusState{
					Revision: 2, SessionID: 2, SessionName: "target",
				},
				StatusValid: true,
			},
		},
	}
	delivery.finish(clientInstanceCommand{Transition: &transition}, false)
	go connection.runDeliveryWorker()

	required := <-connection.commands
	client := &ClientInstance{sessionID: required.Transition.Projection.SessionID}
	client.currentView = client.orderedViewStatus(
		required.Transition.Projection.View,
		required.Transition.Projection.SessionID,
	)
	client.runClientCommand(<-connection.commands)
	if got := client.currentView.Status.SessionName; got != "target" {
		t.Fatalf("pending source status replaced session-switch status: %q", got)
	}
}

func TestOldWindowStatusPendingBeforeNewerLayoutTransitionIsIgnored(t *testing.T) {
	connection, done := newPausedDeliveryConnection()
	defer close(done)
	connection.enqueueStatusRefresh(clientStatusState{
		Revision: 4, SessionID: 7,
		Windows: []WindowStatus{{Title: "old-window"}},
	}, true)
	delivery, ok := connection.reserveRequired()
	if !ok {
		t.Fatal("layout reservation saturated")
	}
	transition := ViewTransition{
		Reason: viewTransitionLayout,
		Projection: ClientProjectionPlan{
			SessionID: 7,
			View: ClientView{
				Status: clientStatusState{
					Revision: 5, SessionID: 7,
					Windows: []WindowStatus{{Title: "new-window"}},
				},
				StatusValid: true,
			},
		},
	}
	delivery.finish(clientInstanceCommand{Transition: &transition}, false)
	go connection.runDeliveryWorker()

	required := <-connection.commands
	client := &ClientInstance{sessionID: 7}
	client.currentView = client.orderedViewStatus(
		required.Transition.Projection.View,
		required.Transition.Projection.SessionID,
	)
	client.runClientCommand(<-connection.commands)
	if got := client.currentView.Status.Windows[0].Title; got != "new-window" {
		t.Fatalf("pending old-window status replaced layout status: %q", got)
	}
}

func TestDaemonAssignsMonotonicStatusRevisionAcrossSessions(t *testing.T) {
	d := newCommandTestDaemon(t)
	client := &ClientIdentity{ID: 9}
	firstSession := NewSessionState(1)
	secondSession := NewSessionState(2)
	var first, second clientStatusState
	d.call(func() {
		first = d.clientStatusSnapshotNow(client, firstSession)
		second = d.clientStatusSnapshotNow(client, secondSession)
	})
	if first.Revision == 0 || second.Revision != first.Revision+1 {
		t.Fatalf("status revisions = %d, %d; want consecutive nonzero revisions", first.Revision, second.Revision)
	}
	if first.SessionID != firstSession.ID || second.SessionID != secondSession.ID {
		t.Fatalf("status sessions = %d, %d", first.SessionID, second.SessionID)
	}
}

func TestCoalescedStatusAroundRequiredTransitionKeepsNewestRevision(t *testing.T) {
	connection, done := newPausedDeliveryConnection()
	defer close(done)

	connection.enqueueStatusRefresh(clientStatusState{Revision: 1, SessionID: 7, SessionName: "old"}, true)
	transitionDelivery, ok := connection.reserveRequired()
	if !ok {
		t.Fatal("transition reservation saturated")
	}
	connection.enqueueStatusRefresh(clientStatusState{Revision: 2, SessionID: 7, SessionName: "older"}, true)
	connection.enqueueStatusRefresh(clientStatusState{Revision: 4, SessionID: 7, SessionName: "newest"}, true)
	transition := ViewTransition{Reason: viewTransitionLayout}
	transitionDelivery.finish(clientInstanceCommand{Transition: &transition}, false)
	go connection.runDeliveryWorker()

	required := <-connection.commands
	if required.Transition == nil {
		t.Fatalf("first delivery = %#v, want required transition", required)
	}
	refresh := <-connection.commands
	if !refresh.RefreshStatus || refresh.Status.Revision != 4 {
		t.Fatalf("coalesced status = %#v, want revision 4", refresh)
	}

	client := &ClientInstance{
		sessionID: 7,
		currentView: ClientView{
			Status:      clientStatusState{Revision: 3, SessionID: 7, SessionName: "transition"},
			StatusValid: true,
		},
	}
	client.runClientCommand(refresh)
	if got := client.currentView.Status.SessionName; got != "newest" {
		t.Fatalf("coalesced status install = %q, want newest", got)
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
