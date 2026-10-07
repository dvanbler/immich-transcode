// Package testmedia generates small sample videos for tests: a rotated phone
// portrait clip, a full-range Pixel-style clip, an HDR clip and a corrupt
// file. Clips are encoded at a high bitrate so an HEVC re-encode is clearly
// smaller.
package testmedia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Fixture is one generated clip and the facts tests assert against.
type Fixture struct {
	Name     string
	Path     string
	Lat, Lon float64 // 0,0 means no GPS
	// CreateDate is the QuickTime CreateDate (UTC) written into the file.
	CreateDate string
}

// Set holds all generated fixtures.
type Set struct {
	Portrait  Fixture // h264, display matrix rotated 90°, GPS, make/model
	FullRange Fixture // h264 yuvj420p / color_range pc, as Pixel phones record
	HDR       Fixture // hevc Main 10, PQ transfer
	Corrupt   Fixture // h264 with damaged packets
	// VFR mimics Pixel timestamps: frames every 3013/90000 s with ±1 tick
	// jitter, so ffmpeg guesses 179/6 fps. Valid, but a naive null-muxer
	// decode check reports non-monotonic dts on it.
	VFR Fixture
}

// Generate writes the fixtures into dir using the given tools.
func Generate(dir, ffmpeg, exiftool string) (*Set, error) {
	s := &Set{
		Portrait:  Fixture{Name: "PXL_20210601_102030123.mp4", Lat: 47.6062, Lon: -122.3321, CreateDate: "2021:06:01 10:20:30"},
		FullRange: Fixture{Name: "PXL_20220315_080910456.mp4", Lat: 51.5072, Lon: -0.1276, CreateDate: "2022:03:15 08:09:10"},
		HDR:       Fixture{Name: "IMG_HDR_0001.mov", CreateDate: "2023:07:04 18:00:00"},
		Corrupt:   Fixture{Name: "VID_CORRUPT_0001.mp4", CreateDate: "2020:01:01 00:00:00"},
		VFR:       Fixture{Name: "PXL_20240725_123038593.TS.mp4", CreateDate: "2024:07:25 12:30:38"},
	}
	for _, f := range []*Fixture{&s.Portrait, &s.FullRange, &s.HDR, &s.Corrupt, &s.VFR} {
		f.Path = filepath.Join(dir, f.Name)
	}
	src := "testsrc2=size=1280x720:rate=30,noise=alls=40:allf=t"
	audio := []string{"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000"}
	const secs = "3"
	// High-quality H.264 so there is plenty to save.
	h264 := []string{"-c:v", "libx264", "-preset", "ultrafast", "-crf", "8", "-c:a", "aac", "-b:a", "128k"}

	steps := [][]string{
		// Portrait: encode landscape pixels, then tag a 90° display matrix
		// the way phones do.
		append(append(append([]string{"-f", "lavfi", "-i", src}, audio...), "-t", secs, "-pix_fmt", "yuv420p"),
			append(h264, "-metadata", "creation_time="+isoDate(s.Portrait.CreateDate), dir+"/portrait-base.mp4")...),
		{"-display_rotation", "90", "-i", dir + "/portrait-base.mp4", "-c", "copy", "-map_metadata", "0", s.Portrait.Path},
		// Full range.
		append(append(append([]string{"-f", "lavfi", "-i", src}, audio...), "-t", secs, "-pix_fmt", "yuvj420p", "-color_range", "pc"),
			append(h264, "-metadata", "creation_time="+isoDate(s.FullRange.CreateDate), s.FullRange.Path)...),
		// HDR: 10-bit HEVC tagged PQ/BT.2020.
		append(append([]string{"-f", "lavfi", "-i", src}, audio...), "-t", secs, "-pix_fmt", "yuv420p10le",
			"-c:v", "libx265", "-preset", "ultrafast", "-crf", "10", "-x265-params", "log-level=error",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-tag:v", "hvc1",
			"-c:a", "aac", "-metadata", "creation_time="+isoDate(s.HDR.CreateDate), s.HDR.Path),
		// VFR: 40 s so the timestamp-slot collision occurs (every ~790
		// frames). Small frame size keeps it quick.
		append(append([]string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30,noise=alls=40:allf=t"}, audio...),
			"-t", "40", "-vf", "settb=1/90000,setpts=N*3013+mod(N\\,2)", "-pix_fmt", "yuv420p",
			"-fps_mode", "passthrough", "-enc_time_base", "1/90000", "-video_track_timescale", "90000",
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "8", "-c:a", "aac", "-b:a", "128k",
			"-metadata", "creation_time="+isoDate(s.VFR.CreateDate), s.VFR.Path),
		// Corrupt: a valid clip, damaged below.
		append(append(append([]string{"-f", "lavfi", "-i", src}, audio...), "-t", secs, "-pix_fmt", "yuv420p"),
			append(h264, "-metadata", "creation_time="+isoDate(s.Corrupt.CreateDate), s.Corrupt.Path)...),
	}
	for _, args := range steps {
		full := append([]string{"-nostdin", "-hide_banner", "-v", "error", "-y"}, args...)
		if out, err := exec.Command(ffmpeg, full...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	os.Remove(dir + "/portrait-base.mp4")

	for _, f := range []Fixture{s.Portrait, s.FullRange} {
		gps := fmt.Sprintf("%.4f %.4f 0", f.Lat, f.Lon)
		out, err := exec.Command(exiftool, "-overwrite_original",
			"-Keys:GPSCoordinates#="+gps, "-UserData:GPSCoordinates#="+gps,
			"-UserData:Make=Google", "-UserData:Model=Pixel 6", f.Path).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("exiftool %s: %v: %s", f.Name, err, out)
		}
	}
	if err := damage(s.Corrupt.Path); err != nil {
		return nil, err
	}
	return s, nil
}

// damage overwrites bytes in the middle of the file (inside mdat for these
// small faststart-less clips) so decoding reports errors.
func damage(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for i := len(b) / 4; i < len(b)*3/4; i += 997 {
		for j := 0; j < 64 && i+j < len(b); j++ {
			b[i+j] = byte(0xA5 ^ j)
		}
	}
	return os.WriteFile(path, b, 0o644)
}

func isoDate(exifDate string) string {
	// "2021:06:01 10:20:30" → "2021-06-01T10:20:30Z"
	d, t, _ := strings.Cut(exifDate, " ")
	return strings.ReplaceAll(d, ":", "-") + "T" + t + "Z"
}
