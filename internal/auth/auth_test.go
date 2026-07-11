package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestTranscriptDeterminism(t *testing.T) {
	ts := time.Unix(1720000000, 1234).UTC()
	a := BuildTranscript("alice", "fp", "id-1", "nonce-1", ts)
	b := BuildTranscript("alice", "fp", "id-1", "nonce-1", ts)
	if !bytes.Equal(a, b) {
		t.Fatalf("BuildTranscript() is not deterministic")
	}
}

func TestVerifySignature(t *testing.T) {
	pub, signer := testSigner(t)
	transcript := BuildTranscript("alice", ssh.FingerprintSHA256(pub), "id", "nonce", time.Now().UTC())
	blob, err := SignTranscript(signer, transcript)
	if err != nil {
		t.Fatalf("SignTranscript() error = %v", err)
	}

	sig := &ssh.Signature{}
	if err := ssh.Unmarshal(blob, sig); err != nil {
		t.Fatalf("ssh.Unmarshal() error = %v", err)
	}
	if err := VerifySignature(pub, transcript, sig); err != nil {
		t.Fatalf("VerifySignature() error = %v", err)
	}
}

func TestExpiredChallengeRejected(t *testing.T) {
	pub, signer := testSigner(t)
	v := NewVerifier()
	now := time.Unix(1720000000, 0).UTC()
	v.now = func() time.Time { return now }
	v.randReader = bytes.NewReader(bytes.Repeat([]byte{1}, 64))
	v.ttl = time.Second

	challenge, err := v.issue("alice", pub)
	if err != nil {
		t.Fatalf("issue() error = %v", err)
	}

	v.now = func() time.Time { return now.Add(2 * time.Second) }
	transcript := BuildTranscript("alice", ssh.FingerprintSHA256(pub), challenge.ID, challenge.Nonce, challenge.ExpiresAt)
	blob, err := SignTranscript(signer, transcript)
	if err != nil {
		t.Fatalf("SignTranscript() error = %v", err)
	}

	if err := v.Verify("alice", ssh.FingerprintSHA256(pub), challenge.ID, blob); err != ErrChallengeExpired {
		t.Fatalf("Verify() error = %v, want %v", err, ErrChallengeExpired)
	}
}

func TestReusedChallengeRejected(t *testing.T) {
	pub, signer := testSigner(t)
	v := NewVerifier()
	now := time.Unix(1720000000, 0).UTC()
	v.now = func() time.Time { return now }
	v.randReader = bytes.NewReader(bytes.Repeat([]byte{2}, 64))

	challenge, err := v.issue("alice", pub)
	if err != nil {
		t.Fatalf("issue() error = %v", err)
	}

	transcript := BuildTranscript("alice", ssh.FingerprintSHA256(pub), challenge.ID, challenge.Nonce, challenge.ExpiresAt)
	blob, err := SignTranscript(signer, transcript)
	if err != nil {
		t.Fatalf("SignTranscript() error = %v", err)
	}

	if err := v.Verify("alice", ssh.FingerprintSHA256(pub), challenge.ID, blob); err != nil {
		t.Fatalf("Verify() first error = %v", err)
	}
	if err := v.Verify("alice", ssh.FingerprintSHA256(pub), challenge.ID, blob); err != ErrChallengeReused {
		t.Fatalf("Verify() second error = %v, want %v", err, ErrChallengeReused)
	}
}

func testSigner(t *testing.T) (ssh.PublicKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatalf("NewSignerFromSigner() error = %v", err)
	}
	return signer.PublicKey(), signer
}
