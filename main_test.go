package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/garindra/meja/internal/client"
	"github.com/garindra/meja/internal/version"
)

func parseTestInvocation(t *testing.T, args ...string) client.Config {
	t.Helper()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	cfg, err := parseInvocation(args, stdin, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestInvocationForwardsCommandArgumentsWithoutCommandParsing(t *testing.T) {
	want := []string{"restore-session", "-t", "work", "--commands=whatever-the-server-supports"}
	cfg := parseTestInvocation(t, want...)
	if !cfg.Local || !reflect.DeepEqual(cfg.CommandArgs, want) {
		t.Fatalf("config = %#v, want exact command argv %v", cfg, want)
	}
}

func TestInvocationExtractsTransportOptionsAnywhereBeforeSeparator(t *testing.T) {
	cfg := parseTestInvocation(t,
		"restore", "-t", "work",
		"-h", "alice@example.com",
		"-L", "dev",
		"-i", "/keys/meja",
		"--port=2202",
		"--remote-path", "/opt/meja",
	)
	if cfg.Local || cfg.Target.Original != "alice@example.com" || cfg.SocketSelector.Profile != "dev" {
		t.Fatalf("remote transport = %#v", cfg)
	}
	if cfg.IdentityFile != "/keys/meja" || cfg.Port != 2202 || !cfg.PortSet || cfg.RemotePath != "/opt/meja" {
		t.Fatalf("SSH options = %#v", cfg)
	}
	want := []string{"restore", "-t", "work"}
	if !reflect.DeepEqual(cfg.CommandArgs, want) {
		t.Fatalf("forwarded argv = %v, want %v", cfg.CommandArgs, want)
	}
}

func TestInvocationPreservesEverythingAtAndAfterSeparator(t *testing.T) {
	want := []string{"new", "-s", "work", "--", "/bin/sh", "-h", "literal", "-L", "literal"}
	cfg := parseTestInvocation(t, want...)
	if !cfg.Local || !reflect.DeepEqual(cfg.CommandArgs, want) {
		t.Fatalf("forwarded argv = %v, want %v", cfg.CommandArgs, want)
	}
}

func TestInvocationDefaultsToNewSessionAndInvokerDirectory(t *testing.T) {
	cfg := parseTestInvocation(t)
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Local || cfg.Cwd != wantCwd || !reflect.DeepEqual(cfg.CommandArgs, []string{"new-session"}) {
		t.Fatalf("default config = %#v", cfg)
	}
}

func TestRemoteInvocationLeavesWorkingDirectoryForForwarder(t *testing.T) {
	cfg := parseTestInvocation(t, "-h", "prod", "new-session", "-r", "~/work")
	if cfg.Local || cfg.Cwd != "" {
		t.Fatalf("remote config = %#v", cfg)
	}
	want := []string{"new-session", "-r", "~/work"}
	if !reflect.DeepEqual(cfg.CommandArgs, want) {
		t.Fatalf("forwarded argv = %v, want %v", cfg.CommandArgs, want)
	}
}

func TestTrailingPositionalArgumentIsForwardedToCommand(t *testing.T) {
	wantArgs := []string{"restore", "-t", "work", "prod"}
	cfg := parseTestInvocation(t, wantArgs...)
	if !cfg.Local {
		t.Fatalf("trailing command argument selected remote transport: %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.CommandArgs, wantArgs) {
		t.Fatalf("forwarded argv = %v, want %v", cfg.CommandArgs, wantArgs)
	}
}

func TestInvocationAcceptsProfileAndSocketSelectors(t *testing.T) {
	profile := parseTestInvocation(t, "list-sessions", "-L", "dev")
	if profile.SocketSelector.Profile != "dev" {
		t.Fatalf("profile selector = %#v", profile.SocketSelector)
	}
	exactPath := filepath.Join(t.TempDir(), "meja.sock")
	exact := parseTestInvocation(t, "-S", exactPath, "list-sessions")
	if exact.SocketSelector.Path != exactPath {
		t.Fatalf("socket selector = %#v", exact.SocketSelector)
	}
}

func TestInvocationUsesInjectedPaneContextForPlainLocalCommands(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "meja.sock")
	t.Setenv("MEJA_SOCKET", socket)
	t.Setenv("MEJA_SESSION_TARGET", "17")

	cfg := parseTestInvocation(t, "set-root", ".")
	if cfg.SocketSelector.Path != socket || cfg.CallerSessionTarget != "17" {
		t.Fatalf("in-pane config = %#v", cfg)
	}

	explicit := parseTestInvocation(t, "-L", "other", "set-root", "-t", "work", ".")
	if explicit.SocketSelector.Profile != "other" || explicit.CallerSessionTarget != "" {
		t.Fatalf("explicit selector retained pane context: %#v", explicit)
	}

	remote := parseTestInvocation(t, "-h", "prod", "new", "-f", "dev.meja")
	if remote.Local || remote.CallerSessionTarget != "" || remote.SocketSelector.Path != "" {
		t.Fatalf("remote invocation retained local pane context: %#v", remote)
	}
}

func TestInvocationRejectsInvalidTransportEnvelope(t *testing.T) {
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	for _, args := range [][]string{
		{"-L", "dev", "-S", "/tmp/dev.sock", "ls"},
		{"-h", "one", "--host", "two", "ls"},
		{"--port", "70000", "ls"},
		{"-h"},
		{"--host=", "ls"},
	} {
		if _, err := parseInvocation(args, stdin, io.Discard, io.Discard); err == nil {
			t.Fatalf("invocation %v was accepted", args)
		}
	}
}

func TestVersionCommandOutput(t *testing.T) {
	previous := version.Value
	version.Value = "v1.2.3"
	t.Cleanup(func() { version.Value = previous })

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"version"}, stdin, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "meja 1.2.3\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
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
