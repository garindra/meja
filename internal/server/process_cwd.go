package server

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"
)

var errStructuredProcessCwdUnavailable = errors.New("structured process cwd is unavailable")

func parseLsofProcessCwds(data []byte) map[int]string {
	cwds := make(map[int]string)
	currentPID := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err != nil || pid <= 0 {
				currentPID = 0
				continue
			}
			currentPID = pid
		case 'n':
			if currentPID != 0 && line[1:] != "" {
				cwds[currentPID] = line[1:]
			}
		}
	}
	return cwds
}
