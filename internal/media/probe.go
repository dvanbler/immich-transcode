package media

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Probe is the subset of ffprobe output the pipeline needs.
type Probe struct {
	FormatName string
	Duration   float64 // seconds
	Size       int64
	BitRate    int64 // container bitrate, bits/s

	Video  VideoStream
	Audio  []AudioStream
	HasVid bool
}

// VideoStream describes the first video stream.
type VideoStream struct {
	Codec          string
	Profile        string
	PixFmt         string
	ColorRange     string
	ColorTransfer  string
	ColorPrimaries string
	BitsPerRaw     int
	Width, Height  int
	FPS            float64
	BitRate        int64
	Rotation       int // degrees, normalised to 0, 90, 180 or 270
}

// AudioStream describes one audio stream.
type AudioStream struct {
	Codec string
}

// DisplaySize returns the dimensions as shown to the viewer, after rotation.
func (v VideoStream) DisplaySize() (int, int) {
	if v.Rotation == 90 || v.Rotation == 270 {
		return v.Height, v.Width
	}
	return v.Width, v.Height
}

// FullRange reports whether the stream uses full-range (JPEG) levels, as
// Pixel and some other phones record.
func (v VideoStream) FullRange() bool {
	return strings.HasPrefix(v.PixFmt, "yuvj") || v.ColorRange == "pc"
}

// HighBitDepthOrHDR reports 10-bit or HDR content, which v1 skips.
func (v VideoStream) HighBitDepthOrHDR() bool {
	if strings.EqualFold(v.Profile, "Main 10") || strings.EqualFold(v.Profile, "Main10") ||
		strings.Contains(strings.ToLower(v.Profile), "high 10") {
		return true
	}
	switch v.ColorTransfer {
	case "smpte2084", "arib-std-b67":
		return true
	}
	if v.BitsPerRaw > 8 {
		return true
	}
	// Pixel formats such as yuv420p10le, p010le, yuv422p12le.
	return strings.Contains(v.PixFmt, "p10") || strings.Contains(v.PixFmt, "p12") ||
		strings.HasPrefix(v.PixFmt, "p01") || strings.HasPrefix(v.PixFmt, "p21")
}

// BPP returns the real bits per pixel per frame for the video stream. It uses
// the stream bitrate when ffprobe reports one, otherwise the container
// bitrate (which includes audio, so slightly overstates bpp).
func (p Probe) BPP() float64 {
	br := p.Video.BitRate
	if br <= 0 {
		br = p.BitRate
	}
	if br <= 0 && p.Duration > 0 {
		br = int64(float64(p.Size*8) / p.Duration)
	}
	fps := p.Video.FPS
	if fps <= 0 {
		fps = 30
	}
	px := float64(p.Video.Width * p.Video.Height)
	if px <= 0 || br <= 0 {
		return 0
	}
	return float64(br) / (px * fps)
}

type rawProbe struct {
	Streams []struct {
		CodecType      string            `json:"codec_type"`
		CodecName      string            `json:"codec_name"`
		Profile        string            `json:"profile"`
		PixFmt         string            `json:"pix_fmt"`
		ColorRange     string            `json:"color_range"`
		ColorTransfer  string            `json:"color_transfer"`
		ColorPrimaries string            `json:"color_primaries"`
		BitsPerRaw     string            `json:"bits_per_raw_sample"`
		Width          int               `json:"width"`
		Height         int               `json:"height"`
		AvgFrameRate   string            `json:"avg_frame_rate"`
		RFrameRate     string            `json:"r_frame_rate"`
		BitRate        string            `json:"bit_rate"`
		NbReadPackets  string            `json:"nb_read_packets"`
		Tags           map[string]string `json:"tags"`
		Disposition    map[string]int    `json:"disposition"`
		SideDataList   []struct {
			SideDataType string  `json:"side_data_type"`
			Rotation     float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
}

// ProbeFile runs ffprobe on path.
func (t Tools) ProbeFile(ctx context.Context, path string) (Probe, error) {
	out, _, err := runCapture(ctx, t.FFprobe, "-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", path)
	if err != nil {
		return Probe{}, err
	}
	return ParseProbe(out)
}

// ParseProbe parses `ffprobe -print_format json -show_streams -show_format`.
func ParseProbe(data []byte) (Probe, error) {
	var r rawProbe
	if err := json.Unmarshal(data, &r); err != nil {
		return Probe{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	p := Probe{
		FormatName: r.Format.FormatName,
		Duration:   atof(r.Format.Duration),
		Size:       atoi(r.Format.Size),
		BitRate:    atoi(r.Format.BitRate),
	}
	for _, s := range r.Streams {
		switch s.CodecType {
		case "video":
			// Cover art in MP4/MOV appears as a video stream; skip it.
			if p.HasVid || s.Disposition["attached_pic"] == 1 {
				continue
			}
			p.HasVid = true
			fps := parseRate(s.AvgFrameRate)
			if fps <= 0 {
				fps = parseRate(s.RFrameRate)
			}
			rot := 0.0
			for _, sd := range s.SideDataList {
				if sd.SideDataType == "Display Matrix" {
					rot = sd.Rotation
				}
			}
			if rot == 0 {
				// Older ffmpeg reports rotation as a tag, clockwise.
				if v, ok := s.Tags["rotate"]; ok {
					rot = -atof(v)
				}
			}
			p.Video = VideoStream{
				Codec:          s.CodecName,
				Profile:        s.Profile,
				PixFmt:         s.PixFmt,
				ColorRange:     s.ColorRange,
				ColorTransfer:  s.ColorTransfer,
				ColorPrimaries: s.ColorPrimaries,
				BitsPerRaw:     int(atoi(s.BitsPerRaw)),
				Width:          s.Width,
				Height:         s.Height,
				FPS:            fps,
				BitRate:        atoi(s.BitRate),
				Rotation:       normRotation(rot),
			}
		case "audio":
			p.Audio = append(p.Audio, AudioStream{Codec: s.CodecName})
		}
	}
	if !p.HasVid {
		return p, fmt.Errorf("no video stream")
	}
	return p, nil
}

// normRotation maps a display-matrix rotation (counter-clockwise degrees,
// possibly negative) to 0, 90, 180 or 270.
func normRotation(deg float64) int {
	r := int(math.Round(deg/90)) * 90
	r %= 360
	if r < 0 {
		r += 360
	}
	return r
}

func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return atof(s)
	}
	n, d := atof(num), atof(den)
	if d == 0 {
		return 0
	}
	return n / d
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func atoi(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// DuplicateTimestamps counts video packets whose presentation timestamp
// repeats an earlier one: frames that would be shown at the same instant.
func (t Tools) DuplicateTimestamps(ctx context.Context, path string) (int, error) {
	out, _, err := runCapture(ctx, t.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pts", "-of", "csv=p=0", path)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	dups := 0
	for _, line := range strings.Split(string(out), "\n") {
		pts := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if pts == "" || pts == "N/A" {
			continue
		}
		if seen[pts] {
			dups++
		}
		seen[pts] = true
	}
	return dups, nil
}

// CountVideoPackets counts packets in the first video stream by demuxing the
// file (fast; does not decode).
func (t Tools) CountVideoPackets(ctx context.Context, path string) (int64, error) {
	out, _, err := runCapture(ctx, t.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-count_packets", "-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	s = strings.TrimSuffix(s, ",")
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse packet count %q: %w", s, err)
	}
	return n, nil
}
