package protocol

import "time"

const ALPN = "tali/5"

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
	MsgInputBytes
	MsgResizePane
	MsgWindowLayout
	MsgPing
	MsgPong
	MsgStatusBar
	MsgRelayoutBarrier
	MsgStyleInstall
	MsgSetWritePosition
	MsgSetWriteStyle
	MsgWriteText
	MsgFill
	MsgCursorUpdate
	MsgScroll
	MsgPresent
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

type Rect struct {
	X      int
	Y      int
	Width  int
	Height int
}

type PanePlacement struct {
	PaneID uint64
	Slot   uint8
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

type RelayoutBarrier struct {
	LayoutRevision uint64
}

type StyleInstall struct {
	ID    uint32
	Style Style
}

type SetWritePosition struct {
	Row    int
	Column int
}

type SetWriteStyle struct {
	StyleID uint32
}

type WriteText struct {
	CellWidth uint8
	Text      []byte
}

type Fill struct {
	Columns int
	Rune    rune
	Width   uint8
}

type CursorUpdate struct {
	Cursor  Cursor
	Visible bool
}

type Scroll struct {
	Delta int
}

type Present struct{}

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
