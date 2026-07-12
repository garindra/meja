package protocol

import "testing"

func BenchmarkWriteTextCodec(b *testing.B) {
	msg := WriteText{CellWidth: 1, Text: []byte("garindra@host:~$ ls")}
	for i := 0; i < b.N; i++ {
		payload, _ := EncodeWriteText(nil, msg)
		_, _ = DecodeWriteText(payload)
	}
}
