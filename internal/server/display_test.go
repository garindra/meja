package server

import (
	"tali/internal/protocol"
	"testing"
)

func TestSendCellCommandsUsesFillAndText(t *testing.T) {
	ch := make(chan protocol.Frame, 16)
	ctrl := &controller{}
	cells := []protocol.Cell{{Rune: ' ', Width: 1}, {Rune: ' ', Width: 1}, {Rune: ' ', Width: 1}, {Rune: 'o', Width: 1}, {Rune: 'k', Width: 1}}
	if err := ctrl.sendCellCommands(ch, 2, 4, cells); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var types []uint64
	for frame := range ch {
		types = append(types, frame.Type)
	}
	want := []uint64{protocol.MsgSetWritePosition, protocol.MsgSetWriteStyle, protocol.MsgFill, protocol.MsgSetWritePosition, protocol.MsgSetWriteStyle, protocol.MsgWriteText}
	if len(types) != len(want) {
		t.Fatalf("types=%v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types=%v want=%v", types, want)
		}
	}
}
