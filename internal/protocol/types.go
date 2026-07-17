package protocol

const (
	ALPN            = "meja/1"
	ProtocolVersion = 7

	// SessionReplacedErrorCode is a terminal QUIC application close: another
	// client has taken ownership of the session, so the displaced client must
	// exit instead of attempting to resume.
	SessionReplacedErrorCode = 0x54414c49
)

const (
	MsgOpenManagementStream uint64 = iota + 1
	MsgOpenInputStream
	_ // former create-pane message; initial panes are command-engine owned
	_ // former pane-created response
	MsgInputBytes
	MsgResizePane
	MsgWindowLayout
	MsgSessionAttach
	MsgSessionAttachOK
	MsgSessionAttachFailed
	MsgSessionResume
	MsgSessionResumeOK
)

const (
	StreamTypeManagement = "management"
	StreamTypeInput      = "input"
)

type StreamOpen struct {
	StreamType string
}

type SessionAttach struct {
	Version int
	Token   string
	Cols    uint16
	Rows    uint16
}

type SessionAttachOK struct {
	Version     int
	ResumeToken string
}

type SessionAttachFailed struct {
	Reason string
}

type SessionResume struct {
	Version     int
	ResumeToken string
	Cols        uint16
	Rows        uint16
}

type SessionResumeOK struct {
	Version int
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
	Blink     bool
	Italic    bool
	Underline bool
	Reverse   bool
	Invisible bool
	FG        Color
	BG        Color
}

// Every pane/render binding reserves style ID 0 for this exact default style.
const CanonicalDefaultStyleID uint32 = 0

func CanonicalDefaultStyle() Style {
	return Style{FG: Color{Mode: "default"}, BG: Color{Mode: "default"}}
}

func IsCanonicalDefaultStyle(style Style) bool {
	return style == CanonicalDefaultStyle()
}

type Cell struct {
	// Cluster is the complete UTF-8 display cluster stored on an anchor cell.
	// An empty cluster with Width 1 is a styled blank; an empty cluster with
	// Width 0 is the continuation of the width-2 anchor immediately to its left.
	Cluster string
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
