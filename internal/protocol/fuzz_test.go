package protocol

import (
	"bytes"
	"testing"
)

func FuzzDisplayCommandDecoders(f *testing.F) {
	f.Add([]byte{byte(DisplayOpcodeStartRender), 1, 1, 1, byte(DisplayOpcodePresent)})
	f.Fuzz(func(t *testing.T, b []byte) {
		decoder := NewDisplayDecoder(bytes.NewReader(b))
		for {
			if _, _, err := decoder.ReadCommand(); err != nil {
				return
			}
		}
	})
}
