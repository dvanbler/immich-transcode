package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := map[string]Size{
		"100":    100,
		"250MB":  250_000_000,
		"1.5GB":  1_500_000_000,
		"2GiB":   2 << 30,
		"512 kb": 512_000,
		"3g":     3_000_000_000,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("%q → %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "MB", "-5MB", "five"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("server_url: http://nas:2283/\nbackup_dir: /mnt/backup\ncrf: 24\nmin_size: 500MB\nbpp:\n  h264: 0.2\n"), 0o644)
	t.Setenv(APIKeyEnv, "secret")
	c, err := Load(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Finalize(); err != nil {
		t.Fatal(err)
	}
	if c.ServerURL != "http://nas:2283" || c.CRF != 24 || c.MinSize != 500*MB || c.APIKey != "secret" {
		t.Errorf("got %+v", c)
	}
	if c.BPP.H264 != 0.2 || c.BPP.HEVC != 0.12 {
		t.Errorf("bpp merge: %+v", c.BPP)
	}
	if c.ManifestPath != "/mnt/backup/manifest.db" || c.WorkDir != "/mnt/backup/work" || c.Preset != "slow" || c.MinSavingsPercent != 25 {
		t.Errorf("defaults: %+v", c)
	}

	os.WriteFile(p, []byte("crf: 24\nbogus: 1\n"), 0o644)
	if _, err := Load(p, true); err == nil {
		t.Error("unknown key accepted")
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml"), false); err != nil {
		t.Errorf("optional missing file: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml"), true); err == nil {
		t.Error("required missing file accepted")
	}
}

// TestYAMLRoundTrip checks that the config command's output is a valid
// config file describing the same settings.
func TestYAMLRoundTrip(t *testing.T) {
	c := Default()
	c.ServerURL = "https://photos.example"
	c.BackupDir = "/mnt/backup"
	c.MinSize = 25 * MB
	c.Preset = "medium"
	c.BPP.H264 = 0.2
	c.TakenBefore, _ = ParseCutoff("90d")
	c.APIKey = "secret"
	if err := c.Finalize(); err != nil {
		t.Fatal(err)
	}
	out, err := c.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret") {
		t.Fatal("API key in output")
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, out, 0o644)
	t.Setenv(APIKeyEnv, "secret")
	got, err := Load(p, true)
	if err != nil {
		t.Fatalf("output does not load: %v\n%s", err, out)
	}
	if got != c {
		t.Errorf("round trip changed settings:\n%+v\n%+v\n%s", got, c, out)
	}
}

func TestCutoff(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.Local)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-09-01", time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local)},
		{"2026-09-01T12:00:00Z", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)},
		{"90d", now.AddDate(0, 0, -90)},
		{"12w", now.AddDate(0, 0, -84)},
		{"0d", now},
	}
	for _, c := range cases {
		cut, err := ParseCutoff(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		got, ok := cut.Before(now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q → %v, want %v", c.in, got, c.want)
		}
	}
	if _, ok := (Cutoff{}).Before(now); ok {
		t.Error("empty cutoff limits")
	}
	for _, bad := range []string{"yesterday", "90", "d", "-5d", "2026-13-01", "3m"} {
		if _, err := ParseCutoff(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
