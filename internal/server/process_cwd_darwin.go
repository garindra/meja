//go:build darwin

package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

func readStructuredProcessCwds(ctx context.Context, pids []int) (map[int]string, error) {
	unique := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid > 0 {
			unique[pid] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return map[int]string{}, nil
	}
	sorted := make([]int, 0, len(unique))
	for pid := range unique {
		sorted = append(sorted, pid)
	}
	sort.Ints(sorted)
	rawPIDs := make([]string, len(sorted))
	for index, pid := range sorted {
		rawPIDs[index] = strconv.Itoa(pid)
	}

	lsof := "/usr/sbin/lsof"
	if _, err := os.Stat(lsof); err != nil {
		var lookupErr error
		lsof, lookupErr = exec.LookPath("lsof")
		if lookupErr != nil {
			return nil, fmt.Errorf("find lsof for process cwd capture: %w", lookupErr)
		}
	}
	output, err := exec.CommandContext(ctx, lsof, "-a", "-d", "cwd", "-Fn", "-p", strings.Join(rawPIDs, ",")).Output()
	cwds := parseLsofProcessCwds(output)
	if err != nil && len(cwds) == 0 {
		return nil, fmt.Errorf("capture process cwd with lsof: %w", err)
	}
	return cwds, nil
}
