//go:build darwin

package server

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func readStructuredProcessArgv(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid process ID %d", pid)
	}
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("read kern.procargs2 for pid %d: %w", pid, err)
	}
	return parseDarwinProcArgs(data)
}
