package terminal

import (
	"strconv"
	"strings"
)

type parserState int

const (
	parserText parserState = iota
	parserESC
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

func parseCSIParams(raw string) []int {
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
