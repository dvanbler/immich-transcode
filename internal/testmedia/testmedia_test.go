package testmedia

import (
	"context"
	"os/exec"
	"testing"

	"immich-transcode/internal/media"
)

func TestGenerate(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed")
	}
	dir := t.TempDir()
	s, err := Generate(dir, "ffmpeg", "exiftool")
	if err != nil {
		t.Fatal(err)
	}
	tools := media.Tools{FFmpeg: "ffmpeg", FFprobe: "ffprobe", ExifTool: "exiftool"}
	ctx := context.Background()

	p, err := tools.ProbeFile(ctx, s.Portrait.Path)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := p.Video.DisplaySize(); w >= h {
		t.Errorf("portrait displays %dx%d (rotation %d)", w, h, p.Video.Rotation)
	}
	m, _ := tools.ReadMeta(ctx, s.Portrait.Path)
	if m.GPSLatitude == nil || m.CreateDate != s.Portrait.CreateDate {
		t.Errorf("portrait meta %+v", m)
	}
	p, _ = tools.ProbeFile(ctx, s.FullRange.Path)
	if !p.Video.FullRange() {
		t.Errorf("full range fixture: %+v", p.Video)
	}
	p, _ = tools.ProbeFile(ctx, s.HDR.Path)
	if !p.Video.HighBitDepthOrHDR() {
		t.Errorf("hdr fixture: %+v", p.Video)
	}
	if probs, err := tools.DecodeCheck(ctx, s.Corrupt.Path); err != nil || probs == "" {
		t.Errorf("corrupt fixture decoded cleanly (err %v)", err)
	}
	if probs, _ := tools.DecodeCheck(ctx, s.Portrait.Path); probs != "" {
		t.Errorf("portrait fixture has decode errors: %s", probs)
	}
}
