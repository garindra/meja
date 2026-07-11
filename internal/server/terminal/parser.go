package terminal

import (
	"strconv"
	"strings"
)

type parserState int

const (
	parserText parserState = iota
	parserESC
	parserESCCharset
	parserCSI
	parserOSC
	parserOSCESC
)

type Parser struct {
	state   parserState
	csiBuf  strings.Builder
	utf8Buf []byte
	oscBuf  strings.Builder
}

type CSISequence struct {
	PrivatePrefix byte
	Params        []int
	Intermediates []byte
	Final         byte
}

func parseCSIParams(raw string) []int {
	return parseDelimitedParams(raw)
}

func parseCSISequence(raw string) CSISequence {
	if raw == "" {
		return CSISequence{}
	}
	seq := CSISequence{Final: raw[len(raw)-1]}
	body := raw[:len(raw)-1]
	if len(body) > 0 {
		switch body[0] {
		case '?', '>', '<', '=':
			seq.PrivatePrefix = body[0]
			body = body[1:]
		}
	}
	paramEnd := 0
	for paramEnd < len(body) {
		b := body[paramEnd]
		if (b >= '0' && b <= '9') || b == ';' {
			paramEnd++
			continue
		}
		break
	}
	paramText := body[:paramEnd]
	if paramText != "" {
		seq.Params = parseDelimitedParams(paramText)
	}
	if paramEnd < len(body) {
		seq.Intermediates = []byte(body[paramEnd:])
	}
	return seq
}

func parseDelimitedParams(raw string) []int {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			out = append(out, 0)
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			out = append(out, 0)
			continue
		}
		out = append(out, n)
	}
	return out
}
