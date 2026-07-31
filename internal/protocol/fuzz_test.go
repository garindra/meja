package protocol

import (
	"bufio"
	"bytes"
	"testing"
)

func FuzzDisplayCommandDecoders(f *testing.F) {
	f.Add([]byte{byte(DisplayOpcodeStartRender), 1, 1, 1})
	f.Add([]byte{byte(DisplayOpcodeScrollRegion), 0, 24, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		decoder := NewDisplayDecoder(bytes.NewReader(b))
		for {
			if _, _, err := decoder.ReadCommand(); err != nil {
				return
			}
		}
	})
}

func FuzzRenderFrameHeader(f *testing.F) {
	f.Add([]byte{0, 1, 1})
	f.Add([]byte{1, 128, 1, 16})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ReadRenderFrameHeader(bufio.NewReader(bytes.NewReader(b)))
	})
}

func FuzzRenderFrameReader(f *testing.F) {
	f.Add([]byte{0, 4, 4, byte(DisplayOpcodeStartRender), 1, 1, 1})
	f.Add([]byte{1, 128, 1, 16, 0x78, 0x9c})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = NewRenderFrameReader(bytes.NewReader(b)).ReadFrame()
	})
}
