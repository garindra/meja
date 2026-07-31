package protocol

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// RenderEncoding identifies the independent encoding used by one reliable
// render frame. The render slot is implicit in the QUIC output stream.
type RenderEncoding uint8

const (
	RenderEncodingRaw RenderEncoding = iota
	RenderEncodingZlib
)

// RenderFrameHeader precedes one bounded display-command payload. It is
// encoded canonically as [flags byte][raw size uvarint][encoded size uvarint].
type RenderFrameHeader struct {
	Encoding    RenderEncoding
	RawSize     uint32
	EncodedSize uint32
}

// DecodedRenderFrame owns a payload valid until the next ReadFrame call.
type DecodedRenderFrame struct {
	Header                RenderFrameHeader
	Payload               []byte
	HeaderSize            int
	DecompressionDuration time.Duration
}

func ValidateRenderFrameHeader(header RenderFrameHeader) error {
	if header.RawSize == 0 || header.EncodedSize == 0 {
		return fmt.Errorf("render frame sizes must be nonzero")
	}
	if header.RawSize > DefaultMaxFrameSize {
		return fmt.Errorf("render raw size %d exceeds max %d", header.RawSize, DefaultMaxFrameSize)
	}
	if header.EncodedSize > DefaultMaxFrameSize {
		return fmt.Errorf("render encoded size %d exceeds max %d", header.EncodedSize, DefaultMaxFrameSize)
	}
	switch header.Encoding {
	case RenderEncodingRaw:
		if header.EncodedSize != header.RawSize {
			return fmt.Errorf("raw render encoded size %d differs from raw size %d", header.EncodedSize, header.RawSize)
		}
	case RenderEncodingZlib:
		if header.EncodedSize >= header.RawSize {
			return fmt.Errorf("zlib render encoded size %d is not smaller than raw size %d", header.EncodedSize, header.RawSize)
		}
	default:
		return fmt.Errorf("unknown render encoding %d", header.Encoding)
	}
	return nil
}

func AppendRenderFrameHeader(dst []byte, header RenderFrameHeader) ([]byte, error) {
	if err := ValidateRenderFrameHeader(header); err != nil {
		return dst, err
	}
	flags := byte(0)
	if header.Encoding == RenderEncodingZlib {
		flags = 1
	}
	dst = append(dst, flags)
	dst = appendUvarint(dst, uint64(header.RawSize))
	dst = appendUvarint(dst, uint64(header.EncodedSize))
	return dst, nil
}

func ReadRenderFrameHeader(r *bufio.Reader) (RenderFrameHeader, error) {
	flags, err := r.ReadByte()
	if err != nil {
		return RenderFrameHeader{}, err
	}
	if flags&^byte(1) != 0 {
		return RenderFrameHeader{}, fmt.Errorf("render frame reserved flags %#x are set", flags&^byte(1))
	}
	rawSize, err := readCanonicalRenderUvarint(r)
	if err != nil {
		return RenderFrameHeader{}, normalizePartialRenderHeaderError(err)
	}
	encodedSize, err := readCanonicalRenderUvarint(r)
	if err != nil {
		return RenderFrameHeader{}, normalizePartialRenderHeaderError(err)
	}
	if rawSize > math.MaxUint32 || encodedSize > math.MaxUint32 {
		return RenderFrameHeader{}, ErrLengthOverflow
	}
	header := RenderFrameHeader{
		Encoding:    RenderEncoding(flags & 1),
		RawSize:     uint32(rawSize),
		EncodedSize: uint32(encodedSize),
	}
	if err := ValidateRenderFrameHeader(header); err != nil {
		return RenderFrameHeader{}, err
	}
	return header, nil
}

func normalizePartialRenderHeaderError(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func readCanonicalRenderUvarint(r *bufio.Reader) (uint64, error) {
	var value uint64
	for index := 0; index < 10; index++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if index == 9 && b > 1 {
			return 0, ErrLengthOverflow
		}
		value |= uint64(b&0x7f) << (7 * index)
		if b < 0x80 {
			if index > 0 && b == 0 {
				return 0, fmt.Errorf("overlong render frame uvarint")
			}
			return value, nil
		}
	}
	return 0, ErrLengthOverflow
}

// RenderFrameReader reads and strictly decodes independent bounded records
// from one long-lived reliable render stream. Returned payload storage is
// reused by the next call.
type RenderFrameReader struct {
	reader        *bufio.Reader
	encoded       []byte
	raw           []byte
	encodedSource bytes.Reader
	zlibReader    io.ReadCloser
}

func NewRenderFrameReader(r io.Reader) *RenderFrameReader {
	return &RenderFrameReader{reader: bufio.NewReader(r)}
}

func (r *RenderFrameReader) ReadFrame() (DecodedRenderFrame, error) {
	header, err := ReadRenderFrameHeader(r.reader)
	if err != nil {
		return DecodedRenderFrame{}, err
	}
	var headerStorage [11]byte
	encodedHeader, err := AppendRenderFrameHeader(headerStorage[:0], header)
	if err != nil {
		return DecodedRenderFrame{}, err
	}
	headerSize := len(encodedHeader)
	encodedSize := int(header.EncodedSize)
	if cap(r.encoded) < encodedSize {
		r.encoded = make([]byte, encodedSize)
	} else {
		r.encoded = r.encoded[:encodedSize]
	}
	if _, err := io.ReadFull(r.reader, r.encoded); err != nil {
		return DecodedRenderFrame{}, io.ErrUnexpectedEOF
	}

	if header.Encoding == RenderEncodingRaw {
		return DecodedRenderFrame{Header: header, Payload: r.encoded, HeaderSize: headerSize}, nil
	}

	rawSize := int(header.RawSize)
	if cap(r.raw) < rawSize {
		r.raw = make([]byte, rawSize)
	} else {
		r.raw = r.raw[:rawSize]
	}
	started := time.Now()
	if err := r.decodeZlib(); err != nil {
		return DecodedRenderFrame{}, err
	}
	return DecodedRenderFrame{
		Header: header, Payload: r.raw, HeaderSize: headerSize,
		DecompressionDuration: time.Since(started),
	}, nil
}

func (r *RenderFrameReader) decodeZlib() error {
	r.encodedSource.Reset(r.encoded)
	if r.zlibReader == nil {
		reader, err := zlib.NewReader(&r.encodedSource)
		if err != nil {
			return fmt.Errorf("open render zlib stream: %w", err)
		}
		r.zlibReader = reader
	} else {
		resetter, ok := r.zlibReader.(zlib.Resetter)
		if !ok {
			return fmt.Errorf("render zlib reader cannot reset")
		}
		if err := resetter.Reset(&r.encodedSource, nil); err != nil {
			r.invalidateZlibReader()
			return fmt.Errorf("reset render zlib stream: %w", err)
		}
	}
	if _, err := io.ReadFull(r.zlibReader, r.raw); err != nil {
		r.invalidateZlibReader()
		return fmt.Errorf("decompress render frame: %w", err)
	}
	var extra [1]byte
	n, err := r.zlibReader.Read(extra[:])
	if n != 0 || err == nil {
		r.invalidateZlibReader()
		return fmt.Errorf("render zlib output exceeds raw size")
	}
	if !errors.Is(err, io.EOF) {
		r.invalidateZlibReader()
		return fmt.Errorf("finish render zlib stream: %w", err)
	}
	if r.encodedSource.Len() != 0 {
		r.invalidateZlibReader()
		return fmt.Errorf("render zlib payload has %d trailing bytes", r.encodedSource.Len())
	}
	return nil
}

func (r *RenderFrameReader) invalidateZlibReader() {
	if r.zlibReader != nil {
		_ = r.zlibReader.Close()
		r.zlibReader = nil
	}
}
