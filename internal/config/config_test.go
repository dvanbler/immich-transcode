package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if c.ManifestPath != "/mnt/backup/manifest.db" || c.Preset != "slow" || c.MinSavingsPercent != 25 {
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
