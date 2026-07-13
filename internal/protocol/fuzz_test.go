package protocol

import (
	"bytes"
	"testing"
)

func FuzzDisplayCommandDecoders(f *testing.F) {
	f.Add([]byte{byte(DisplayOpcodeRelayoutBarrier), 1, byte(DisplayOpcodePresent)})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = NewDisplayDecoder(bytes.NewReader(b)).ReadBatch()
	})
}
