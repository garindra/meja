package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestSelectIdentityPrefersMatchingAgentKey(t *testing.T) {
	dir := t.TempDir()
	keyA := writeED25519Identity(t, dir, "a", nil)
	keyB := writeED25519Identity(t, dir, "b", nil)

	keyring := agent.NewKeyring()
	addAgentKey(t, keyring, keyA.private)
	addAgentKey(t, keyring, keyB.private)

	identity, err := selectIdentity(keyring, nil, SelectOptions{
		IdentityFiles: []string{keyB.path},
		Agent:         keyring,
	})
	if err != nil {
		t.Fatalf("selectIdentity() error = %v", err)
	}
	if !publicKeysEqual(identity.PublicKey, keyB.signer.PublicKey()) {
		t.Fatalf("selected public key does not match configured key")
	}
	if !strings.HasPrefix(identity.Source, "ssh-agent key ") {
		t.Fatalf("Source = %q, want ssh-agent source", identity.Source)
	}
}

func TestSelectIdentityFallsBackToDisk(t *testing.T) {
	dir := t.TempDir()
	keyA := writeED25519Identity(t, dir, "disk", nil)
	keyring := agent.NewKeyring()

	identity, err := selectIdentity(keyring, nil, SelectOptions{
		IdentityFiles: []string{keyA.path},
		Agent:         keyring,
	})
	if err != nil {
		t.Fatalf("selectIdentity() error = %v", err)
	}
	if identity.Source != keyA.path {
		t.Fatalf("Source = %q, want %q", identity.Source, keyA.path)
	}
}

func TestSelectIdentityEncryptedKey(t *testing.T) {
	dir := t.TempDir()
	passphrase := []byte("test-passphrase")
	keyA := writeED25519Identity(t, dir, "enc", passphrase)

	identity, err := selectIdentity(nil, errorsNew("SSH agent unavailable"), SelectOptions{
		IdentityFiles: []string{keyA.path},
		Prompt: func(string) ([]byte, error) {
			return append([]byte(nil), passphrase...), nil
		},
	})
	if err != nil {
		t.Fatalf("selectIdentity() error = %v", err)
	}
	if !publicKeysEqual(identity.PublicKey, keyA.signer.PublicKey()) {
		t.Fatalf("selected public key mismatch")
	}
}

func TestSelectIdentityRejectsNonED25519(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id_rsa")
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "rsa")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = selectIdentity(nil, nil, SelectOptions{IdentityFiles: []string{path}})
	if err == nil || !strings.Contains(err.Error(), "not an Ed25519 key") {
		t.Fatalf("selectIdentity() error = %v, want non-Ed25519 error", err)
	}
}

func TestSelectIdentityRejectsUnsafePermissions(t *testing.T) {
	dir := t.TempDir()
	keyA := writeED25519Identity(t, dir, "unsafe", nil)
	if err := os.Chmod(keyA.path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := selectIdentity(nil, nil, SelectOptions{IdentityFiles: []string{keyA.path}})
	if err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("selectIdentity() error = %v, want unsafe permissions error", err)
	}
}

type writtenIdentity struct {
	path    string
	private ed25519.PrivateKey
	signer  ssh.Signer
}

func writeED25519Identity(t *testing.T, dir, name string, passphrase []byte) writtenIdentity {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	var block *pem.Block
	if len(passphrase) > 0 {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, name, passphrase)
	} else {
		block, err = ssh.MarshalPrivateKey(priv, name)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644); err != nil {
		t.Fatal(err)
	}
	return writtenIdentity{path: path, private: priv, signer: signer}
}

func addAgentKey(t *testing.T, keyring agent.Agent, key ed25519.PrivateKey) {
	t.Helper()
	if err := keyring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
}

func errorsNew(msg string) error {
	return &staticError{msg: msg}
}

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }
