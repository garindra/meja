//go:build !darwin

package server

func readStructuredProcessArgv(int) ([]string, error) {
	return nil, errStructuredProcessArgvUnavailable
}
