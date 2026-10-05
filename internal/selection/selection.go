// Package selection decides which videos are worth re-encoding, first from
// Immich API data (scan) and then from ffprobe data (per-asset pipeline).
package selection

import (
	"fmt"
	"sort"

	"immich-transcode/internal/config"
	"immich-transcode/internal/media"
)

// AssumedFPS is used for estimates before ffprobe has run.
const AssumedFPS = 30

// Estimate is what can be inferred about a video from the API alone.
type Estimate struct {
	Size          int64
	DurationSec   float64
	Width, Height int
	Bitrate       float64 // bits/s, from size ÷ duration
	BPP           float64 // bits per pixel per frame at AssumedFPS
	EstNewSize    int64
	EstSavings    int64
}

// Estimates computes bitrate, bpp and expected savings. outBPP is the bits
// per pixel the HEVC output is assumed to reach.
func Estimates(size int64, durationSec float64, width, height int, outBPP float64) Estimate {
	e := Estimate{Size: size, DurationSec: durationSec, Width: width, Height: height}
	if durationSec <= 0 || width <= 0 || height <= 0 {
		return e
	}
	e.Bitrate = float64(size*8) / durationSec
	e.BPP = e.Bitrate / (float64(width*height) * AssumedFPS)
	newBits := outBPP * float64(width*height) * AssumedFPS * durationSec
	e.EstNewSize = int64(newBits / 8)
	if e.EstNewSize > size {
		e.EstNewSize = size
	}
	e.EstSavings = size - e.EstNewSize
	return e
}

// PreReason explains why scan rejected an asset, or "" if it is a candidate.
func PreReason(e Estimate, minSize int64, minBPP float64) string {
	switch {
	case e.Size < minSize:
		return fmt.Sprintf("smaller than %s", config.HumanBytes(minSize))
	case e.DurationSec <= 0:
		return "unknown duration"
	case e.Width <= 0 || e.Height <= 0:
		return "unknown dimensions"
	case e.BPP < minBPP:
		return fmt.Sprintf("estimated %.3f bpp is already below %.3f", e.BPP, minBPP)
	}
	return ""
}

// Rank sorts candidates by estimated savings, largest first. key returns the
// savings for item i.
func Rank[T any](items []T, savings func(T) int64) {
	sort.SliceStable(items, func(i, j int) bool { return savings(items[i]) > savings(items[j]) })
}

// Verdict is the outcome of inspecting a probed source file.
type Verdict int

const (
	Transcode     Verdict = iota
	SkipCodec             // already AV1
	SkipEfficient         // already below the codec's bpp threshold
	SkipHDR               // 10-bit or HDR, not handled in v1
)

// Decide applies the codec, bpp and HDR rules to a probed source.
func Decide(p media.Probe, th config.BPPThresholds) (Verdict, string) {
	v := p.Video
	if v.Codec == "av1" {
		return SkipCodec, "already AV1"
	}
	if v.HighBitDepthOrHDR() {
		return SkipHDR, fmt.Sprintf("10-bit/HDR (profile %q, pix_fmt %s, transfer %s)", v.Profile, v.PixFmt, v.ColorTransfer)
	}
	bpp := p.BPP()
	limit := th.Other
	switch v.Codec {
	case "h264":
		limit = th.H264
	case "hevc":
		limit = th.HEVC
	}
	if bpp < limit {
		return SkipEfficient, fmt.Sprintf("%s at %.3f bpp is below %.3f", v.Codec, bpp, limit)
	}
	return Transcode, fmt.Sprintf("%s at %.3f bpp (%.2f fps)", v.Codec, bpp, v.FPS)
}
