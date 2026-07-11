package auth

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

const TranscriptLabel = "tali-auth-v1"

type Identity struct {
	Signer    ssh.Signer
	PublicKey ssh.PublicKey
	Source    string
}

type SelectOptions struct {
	IdentityFiles  []string
	IdentitiesOnly bool
	Prompt         func(string) ([]byte, error)
	Agent          agent.Agent
	AgentErr       error
}

type candidateIdentity struct {
	Path      string
	PublicKey ssh.PublicKey
}

func (i *Identity) Fingerprint() string {
	return ssh.FingerprintSHA256(i.PublicKey)
}

func (i *Identity) AuthorizedKey() string {
	return string(ssh.MarshalAuthorizedKey(i.PublicKey))
}

func ConnectAgent() (agent.Agent, net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, errors.New("SSH agent unavailable: SSH_AUTH_SOCK is not set")
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH agent unavailable: connect ssh-agent: %w", err)
	}
	return agent.NewClient(conn), conn, nil
}

func SelectIdentity(opts SelectOptions) (*Identity, error) {
	var (
		agentClient agent.Agent
		closeConn   net.Conn
		agentErr    = opts.AgentErr
	)

	if opts.Agent != nil {
		agentClient = opts.Agent
	} else if agentErr == nil {
		var err error
		agentClient, closeConn, err = ConnectAgent()
		if err != nil {
			agentErr = err
		}
	}
	if closeConn != nil {
		defer closeConn.Close()
	}

	return selectIdentity(agentClient, agentErr, opts)
}

func selectIdentity(agentClient agent.Agent, agentErr error, opts SelectOptions) (*Identity, error) {
	candidates, err := buildCandidates(opts.IdentityFiles)
	if err != nil {
		return nil, err
	}

	agentSigners := map[string]ssh.Signer{}
	usedAgent := map[string]bool{}
	if agentClient != nil {
		signers, err := agentClient.Signers()
		if err != nil {
			agentErr = fmt.Errorf("SSH agent unavailable: list agent signers: %w", err)
		} else {
			for _, signer := range signers {
				pub := signer.PublicKey()
				if pub.Type() != ssh.KeyAlgoED25519 {
					continue
				}
				agentSigners[string(pub.Marshal())] = signer
			}
		}
	}

	prompt := opts.Prompt
	if prompt == nil {
		prompt = promptPassphrase
	}

	var errs []string
	for _, candidate := range candidates {
		if candidate.PublicKey != nil {
			blob := string(candidate.PublicKey.Marshal())
			if signer, ok := agentSigners[blob]; ok {
				usedAgent[blob] = true
				return &Identity{
					Signer:    signer,
					PublicKey: signer.PublicKey(),
					Source:    "ssh-agent key " + ssh.FingerprintSHA256(signer.PublicKey()),
				}, nil
			}
		}

		signer, pub, err := loadPrivateIdentity(candidate.Path, prompt)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		return &Identity{
			Signer:    signer,
			PublicKey: pub,
			Source:    candidate.Path,
		}, nil
	}

	if !opts.IdentitiesOnly {
		for blob, signer := range agentSigners {
			if usedAgent[blob] {
				continue
			}
			return &Identity{
				Signer:    signer,
				PublicKey: signer.PublicKey(),
				Source:    "ssh-agent key " + ssh.FingerprintSHA256(signer.PublicKey()),
			}, nil
		}
	}

	if len(errs) > 0 {
		if agentErr != nil && !opts.IdentitiesOnly {
			errs = append(errs, agentErr.Error())
		}
		return nil, errors.New(strings.Join(errs, "; "))
	}
	if agentErr != nil {
		return nil, agentErr
	}
	return nil, errors.New("no usable Ed25519 identity found")
}

func buildCandidates(paths []string) ([]candidateIdentity, error) {
	candidates := make([]candidateIdentity, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("identity file does not exist: %s", path)
			}
			return nil, fmt.Errorf("stat identity file %s: %w", path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("identity file is a directory: %s", path)
		}

		pub, err := candidatePublicKey(path)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidateIdentity{Path: path, PublicKey: pub})
	}
	return candidates, nil
}

func candidatePublicKey(path string) (ssh.PublicKey, error) {
	pubPath := path + ".pub"
	if data, err := os.ReadFile(pubPath); err == nil {
		pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse public key %s: %w", pubPath, err)
		}
		if pub.Type() != ssh.KeyAlgoED25519 {
			return nil, fmt.Errorf("identity file is not an Ed25519 key: %s", path)
		}
		return pub, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			if missing.PublicKey != nil && missing.PublicKey.Type() != ssh.KeyAlgoED25519 {
				return nil, fmt.Errorf("identity file is not an Ed25519 key: %s", path)
			}
			return missing.PublicKey, nil
		}
		return nil, nil
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, fmt.Errorf("identity file is not an Ed25519 key: %s", path)
	}
	return signer.PublicKey(), nil
}

func loadPrivateIdentity(path string, prompt func(string) ([]byte, error)) (ssh.Signer, ssh.PublicKey, error) {
	if err := checkPrivateKeyPermissions(path); err != nil {
		return nil, nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("identity file does not exist: %s", path)
		}
		return nil, nil, fmt.Errorf("read identity file %s: %w", path, err)
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
			return nil, nil, fmt.Errorf("identity file is not an Ed25519 key: %s", path)
		}
		return signer, signer.PublicKey(), nil
	}

	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		passphrase, promptErr := prompt(path)
		if promptErr != nil {
			return nil, nil, promptErr
		}
		defer zeroBytes(passphrase)

		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, passphrase)
		if err != nil {
			return nil, nil, fmt.Errorf("incorrect passphrase for identity file %s: %w", path, err)
		}
		if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
			return nil, nil, fmt.Errorf("identity file is not an Ed25519 key: %s", path)
		}
		return signer, signer.PublicKey(), nil
	}

	return nil, nil, fmt.Errorf("parse identity file %s: %w", path, err)
}

func promptPassphrase(path string) ([]byte, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("identity requires a passphrase but no interactive terminal is available: %s", path)
	}
	defer tty.Close()

	if _, err := fmt.Fprintf(tty, "Enter passphrase for %s: ", filepath.Base(path)); err != nil {
		return nil, fmt.Errorf("prompt for passphrase: %w", err)
	}
	passphrase, err := term.ReadPassword(int(tty.Fd()))
	if _, printErr := fmt.Fprintln(tty); printErr != nil && err == nil {
		err = printErr
	}
	if err != nil {
		return nil, fmt.Errorf("read passphrase for %s: %w", path, err)
	}
	return passphrase, nil
}

func checkPrivateKeyPermissions(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat identity file %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("identity file has unsafe permissions: %s", path)
	}
	return nil
}

func zeroBytes(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}

func SignTranscript(signer ssh.Signer, transcript []byte) ([]byte, error) {
	if algSigner, ok := signer.(ssh.AlgorithmSigner); ok {
		sig, err := algSigner.SignWithAlgorithm(rand.Reader, transcript, ssh.KeyAlgoED25519)
		if err == nil {
			return ssh.Marshal(sig), nil
		}
	}

	sig, err := signer.Sign(rand.Reader, transcript)
	if err != nil {
		return nil, fmt.Errorf("sign transcript: %w", err)
	}
	return ssh.Marshal(sig), nil
}

func publicKeysEqual(a, b ssh.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}
	return bytes.Equal(a.Marshal(), b.Marshal())
}
