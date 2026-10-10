package cli

import "testing"

func TestResolveVersion(t *testing.T) {
	if got := resolveVersion("v0.1.0"); got != "v0.1.0" {
		t.Errorf("stamped version: got %q", got)
	}
	// Test binaries carry no stamp; the fallback must still give something.
	if got := resolveVersion(""); got == "" {
		t.Error("empty fallback version")
	}
}
