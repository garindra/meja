package main

import (
	"reflect"
	"testing"
)

func TestParseAfterTargetAcceptsSessionFlagAfterTarget(t *testing.T) {
	command, session, err := parseAfterTarget([]string{"-s", "42", "--", "/bin/sh", "-l"})
	if err != nil {
		t.Fatal(err)
	}
	if session != "42" || !reflect.DeepEqual(command, []string{"/bin/sh", "-l"}) {
		t.Fatalf("command=%#v session=%q", command, session)
	}
}

func TestParseAfterTargetLeavesRemoteArgsAloneAfterDoubleDash(t *testing.T) {
	command, session, err := parseAfterTarget([]string{"--", "-s", "not-a-session"})
	if err != nil {
		t.Fatal(err)
	}
	if session != "" || !reflect.DeepEqual(command, []string{"-s", "not-a-session"}) {
		t.Fatalf("command=%#v session=%q", command, session)
	}
}
