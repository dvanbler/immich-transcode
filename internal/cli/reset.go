package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"immich-transcode/internal/manifest"
)

func (a *app) resetCmd() *cobra.Command {
	var states, assets []string
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Send skipped or failed assets back to pending so they are re-checked",
		Long: `Send skipped or failed assets back to pending, e.g. after a check was fixed or
the bpp thresholds were changed. The next resume, run or scan considers them
again.

Only the manifest changes: nothing is sent to Immich, and backups are kept
(a re-run verifies the existing backup by checksum instead of downloading).
Done assets cannot be reset, and needs_confirm assets are continued with
--allow-face-loss instead.`,
		Example: `  immich-transcode reset --state skipped_corrupt
  immich-transcode reset --state skipped_efficient,skipped_low_gain
  immich-transcode reset --asset 62446a94-8936-4572-a8f4-a389841bc9a7`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(states) == 0 && len(assets) == 0 {
				return fmt.Errorf("choose what to reset with --state and/or --asset")
			}
			cfg := a.cfg
			if err := cfg.Finalize(); err != nil {
				return err
			}
			s := &session{cfg: cfg}
			if err := a.openLocal(s); err != nil {
				return err
			}
			defer s.close()
			ctx := context.Background()
			recs, err := s.store.List(ctx)
			if err != nil {
				return err
			}
			wantState := map[manifest.State]bool{}
			for _, st := range states {
				if !resettable(manifest.State(st)) {
					return fmt.Errorf("state %q cannot be reset (allowed: skipped_*, failed)", st)
				}
				wantState[manifest.State(st)] = true
			}
			wantAsset := map[string]bool{}
			for _, id := range assets {
				wantAsset[id] = true
			}
			n := 0
			for _, r := range recs {
				if len(wantState) > 0 && !wantState[r.State] {
					continue
				}
				if len(wantAsset) > 0 && !wantAsset[r.AssetID] {
					continue
				}
				if !resettable(r.State) {
					a.console.Printf("%s (%s): %s cannot be reset", r.FileName, r.AssetID, r.State)
					continue
				}
				prev := r.State
				r.Error, r.Detail, r.ResumeFrom = "", "", ""
				if err := s.store.Transition(ctx, &r, manifest.Pending, "reset from "+string(prev)); err != nil {
					return err
				}
				s.log.Info("reset", "asset", r.AssetID, "from", prev)
				a.console.Printf("%s: %s → pending", r.FileName, prev)
				n++
			}
			a.console.Printf("Reset %d asset(s).", n)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&states, "state", nil, "reset assets in these states (e.g. skipped_corrupt)")
	cmd.Flags().StringSliceVar(&assets, "asset", nil, "reset this asset id (repeatable)")
	return cmd
}

// resettable: skipped_* and failed. needs_confirm is continued with
// --allow-face-loss, and done cannot be undone (the original is trashed).
func resettable(s manifest.State) bool {
	return s.Terminal() && s != manifest.NeedsConfirm && manifest.CheckTransition(s, manifest.Pending) == nil
}
