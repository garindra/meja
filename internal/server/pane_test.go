package server

import (
	"bytes"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPaneWriterSerializesNetworkInputAndDeviceReply(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	pane := &Pane{ID: 1, PTY: writer, terminal: newTerminal(8, 3)}
	pane.initializeRuntime()
	go pane.run()
	writeFailed := make(chan error, 1)
	go runPTYWriter(pane, func(err error) { writeFailed <- err })

	for _, b := range []byte("用户") {
		if err := pane.sendInput([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	query := ptyReadBuffers.Get().([]byte)
	n := copy(query, "\x1b[?1h\x1b[6n")
	pane.ptyOutput <- query[:n]

	want := []byte("用户\x1b[1;1R")
	got := make([]byte, len(want))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(reader, got)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-writeFailed:
		t.Fatalf("PTY writer failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pane input")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("PTY input = %q, want %q", got, want)
	}
	if !pane.InputMode().applicationCursorKeys {
		t.Fatal("pane main loop did not publish application cursor mode")
	}

	close(pane.ptyOutput)
	<-pane.mainDone
	pane.stop()
	<-pane.writerDone
}

func TestPaneStartupInputWaitsForInitialOutputToSettle(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	pane := &Pane{
		ID:           1,
		PTY:          writer,
		terminal:     newTerminal(16, 3),
		startupInput: []byte("vi mnt.sh"),
	}
	pane.initializeRuntime()
	go pane.run()
	writeFailed := make(chan error, 1)
	go runPTYWriter(pane, func(err error) { writeFailed <- err })
	defer func() {
		close(pane.ptyOutput)
		<-pane.mainDone
		pane.stop()
		<-pane.writerDone
	}()

	prompt := ptyReadBuffers.Get().([]byte)
	n := copy(prompt, "user@host:~$ ")
	pane.ptyOutput <- prompt[:n]

	got := make([]byte, len("vi mnt.sh"))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(reader, got)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-writeFailed:
		t.Fatalf("PTY writer failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for startup input")
	}
	if string(got) != "vi mnt.sh" {
		t.Fatalf("startup input = %q", got)
	}
}

func TestBuildEnvPreservesUTF8Locale(t *testing.T) {
	t.Setenv("LANG", "zh_CN.UTF-8")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "zh_CN.UTF-8")

	env := buildEnv(&user.User{HomeDir: "/home/test", Username: "test"}, "/bin/sh")
	if !slices.Contains(env, "LANG=zh_CN.UTF-8") || !slices.Contains(env, "LC_CTYPE=zh_CN.UTF-8") {
		t.Fatalf("pane environment omitted UTF-8 locale: %#v", env)
	}
}

func TestBuildEnvPreservesPath(t *testing.T) {
	t.Setenv("PATH", "/home/test/bin:/opt/tools/bin")

	env := buildEnv(&user.User{HomeDir: "/home/test", Username: "test"}, "/bin/sh")
	if !slices.Contains(env, "PATH=/home/test/bin:/opt/tools/bin") {
		t.Fatalf("pane environment omitted inherited PATH: %#v", env)
	}
}

func TestBuildPaneEnvInjectsStableMejaContext(t *testing.T) {
	env := buildPaneEnv(&user.User{HomeDir: "/home/test", Username: "test"}, "/bin/sh", 4, paneRequest{
		MejaSocket:        "/srv/meja.sock",
		MejaSessionTarget: "17",
	})
	for _, want := range []string{
		"MEJA_SOCKET=/srv/meja.sock",
		"MEJA_SESSION_TARGET=17",
		"MEJA_PANE_ID=4",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("pane environment omitted %q: %#v", want, env)
		}
	}
}

func TestDefaultShellUsesEnvironmentWithSafeFallback(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if got := defaultShell(); got != "/bin/zsh" {
		t.Fatalf("default shell = %q, want /bin/zsh", got)
	}
	t.Setenv("SHELL", "relative-shell")
	if got := defaultShell(); got != "/bin/sh" {
		t.Fatalf("relative shell fallback = %q, want /bin/sh", got)
	}
}

func TestResolveStartingDirectoryExpandsTargetUserHome(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "projects", "meja")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	unixUser := &user.User{HomeDir: home, Username: "test"}
	for raw, want := range map[string]string{
		"":                home,
		"~":               home,
		"~/projects/meja": project,
		project:           project,
	} {
		got, err := resolveStartingDirectoryForUser(raw, unixUser)
		if err != nil {
			t.Fatalf("resolve %q: %v", raw, err)
		}
		if got != want {
			t.Fatalf("resolve %q = %q, want %q", raw, got, want)
		}
	}
}

func TestResolveStartingDirectoryRejectsAmbiguousOrInvalidPaths(t *testing.T) {
	home := t.TempDir()
	unixUser := &user.User{HomeDir: home, Username: "test"}
	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"relative/path", "~other/path", file, filepath.Join(home, "missing")} {
		if _, err := resolveStartingDirectoryForUser(raw, unixUser); err == nil {
			t.Fatalf("resolve %q unexpectedly succeeded", raw)
		}
	}
}

func TestResolveRootDirectoryUsesHostReferenceForRelativePaths(t *testing.T) {
	reference := t.TempDir()
	want := filepath.Join(reference, "project")
	if err := os.Mkdir(want, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRootDirectory("project", reference)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved root = %q, want %q", got, want)
	}
	if _, err := resolveRootDirectory("project", "relative-reference"); err == nil {
		t.Fatal("relative host reference produced a non-absolute root")
	}
}

func TestResolveRootDirectoryDefaultsToHostUserHome(t *testing.T) {
	want, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveRootDirectory("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("default root = %q, want host home %q", got, want)
	}
}

func TestPaneResizeRunsOnPaneMainLoop(t *testing.T) {
	pane := &Pane{ID: 1, terminal: newTerminal(8, 3)}
	pane.initializeRuntime()
	go pane.run()
	defer func() {
		close(pane.ptyOutput)
		<-pane.mainDone
	}()

	if err := pane.resize(12, 5); err != nil {
		t.Fatal(err)
	}
	if cols, rows := pane.TerminalSize(); cols != 12 || rows != 5 {
		t.Fatalf("terminal size = %dx%d, want 12x5", cols, rows)
	}
}
