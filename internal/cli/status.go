package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"immich-transcode/internal/config"
	"immich-transcode/internal/manifest"
)

func (a *app) statusCmd() *cobra.Command {
	var states []string
	var all bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the manifest: each asset's state, bytes saved and failures",
		Long: `Show every asset in the manifest with its state, sizes and, for failures and
skips, the reason. Skipped and restored assets are hidden unless --all or
--state asks for them. Nothing is changed and the server is not contacted.

` + stateHelp(),
		Example: `  immich-transcode status
  immich-transcode status --state failed,needs_confirm
  immich-transcode status --all`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			want := map[string]bool{}
			for _, st := range states {
				if !manifest.State(st).Valid() {
					return fmt.Errorf("unknown state %q (valid states: %s)", st, stateNames())
				}
				want[st] = true
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
			recs, err := s.store.List(context.Background())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(a.console.Writer(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "STATE\tORIGINAL\tNEW\tSAVED\tUPDATED\tASSET\tFILE\tDETAIL")
			counts := map[manifest.State]int{}
			var saved int64
			shown := 0
			for _, r := range recs {
				counts[r.State]++
				if r.State == manifest.Done {
					saved += r.OriginalSize - r.NewSize
				}
				if len(want) > 0 && !want[string(r.State)] {
					continue
				}
				if !all && len(want) == 0 && r.State.Permanent() && r.State != manifest.Done {
					continue
				}
				shown++
				newSize, savedCol := "", ""
				if r.NewSize > 0 {
					newSize = config.HumanBytes(r.NewSize)
					savedCol = fmt.Sprintf("%.1f%%", 100*float64(r.OriginalSize-r.NewSize)/float64(max(r.OriginalSize, 1)))
				}
				detail := r.Error
				if detail == "" {
					detail = r.Detail
				}
				if r.People != "" && r.State == manifest.NeedsConfirm {
					detail = "people: " + r.People
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.State, config.HumanBytes(r.OriginalSize),
					newSize, savedCol, r.UpdatedAt.Local().Format("2006-01-02 15:04"), r.AssetID, r.FileName, truncate(detail, 100))
			}
			w.Flush()
			if hidden := len(recs) - shown; hidden > 0 {
				a.console.Printf("(%s)", hiddenNote(hidden, len(want) > 0))
			}
			a.console.Printf("\n%d assets in manifest %s", len(recs), cfg.ManifestPath)
			for _, st := range slices.Sorted(maps.Keys(counts)) {
				a.console.Printf("  %-20s %d", st, counts[st])
			}
			a.console.Printf("Total saved: %s", config.HumanBytes(saved))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&states, "state", nil, "only show these states (e.g. failed,needs_confirm)")
	cmd.Flags().BoolVar(&all, "all", false, "include skipped and restored assets")
	return cmd
}

// stateHelp lists every state with its meaning, for help text.
func stateHelp() string {
	var b strings.Builder
	section := func(title string, terminal bool) {
		b.WriteString(title + "\n")
		for _, st := range manifest.All() {
			if st.Terminal() == terminal {
				fmt.Fprintf(&b, "  %-20s %s\n", st, st.Description())
			}
		}
	}
	section("States of an asset partway through, each named for the last step that finished:", false)
	b.WriteString("\n")
	section("States of an asset that is finished or waiting for you:", true)
	return strings.TrimRight(b.String(), "\n")
}

// stateNames is every state name, comma separated.
func stateNames() string {
	names := make([]string, 0, len(manifest.All()))
	for _, st := range manifest.All() {
		names = append(names, string(st))
	}
	return strings.Join(names, ", ")
}

// hiddenNote says why status left hidden assets out of the table: --state
// selected other states, or (by default) they are skipped assets.
func hiddenNote(hidden int, filtered bool) string {
	if filtered {
		return fmt.Sprintf("%d assets in other states not shown", hidden)
	}
	return fmt.Sprintf("%d skipped assets hidden; use --all", hidden)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
