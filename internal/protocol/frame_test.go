package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	want := Frame{Type: MsgSessionAttach, Payload: []byte(`{"version":1,"sessionId":1,"attachToken":"token"}`)}
	if err := enc.WriteFrame(want); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}

	got, err := NewDecoder(&buf, 1024).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("ReadFrame() = %#v, want %#v", got, want)
	}
}

func TestFrameOversized(t *testing.T) {
	var buf bytes.Buffer
	var header [binary.MaxVarintLen64 * 2]byte
	n := binary.PutUvarint(header[:], MsgSessionAttach)
	n += binary.PutUvarint(header[n:], 10)
	if _, err := buf.Write(header[:n]); err != nil {
		t.Fatal(err)
	}

	_, err := NewDecoder(&buf, 8).ReadFrame()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadFrame() error = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameMalformed(t *testing.T) {
	data := []byte{0x04, 0x05, 0x01}
	_, err := NewDecoder(bytes.NewReader(data), 1024).ReadFrame()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame() error = %v, want io.ErrUnexpectedEOF", err)
	}
}
