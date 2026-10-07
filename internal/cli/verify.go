package cli

import (
	"fmt"
	"sync/atomic"

	"github.com/spf13/cobra"

	"immich-transcode/internal/config"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/pipeline"
)

func (a *app) verifyCmd() *cobra.Command {
	var assets []string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Download done replacements from Immich and re-check them against the backups",
		Long: `Download the replacement of each done asset from Immich and run the
pre-upload validation on it again, against the local backup of the
original: clean decode, duration, displayed size and orientation, audio
streams, frame count, duplicate timestamps, creation date and GPS.

Read-only towards Immich: replacements are downloaded to the work directory,
checked and deleted. Exits non-zero if any replacement has a problem.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stopping := atomic.Bool{}
			ctx, stop := a.interruptible(func() { stopping.Store(true) })
			defer stop()
			s, err := a.preflight(ctx, preflightOpts{perms: permsDryRun, needTools: true})
			if err != nil {
				return err
			}
			defer s.close()

			var recs []manifest.Record
			if len(assets) > 0 {
				for _, id := range assets {
					r, ok, err := s.store.Get(ctx, id)
					if err != nil {
						return err
					}
					if !ok {
						return fmt.Errorf("asset %s is not in the manifest", id)
					}
					recs = append(recs, r)
				}
			} else {
				all, err := s.store.List(ctx)
				if err != nil {
					return err
				}
				for _, r := range all {
					if r.State == manifest.Done {
						recs = append(recs, r)
					}
				}
			}
			if len(recs) == 0 {
				a.console.Printf("No done assets to verify.")
				return nil
			}
			a.console.Printf("\nVerifying %d replacement(s) in Immich against the local backups\n", len(recs))

			var bad, failed []string
			checked := 0
			for i, r := range recs {
				if stopping.Load() || ctx.Err() != nil {
					break
				}
				var got atomic.Int64
				a.console.Status(fmt.Sprintf("[%d/%d] %s: downloading replacement", i+1, len(recs), r.FileName))
				problem, err := pipeline.VerifyReplacement(ctx, s.client, s.tools, r, s.cfg.WorkDir, func(n int64) {
					v := got.Add(n)
					a.console.Status(fmt.Sprintf("[%d/%d] %s: downloading replacement %s of %s",
						i+1, len(recs), r.FileName, config.HumanBytes(v), config.HumanBytes(r.NewSize)))
				})
				a.console.Status("")
				switch {
				case ctx.Err() != nil:
				case err != nil:
					failed = append(failed, r.AssetID)
					a.console.Printf("%s: COULD NOT CHECK: %v", r.FileName, err)
					s.log.Error("verify could not run", "asset", r.AssetID, "new_asset", r.NewAssetID, "err", err)
				case problem != "":
					bad = append(bad, r.AssetID)
					a.console.Printf("%s: PROBLEM: %s", r.FileName, problem)
					s.log.Warn("verify found a problem", "asset", r.AssetID, "new_asset", r.NewAssetID, "problem", problem)
				default:
					a.console.Printf("%s: ok", r.FileName)
					s.log.Info("verify ok", "asset", r.AssetID, "new_asset", r.NewAssetID)
				}
				if ctx.Err() == nil {
					checked++
				}
			}

			a.console.Printf("\nChecked %d of %d: %d ok, %d with problems, %d could not be checked.",
				checked, len(recs), checked-len(bad)-len(failed), len(bad), len(failed))
			if len(bad) > 0 {
				a.console.Printf("Replacements with problems (original asset ids):")
				for _, id := range bad {
					a.console.Printf("  %s", id)
				}
			}
			switch {
			case checked < len(recs):
				return fmt.Errorf("interrupted")
			case len(bad) > 0 || len(failed) > 0:
				return fmt.Errorf("%d replacement(s) with problems, %d not checked", len(bad), len(failed))
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&assets, "asset", nil, "verify only this asset id (repeatable; original asset id as shown by status)")
	return cmd
}
