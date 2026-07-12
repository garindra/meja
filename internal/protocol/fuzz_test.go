package protocol

import "testing"

func FuzzDisplayCommandDecoders(f *testing.F) {
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = DecodeRelayoutBarrier(b)
		_, _ = DecodeStyleInstall(b)
		_, _ = DecodeSetWritePosition(b)
		_, _ = DecodeSetWriteStyle(b)
		_, _ = DecodeWriteText(b)
		_, _ = DecodeFill(b)
		_, _ = DecodeCursorUpdate(b)
		_, _ = DecodeScroll(b)
		_, _ = DecodePresent(b)
	})
}
