package protocol

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var ErrFrameTooLarge = errors.New("frame too large")

type Frame struct {
	Type    uint64
	Payload []byte
}

type Decoder struct {
	r            *bufio.Reader
	maxFrameSize uint64
	buf          []byte
}

func NewDecoder(r io.Reader, maxFrameSize uint64) *Decoder {
	if maxFrameSize == 0 {
		maxFrameSize = DefaultMaxFrameSize
	}
	return &Decoder{
		r:            bufio.NewReader(r),
		maxFrameSize: maxFrameSize,
	}
}

func (d *Decoder) ReadFrame() (Frame, error) {
	msgType, err := binary.ReadUvarint(d.r)
	if err != nil {
		return Frame{}, err
	}

	payloadLen, err := binary.ReadUvarint(d.r)
	if err != nil {
		return Frame{}, err
	}
	if payloadLen > d.maxFrameSize {
		return Frame{}, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, payloadLen, d.maxFrameSize)
	}

	if uint64(cap(d.buf)) < payloadLen {
		d.buf = make([]byte, payloadLen)
	} else {
		d.buf = d.buf[:payloadLen]
	}
	if _, err := io.ReadFull(d.r, d.buf); err != nil {
		return Frame{}, err
	}

	return Frame{Type: msgType, Payload: d.buf}, nil
}

type Encoder struct {
	w io.Writer
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

func (e *Encoder) WriteFrame(frame Frame) error {
	var header [binary.MaxVarintLen64 * 2]byte
	n := binary.PutUvarint(header[:], frame.Type)
	n += binary.PutUvarint(header[n:], uint64(len(frame.Payload)))

	if _, err := e.w.Write(header[:n]); err != nil {
		return err
	}
	if len(frame.Payload) == 0 {
		return nil
	}
	_, err := e.w.Write(frame.Payload)
	return err
}
