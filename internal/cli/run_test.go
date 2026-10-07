package cli

import (
	"testing"

	"immich-transcode/internal/manifest"
)

func TestResumable(t *testing.T) {
	none := runFlags{}
	both := runFlags{retryFailed: true, allowFaceLoss: true}
	cases := []struct {
		st        manifest.State
		plain     bool // without flags
		withFlags bool // --retry-failed --allow-face-loss
	}{
		{manifest.Pending, true, true}, // e.g. sent back by reset
		{manifest.Downloaded, true, true},
		{manifest.Validated, true, true}, // dry-run encode awaiting --apply
		{manifest.Uploaded, true, true},
		{manifest.Trashed, true, true},
		{manifest.Failed, false, true},
		{manifest.NeedsConfirm, false, true},
		{manifest.Done, false, false},
		{manifest.SkippedCorrupt, false, false},
		{manifest.SkippedLowGain, false, false},
	}
	for _, c := range cases {
		if got := resumable(c.st, none); got != c.plain {
			t.Errorf("%s without flags: %v, want %v", c.st, got, c.plain)
		}
		if got := resumable(c.st, both); got != c.withFlags {
			t.Errorf("%s with flags: %v, want %v", c.st, got, c.withFlags)
		}
	}
}
