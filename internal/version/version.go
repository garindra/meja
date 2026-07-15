package version

import (
	"runtime/debug"
	"strings"
)

// Value is set by release builds using a linker flag.
var Value string

// Current returns the version injected by the build, the module version
// recorded by the Go toolchain, or "dev" for an unversioned local build.
func Current() string {
	if Value != "" {
		return normalize(Value)
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		if value := info.Main.Version; value != "" && value != "(devel)" {
			return normalize(value)
		}
	}

	return "dev"
}

func normalize(value string) string {
	return strings.TrimPrefix(value, "v")
}
