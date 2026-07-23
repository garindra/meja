package protocol

import (
	"reflect"
	"testing"
)

func TestClientHandshakeMessagesRoundTrip(t *testing.T) {
	attach := SessionAttach{Token: "attach", Cols: 100, Rows: 40}
	payload, err := EncodeSessionAttach(nil, attach)
	if err != nil {
		t.Fatal(err)
	}
	decodedAttach, err := DecodeSessionAttach(payload)
	if err != nil || !reflect.DeepEqual(decodedAttach, attach) {
		t.Fatalf("attach = %#v, err = %v", decodedAttach, err)
	}

	attachOK := SessionAttachOK{ResumeToken: "resume"}
	payload, err = EncodeSessionAttachOK(nil, attachOK)
	if err != nil {
		t.Fatal(err)
	}
	decodedAttachOK, err := DecodeSessionAttachOK(payload)
	if err != nil || !reflect.DeepEqual(decodedAttachOK, attachOK) {
		t.Fatalf("attach OK = %#v, err = %v", decodedAttachOK, err)
	}

	resume := ClientResume{ResumeToken: "resume", Cols: 100, Rows: 40}
	payload, err = EncodeClientResume(nil, resume)
	if err != nil {
		t.Fatal(err)
	}
	decodedResume, err := DecodeClientResume(payload)
	if err != nil || !reflect.DeepEqual(decodedResume, resume) {
		t.Fatalf("resume = %#v, err = %v", decodedResume, err)
	}

	resumeOK := ClientResumeOK{}
	payload, err = EncodeClientResumeOK(nil, resumeOK)
	if err != nil {
		t.Fatal(err)
	}
	decodedResumeOK, err := DecodeClientResumeOK(payload)
	if err != nil || !reflect.DeepEqual(decodedResumeOK, resumeOK) {
		t.Fatalf("resume OK = %#v, err = %v", decodedResumeOK, err)
	}
}

func TestFrontendInputBytesSourceIdleRoundTrip(t *testing.T) {
	want := FrontendInputBytes{LayoutRevision: 42, SourceIdle: true, Data: []byte{0x1b}}
	payload, err := EncodeFrontendInputBytes(nil, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFrontendInputBytes(payload)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("frontend input = %#v, err = %v", got, err)
	}
}

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
