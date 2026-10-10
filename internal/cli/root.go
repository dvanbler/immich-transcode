// Package cli defines the immich-transcode commands.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"immich-transcode/internal/config"
	"immich-transcode/internal/immich"
	"immich-transcode/internal/immich/api"
	"immich-transcode/internal/logging"
	"immich-transcode/internal/manifest"
	"immich-transcode/internal/media"
)

type app struct {
	version    string
	configPath string
	loadedPath string // config file actually read ("" if none existed)
	cfg        config.Config
	console    *logging.Console

	// flag overrides
	server, backupDir, workDir, manifestPath, logFile string
	ffmpeg, ffprobe, exiftool, preset                 string
	concurrency                                       int
}

// Execute runs the root command.
func Execute(stampedVersion string) int {
	a := &app{console: logging.NewConsole(), version: resolveVersion(stampedVersion)}
	root := &cobra.Command{
		Use:     "immich-transcode",
		Version: a.version,
		Short:   "Re-encode large, inefficient videos in an Immich library to HEVC",
		Long: `immich-transcode finds large, inefficiently encoded videos in an Immich v3
library and replaces each with an HEVC re-encode, keeping a verified local
backup of the original and preserving timeline date, location, albums,
favorites and other metadata.

The API key is read from the ` + config.APIKeyEnv + ` environment variable.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.loadConfig(cmd)
		},
	}
	f := root.PersistentFlags()
	f.StringVar(&a.configPath, "config", "", "config file (default "+config.DefaultPath()+")")
	f.StringVar(&a.server, "server", "", "Immich server URL")
	f.StringVar(&a.backupDir, "backup-dir", "", "directory for original backups")
	f.StringVar(&a.workDir, "work-dir", "", "directory for temporary encodes")
	f.StringVar(&a.manifestPath, "manifest", "", "manifest database (default <backup-dir>/manifest.db)")
	f.StringVar(&a.logFile, "log-file", "", "structured log file (default <backup-dir>/immich-transcode.log)")
	f.StringVar(&a.ffmpeg, "ffmpeg", "", "path to ffmpeg")
	f.StringVar(&a.ffprobe, "ffprobe", "", "path to ffprobe")
	f.StringVar(&a.exiftool, "exiftool", "", "path to exiftool")
	f.StringVar(&a.preset, "preset", "", "x265 preset")
	f.IntVar(&a.concurrency, "concurrency", 0, "number of assets transcoded at once")

	root.SetVersionTemplate("immich-transcode {{.Version}}\n")
	root.AddCommand(a.scanCmd(), a.runCmd(), a.statusCmd(), a.resumeCmd(), a.resetCmd(), a.verifyCmd(), a.redoCmd(), a.restoreCmd(), a.configCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func (a *app) loadConfig(cmd *cobra.Command) error {
	path, required := a.configPath, true
	if path == "" {
		path, required = config.DefaultPath(), false
	}
	cfg, err := config.Load(path, required)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		a.loadedPath = path
	}
	set := func(name string, dst *string, v string) {
		if cmd.Flags().Changed(name) {
			*dst = v
		}
	}
	set("server", &cfg.ServerURL, a.server)
	set("backup-dir", &cfg.BackupDir, a.backupDir)
	set("work-dir", &cfg.WorkDir, a.workDir)
	set("manifest", &cfg.ManifestPath, a.manifestPath)
	set("log-file", &cfg.LogFile, a.logFile)
	set("ffmpeg", &cfg.FFmpeg, a.ffmpeg)
	set("ffprobe", &cfg.FFprobe, a.ffprobe)
	set("exiftool", &cfg.ExifTool, a.exiftool)
	set("preset", &cfg.Preset, a.preset)
	if cmd.Flags().Changed("concurrency") {
		cfg.Concurrency = a.concurrency
	}
	a.cfg = cfg
	return nil
}

// Permission sets per mode.
var (
	permsRead   = []api.Permission{api.AssetRead, api.UserRead}
	permsDryRun = append(append([]api.Permission{}, permsRead...), api.AssetDownload)
	permsApply  = append(append([]api.Permission{}, permsDryRun...),
		api.AssetUpload, api.AssetUpdate, api.AssetCopy, api.AssetDelete, api.TagAsset)
)

// session is everything a command needs after preflight.
type session struct {
	cfg    config.Config
	client *immich.Client
	tools  media.Tools
	store  *manifest.Store
	log    *slog.Logger
	userID string
	close  func()
}

type preflightOpts struct {
	perms     []api.Permission
	needTools bool
}

// preflight validates config, external tools, server version and API key
// permissions, then opens the manifest and log file.
func (a *app) preflight(ctx context.Context, o preflightOpts) (*session, error) {
	cfg := a.cfg
	if err := cfg.Finalize(); err != nil {
		return nil, err
	}
	s := &session{cfg: cfg}
	a.console.Printf("immich-transcode %s", a.version)
	if o.needTools {
		s.tools = media.Tools{FFmpeg: cfg.FFmpeg, FFprobe: cfg.FFprobe, ExifTool: cfg.ExifTool}
		infos, err := s.tools.Check(ctx)
		for _, i := range infos {
			a.console.Printf("%-8s %s (%s)", i.Name, i.Version, i.Path)
		}
		if err != nil {
			return nil, err
		}
	}
	client, err := immich.New(cfg.ServerURL, cfg.APIKey)
	if err != nil {
		return nil, err
	}
	v, err := client.ServerVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("contact Immich at %s: %w", cfg.ServerURL, err)
	}
	if v.Major < immich.MinMajor {
		return nil, fmt.Errorf("Immich server is %s; immich-transcode requires v%d or later", v, immich.MinMajor)
	}
	a.console.Printf("immich   %s at %s (client built from the v%s API spec)", v, cfg.ServerURL, immich.SpecVersion)
	missing, err := client.MissingPermissions(ctx, o.perms)
	if err != nil {
		return nil, fmt.Errorf("check API key permissions: %w", err)
	}
	if len(missing) > 0 {
		names := make([]string, len(missing))
		for i, p := range missing {
			names[i] = string(p)
		}
		return nil, fmt.Errorf("the API key is missing permissions: %s\nedit the key in Immich (Account Settings → API Keys) and grant them", strings.Join(names, ", "))
	}
	s.client = client
	if s.userID, err = client.MyUserID(ctx); err != nil {
		return nil, err
	}
	return s, a.openLocal(s)
}

func (a *app) openLocal(s *session) error {
	store, err := manifest.Open(s.cfg.ManifestPath)
	if err != nil {
		return err
	}
	log, logCloser, err := logging.OpenFile(s.cfg.LogFile)
	if err != nil {
		store.Close()
		return err
	}
	s.store, s.log = store, log
	s.close = func() {
		store.Close()
		logCloser.Close()
	}
	return nil
}

// interruptible returns a context that is cancelled on the second Ctrl-C;
// the first calls onFirst so work can stop at a step boundary.
func (a *app) interruptible(onFirst func()) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		n := 0
		for range sigs {
			n++
			if n == 1 && onFirst != nil {
				a.console.Printf("\nStopping after the current step (Ctrl-C again to abort it and clean up)...")
				onFirst()
				continue
			}
			a.console.Printf("\nAborting current step...")
			cancel()
			return
		}
	}()
	return ctx, func() { signal.Stop(sigs); cancel() }
}
