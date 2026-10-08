package cli

import (
	"context"
	"sort"

	"immich-transcode/internal/immich"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/selection"
)

// candidate is a video selected from API data alone.
type candidate struct {
	Asset    immich.Asset
	Est      selection.Estimate
	Manifest manifest.State // "" when not in the manifest
}

type exclusion struct {
	reason string
	count  int
}

// findCandidates pages through the library and applies the API-data
// pre-filter, returning candidates ranked by estimated savings and counts of
// excluded assets by reason.
func findCandidates(ctx context.Context, s *session, minSize int64) ([]candidate, []exclusion, error) {
	states, err := s.store.States(ctx)
	if err != nil {
		return nil, nil, err
	}
	replacements, err := s.store.Replacements(ctx)
	if err != nil {
		return nil, nil, err
	}
	excluded := map[string]int{}
	var out []candidate
	err = s.client.SearchVideos(ctx, minSize, func(page []immich.Asset) error {
		for _, a := range page {
			if _, ok := replacements[a.Id.String()]; ok {
				// Already HEVC from this tool; redo works from the backup.
				excluded["uploaded by immich-transcode (replacement or restore)"]++
				continue
			}
			if reason := exclude(a, s.userID, states[a.Id.String()]); reason != "" {
				excluded[reason]++
				continue
			}
			est := estimate(a, s.cfg.BPP.EstimatedOutput)
			if reason := selection.PreReason(est, minSize, s.cfg.BPP.ScanMin); reason != "" {
				excluded[firstWord(reason)]++
				continue
			}
			out = append(out, candidate{Asset: a, Est: est, Manifest: states[a.Id.String()]})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	selection.Rank(out, func(c candidate) int64 { return c.Est.EstSavings })
	var ex []exclusion
	for r, n := range excluded {
		ex = append(ex, exclusion{r, n})
	}
	sort.Slice(ex, func(i, j int) bool { return ex[i].count > ex[j].count })
	return out, ex, nil
}

// exclude returns why an asset can never be processed, or "".
func exclude(a immich.Asset, userID string, st manifest.State) string {
	switch {
	case a.OwnerId.String() != userID:
		return "partner asset (not owned)"
	case a.IsTrashed:
		return "in trash"
	case a.LibraryId != nil:
		// External-library files are re-imported from disk; replacing them
		// in Immich would not shrink anything.
		return "external library"
	case a.IsOffline:
		return "offline"
	case a.IsEdited:
		return "has edits (would be lost)"
	case st == manifest.Done:
		return "already done"
	case st.Permanent():
		return "skipped in an earlier run (" + string(st) + ")"
	}
	return ""
}

func estimate(a immich.Asset, outBPP float64) selection.Estimate {
	var size int64
	if a.ExifInfo != nil && a.ExifInfo.FileSizeInByte != nil {
		size = int64(*a.ExifInfo.FileSizeInByte)
	}
	dur := 0.0
	if a.Duration != nil {
		dur = float64(*a.Duration) / 1000
	}
	w, h := deref(a.Width), deref(a.Height)
	if (w == 0 || h == 0) && a.ExifInfo != nil {
		w, h = deref(a.ExifInfo.ExifImageWidth), deref(a.ExifInfo.ExifImageHeight)
	}
	return selection.Estimates(size, dur, w, h, outBPP)
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func firstWord(reason string) string {
	// Group pre-filter reasons ("estimated 0.081 bpp ...") by kind.
	switch {
	case len(reason) >= 9 && reason[:9] == "estimated":
		return "already efficient (estimated bpp)"
	case len(reason) >= 7 && reason[:7] == "smaller":
		return "below minimum size"
	}
	return reason
}
