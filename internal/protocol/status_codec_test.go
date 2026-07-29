package protocol

import (
	"reflect"
	"strings"
	"testing"
)

func TestClientStatusRoundTrip(t *testing.T) {
	tests := []ClientStatus{
		{
			Revision: 7, SessionID: 41, SessionName: "work",
			ServerHostname: "host", ServerHome: "/home/test", Root: "/home/test/src",
			Windows: []ClientStatusWindow{
				{WindowID: 91, Index: 0, Title: "shell", Active: true},
				{WindowID: 92, Index: 1, Title: "editor", Zoomed: true},
			},
			Kind: ClientStatusNormal,
		},
		{
			Revision: 8, SessionID: 41, SessionName: "work",
			ServerHostname: "host", ServerHome: "/home/test", Root: "/home/test/src",
			Windows: []ClientStatusWindow{{WindowID: 91, Index: 0, Title: "shell", Active: true}},
			Kind:    ClientStatusPrompt,
			Prompt: ClientStatusPromptState{
				Mode: ClientStatusPromptText, Label: "(rename-window) ", Text: "編輯", Cursor: 1,
			},
		},
		{
			Revision: 9, SessionID: 41, SessionName: "work",
			ServerHostname: "host", ServerHome: "/home/test", Root: "/home/test/src",
			Windows: []ClientStatusWindow{},
			Kind:    ClientStatusMessage,
			Message: ClientStatusMessageState{ID: 3, Text: "window closed"},
		},
	}
	for _, want := range tests {
		payload, err := EncodeClientStatus(nil, want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeClientStatus(payload)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("status = %#v, want %#v", got, want)
		}
	}
}

func TestClientStatusCodecRejectsInvalidValues(t *testing.T) {
	valid := ClientStatus{Revision: 1, Kind: ClientStatusNormal}
	tests := []struct {
		name string
		edit func(*ClientStatus)
	}{
		{name: "zero revision", edit: func(msg *ClientStatus) { msg.Revision = 0 }},
		{name: "invalid kind", edit: func(msg *ClientStatus) { msg.Kind = 99 }},
		{name: "negative index", edit: func(msg *ClientStatus) {
			msg.Windows = []ClientStatusWindow{{WindowID: 1, Index: -1}}
		}},
		{name: "invalid prompt mode", edit: func(msg *ClientStatus) {
			msg.Kind = ClientStatusPrompt
			msg.Prompt.Mode = 99
		}},
		{name: "prompt cursor", edit: func(msg *ClientStatus) {
			msg.Kind = ClientStatusPrompt
			msg.Prompt = ClientStatusPromptState{Mode: ClientStatusPromptText, Text: "x", Cursor: 2}
		}},
		{name: "oversized string", edit: func(msg *ClientStatus) {
			msg.SessionName = strings.Repeat("x", int(MaxStringLen)+1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := valid
			test.edit(&msg)
			if _, err := EncodeClientStatus(nil, msg); err == nil {
				t.Fatal("EncodeClientStatus accepted invalid value")
			}
		})
	}
}

func TestDecodeClientStatusRejectsMalformedPayload(t *testing.T) {
	payload, err := EncodeClientStatus(nil, ClientStatus{
		Revision: 1,
		Kind:     ClientStatusPrompt,
		Prompt:   ClientStatusPromptState{Mode: ClientStatusPromptText, Text: "abc", Cursor: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeClientStatus(append(payload, 0)); err == nil {
		t.Fatal("DecodeClientStatus accepted trailing bytes")
	}
	if _, err := DecodeClientStatus(nil); err == nil {
		t.Fatal("DecodeClientStatus accepted an empty payload")
	}
}
