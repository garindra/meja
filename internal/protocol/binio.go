package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

var (
	ErrShortPayload     = errors.New("short payload")
	ErrTrailingBytes    = errors.New("trailing bytes")
	ErrInvalidBoolean   = errors.New("invalid boolean")
	ErrLengthOverflow   = errors.New("length overflow")
	ErrInvalidRune      = errors.New("invalid rune")
	ErrInvalidCellWidth = errors.New("invalid cell width")
)

type PayloadWriter struct {
	Buf []byte
}

func (w *PayloadWriter) Uvarint(v uint64) {
	w.Buf = binary.AppendUvarint(w.Buf, v)
}

func (w *PayloadWriter) Byte(v byte) {
	w.Buf = append(w.Buf, v)
}

func (w *PayloadWriter) Bool(v bool) {
	if v {
		w.Buf = append(w.Buf, 1)
		return
	}
	w.Buf = append(w.Buf, 0)
}

func (w *PayloadWriter) Raw(v []byte) {
	w.Buf = append(w.Buf, v...)
}

func (w *PayloadWriter) String(v string) {
	w.Uvarint(uint64(len(v)))
	w.Buf = append(w.Buf, v...)
}

type PayloadReader struct {
	Data []byte
	Off  int
}

func (r *PayloadReader) Uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.Data[r.Off:])
	if n <= 0 {
		if n == 0 {
			return 0, ErrShortPayload
		}
		return 0, ErrLengthOverflow
	}
	r.Off += n
	return v, nil
}

func (r *PayloadReader) Byte() (byte, error) {
	if r.Off >= len(r.Data) {
		return 0, ErrShortPayload
	}
	v := r.Data[r.Off]
	r.Off++
	return v, nil
}

func (r *PayloadReader) Bool() (bool, error) {
	v, err := r.Byte()
	if err != nil {
		return false, err
	}
	switch v {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, ErrInvalidBoolean
	}
}

func (r *PayloadReader) Bytes(max uint64) ([]byte, error) {
	n, err := r.Uvarint()
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("length %d exceeds max %d", n, max)
	}
	if n > uint64(len(r.Data)-r.Off) {
		return nil, ErrShortPayload
	}
	end := r.Off + int(n)
	out := r.Data[r.Off:end]
	r.Off = end
	return out, nil
}

func (r *PayloadReader) String(max uint64) (string, error) {
	b, err := r.Bytes(max)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("invalid utf-8")
	}
	return string(b), nil
}

func (r *PayloadReader) Remaining() []byte {
	return r.Data[r.Off:]
}

func (r *PayloadReader) Done() error {
	if r.Off != len(r.Data) {
		return ErrTrailingBytes
	}
	return nil
}

func readCount(r *PayloadReader, max uint64) (int, error) {
	n, err := r.Uvarint()
	if err != nil {
		return 0, err
	}
	if n > max {
		return 0, fmt.Errorf("count %d exceeds max %d", n, max)
	}
	if n > math.MaxInt {
		return 0, ErrLengthOverflow
	}
	return int(n), nil
}

func validRune(r rune) bool {
	return r >= 0 && r <= utf8.MaxRune && !(r >= 0xD800 && r <= 0xDFFF)
}
