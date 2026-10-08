package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"immich-transcode/internal/config"
)

func (a *app) scanCmd() *cobra.Command {
	minSize := config.Default().MinSize
	var takenBefore config.Cutoff
	var limit int
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "List candidate videos with estimated savings (read-only)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()
			if !cmd.Flags().Changed("min-size") {
				minSize = a.cfg.MinSize
			}
			if !cmd.Flags().Changed("taken-before") {
				takenBefore = a.cfg.TakenBefore
			}
			s, err := a.preflight(ctx, preflightOpts{perms: permsRead})
			if err != nil {
				return err
			}
			defer s.close()
			cands, excl, err := findCandidates(ctx, s, int64(minSize), takenBefore)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(a.console.Writer(), 0, 0, 2, ' ', tabwriter.AlignRight)
			fmt.Fprintln(w, "#\tsize\tduration\tresolution\test. bpp\test. saving\t\tasset id\tfile\t")
			var total int64
			for i, c := range cands {
				total += c.Est.EstSavings
				if limit > 0 && i >= limit {
					continue
				}
				note := ""
				if c.Manifest != "" {
					note = "[" + string(c.Manifest) + "]"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%dx%d\t%.3f\t%s\t%s\t%s\t%s\t\n", i+1,
					config.HumanBytes(c.Est.Size),
					(time.Duration(c.Est.DurationSec) * time.Second).String(),
					c.Est.Width, c.Est.Height, c.Est.BPP,
					config.HumanBytes(c.Est.EstSavings), note,
					c.Asset.Id, c.Asset.OriginalFileName)
			}
			w.Flush()
			if limit > 0 && len(cands) > limit {
				a.console.Printf("... %d more (use --limit 0 to list all)", len(cands)-limit)
			}
			a.console.Printf("\n%d candidates of at least %s; estimated saving %s (rough: assumes %.2f bpp output at 30 fps).",
				len(cands), config.HumanBytes(int64(minSize)), config.HumanBytes(total), s.cfg.BPP.EstimatedOutput)
			a.console.Printf("Taken before: %s (taken_before)", takenBefore.Describe(time.Now()))
			for _, e := range excl {
				a.console.Printf("  excluded %5d  %s", e.count, e.reason)
			}
			return nil
		},
	}
	cmd.Flags().Var(&minSize, "min-size", "only consider videos at least this large (e.g. 200MB; config min_size)")
	cmd.Flags().Var(&takenBefore, "taken-before", "only consider videos taken before a date (2026-09-01) or older than an age (90d); config taken_before")
	cmd.Flags().IntVar(&limit, "limit", 50, "number of candidates to list (0 for all)")
	return cmd
}
