package version

import "testing"

func TestCurrentUsesInjectedVersion(t *testing.T) {
	previous := Value
	Value = "v1.2.3"
	t.Cleanup(func() { Value = previous })

	if got := Current(); got != "1.2.3" {
		t.Fatalf("Current() = %q, want %q", got, "1.2.3")
	}
}

func TestCurrentDevelopmentVersionIsNonEmpty(t *testing.T) {
	previous := Value
	Value = ""
	t.Cleanup(func() { Value = previous })

	if got := Current(); got == "" {
		t.Fatal("Current() returned an empty version")
	}
}
