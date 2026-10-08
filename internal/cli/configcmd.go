package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"immich-transcode/internal/config"
)

func (a *app) configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show the effective configuration",
		Long: `Show the settings immich-transcode would use: built-in defaults, overridden by
the config file, overridden by any flags given with this command. Derived
values (work_dir, manifest, log_file) are filled in. The output is valid
config file YAML.

The API key is never shown; only whether ` + config.APIKeyEnv + ` is set.
Nothing is changed and the server is not contacted.`,
		Example: `  immich-transcode config
  immich-transcode config --preset medium     # see the effect of a flag
  immich-transcode config > my-config.yaml    # start a config file`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := a.cfg
			finalizeErr := cfg.Finalize()
			out, err := cfg.YAML()
			if err != nil {
				return err
			}
			w := a.console.Writer()
			if a.loadedPath != "" {
				fmt.Fprintf(w, "# config file: %s\n", a.loadedPath)
			} else {
				fmt.Fprintf(w, "# config file: none found (looked for %s); showing defaults\n", configLookup(a))
			}
			if cfg.APIKey != "" {
				fmt.Fprintf(w, "# %s: set (%d characters)\n", config.APIKeyEnv, len(cfg.APIKey))
			} else {
				fmt.Fprintf(w, "# %s: NOT SET\n", config.APIKeyEnv)
			}
			fmt.Fprintf(w, "%s", out)
			if finalizeErr != nil {
				return fmt.Errorf("configuration is not usable yet: %w", finalizeErr)
			}
			return nil
		},
	}
}

func configLookup(a *app) string {
	if a.configPath != "" {
		return a.configPath
	}
	return config.DefaultPath()
}
