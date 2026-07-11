package terminal

import (
	"fmt"
	"io"
	"sync"
)

var debugLogger struct {
	mu sync.Mutex
	w  io.Writer
}

func SetDebugLogger(w io.Writer) {
	debugLogger.mu.Lock()
	defer debugLogger.mu.Unlock()
	debugLogger.w = w
}

func logUnsupportedf(format string, args ...any) {
	debugLogger.mu.Lock()
	defer debugLogger.mu.Unlock()
	if debugLogger.w == nil {
		return
	}
	_, _ = fmt.Fprintf(debugLogger.w, "tali terminal: "+format+"\n", args...)
}
