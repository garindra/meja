package protocol

import "testing"

func FuzzDecodeInputBytes(f *testing.F) {
	f.Add([]byte{0x01, 'l', 's'})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = DecodeInputBytes(payload)
	})
}

func FuzzDecodeSetRun(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x00, 0x00, 0x01, 0x41, 0x00, 0x01})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = DecodeSetRun(payload)
	})
}

func FuzzDecodeReplacePane(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x41, 0x00, 0x01})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = DecodeReplacePane(payload)
	})
}

func FuzzDecodeWindowList(f *testing.F) {
	f.Add([]byte{0x01, 0x00})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = DecodeWindowList(payload)
	})
}

func FuzzDecodeDefineStyle(f *testing.F) {
	f.Add([]byte{0x01, 0x01, 0x00, 0x00})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = DecodeDefineStyle(payload)
	})
}
