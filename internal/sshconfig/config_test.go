package sshconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseTarget(t *testing.T) {
	target, err := ParseTarget("alice@example.com")
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	if !target.HasExplicitUser || target.Username != "alice" || target.Host != "example.com" {
		t.Fatalf("ParseTarget() = %#v", target)
	}

	target, err = ParseTarget("myserver")
	if err != nil {
		t.Fatalf("ParseTarget() host-only error = %v", err)
	}
	if target.HasExplicitUser || target.Username != "" || target.Host != "myserver" {
		t.Fatalf("ParseTarget() host-only = %#v", target)
	}
}

func TestResolveAlias(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
Host myserver
    HostName 192.168.18.11
    User garindra
    IdentityFile ~/.ssh/id_tali
    IdentitiesOnly yes
    Port 2200
`)

	target, _ := ParseTarget("myserver")
	resolved, err := Resolve(target, ResolveOptions{HomeDir: home, LocalUsername: "local"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Hostname != "192.168.18.11" || resolved.Username != "garindra" || resolved.Port != 2200 || !resolved.IdentitiesOnly {
		t.Fatalf("Resolve() = %#v", resolved)
	}
	wantIDs := []string{filepath.Join(home, ".ssh", "id_tali")}
	if !reflect.DeepEqual(resolved.IdentityFiles, wantIDs) {
		t.Fatalf("IdentityFiles = %#v, want %#v", resolved.IdentityFiles, wantIDs)
	}
}

func TestResolveWildcardAndNegatedHost(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
Host *.example.com !blocked.example.com
    User alice
Host blocked.example.com
    User bob
`)

	target, _ := ParseTarget("app.example.com")
	resolved, err := Resolve(target, ResolveOptions{HomeDir: home, LocalUsername: "local"})
	if err != nil {
		t.Fatalf("Resolve() wildcard error = %v", err)
	}
	if resolved.Username != "alice" {
		t.Fatalf("Resolve() username = %q, want alice", resolved.Username)
	}

	target, _ = ParseTarget("blocked.example.com")
	resolved, err = Resolve(target, ResolveOptions{HomeDir: home, LocalUsername: "local"})
	if err != nil {
		t.Fatalf("Resolve() negated error = %v", err)
	}
	if resolved.Username != "bob" {
		t.Fatalf("Resolve() blocked username = %q, want bob", resolved.Username)
	}
}

func TestResolvePrecedenceAndExpansion(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
Host myserver
    HostName target.example.com
    User wrong
    Port 2200
    IdentityFile ~/.ssh/%r@%h-%u
    IdentityFile ~/.ssh/second
`)

	target, _ := ParseTarget("explicit@myserver")
	resolved, err := Resolve(target, ResolveOptions{
		HomeDir:              home,
		LocalUsername:        "local",
		ExplicitIdentityFile: "~/override",
		ExplicitPort:         4434,
		ExplicitPortSet:      true,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Username != "explicit" || resolved.Hostname != "target.example.com" || resolved.Port != 4434 {
		t.Fatalf("Resolve() = %#v", resolved)
	}
	wantIDs := []string{filepath.Join(home, "override")}
	if !reflect.DeepEqual(resolved.IdentityFiles, wantIDs) {
		t.Fatalf("IdentityFiles = %#v, want %#v", resolved.IdentityFiles, wantIDs)
	}
}

func TestResolveMultipleIdentityFilesOrder(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
Host myserver
    IdentityFile ~/.ssh/one
    IdentityFile ~/.ssh/two
`)
	target, _ := ParseTarget("myserver")
	resolved, err := Resolve(target, ResolveOptions{HomeDir: home, LocalUsername: "local"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := []string{
		filepath.Join(home, ".ssh", "one"),
		filepath.Join(home, ".ssh", "two"),
	}
	if !reflect.DeepEqual(resolved.IdentityFiles, want) {
		t.Fatalf("IdentityFiles = %#v, want %#v", resolved.IdentityFiles, want)
	}
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	configDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
