//go:build !darwin

package server

import "context"

func readStructuredProcessCwds(context.Context, []int) (map[int]string, error) {
	return nil, errStructuredProcessCwdUnavailable
}
