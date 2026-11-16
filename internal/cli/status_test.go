package cli

import "testing"

func TestHiddenNote(t *testing.T) {
	if got, want := hiddenNote(3, false), "3 skipped assets hidden; use --all"; got != want {
		t.Errorf("default view: %q, want %q", got, want)
	}
	if got, want := hiddenNote(3, true), "3 assets in other states not shown"; got != want {
		t.Errorf("--state view: %q, want %q", got, want)
	}
}
