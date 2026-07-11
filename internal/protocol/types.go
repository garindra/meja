package protocol

import "time"

const ALPN = "tali/2"

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
	MsgCreateWindow
	MsgWindowCreated
	MsgCloseWindow
	MsgWindowClosed
	MsgSelectWindow
	MsgWindowSelected
	MsgListWindows
	MsgWindowList
	MsgWindowTitleChanged
	MsgBindRenderStream
	MsgReplacePane
	MsgSetRun
	MsgSetCursor
	MsgSetCursorVisible
	MsgDefineStyle
	MsgPing
	MsgPong
)

const (
	StreamTypeManagement = "management"
	StreamTypeInput      = "input"
	StreamTypePaneOutput = "pane-output"
)

type StreamOpen struct {
	StreamType string
	PaneID     uint64
}

type ClientHello struct {
	Version int
}

type AuthBegin struct {
	Username  string
	PublicKey string
}

type AuthChallenge struct {
	ChallengeID string
	Nonce       string
	ExpiresAt   time.Time
}

type AuthResponse struct {
	ChallengeID string
	Signature   []byte
}

type AuthOK struct {
	Username string
	HomeDir  string
	Shell    string
}

type AuthFailed struct {
	Reason string
}

type CreatePane struct {
	Cwd  string
	Argv []string
	Cols uint16
	Rows uint16
}

type PaneCreated struct {
	PaneID uint64
}

type PaneExited struct {
	PaneID   uint64
	ExitCode int
	Signal   string
}

type InputBytes struct {
	PaneID uint64
	Data   []byte
}

type InputBytesView struct {
	PaneID uint64
	Data   []byte
}

type ResizePane struct {
	PaneID uint64
	Cols   uint16
	Rows   uint16
}

type RequestPaneSnapshot struct {
	PaneID uint64
}

type CreateWindow struct {
	Cwd  string
	Argv []string
}

type WindowInfo struct {
	WindowID uint64
	PaneID   uint64
	Index    int
	Title    string
	Active   bool
}

type WindowCreated struct {
	Window WindowInfo
}

type CloseWindow struct {
	WindowID uint64
}

type WindowClosed struct {
	WindowID uint64
}

type SelectWindow struct {
	WindowID uint64
}

type WindowSelected struct {
	WindowID uint64
	PaneID   uint64
}

type ListWindows struct{}

type WindowList struct {
	Windows        []WindowInfo
	ActiveWindowID uint64
}

type WindowTitleChanged struct {
	WindowID uint64
	Title    string
}

type Ping struct {
	Seq           uint64
	SentUnixMilli int64
}

type Pong struct {
	Seq           uint64
	SentUnixMilli int64
}

type Color struct {
	Mode  string
	Index uint8
	R     uint8
	G     uint8
	B     uint8
}

type Style struct {
	Bold      bool
	Dim       bool
	Italic    bool
	Underline bool
	Reverse   bool
	FG        Color
	BG        Color
}

type Cell struct {
	Rune    rune
	StyleID uint32
	Width   uint8
}

type Cursor struct {
	X int
	Y int
}

type StyleDefinition struct {
	ID    uint32
	Style Style
}

type BindRenderStream struct {
	SessionID         uint64
	WindowID          uint64
	PaneID            uint64
	BindingGeneration uint64
}

type ReplacePane struct {
	SessionID         uint64
	WindowID          uint64
	PaneID            uint64
	BindingGeneration uint64
	Generation        uint64
	Cols              int
	Rows              int
	Cells             []Cell
	Styles            []StyleDefinition
	Cursor            Cursor
	CursorVisible     bool
}

type SetRun struct {
	SessionID         uint64
	WindowID          uint64
	PaneID            uint64
	BindingGeneration uint64
	BaseGeneration    uint64
	Generation        uint64
	Row               int
	Column            int
	Cells             []Cell
}

type SetCursor struct {
	SessionID         uint64
	WindowID          uint64
	PaneID            uint64
	BindingGeneration uint64
	BaseGeneration    uint64
	Generation        uint64
	Cursor            Cursor
}

type SetCursorVisible struct {
	SessionID         uint64
	WindowID          uint64
	PaneID            uint64
	BindingGeneration uint64
	BaseGeneration    uint64
	Generation        uint64
	Visible           bool
}

type DefineStyle struct {
	PaneID            uint64
	BindingGeneration uint64
	ID                uint32
	Style             Style
}
