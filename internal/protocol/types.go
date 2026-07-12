package protocol

import "time"

const ALPN = "tali/3"

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
	MsgWindowLayout
	MsgBindRenderStream
	MsgReplacePane
	MsgSetRun
	MsgSetCursor
	MsgSetCursorVisible
	MsgDefineStyle
	MsgPing
	MsgPong
	MsgPaneUpdate
	MsgScrollPane
	MsgStatusBar
)

const (
	StreamTypeManagement = "management"
	StreamTypeInput      = "input"
	StreamTypePaneOutput = "pane-output"
)

type StreamOpen struct {
	StreamType string
	Slot       uint8
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
	Data []byte
}

type InputBytesView struct {
	Data []byte
}

type ResizePane struct {
	Cols uint16
	Rows uint16
}

type RequestPaneSnapshot struct {
	PaneID uint64
}

type Rect struct {
	X      int
	Y      int
	Width  int
	Height int
}

type PanePlacement struct {
	PaneID uint64
	Rect   Rect
}

type WindowLayout struct {
	WindowID       uint64
	FocusedPaneID  uint64
	LayoutRevision uint64
	Panes          []PanePlacement
}

type StatusBar struct {
	Cols   int
	Cells  []Cell
	Styles []StyleDefinition
}

type ScrollPane struct {
	Delta int
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
	Slot              uint8
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
	BindingGeneration uint64
	BaseGeneration    uint64
	Generation        uint64
	Row               int
	Column            int
	Cells             []Cell
}

type CellRun struct {
	Row    int
	Column int
	Cells  []Cell
}

type PaneUpdate struct {
	BindingGeneration    uint64
	BaseGeneration       uint64
	Generation           uint64
	Styles               []StyleDefinition
	Runs                 []CellRun
	CursorChanged        bool
	Cursor               Cursor
	CursorVisibleChanged bool
	CursorVisible        bool
}

type SetCursor struct {
	SessionID         uint64
	WindowID          uint64
	BindingGeneration uint64
	BaseGeneration    uint64
	Generation        uint64
	Cursor            Cursor
}

type SetCursorVisible struct {
	SessionID         uint64
	WindowID          uint64
	BindingGeneration uint64
	BaseGeneration    uint64
	Generation        uint64
	Visible           bool
}

type DefineStyle struct {
	BindingGeneration uint64
	ID                uint32
	Style             Style
}
