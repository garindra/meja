package protocol

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	CommandProtocolVersion  = 1
	CommandBootstrapVersion = 2
	CommandMaxFrameSize     = 4 << 20
	CommandOutputChunkSize  = 32 << 10
	DefaultUDPMin           = 60000
	DefaultUDPMax           = 61000
)

const (
	CommandFrameStdout = "stdout"
	CommandFrameStderr = "stderr"
	CommandFrameAttach = "attach"
	CommandFrameExit   = "exit"
)

type CommandBootstrap struct {
	Version        int       `json:"version"`
	SessionID      uint64    `json:"sessionId"`
	Port           uint16    `json:"port"`
	AttachToken    string    `json:"attachToken"`
	ExpiresAt      time.Time `json:"expiresAt"`
	CertSPKISHA256 string    `json:"certSpkiSha256"`
}

type CommandRequest struct {
	Version          int      `json:"version"`
	Args             []string `json:"args"`
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	TerminalCols     uint16   `json:"terminalCols,omitempty"`
	TerminalRows     uint16   `json:"terminalRows,omitempty"`
}

type CommandFrame struct {
	Version   int               `json:"version"`
	Type      string            `json:"type"`
	Data      []byte            `json:"data,omitempty"`
	Bootstrap *CommandBootstrap `json:"bootstrap,omitempty"`
	ExitCode  int               `json:"exitCode,omitempty"`
}

func WriteCommandRequest(w io.Writer, request CommandRequest) error {
	request.Version = CommandProtocolVersion
	request.Args = append([]string(nil), request.Args...)
	return writeCommandPacket(w, request)
}

func ReadCommandRequest(r io.Reader) (CommandRequest, error) {
	var request CommandRequest
	if err := readCommandPacket(r, &request); err != nil {
		return CommandRequest{}, err
	}
	if request.Version != CommandProtocolVersion || len(request.Args) == 0 {
		return CommandRequest{}, errors.New("invalid command request")
	}
	return request, nil
}

func WriteCommandFrame(w io.Writer, frame CommandFrame) error {
	frame.Version = CommandProtocolVersion
	return writeCommandPacket(w, frame)
}

func ReadCommandFrame(r io.Reader) (CommandFrame, error) {
	var frame CommandFrame
	if err := readCommandPacket(r, &frame); err != nil {
		return CommandFrame{}, err
	}
	if frame.Version != CommandProtocolVersion {
		return CommandFrame{}, fmt.Errorf("unsupported command frame version %d", frame.Version)
	}
	return frame, nil
}

func WriteCommandOutput(w io.Writer, frameType string, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), CommandOutputChunkSize)
		if err := WriteCommandFrame(w, CommandFrame{Type: frameType, Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func writeCommandPacket(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) > CommandMaxFrameSize {
		return fmt.Errorf("command frame exceeds %d bytes", CommandMaxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readCommandPacket(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > CommandMaxFrameSize {
		return fmt.Errorf("invalid command frame size %d", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}

func (b CommandBootstrap) Validate(now time.Time) error {
	if b.Version != CommandBootstrapVersion {
		return fmt.Errorf("unsupported bootstrap version %d", b.Version)
	}
	if b.SessionID == 0 || b.Port < DefaultUDPMin || b.Port > DefaultUDPMax {
		return errors.New("invalid bootstrap session or port")
	}
	if b.ExpiresAt.IsZero() || !b.ExpiresAt.After(now) {
		return errors.New("bootstrap has expired")
	}
	token, err := base64.RawURLEncoding.DecodeString(b.AttachToken)
	if err != nil || len(token) != 32 {
		return errors.New("invalid bootstrap attach token")
	}
	if len(b.CertSPKISHA256) != 64 {
		return errors.New("invalid certificate SPKI hash")
	}
	if _, err := hex.DecodeString(b.CertSPKISHA256); err != nil {
		return errors.New("invalid certificate SPKI hash")
	}
	return nil
}

func NewAuthToken() ([]byte, error) {
	token := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return nil, err
	}
	return token, nil
}

func EncodeAuthToken(token []byte) string {
	return base64.RawURLEncoding.EncodeToString(token)
}

func EqualAuthToken(encoded string, token []byte) bool {
	got, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare(got, token) == 1
}
