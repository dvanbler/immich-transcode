// Package config loads immich-transcode settings from a YAML file, with
// command-line flags applied on top by the CLI layer.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// APIKeyEnv is the environment variable holding the Immich API key.
const APIKeyEnv = "IMMICH_API_KEY"

// Config holds every tunable setting.
type Config struct {
	ServerURL    string `yaml:"server_url"`
	BackupDir    string `yaml:"backup_dir"`
	WorkDir      string `yaml:"work_dir"`
	ManifestPath string `yaml:"manifest"`
	LogFile      string `yaml:"log_file"`

	FFmpeg   string `yaml:"ffmpeg"`
	FFprobe  string `yaml:"ffprobe"`
	ExifTool string `yaml:"exiftool"`

	CRF               int     `yaml:"crf"`
	Preset            string  `yaml:"preset"`
	MinSavingsPercent float64 `yaml:"min_savings_percent"`
	MinSize           Size    `yaml:"min_size"`
	Concurrency       int     `yaml:"concurrency"`

	BPP BPPThresholds `yaml:"bpp"`

	// MetadataTimeout bounds how long to wait for Immich to finish metadata
	// extraction on a newly uploaded asset.
	MetadataTimeout time.Duration `yaml:"metadata_timeout"`

	APIKey string `yaml:"-"`
}

// BPPThresholds are bits-per-pixel limits used to decide whether a video is
// already efficiently encoded.
type BPPThresholds struct {
	// ScanMin is the estimated bpp below which scan ignores an asset. The
	// codec is unknown at scan time, so this is the lowest per-codec limit.
	ScanMin float64 `yaml:"scan_min"`
	H264    float64 `yaml:"h264"`
	HEVC    float64 `yaml:"hevc"`
	// Other applies to any codec without its own limit (vp9, mpeg4, ...).
	Other float64 `yaml:"other"`
	// EstimatedOutput is the bpp assumed for the HEVC output when estimating
	// savings before an encode has run.
	EstimatedOutput float64 `yaml:"estimated_output"`
}

// Default returns the built-in settings.
func Default() Config {
	return Config{
		ServerURL:         "http://localhost:2283",
		FFmpeg:            "ffmpeg",
		FFprobe:           "ffprobe",
		ExifTool:          "exiftool",
		CRF:               26,
		Preset:            "slow",
		MinSavingsPercent: 25,
		MinSize:           100 * MB,
		Concurrency:       1,
		BPP: BPPThresholds{
			ScanMin:         0.12,
			H264:            0.15,
			HEVC:            0.12,
			Other:           0.12,
			EstimatedOutput: 0.05,
		},
		MetadataTimeout: 10 * time.Minute,
	}
}

// DefaultPath is where the config file is looked for when --config is unset.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "immich-transcode.yaml"
	}
	return filepath.Join(dir, "immich-transcode", "config.yaml")
}

// Load reads path over the defaults. A missing file is not an error unless
// required is set (the user named it explicitly).
func Load(path string, required bool) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !required:
	case err != nil:
		return cfg, fmt.Errorf("read config: %w", err)
	default:
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	cfg.APIKey = os.Getenv(APIKeyEnv)
	return cfg, nil
}

// Finalize fills derived defaults and validates. Call after flag overrides.
func (c *Config) Finalize() error {
	if c.ServerURL == "" {
		return errors.New("server_url is required")
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	if c.BackupDir == "" {
		return errors.New("backup_dir is required (set it in the config file or with --backup-dir)")
	}
	if c.WorkDir == "" {
		// Not under /tmp: validated encodes must survive a reboot so a dry
		// run's work is not redone before --apply.
		c.WorkDir = filepath.Join(c.BackupDir, "work")
	}
	if c.ManifestPath == "" {
		// Kept with the backups: both must outlive the work directory.
		c.ManifestPath = filepath.Join(c.BackupDir, "manifest.db")
	}
	if c.LogFile == "" {
		c.LogFile = filepath.Join(c.BackupDir, "immich-transcode.log")
	}
	if c.CRF < 0 || c.CRF > 51 {
		return fmt.Errorf("crf %d out of range 0-51", c.CRF)
	}
	if c.MinSavingsPercent < 0 || c.MinSavingsPercent >= 100 {
		return fmt.Errorf("min_savings_percent %.1f out of range 0-99", c.MinSavingsPercent)
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}
	if c.MetadataTimeout <= 0 {
		c.MetadataTimeout = 10 * time.Minute
	}
	return nil
}

// Size is a byte count that accepts human units ("500MB", "1.5GiB") in
// config files and flags.
type Size int64

const (
	KB Size = 1000
	MB      = 1000 * KB
	GB      = 1000 * MB
)

var sizeUnits = []struct {
	suffix string
	mult   float64
}{
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
	{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"tb", 1e12},
	{"k", 1e3}, {"m", 1e6}, {"g", 1e9}, {"t", 1e12},
	{"b", 1},
}

// ParseSize parses a size like "250MB", "2GiB" or "1048576".
func ParseSize(s string) (Size, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, errors.New("empty size")
	}
	mult := 1.0
	for _, u := range sizeUnits {
		if strings.HasSuffix(t, u.suffix) {
			mult = u.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return Size(v * mult), nil
}

func (s Size) String() string { return HumanBytes(int64(s)) }

// Set and Type implement pflag.Value.
func (s *Size) Set(v string) error {
	p, err := ParseSize(v)
	if err != nil {
		return err
	}
	*s = p
	return nil
}

func (s *Size) Type() string { return "size" }

func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	return s.Set(n.Value)
}

// HumanBytes formats n with decimal units, e.g. "1.23 GB".
func HumanBytes(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	sign := ""
	if neg {
		sign = "-"
	}
	if i == 0 {
		return fmt.Sprintf("%s%d B", sign, n)
	}
	return fmt.Sprintf("%s%.2f %s", sign, v, units[i])
}
