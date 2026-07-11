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
	MsgRequestPaneSnapshot
	MsgReplacePane
	MsgSetRun
	MsgSetCursor
	MsgSetCursorVisible
	MsgDefineStyle
)

const (
	StreamTypeManagement = "management"
	StreamTypeInput      = "input"
	StreamTypePaneOutput = "pane-output"
)

type StreamOpen struct {
	StreamType string `json:"stream_type"`
	PaneID     uint64 `json:"pane_id"`
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
	PaneID uint64 `json:"pane_id"`
}

type PaneExited struct {
	PaneID   uint64 `json:"pane_id"`
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
}

type InputBytes struct {
	PaneID uint64 `json:"pane_id"`
	Data   []byte `json:"data"`
}

type ResizePane struct {
	PaneID uint64 `json:"pane_id"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

type RequestPaneSnapshot struct {
	PaneID uint64 `json:"pane_id"`
}

type Color struct {
	Mode  string `json:"mode"`
	Index uint8  `json:"index,omitempty"`
	R     uint8  `json:"r,omitempty"`
	G     uint8  `json:"g,omitempty"`
	B     uint8  `json:"b,omitempty"`
}

type Style struct {
	Bold      bool  `json:"bold,omitempty"`
	Italic    bool  `json:"italic,omitempty"`
	Underline bool  `json:"underline,omitempty"`
	Reverse   bool  `json:"reverse,omitempty"`
	FG        Color `json:"fg"`
	BG        Color `json:"bg"`
}

type Cell struct {
	Rune    rune   `json:"rune"`
	StyleID uint32 `json:"style_id"`
	Width   uint8  `json:"width"`
}

type Cursor struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type StyleDefinition struct {
	ID    uint32 `json:"id"`
	Style Style  `json:"style"`
}

type ReplacePane struct {
	SessionID     uint64            `json:"session_id"`
	WindowID      uint64            `json:"window_id"`
	PaneID        uint64            `json:"pane_id"`
	Generation    uint64            `json:"generation"`
	Cols          int               `json:"cols"`
	Rows          int               `json:"rows"`
	Cells         []Cell            `json:"cells"`
	Styles        []StyleDefinition `json:"styles"`
	Cursor        Cursor            `json:"cursor"`
	CursorVisible bool              `json:"cursor_visible"`
}

type SetRun struct {
	SessionID      uint64 `json:"session_id"`
	WindowID       uint64 `json:"window_id"`
	PaneID         uint64 `json:"pane_id"`
	BaseGeneration uint64 `json:"base_generation"`
	Generation     uint64 `json:"generation"`
	Row            int    `json:"row"`
	Column         int    `json:"column"`
	Cells          []Cell `json:"cells"`
}

type SetCursor struct {
	SessionID      uint64 `json:"session_id"`
	WindowID       uint64 `json:"window_id"`
	PaneID         uint64 `json:"pane_id"`
	BaseGeneration uint64 `json:"base_generation"`
	Generation     uint64 `json:"generation"`
	Cursor         Cursor `json:"cursor"`
}

type SetCursorVisible struct {
	SessionID      uint64 `json:"session_id"`
	WindowID       uint64 `json:"window_id"`
	PaneID         uint64 `json:"pane_id"`
	BaseGeneration uint64 `json:"base_generation"`
	Generation     uint64 `json:"generation"`
	Visible        bool   `json:"visible"`
}

type DefineStyle struct {
	PaneID uint64 `json:"pane_id"`
	ID     uint32 `json:"id"`
	Style  Style  `json:"style"`
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
