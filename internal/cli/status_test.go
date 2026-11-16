package cli

import (
	"strings"
	"testing"

	"immich-transcode/internal/manifest"
)

// The help text and the unknown-state error name every state.
func TestStateHelpListsEveryState(t *testing.T) {
	help, names := stateHelp(), stateNames()
	for _, st := range manifest.All() {
		if !strings.Contains(help, "  "+string(st)+" ") || !strings.Contains(help, st.Description()) {
			t.Errorf("help text lacks %s", st)
		}
		if !strings.Contains(names, string(st)) {
			t.Errorf("state names lack %s", st)
		}
	}
}

func TestHiddenNote(t *testing.T) {
	if got, want := hiddenNote(3, false), "3 skipped assets hidden; use --all"; got != want {
		t.Errorf("default view: %q, want %q", got, want)
	}
	if got, want := hiddenNote(3, true), "3 assets in other states not shown"; got != want {
		t.Errorf("--state view: %q, want %q", got, want)
	}
}
