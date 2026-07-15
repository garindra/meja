package server

import (
	"testing"
	"time"

	"github.com/garindra/meja/internal/protocol"
)

func TestSessionCoordinatorSerializesOperations(t *testing.T) {
	state := &Session{operations: make(chan sessionOperation)}
	go state.runOperations()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- state.coordinate(func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- state.coordinate(func() error {
			close(secondStarted)
			return nil
		})
	}()
	select {
	case <-secondStarted:
		t.Fatal("second operation overlapped the first")
	case <-time.After(10 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second operation did not start after the first completed")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestStaleControllerCannotRunSessionOperation(t *testing.T) {
	current := make(chan protocol.Frame)
	stale := make(chan protocol.Frame)
	state := &Session{
		operations: make(chan sessionOperation),
	}
	state.connection = &Connection{managementOut: current}
	go state.runOperations()
	Connection := &Connection{Session: state, managementOut: stale}
	run := false
	if err := state.coordinateConnection(Connection, func() error {
		run = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if run {
		t.Fatal("stale connection handler operation ran")
	}
}
