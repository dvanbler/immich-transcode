package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"immich-transcode/internal/config"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/pipeline"
)

type runFlags struct {
	apply         bool
	allowFaceLoss bool
	retryFailed   bool
	limit         int
	assets        []string
	minSize       config.Size
	takenBefore   config.Cutoff
	crf           int
	crfExplicit   bool
}

func (a *app) runCmd() *cobra.Command {
	f := runFlags{minSize: config.Default().MinSize}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Process candidates (dry run unless --apply)",
		Long: `Process candidate videos through the pipeline.

Without --apply this is a dry run: originals are downloaded to the backup
directory, transcoded and validated locally, but nothing is uploaded to or
deleted from Immich. A later "run --apply" or "resume --apply" continues
from the validated encode.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("min-size") {
				f.minSize = a.cfg.MinSize
			}
			if !cmd.Flags().Changed("taken-before") {
				f.takenBefore = a.cfg.TakenBefore
			}
			if _, ok := f.takenBefore.Before(time.Now()); ok && len(f.assets) == 0 {
				a.console.Printf("Only videos taken before %s (taken_before)", f.takenBefore.Describe(time.Now()))
			}
			f.crfExplicit = cmd.Flags().Changed("crf")
			if !f.crfExplicit {
				f.crf = a.cfg.CRF
			}
			a.cfg.CRF = f.crf
			return a.process(f, func(ctx context.Context, s *session) ([]string, error) {
				if len(f.assets) > 0 {
					return f.assets, nil
				}
				cands, _, err := findCandidates(ctx, s, int64(f.minSize), f.takenBefore)
				if err != nil {
					return nil, err
				}
				var ids []string
				for _, c := range cands {
					switch c.Manifest {
					case manifest.Failed:
						if !f.retryFailed {
							continue
						}
					case manifest.NeedsConfirm:
						if !f.allowFaceLoss {
							continue
						}
					}
					ids = append(ids, c.Asset.Id.String())
				}
				return ids, nil
			})
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.apply, "apply", false, "upload replacements and trash originals (default is a dry run)")
	fl.IntVar(&f.limit, "limit", 0, "process at most N assets (0 = no limit)")
	fl.StringSliceVar(&f.assets, "asset", nil, "process only this asset id (repeatable)")
	fl.Var(&f.minSize, "min-size", "only consider videos at least this large (e.g. 200MB; config min_size)")
	fl.Var(&f.takenBefore, "taken-before", "only consider videos taken before a date (2026-09-01) or older than an age (90d); config taken_before")
	fl.IntVar(&f.crf, "crf", 0, "x265 CRF (default from config, 26); also re-encodes existing local encodes made at another CRF")
	fl.BoolVar(&f.allowFaceLoss, "allow-face-loss", false, "replace videos even if named people will lose their face assignments")
	fl.BoolVar(&f.retryFailed, "retry-failed", false, "also retry assets that failed in an earlier run")
	return cmd
}

func (a *app) resumeCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Continue unfinished assets from the manifest",
		Long: `Continue every unfinished asset in the manifest: those left partway through
the pipeline (including encodes validated by a dry run) and pending ones,
such as assets sent back by reset. Unlike run, resume does not search Immich
for new candidates.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.process(f, func(ctx context.Context, s *session) ([]string, error) {
				recs, err := s.store.List(ctx)
				if err != nil {
					return nil, err
				}
				var ids []string
				for _, r := range recs {
					if resumable(r.State, f) {
						ids = append(ids, r.AssetID)
					}
				}
				if len(ids) == 0 {
					a.console.Printf("Nothing to resume.")
				}
				return ids, nil
			})
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.apply, "apply", false, "upload replacements and trash originals (default is a dry run)")
	fl.IntVar(&f.limit, "limit", 0, "process at most N assets (0 = no limit)")
	fl.BoolVar(&f.allowFaceLoss, "allow-face-loss", false, "also continue needs_confirm assets, dropping face assignments")
	fl.BoolVar(&f.retryFailed, "retry-failed", false, "also retry failed assets")
	return cmd
}

// resumable reports whether resume picks up an asset in state st: anything
// unfinished (pending or partway), plus failed and needs_confirm assets when
// their flags are given.
func resumable(st manifest.State, f runFlags) bool {
	switch {
	case st == manifest.Pending, st.InProgress():
		return true
	case st == manifest.Failed:
		return f.retryFailed
	case st == manifest.NeedsConfirm:
		return f.allowFaceLoss
	}
	return false
}

// process runs the pipeline over the ids chosen by pick.
func (a *app) process(f runFlags, pick func(context.Context, *session) ([]string, error)) error {
	perms := permsDryRun
	if f.apply {
		perms = permsApply
	}
	var proc *pipeline.Processor
	ctx, stop := a.interruptible(func() {
		if proc != nil {
			proc.RequestStop()
		}
	})
	defer stop()
	s, err := a.preflight(ctx, preflightOpts{perms: perms, needTools: true})
	if err != nil {
		return err
	}
	defer s.close()

	opts := pipeline.Options{Apply: f.apply, AllowFaceLoss: f.allowFaceLoss, RetryFailed: f.retryFailed,
		CRF: s.cfg.CRF, CRFExplicit: f.crfExplicit}
	picked, err := pick(ctx, s)
	if err != nil {
		return err
	}
	// Drop assets with nothing to do before applying --limit, so repeated
	// dry runs move on to new candidates instead of revisiting validated ones.
	var ids []string
	idle := 0
	for _, id := range picked {
		rec, ok, err := s.store.Get(ctx, id)
		if err != nil {
			return err
		}
		if ok && !opts.HasWork(rec) {
			idle++
			continue
		}
		ids = append(ids, id)
	}
	if idle > 0 {
		a.console.Printf("%d asset(s) already validated by an earlier dry run; skipped (use --apply to finish them, or --crf to re-encode)", idle)
	}
	if f.limit > 0 && len(ids) > f.limit {
		ids = ids[:f.limit]
	}
	if len(ids) == 0 {
		return nil
	}
	mode := "DRY RUN (nothing will be uploaded or deleted; add --apply)"
	if f.apply {
		mode = "APPLY"
	}
	a.console.Printf("\n%s: %d asset(s), crf %d, preset %s, concurrency %d\n", mode, len(ids), s.cfg.CRF, s.cfg.Preset, s.cfg.Concurrency)
	s.log.Info("run start", "version", a.version, "mode", mode, "assets", len(ids), "crf", s.cfg.CRF, "preset", s.cfg.Preset)

	proc = &pipeline.Processor{
		Cfg:     s.cfg,
		Opts:    opts,
		Immich:  s.client,
		Tools:   s.tools,
		Store:   s.store,
		Log:     s.log,
		Console: a.console,
		UserID:  s.userID,
	}
	outcomes := proc.Run(ctx, ids)

	var saved, potential int64
	counts := map[manifest.State]int{}
	failed, interrupted := 0, false
	for _, o := range outcomes {
		interrupted = interrupted || o.Interrupted
		counts[o.State]++
		saved += o.Saved()
		if o.State == manifest.Validated {
			potential += o.OldSize - o.NewSize
		}
		if o.Err != nil {
			failed++
		}
	}
	a.console.Printf("\nProcessed %d of %d asset(s).", len(outcomes), len(ids))
	for st, n := range counts {
		if st != "" {
			a.console.Printf("  %-20s %d", st, n)
		}
	}
	a.console.Printf("Saved this run: %s", config.HumanBytes(saved))
	if potential > 0 {
		a.console.Printf("Validated, awaiting --apply: %s would be saved", config.HumanBytes(potential))
	}
	s.log.Info("run end", "processed", len(outcomes), "saved_bytes", saved, "errors", failed)
	if interrupted || len(outcomes) < len(ids) {
		return fmt.Errorf("interrupted; run `immich-transcode resume` to continue")
	}
	if failed > 0 {
		return fmt.Errorf("%d asset(s) had errors; see `immich-transcode status` and %s", failed, s.cfg.LogFile)
	}
	return nil
}
