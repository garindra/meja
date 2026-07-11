package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	ErrUnsupportedKeyType = errors.New("only ssh-ed25519 keys are supported")
	ErrChallengeExpired   = errors.New("challenge expired")
	ErrChallengeReused    = errors.New("challenge already used")
	ErrUnknownChallenge   = errors.New("unknown challenge")
)

type Challenge struct {
	ID        string
	Nonce     string
	ExpiresAt time.Time
}

type BeginResult struct {
	User        *user.User
	PublicKey   ssh.PublicKey
	Fingerprint string
	Challenge   Challenge
}

type challengeRecord struct {
	username    string
	fingerprint string
	publicKey   ssh.PublicKey
	nonce       string
	expiresAt   time.Time
	used        bool
}

type Verifier struct {
	mu         sync.Mutex
	challenges map[string]*challengeRecord
	now        func() time.Time
	randReader io.Reader
	ttl        time.Duration
}

func NewVerifier() *Verifier {
	return &Verifier{
		challenges: make(map[string]*challengeRecord),
		now:        time.Now,
		randReader: rand.Reader,
		ttl:        2 * time.Minute,
	}
}

func (v *Verifier) Begin(username, authorizedKey string) (*BeginResult, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return nil, fmt.Errorf("parse offered public key: %w", err)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return nil, ErrUnsupportedKeyType
	}

	unixUser, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("lookup user %q: %w", username, err)
	}
	if err := ensureAuthorizedKey(unixUser, pub); err != nil {
		return nil, err
	}

	challenge, err := v.issue(username, pub)
	if err != nil {
		return nil, err
	}

	return &BeginResult{
		User:        unixUser,
		PublicKey:   pub,
		Fingerprint: ssh.FingerprintSHA256(pub),
		Challenge:   challenge,
	}, nil
}

func (v *Verifier) Verify(username, fingerprint string, responseChallengeID string, signatureBlob []byte) error {
	v.mu.Lock()
	record, ok := v.challenges[responseChallengeID]
	if !ok {
		v.mu.Unlock()
		return ErrUnknownChallenge
	}

	now := v.now()
	if now.After(record.expiresAt) {
		delete(v.challenges, responseChallengeID)
		v.mu.Unlock()
		return ErrChallengeExpired
	}
	if record.used {
		v.mu.Unlock()
		return ErrChallengeReused
	}
	if record.username != username || record.fingerprint != fingerprint {
		v.mu.Unlock()
		return errors.New("challenge subject mismatch")
	}

	record.used = true
	v.mu.Unlock()

	sig := &ssh.Signature{}
	if err := ssh.Unmarshal(signatureBlob, sig); err != nil {
		return fmt.Errorf("unmarshal signature: %w", err)
	}

	transcript := BuildTranscript(username, fingerprint, responseChallengeID, record.nonce, record.expiresAt)
	if err := VerifySignature(record.publicKey, transcript, sig); err != nil {
		v.mu.Lock()
		record.used = false
		v.mu.Unlock()
		return err
	}

	return nil
}

func BuildTranscript(username, fingerprint, challengeID, nonce string, expiresAt time.Time) []byte {
	return []byte(strings.Join([]string{
		TranscriptLabel,
		username,
		fingerprint,
		challengeID,
		nonce,
		expiresAt.UTC().Format(time.RFC3339Nano),
	}, "\n"))
}

func VerifySignature(pub ssh.PublicKey, transcript []byte, sig *ssh.Signature) error {
	if sig.Format != ssh.KeyAlgoED25519 {
		return fmt.Errorf("unexpected signature format %q", sig.Format)
	}
	if err := pub.Verify(transcript, sig); err != nil {
		return fmt.Errorf("verify signature: %w", err)
	}
	return nil
}

func (v *Verifier) issue(username string, pub ssh.PublicKey) (Challenge, error) {
	var rawID [16]byte
	if _, err := io.ReadFull(v.randReader, rawID[:]); err != nil {
		return Challenge{}, fmt.Errorf("generate challenge id: %w", err)
	}
	var rawNonce [32]byte
	if _, err := io.ReadFull(v.randReader, rawNonce[:]); err != nil {
		return Challenge{}, fmt.Errorf("generate challenge nonce: %w", err)
	}

	challenge := Challenge{
		ID:        hex.EncodeToString(rawID[:]),
		Nonce:     base64.StdEncoding.EncodeToString(rawNonce[:]),
		ExpiresAt: v.now().Add(v.ttl).UTC(),
	}

	v.mu.Lock()
	v.challenges[challenge.ID] = &challengeRecord{
		username:    username,
		fingerprint: ssh.FingerprintSHA256(pub),
		publicKey:   pub,
		nonce:       challenge.Nonce,
		expiresAt:   challenge.ExpiresAt,
	}
	v.mu.Unlock()

	return challenge, nil
}

func ensureAuthorizedKey(unixUser *user.User, offered ssh.PublicKey) error {
	path := filepath.Join(unixUser.HomeDir, ".ssh", "authorized_keys")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	for len(data) > 0 {
		pub, _, options, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		data = rest

		if len(options) > 0 {
			return fmt.Errorf("unsupported authorized_keys options for %s", path)
		}
		if bytes.Equal(pub.Marshal(), offered.Marshal()) {
			return nil
		}
	}

	return errors.New("offered key is not authorized for requested account")
}
