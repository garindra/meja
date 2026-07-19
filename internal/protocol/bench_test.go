package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestRepresentativeDisplayWireSizes(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*DisplayEncoder)
	}{
		{"single typed character", func(e *DisplayEncoder) {
			e.AppendSetWritePosition(SetWritePosition{Row: 1, Column: 1})
			e.AppendSetWriteStyle(SetWriteStyle{StyleID: 1})
			e.AppendWriteText(WriteText{CellWidth: 1, Text: []byte("x")})
			e.AppendPresent()
		}},
		{"ls-like output", func(e *DisplayEncoder) {
			e.AppendSetWritePosition(SetWritePosition{Row: 0, Column: 0})
			e.AppendWriteTextUTF8Default([]byte("total 12\ndrwxr-xr-x file\n"))
			e.AppendPresent()
		}},
		{"TUI-like batch", func(e *DisplayEncoder) {
			for row := 0; row < 24; row++ {
				e.AppendSetWritePosition(SetWritePosition{Row: row, Column: 0})
				e.AppendWriteTextUTF8Default(bytes.Repeat([]byte(" "), 80))
			}
			e.AppendPresent()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoder := NewDisplayEncoder(nil)
			tc.make(encoder)
			before := conceptualFramedDisplaySize(encoder.Bytes())
			commands := countDisplayCommands(encoder.Bytes())
			t.Logf("commands=%d before_framed=%d after_display=%d savings=%d", commands, before, len(encoder.Bytes()), before-len(encoder.Bytes()))
		})
	}
}

func countDisplayCommands(stream []byte) int {
	decoder := NewDisplayDecoder(bytes.NewReader(stream))
	count := 0
	for {
		if _, _, err := decoder.ReadCommand(); err != nil {
			return count
		}
		count++
	}
}

func BenchmarkDisplayCommandCodec(b *testing.B) {
	text := []byte("garindra@host:~$ ls")
	for i := 0; i < b.N; i++ {
		encoder := NewDisplayEncoder(nil)
		_ = encoder.AppendWriteTextUTF8(text)
		encoder.AppendPresent()
		decoder := NewDisplayDecoder(bytes.NewReader(encoder.Bytes()))
		for {
			if _, _, err := decoder.ReadCommand(); err != nil {
				break
			}
		}
	}
}

// conceptualFramedDisplaySize models the removed [type][length][payload]
// wrapper around each command, using the already encoded command fields.
func conceptualFramedDisplaySize(stream []byte) int {
	decoder := NewDisplayDecoder(bytes.NewReader(stream))
	total := 0
	for {
		command, wireBytes, err := decoder.ReadCommand()
		if err != nil {
			break
		}
		payload := int(wireBytes) - 1
		var buf [binary.MaxVarintLen64]byte
		frameType := binary.PutUvarint(buf[:], uint64(command.Opcode))
		total += frameType + binary.PutUvarint(buf[:], uint64(payload)) + payload
	}
	return total
}
