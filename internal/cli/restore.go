package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"immich-transcode/internal/config"
	"immich-transcode/internal/immich"
	"immich-transcode/internal/manifest"
)

func (a *app) restoreCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "restore --asset <id> [--asset <id> ...]",
		Short: "Put backed-up originals back in Immich in place of their replacements",
		Long: `Put the backup of a done asset's original back in Immich, in place of its
replacement.

If the original is still in the Immich trash, it is restored from there
(the backup's bytes identify it), which also brings back its face
assignments. Otherwise the backup is uploaded as a new asset. Either way,
albums, favorite, shared links, stack, description, rating, tags and date
are copied from the current replacement, the timeline is verified, and only
then is the replacement moved to the trash. The backup file is never moved
or deleted.

Without --apply this only shows what would happen. Restored assets are not
selected by scan or run again. Asset ids are the original ids, as shown by
status.`,
		Example: `  immich-transcode restore --asset e5a7d64b-e29c-46d2-bb04-df0044428c85
  immich-transcode restore --asset e5a7d64b-e29c-46d2-bb04-df0044428c85 --apply`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(f.assets) == 0 {
				return fmt.Errorf("choose the assets to restore with --asset")
			}
			if !f.apply {
				return a.restorePlan(f.assets)
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
						if _, err := os.Stat(r.BackupPath); err != nil {
							return nil, fmt.Errorf("%s: backup %s is missing", r.FileName, r.BackupPath)
						}
						if err := s.store.MarkRestore(ctx, &r); err != nil {
							return nil, err
						}
						s.log.Info("restore", "asset", r.AssetID, "replacing", r.SourceAssetID)
					case r.Mode == manifest.ModeRestore && !r.State.Permanent():
						// A restore already under way: just continue it.
					default:
						return nil, fmt.Errorf("asset %s (%s) is %s, not done; only done assets can be restored", id, r.FileName, r.State)
					}
					ids = append(ids, id)
				}
				return ids, nil
			})
		},
	}
	fl := cmd.Flags()
	fl.StringSliceVar(&f.assets, "asset", nil, "original asset id to restore (repeatable)")
	fl.BoolVar(&f.apply, "apply", false, "restore (default only shows what would happen)")
	fl.BoolVar(&f.allowFaceLoss, "allow-face-loss", false, "restore even if people named on the replacement will lose their face assignments")
	return cmd
}

// restorePlan reports, without changing anything, what restore would do.
func (a *app) restorePlan(ids []string) error {
	ctx := context.Background()
	s, err := a.preflight(ctx, preflightOpts{perms: permsRead})
	if err != nil {
		return err
	}
	defer s.close()
	problems := 0
	for _, id := range ids {
		r, ok, err := s.store.Get(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			a.console.Printf("%s: not in the manifest", id)
			problems++
			continue
		}
		if r.State != manifest.Done {
			a.console.Printf("%s: is %s, not done; cannot restore", r.FileName, r.State)
			problems++
			continue
		}
		if _, err := os.Stat(r.BackupPath); err != nil {
			a.console.Printf("%s: backup %s is missing; cannot restore", r.FileName, r.BackupPath)
			problems++
			continue
		}
		how := "original was removed from the trash: the backup will be uploaded as a new asset (face assignments are not restored)"
		orig, err := s.client.GetAsset(ctx, r.AssetID)
		var httpErr *immich.HTTPError
		switch {
		case err == nil && orig.IsTrashed:
			how = "original is still in the Immich trash: it will be restored, with its face assignments"
		case err == nil:
			how = "original is not in the trash (restored by hand?): the replacement will be trashed and the original kept"
		case errors.As(err, &httpErr) && (httpErr.Status == http.StatusNotFound || httpErr.Status == http.StatusBadRequest):
		default:
			return err
		}
		a.console.Printf("%s: %s backup; %s; replacement %s would be moved to the trash",
			r.FileName, config.HumanBytes(r.OriginalSize), how, r.NewAssetID)
	}
	a.console.Printf("\nNothing changed. Add --apply to restore.")
	if problems > 0 {
		return fmt.Errorf("%d asset(s) cannot be restored", problems)
	}
	return nil
}
