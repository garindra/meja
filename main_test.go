package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/garindra/meja/internal/client"
	"github.com/garindra/meja/internal/control"
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

func TestDefaultLocalCwdUsesInvokerDirectoryUnlessExplicit(t *testing.T) {
	want, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg := client.Config{}
	if err := setDefaultLocalCwd(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Cwd != want {
		t.Fatalf("default local cwd = %q, want %q", cfg.Cwd, want)
	}

	cfg.Cwd = "~/explicit"
	if err := setDefaultLocalCwd(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Cwd != "~/explicit" {
		t.Fatalf("explicit cwd was replaced with %q", cfg.Cwd)
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
	exact := filepath.Join(t.TempDir(), "meja.sock")
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

func TestDebugEnvironmentConfiguresClientDiagnostics(t *testing.T) {
	t.Setenv("MEJA_DEBUG", "")
	t.Setenv("MEJA_DEBUG_RENDER", "true")
	t.Setenv("MEJA_DEBUG_LOG", "/tmp/meja-render.log")
	cfg := client.Config{}
	applyDebugEnvironment(&cfg)
	if !cfg.RenderDiagnostics || cfg.RenderDiagnosticsLogPath != "/tmp/meja-render.log" {
		t.Fatalf("debug config = %#v", cfg)
	}
}

func TestDebugLogEnvironmentEnablesDiagnostics(t *testing.T) {
	t.Setenv("MEJA_DEBUG", "")
	t.Setenv("MEJA_DEBUG_RENDER", "")
	t.Setenv("MEJA_DEBUG_LOG", "/tmp/meja-render.log")
	cfg := client.Config{}
	applyDebugEnvironment(&cfg)
	if !cfg.RenderDiagnostics {
		t.Fatal("MEJA_DEBUG_LOG did not enable diagnostics")
	}
}

func TestRenderDebugFlagsAreNotAccepted(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var flags connectionFlags
	flags.register(fs)
	if err := fs.Parse([]string{"--debug-render"}); err == nil {
		t.Fatal("--debug-render was accepted")
	}
	if err := fs.Parse([]string{"--debug-render-log", "/tmp/render.log"}); err == nil {
		t.Fatal("--debug-render-log was accepted")
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

func TestWriteSessionListShowsNamesAndAttachmentState(t *testing.T) {
	var output bytes.Buffer
	err := writeSessionList(&output, []control.SessionInfo{
		{ID: 1},
		{ID: 2, Name: "work", Attached: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Active Sessions\nID  NAME       STATUS\n1   <unnamed>  detached\n2   work       attached\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
