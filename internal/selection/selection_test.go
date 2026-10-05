package selection

import (
	"math"
	"testing"

	"immich-transcode/internal/config"
	"immich-transcode/internal/media"
)

func TestEstimates(t *testing.T) {
	// 1 GB, 100 s, 1920x1080: 80 Mbit/s → 80e6 / (2073600*30) ≈ 1.286 bpp.
	e := Estimates(1_000_000_000, 100, 1920, 1080, 0.05)
	if math.Abs(e.Bitrate-80e6) > 1 {
		t.Fatalf("bitrate = %v", e.Bitrate)
	}
	if math.Abs(e.BPP-1.286) > 0.001 {
		t.Fatalf("bpp = %v", e.BPP)
	}
	// 0.05 bpp × 2073600 × 30 × 100 s / 8 = 38,880,000 bytes.
	if e.EstNewSize != 38_880_000 || e.EstSavings != 1_000_000_000-38_880_000 {
		t.Fatalf("est new %d savings %d", e.EstNewSize, e.EstSavings)
	}
}

func TestEstimatesNeverNegative(t *testing.T) {
	e := Estimates(1000, 100, 1920, 1080, 0.05)
	if e.EstSavings != 0 || e.EstNewSize != 1000 {
		t.Fatalf("got new %d savings %d", e.EstNewSize, e.EstSavings)
	}
}

func TestEstimatesUnknownDuration(t *testing.T) {
	e := Estimates(1_000_000_000, 0, 1920, 1080, 0.05)
	if e.BPP != 0 || e.EstSavings != 0 {
		t.Fatalf("expected zero estimate, got %+v", e)
	}
}

func TestPreReason(t *testing.T) {
	big := Estimates(1_000_000_000, 100, 1920, 1080, 0.05)
	cases := []struct {
		name string
		e    Estimate
		want bool // rejected
	}{
		{"candidate", big, false},
		{"too small", Estimates(50_000_000, 10, 1920, 1080, 0.05), true},
		{"no duration", Estimates(1_000_000_000, 0, 1920, 1080, 0.05), true},
		{"no dimensions", Estimates(1_000_000_000, 100, 0, 0, 0.05), true},
		// 200 MB over 600 s at 1080p ≈ 0.043 bpp: already efficient.
		{"efficient", Estimates(200_000_000, 600, 1920, 1080, 0.05), true},
	}
	for _, c := range cases {
		got := PreReason(c.e, 100_000_000, 0.12) != ""
		if got != c.want {
			t.Errorf("%s: rejected=%v want %v (%s)", c.name, got, c.want, PreReason(c.e, 100_000_000, 0.12))
		}
	}
}

func TestRank(t *testing.T) {
	xs := []int64{5, 50, 20}
	Rank(xs, func(v int64) int64 { return v })
	if xs[0] != 50 || xs[1] != 20 || xs[2] != 5 {
		t.Fatalf("got %v", xs)
	}
}

func probeWith(codec string, bitrate int64, mod func(*media.VideoStream)) media.Probe {
	v := media.VideoStream{Codec: codec, PixFmt: "yuv420p", Width: 1920, Height: 1080, FPS: 30, BitRate: bitrate, Profile: "Main"}
	if mod != nil {
		mod(&v)
	}
	return media.Probe{Video: v, HasVid: true, Duration: 10}
}

// bitrateFor returns the bitrate giving bpp at 1080p30.
func bitrateFor(bpp float64) int64 { return int64(bpp * 1920 * 1080 * 30) }

func TestDecide(t *testing.T) {
	th := config.Default().BPP
	cases := []struct {
		name string
		p    media.Probe
		want Verdict
	}{
		{"av1", probeWith("av1", bitrateFor(1), nil), SkipCodec},
		{"h264 high bpp", probeWith("h264", bitrateFor(0.30), nil), Transcode},
		{"h264 at threshold", probeWith("h264", bitrateFor(0.151), nil), Transcode},
		{"h264 low bpp", probeWith("h264", bitrateFor(0.14), nil), SkipEfficient},
		{"hevc 0.13", probeWith("hevc", bitrateFor(0.13), nil), Transcode},
		{"hevc 0.11", probeWith("hevc", bitrateFor(0.11), nil), SkipEfficient},
		{"h264 0.13 is below h264 limit", probeWith("h264", bitrateFor(0.13), nil), SkipEfficient},
		{"main10", probeWith("hevc", bitrateFor(1), func(v *media.VideoStream) { v.Profile = "Main 10" }), SkipHDR},
		{"pq", probeWith("hevc", bitrateFor(1), func(v *media.VideoStream) { v.ColorTransfer = "smpte2084" }), SkipHDR},
		{"hlg", probeWith("h264", bitrateFor(1), func(v *media.VideoStream) { v.ColorTransfer = "arib-std-b67" }), SkipHDR},
		{"10-bit pix_fmt", probeWith("h264", bitrateFor(1), func(v *media.VideoStream) { v.PixFmt = "yuv420p10le"; v.Profile = "High" }), SkipHDR},
		{"real fps lowers bpp", probeWith("h264", bitrateFor(0.20), func(v *media.VideoStream) { v.FPS = 60 }), SkipEfficient},
		{"mpeg4 uses other", probeWith("mpeg4", bitrateFor(0.5), nil), Transcode},
	}
	for _, c := range cases {
		got, why := Decide(c.p, th)
		if got != c.want {
			t.Errorf("%s: got %v want %v (%s)", c.name, got, c.want, why)
		}
	}
}
