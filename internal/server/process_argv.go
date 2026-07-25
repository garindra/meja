package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

var errStructuredProcessArgvUnavailable = errors.New("structured process argv is unavailable")

// parseDarwinProcArgs decodes the kern.procargs2 layout: argc, the executable
// path, alignment NULs, and argc NUL-terminated argv entries. Environment
// entries may follow argv and are intentionally ignored.
func parseDarwinProcArgs(data []byte) ([]string, error) {
	const argcSize = 4
	if len(data) < argcSize {
		return nil, errors.New("truncated kern.procargs2 argc")
	}
	argc := int(int32(binary.LittleEndian.Uint32(data[:argcSize])))
	if argc <= 0 || argc > len(data) {
		return nil, fmt.Errorf("invalid kern.procargs2 argc %d", argc)
	}

	data = data[argcSize:]
	executableEnd := bytes.IndexByte(data, 0)
	if executableEnd < 0 {
		return nil, errors.New("unterminated kern.procargs2 executable path")
	}
	data = data[executableEnd+1:]
	for len(data) > 0 && data[0] == 0 {
		data = data[1:]
	}

	argv := make([]string, 0, argc)
	for len(argv) < argc {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			return nil, fmt.Errorf("truncated kern.procargs2 argv at argument %d", len(argv))
		}
		argv = append(argv, string(data[:end]))
		data = data[end+1:]
	}
	return argv, nil
}
