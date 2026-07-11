package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

const ALPN = "tali/1"

const (
	MsgOpenManagementStream uint64 = iota + 1
	MsgOpenInputStream
	MsgOpenPaneOutputStream
	MsgClientHello
	MsgAuthBegin
	MsgAuthChallenge
	MsgAuthResponse
	MsgAuthOK
	MsgAuthFailed
	MsgCreatePane
	MsgPaneCreated
	MsgPaneExited
	MsgInputBytes
	MsgResizePane
	MsgPTYOutput
)

const (
	StreamTypeManagement = "management"
	StreamTypeInput      = "input"
	StreamTypePaneOutput = "pane-output"
)

type StreamOpen struct {
	StreamType string `json:"stream_type"`
	PaneID     string `json:"pane_id,omitempty"`
}

type ClientHello struct {
	Version int `json:"version"`
}

type AuthBegin struct {
	Username  string `json:"username"`
	PublicKey string `json:"public_key"`
}

type AuthChallenge struct {
	ChallengeID string    `json:"challenge_id"`
	Nonce       string    `json:"nonce"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type AuthResponse struct {
	ChallengeID string `json:"challenge_id"`
	Signature   []byte `json:"signature"`
}

type AuthOK struct {
	Username string `json:"username"`
	HomeDir  string `json:"home_dir"`
	Shell    string `json:"shell"`
}

type AuthFailed struct {
	Reason string `json:"reason"`
}

type CreatePane struct {
	Cwd  string   `json:"cwd"`
	Argv []string `json:"argv,omitempty"`
	Cols uint16   `json:"cols"`
	Rows uint16   `json:"rows"`
}

type PaneCreated struct {
	PaneID string `json:"pane_id"`
}

type PaneExited struct {
	PaneID   string `json:"pane_id,omitempty"`
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
}

type InputBytes struct {
	PaneID string `json:"pane_id"`
	Data   []byte `json:"data"`
}

type ResizePane struct {
	PaneID string `json:"pane_id"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

type PTYOutput struct {
	Data []byte `json:"data"`
}

func EncodeMessage(msgType uint64, v any) (Frame, error) {
	var payload []byte
	var err error
	if v != nil {
		payload, err = json.Marshal(v)
		if err != nil {
			return Frame{}, fmt.Errorf("marshal message %d: %w", msgType, err)
		}
	}
	return Frame{Type: msgType, Payload: payload}, nil
}

func DecodeMessage(frame Frame, v any) error {
	if len(frame.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(frame.Payload, v); err != nil {
		return fmt.Errorf("unmarshal message %d: %w", frame.Type, err)
	}
	return nil
}
