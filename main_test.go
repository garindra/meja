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
	cfg := parseTestInvocation(t, "-h", "prod", "new-session", "-c", "~/work")
	if cfg.Local || cfg.Cwd != "" {
		t.Fatalf("remote config = %#v", cfg)
	}
	want := []string{"new-session", "-c", "~/work"}
	if !reflect.DeepEqual(cfg.CommandArgs, want) {
		t.Fatalf("forwarded argv = %v, want %v", cfg.CommandArgs, want)
	}
}

func TestLegacyTrailingHostPreservesRemotePathAndDaemonArguments(t *testing.T) {
	remotePath := "/home/garindra/extra-storage/home/garindra/projects/tali/bin/linux-amd64/meja"
	cfg := parseTestInvocation(t,
		"-L", "dev",
		"restore", "-t", "YOYO",
		"--remote-path="+remotePath,
		"ubuntu-kas8",
	)
	if cfg.Local || cfg.Target.Original != "ubuntu-kas8" {
		t.Fatalf("SSH target = %#v", cfg.Target)
	}
	if cfg.RemotePath != remotePath {
		t.Fatalf("remote path = %q, want %q", cfg.RemotePath, remotePath)
	}
	wantArgs := []string{"restore", "-t", "YOYO"}
	if !reflect.DeepEqual(cfg.CommandArgs, wantArgs) {
		t.Fatalf("forwarded argv = %v, want %v", cfg.CommandArgs, wantArgs)
	}
}

func TestLegacyTrailingHostWorksAroundInitialCommandSeparator(t *testing.T) {
	cfg := parseTestInvocation(t, "new", "-s", "work", "prod", "--", "/bin/sh", "-h", "literal")
	if cfg.Local || cfg.Target.Original != "prod" {
		t.Fatalf("SSH target = %#v", cfg.Target)
	}
	wantArgs := []string{"new", "-s", "work", "--", "/bin/sh", "-h", "literal"}
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
