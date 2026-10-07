package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"immich-transcode/internal/manifest"
)

func (a *app) redoCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "redo --asset <id> [--asset <id> ...]",
		Short: "Re-encode done assets from their backups and replace their current replacements",
		Long: `Re-encode done assets from the local backups of their originals, and replace
the replacement currently in Immich (for example one that verify reported).

The current replacement is treated like an original: albums, favorite,
shared links, stack, description, rating, tags and date are copied from it
to the new upload, which is verified before the current replacement is moved
to the trash. Nothing is downloaded; the backup must exist.

Like run, this is a dry run unless --apply is given: without it the new
encode is made and validated, and "resume --apply" finishes it later.
Asset ids are the original ids, as shown by status and verify.`,
		Example: `  immich-transcode redo --asset e5a7d64b-e29c-46d2-bb04-df0044428c85 --apply`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(f.assets) == 0 {
				return fmt.Errorf("choose the assets to redo with --asset")
			}
			f.crf = a.cfg.CRF
			return a.process(f, func(ctx context.Context, s *session) ([]string, error) {
				var ids []string
				for _, id := range f.assets {
					r, ok, err := s.store.Get(ctx, id)
					if err != nil {
						return nil, err
					}
					switch {
					case !ok:
						return nil, fmt.Errorf("asset %s is not in the manifest", id)
					case r.State == manifest.Done:
						if err := s.store.MarkRedo(ctx, &r); err != nil {
							return nil, err
						}
						a.console.Printf("%s: marked for redo (replacing %s)", r.FileName, r.SourceAssetID)
						s.log.Info("redo", "asset", r.AssetID, "replacing", r.SourceAssetID)
					case r.SourceAssetID != "" && !r.State.Permanent():
						// A redo already under way: just continue it.
					default:
						return nil, fmt.Errorf("asset %s (%s) is %s, not done; only done assets can be redone", id, r.FileName, r.State)
					}
					ids = append(ids, id)
				}
				return ids, nil
			})
		},
	}
	fl := cmd.Flags()
	fl.StringSliceVar(&f.assets, "asset", nil, "original asset id to redo (repeatable)")
	fl.BoolVar(&f.apply, "apply", false, "upload the new encodes and trash the current replacements (default is a dry run)")
	fl.BoolVar(&f.allowFaceLoss, "allow-face-loss", false, "redo even if named people on the current replacement will lose their face assignments")
	return cmd
}
