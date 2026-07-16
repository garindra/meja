package protocol

import (
	"bytes"
	"reflect"
	"testing"
)

func TestCommandRequestRoundTripPreservesArgumentsAndContext(t *testing.T) {
	want := CommandRequest{
		Args:             []string{"new", "-s", "my work", "--", "printf", "a b"},
		WorkingDirectory: "/srv/work tree",
		TerminalCols:     123,
		TerminalRows:     45,
	}
	var wire bytes.Buffer
	if err := WriteCommandRequest(&wire, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCommandRequest(&wire)
	if err != nil {
		t.Fatal(err)
	}
	want.Version = CommandProtocolVersion
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %#v, want %#v", got, want)
	}
}

func TestCommandFrameRejectsOversizedPayload(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteCommandFrame(&wire, CommandFrame{Type: CommandFrameStdout, Data: make([]byte, CommandMaxFrameSize)}); err == nil {
		t.Fatal("oversized command frame was accepted")
	}
}
