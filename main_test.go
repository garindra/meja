package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/garindra/meja/internal/client"
	"github.com/garindra/meja/internal/server"
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

func shortUnixSocketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "meja-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "meja.sock")
}

func waitForTestServerSocket(t *testing.T, socket string, serverDone chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		select {
		case err := <-serverDone:
			// Preserve the result for the test cleanup after reporting it.
			serverDone <- err
			t.Fatalf("server stopped before creating control socket: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server control socket was not created within 10 seconds")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInteractiveResizeBurstKeepsDetachResponsive(t *testing.T) {
	socket := shortUnixSocketPath(t)
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(serverCtx, server.Config{ControlPath: socket, Stdout: io.Discard, Stderr: io.Discard})
	}()
	t.Cleanup(func() {
		stopServer()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
		}
	})
	waitForTestServerSocket(t, socket, serverDone)

	terminal, frontend, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer frontend.Close()
	if err := pty.Setsize(terminal, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	var outputMu sync.Mutex
	var output bytes.Buffer
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := terminal.Read(buffer)
			if n > 0 {
				outputMu.Lock()
				_, _ = output.Write(buffer[:n])
				outputMu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	waitForOutput := func(want string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			outputMu.Lock()
			found := bytes.Contains(output.Bytes(), []byte(want))
			outputMu.Unlock()
			if found {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("terminal output did not contain %q", want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	var stderr bytes.Buffer
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- run(clientCtx, []string{"-S", socket, "new-session", "--", "/bin/sh"}, frontend, frontend, &stderr)
	}()

	// Answer the frontend's DEC rectangular-scroll capability query as a modern
	// terminal would. This keeps horizontal margins and native pane scrolling
	// enabled while terminal widths alternate across the resize burst.
	waitForOutput("\x1b[?69$p", 3*time.Second)
	if _, err := terminal.Write([]byte("\x1b[?69;1$y")); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("printf '__MEJA_READY__\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput("__MEJA_READY__", 3*time.Second)
	if _, err := terminal.Write([]byte{0x02, '"'}); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("printf '__SECOND_PANE_READY__\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput("__SECOND_PANE_READY__", 3*time.Second)
	// The horizontal split focuses its lower pane. Exercise the same prefix
	// arrow routing that must remain responsive after the resize burst.
	if _, err := terminal.Write([]byte{0x02, 0x1b, '[', 'A'}); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("printf '__FIRST_PANE_FOCUSED__\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput("__FIRST_PANE_FOCUSED__", 3*time.Second)
	if _, err := terminal.Write([]byte{0x02, 0x1b, '[', 'B'}); err != nil {
		t.Fatal(err)
	}
	// Keep the pane renderer busy while resize handoffs move its output lease.
	// Quiet panes do not expose late incremental frames racing a replacement
	// layout, which is the failure mode this integration test is meant to cover.
	if _, err := terminal.Write([]byte("(i=0; while [ \"$i\" -lt 2000 ]; do printf 'resize-output-%04d........................................\\n' \"$i\"; i=$((i+1)); done) &\n")); err != nil {
		t.Fatal(err)
	}

	for index := 0; index < 80; index++ {
		cols := uint16(94)
		if index%2 != 0 {
			cols = 102
		}
		if err := pty.Setsize(terminal, &pty.Winsize{Cols: cols, Rows: 24}); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := terminal.Write([]byte{0x02, 0x1b, '[', 'A'}); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("printf '__FOCUS_AFTER_RESIZE__\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput("__FOCUS_AFTER_RESIZE__", 3*time.Second)
	if _, err := terminal.Write([]byte{0x02, 'd'}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("interactive client failed after resize burst: %v; stderr: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Ctrl+B d was not processed after resize burst; stderr: %s", stderr.String())
	}

	outputMu.Lock()
	reconnected := bytes.Contains(output.Bytes(), []byte("Reconnecting"))
	outputMu.Unlock()
	if reconnected {
		t.Fatal("resize burst broke the display stream and entered reconnect mode")
	}
}

func TestInteractiveShellExitFallsBackToLiveWindow(t *testing.T) {
	socket := shortUnixSocketPath(t)
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(serverCtx, server.Config{ControlPath: socket, Stdout: io.Discard, Stderr: io.Discard})
	}()
	t.Cleanup(func() {
		stopServer()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
		}
	})
	waitForTestServerSocket(t, socket, serverDone)

	terminal, frontend, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer frontend.Close()
	if err := pty.Setsize(terminal, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	var outputMu sync.Mutex
	var output bytes.Buffer
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := terminal.Read(buffer)
			if n > 0 {
				outputMu.Lock()
				_, _ = output.Write(buffer[:n])
				outputMu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	waitForOutput := func(want string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			outputMu.Lock()
			found := bytes.Contains(output.Bytes(), []byte(want))
			outputMu.Unlock()
			if found {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("terminal output did not contain %q", want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	var stderr bytes.Buffer
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- run(clientCtx, []string{"-S", socket, "new-session", "--", "/bin/sh"}, frontend, frontend, &stderr)
	}()
	waitForOutput("\x1b[?69$p", 3*time.Second)
	if _, err := terminal.Write([]byte("\x1b[?69;1$y")); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("printf '__FIRST_WINDOW_READY__\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput("__FIRST_WINDOW_READY__", 3*time.Second)
	if _, err := terminal.Write([]byte{0x02, 'c'}); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("printf '__EXITING_WINDOW_READY__\\n'\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput("__EXITING_WINDOW_READY__", 3*time.Second)
	if _, err := terminal.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}

	// Do not assume a fixed process-exit latency. Repeatedly address the
	// surviving shell until its output proves that fallback input and rendering
	// are both live; commands sent before fallback simply reach the exiting PTY.
	// Keep the expected marker out of the input bytes: a dead/frozen pane can
	// still echo typed text, which previously made this test pass without the
	// fallback shell ever executing the command.
	marker := "__FALLBACK_AFTER_EXIT_739241__"
	markerCommand := "printf '__FALLBACK_AFTER_EXIT_%d__\\n' $((739240+1))\n"
	deadline := time.Now().Add(3 * time.Second)
	for {
		outputMu.Lock()
		found := bytes.Contains(output.Bytes(), []byte(marker))
		outputMu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("surviving window did not accept input/render after shell exit; stderr: %s", stderr.String())
		}
		if _, err := terminal.Write([]byte(markerCommand)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := terminal.Write([]byte{0x02, 'd'}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("client failed after pane-exit fallback: %v; stderr: %s", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not detach after pane-exit fallback")
	}
}

func TestInvocationForwardsCommandArgumentsWithoutCommandParsing(t *testing.T) {
	want := []string{"restore-session", "-t", "work", "--commands=whatever-the-server-supports"}
	cfg := parseTestInvocation(t, want...)
	if !cfg.Local || !reflect.DeepEqual(cfg.CommandArgs, want) {
		t.Fatalf("config = %#v, want exact command argv %v", cfg, want)
	}
}

func TestInvocationExtractsTransportOptionsBeforeCommand(t *testing.T) {
	cfg := parseTestInvocation(t,
		"-h", "alice@example.com",
		"-L", "dev",
		"-i", "/keys/meja",
		"--port=2202",
		"--remote-path", "/opt/meja",
		"restore", "-t", "work",
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

func TestInvocationPreservesCommandFlagsThatCollideWithTransportOptions(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "horizontal split", args: []string{"split-window", "-h"}, want: []string{"split-window", "-h"}},
		{name: "capture start line", args: []string{"capture-pane", "-S", "10"}, want: []string{"capture-pane", "-S", "10"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := parseTestInvocation(t, test.args...)
			if !cfg.Local {
				t.Fatalf("invocation selected remote transport: %#v", cfg)
			}
			if !reflect.DeepEqual(cfg.CommandArgs, test.want) {
				t.Fatalf("forwarded argv = %v, want %v", cfg.CommandArgs, test.want)
			}
		})
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
	profile := parseTestInvocation(t, "-L", "dev", "list-sessions")
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
	t.Setenv("MEJA_PANE_ID", "41")

	cfg := parseTestInvocation(t, "set-root", ".")
	if cfg.SocketSelector.Path != socket || cfg.CallerSessionTarget != "17" || cfg.CallerPaneID != 41 {
		t.Fatalf("in-pane config = %#v", cfg)
	}

	explicit := parseTestInvocation(t, "-L", "other", "set-root", "-t", "work", ".")
	if explicit.SocketSelector.Profile != "other" || explicit.CallerSessionTarget != "" || explicit.CallerPaneID != 0 {
		t.Fatalf("explicit selector retained pane context: %#v", explicit)
	}

	remote := parseTestInvocation(t, "-h", "prod", "new", "-f", "dev.meja")
	if remote.Local || remote.CallerSessionTarget != "" || remote.CallerPaneID != 0 || remote.SocketSelector.Path != "" {
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
