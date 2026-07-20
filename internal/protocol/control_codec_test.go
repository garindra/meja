package protocol

import (
	"reflect"
	"testing"
)

func TestFrontendTerminalWriteRoundTrip(t *testing.T) {
	want := FrontendTerminalWrite{Data: []byte("\x1b[?1003h")}
	payload, err := EncodeFrontendTerminalWrite(nil, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFrontendTerminalWrite(payload)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal write = %#v, err = %v", got, err)
	}
}

func TestFrontendRegisterTerminalExitCommandRoundTrip(t *testing.T) {
	want := FrontendRegisterTerminalExitCommand{Data: []byte("\x1b[?1003l")}
	payload, err := EncodeFrontendRegisterTerminalExitCommand(nil, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFrontendRegisterTerminalExitCommand(payload)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal exit command = %#v, err = %v", got, err)
	}
}
