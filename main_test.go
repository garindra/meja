package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCommandAfterTarget(t *testing.T) {
	command, err := commandAfterTarget([]string{"--", "/bin/sh", "-l"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(command, []string{"/bin/sh", "-l"}) {
		t.Fatalf("command = %#v", command)
	}
}

func TestParseGlobalOptionsDefaultsToDefaultProfile(t *testing.T) {
	selector, args, err := parseGlobalOptions([]string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	if selector.Profile != "default" || selector.Path != "" || !reflect.DeepEqual(args, []string{"ls"}) {
		t.Fatalf("selector=%#v args=%v", selector, args)
	}
}

func TestParseGlobalOptionsAcceptsProfileAndSocket(t *testing.T) {
	selector, args, err := parseGlobalOptions([]string{"-L", "dev", "attach", "-t", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if selector.Profile != "dev" || !reflect.DeepEqual(args, []string{"attach", "-t", "3"}) {
		t.Fatalf("selector=%#v args=%v", selector, args)
	}
	exact := filepath.Join(t.TempDir(), "tali.sock")
	selector, _, err = parseGlobalOptions([]string{"-S", exact, "ls"})
	if err != nil || selector.Path != exact {
		t.Fatalf("selector=%#v err=%v", selector, err)
	}
}

func TestParseGlobalOptionsRejectsProfileAndSocketTogether(t *testing.T) {
	if _, _, err := parseGlobalOptions([]string{"-L", "dev", "-S", "/tmp/dev.sock"}); err == nil {
		t.Fatal("-L with -S was accepted")
	}
}

func TestUnrecognizedFirstWordRoutesToRemoteConnect(t *testing.T) {
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{"prod"}, stdin, &stdout, &stderr)
	if _, isUsageError := err.(usageError); isUsageError {
		t.Fatalf("shorthand was treated as a command error: %v", err)
	}
}

func TestCommandAfterTargetRequiresSeparator(t *testing.T) {
	if _, err := commandAfterTarget([]string{"uname"}); err == nil {
		t.Fatal("command without -- was accepted")
	}
}
