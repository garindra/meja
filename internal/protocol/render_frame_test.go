package protocol

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"testing"
)

func compressRenderTestPayload(t testing.TB, raw []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	w := zlib.NewWriter(&encoded)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func appendRenderTestFrame(t testing.TB, dst []byte, encoding RenderEncoding, raw, encoded []byte) []byte {
	t.Helper()
	header := RenderFrameHeader{Encoding: encoding, RawSize: uint32(len(raw)), EncodedSize: uint32(len(encoded))}
	var err error
	dst, err = AppendRenderFrameHeader(dst, header)
	if err != nil {
		t.Fatal(err)
	}
	return append(dst, encoded...)
}

func TestRenderFrameHeaderCanonicalRoundTrips(t *testing.T) {
	for _, header := range []RenderFrameHeader{
		{Encoding: RenderEncodingRaw, RawSize: 127, EncodedSize: 127},
		{Encoding: RenderEncodingZlib, RawSize: 4096, EncodedSize: 128},
	} {
		wire, err := AppendRenderFrameHeader(nil, header)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadRenderFrameHeader(bufio.NewReader(bytes.NewReader(wire)))
		if err != nil || got != header {
			t.Fatalf("header=%#v err=%v want %#v", got, err, header)
		}
	}
}

func TestRenderFrameHeaderRejectsMalformedValues(t *testing.T) {
	oversize := uint32(DefaultMaxFrameSize + 1)
	for name, header := range map[string]RenderFrameHeader{
		"zero raw":         {Encoding: RenderEncodingRaw, EncodedSize: 1},
		"zero encoded":     {Encoding: RenderEncodingRaw, RawSize: 1},
		"oversize raw":     {Encoding: RenderEncodingRaw, RawSize: oversize, EncodedSize: oversize},
		"oversize encoded": {Encoding: RenderEncodingRaw, RawSize: 1, EncodedSize: oversize},
		"raw mismatch":     {Encoding: RenderEncodingRaw, RawSize: 2, EncodedSize: 1},
		"zlib not smaller": {Encoding: RenderEncodingZlib, RawSize: 2, EncodedSize: 2},
		"unknown encoding": {Encoding: 2, RawSize: 2, EncodedSize: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateRenderFrameHeader(header); err == nil {
				t.Fatalf("accepted %#v", header)
			}
		})
	}
}

func TestRenderFrameHeaderRejectsReservedAndInvalidUvarints(t *testing.T) {
	for name, wire := range map[string][]byte{
		"reserved": {0x02, 0x01, 0x01},
		"overlong": {0x00, 0x81, 0x00, 0x01},
		"overflow": {0x00, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadRenderFrameHeader(bufio.NewReader(bytes.NewReader(wire))); err == nil {
				t.Fatalf("accepted %x", wire)
			}
		})
	}
}

func TestRenderFrameHeaderEOFClassification(t *testing.T) {
	if _, err := ReadRenderFrameHeader(bufio.NewReader(bytes.NewReader(nil))); !errors.Is(err, io.EOF) {
		t.Fatalf("empty header error=%v", err)
	}
	for _, wire := range [][]byte{{0}, {0, 0x80}, {0, 1}} {
		if _, err := ReadRenderFrameHeader(bufio.NewReader(bytes.NewReader(wire))); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("partial %x error=%v", wire, err)
		}
	}
}

func TestRenderFrameReaderRawZlibAndBackToBack(t *testing.T) {
	rawOne := bytes.Repeat([]byte("raw-one-"), 32)
	rawTwo := bytes.Repeat([]byte("zlib-two-"), 64)
	wire := appendRenderTestFrame(t, nil, RenderEncodingRaw, rawOne, rawOne)
	wire = appendRenderTestFrame(t, wire, RenderEncodingZlib, rawTwo, compressRenderTestPayload(t, rawTwo))
	reader := NewRenderFrameReader(bytes.NewReader(wire))
	first, err := reader.ReadFrame()
	if err != nil || !bytes.Equal(first.Payload, rawOne) {
		t.Fatalf("first payload error=%v", err)
	}
	second, err := reader.ReadFrame()
	if err != nil || !bytes.Equal(second.Payload, rawTwo) {
		t.Fatalf("second payload error=%v", err)
	}
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("final error=%v", err)
	}
}

func TestRenderFrameReaderIndependentZlibFrames(t *testing.T) {
	wants := [][]byte{bytes.Repeat([]byte("alpha"), 100), bytes.Repeat([]byte("beta"), 100)}
	var wire []byte
	for _, raw := range wants {
		wire = appendRenderTestFrame(t, wire, RenderEncodingZlib, raw, compressRenderTestPayload(t, raw))
	}
	reader := NewRenderFrameReader(bytes.NewReader(wire))
	for _, want := range wants {
		frame, err := reader.ReadFrame()
		if err != nil || !bytes.Equal(frame.Payload, want) {
			t.Fatalf("payload error=%v", err)
		}
	}
}

func TestRenderFrameReaderRejectsPartialPayload(t *testing.T) {
	raw := []byte("abc")
	wire := appendRenderTestFrame(t, nil, RenderEncodingRaw, raw, raw)
	if _, err := NewRenderFrameReader(bytes.NewReader(wire[:len(wire)-1])).ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial payload error=%v", err)
	}
}

func TestRenderFrameReaderRejectsMalformedZlib(t *testing.T) {
	largeRaw := bytes.Repeat([]byte("abcdefgh"), 128)
	valid := compressRenderTestPayload(t, largeRaw)
	checksum := bytes.Clone(valid)
	checksum[len(checksum)-1] ^= 0xff
	truncated := valid[:len(valid)-1]
	shortRaw := largeRaw[:len(largeRaw)-1]
	longRaw := append(bytes.Clone(largeRaw), 'x')
	trailing := append(bytes.Clone(valid), 0)
	concatenated := append(bytes.Clone(valid), valid...)
	for name, test := range map[string]struct {
		rawSize uint32
		encoded []byte
	}{
		"checksum":       {uint32(len(largeRaw)), checksum},
		"truncated":      {uint32(len(largeRaw)), truncated},
		"malformed":      {uint32(len(largeRaw)), []byte{1, 2, 3}},
		"short output":   {uint32(len(largeRaw)), compressRenderTestPayload(t, shortRaw)},
		"long output":    {uint32(len(largeRaw)), compressRenderTestPayload(t, longRaw)},
		"trailing bytes": {uint32(len(largeRaw)), trailing},
		"concatenated":   {uint32(len(largeRaw)), concatenated},
	} {
		t.Run(name, func(t *testing.T) {
			header := RenderFrameHeader{Encoding: RenderEncodingZlib, RawSize: test.rawSize, EncodedSize: uint32(len(test.encoded))}
			wire, err := AppendRenderFrameHeader(nil, header)
			if err != nil {
				t.Fatal(err)
			}
			wire = append(wire, test.encoded...)
			if _, err := NewRenderFrameReader(bytes.NewReader(wire)).ReadFrame(); err == nil {
				t.Fatal("accepted malformed zlib frame")
			}
		})
	}
}

type repeatingRenderReader struct {
	data   []byte
	offset int
}

func (r *repeatingRenderReader) Read(dst []byte) (int, error) {
	for index := range dst {
		dst[index] = r.data[r.offset]
		r.offset++
		if r.offset == len(r.data) {
			r.offset = 0
		}
	}
	return len(dst), nil
}

func BenchmarkRenderFrameHeader(b *testing.B) {
	header := RenderFrameHeader{Encoding: RenderEncodingZlib, RawSize: 4096, EncodedSize: 1024}
	var storage [11]byte
	wire, err := AppendRenderFrameHeader(storage[:0], header)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("encode", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := AppendRenderFrameHeader(storage[:0], header); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode", func(b *testing.B) {
		reader := bufio.NewReaderSize(&repeatingRenderReader{data: wire}, len(wire))
		b.ReportAllocs()
		for range b.N {
			if _, err := ReadRenderFrameHeader(reader); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkRenderFrameReaderWarm(b *testing.B) {
	raw := bytes.Repeat([]byte("render-frame-"), 256)
	for _, encoding := range []RenderEncoding{RenderEncodingRaw, RenderEncodingZlib} {
		b.Run(map[RenderEncoding]string{RenderEncodingRaw: "raw", RenderEncodingZlib: "zlib"}[encoding], func(b *testing.B) {
			encoded := raw
			if encoding == RenderEncodingZlib {
				encoded = compressRenderTestPayload(b, raw)
			}
			wire := appendRenderTestFrame(b, nil, encoding, raw, encoded)
			reader := NewRenderFrameReader(&repeatingRenderReader{data: wire})
			if _, err := reader.ReadFrame(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for range b.N {
				if _, err := reader.ReadFrame(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
