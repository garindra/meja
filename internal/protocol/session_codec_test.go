package protocol

import (
	"reflect"
	"testing"
)

func TestClientInstanceSessionMessagesRoundTrip(t *testing.T) {
	attach := SessionAttach{Version: ProtocolVersion, Token: "attach", Cols: 100, Rows: 40}
	payload, err := EncodeSessionAttach(nil, attach)
	if err != nil {
		t.Fatal(err)
	}
	decodedAttach, err := DecodeSessionAttach(payload)
	if err != nil || !reflect.DeepEqual(decodedAttach, attach) {
		t.Fatalf("attach = %#v, err = %v", decodedAttach, err)
	}

	attachOK := SessionAttachOK{Version: ProtocolVersion, ResumeToken: "resume"}
	payload, err = EncodeSessionAttachOK(nil, attachOK)
	if err != nil {
		t.Fatal(err)
	}
	decodedAttachOK, err := DecodeSessionAttachOK(payload)
	if err != nil || !reflect.DeepEqual(decodedAttachOK, attachOK) {
		t.Fatalf("attach OK = %#v, err = %v", decodedAttachOK, err)
	}

	resume := SessionResume{Version: ProtocolVersion, ResumeToken: "resume", Cols: 100, Rows: 40}
	payload, err = EncodeSessionResume(nil, resume)
	if err != nil {
		t.Fatal(err)
	}
	decodedResume, err := DecodeSessionResume(payload)
	if err != nil || !reflect.DeepEqual(decodedResume, resume) {
		t.Fatalf("resume = %#v, err = %v", decodedResume, err)
	}

	resumeOK := SessionResumeOK{Version: ProtocolVersion}
	payload, err = EncodeSessionResumeOK(nil, resumeOK)
	if err != nil {
		t.Fatal(err)
	}
	decodedResumeOK, err := DecodeSessionResumeOK(payload)
	if err != nil || !reflect.DeepEqual(decodedResumeOK, resumeOK) {
		t.Fatalf("resume OK = %#v, err = %v", decodedResumeOK, err)
	}
}
