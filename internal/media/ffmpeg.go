package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DecodeCheck fully decodes the first video stream. Any message ffmpeg
// prints at error level means the file is damaged; it is returned as the
// error text.
func (t Tools) DecodeCheck(ctx context.Context, path string) (problems string, err error) {
	cmd := exec.CommandContext(ctx, t.FFmpeg, "-nostdin", "-hide_banner", "-v", "error",
		"-i", path, "-map", "0:v:0", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	msg := strings.TrimSpace(stderr.String())
	if runErr != nil && msg == "" {
		msg = runErr.Error()
	}
	return head(msg, 4000), nil
}

// Container is the output muxer and file extension.
type Container struct {
	Format string // ffmpeg muxer name
	Ext    string // including the dot
}

var (
	MP4 = Container{Format: "mp4", Ext: ".mp4"}
	MOV = Container{Format: "mov", Ext: ".mov"}
)

// ContainerFor keeps QuickTime sources (.mov, typical of iPhones) in MOV so
// the uploaded filename and metadata atoms stay as close to the original as
// possible; everything else becomes MP4.
func ContainerFor(originalName string) Container {
	if strings.EqualFold(filepath.Ext(originalName), ".mov") {
		return MOV
	}
	return MP4
}

// OutputName is the original base name with the container's extension, e.g.
// "PXL_20240101.mp4" or "IMG_0001.MOV" (original extension case kept).
func OutputName(originalName string, c Container) string {
	ext := filepath.Ext(originalName)
	if strings.EqualFold(ext, c.Ext) {
		return originalName
	}
	return strings.TrimSuffix(originalName, ext) + c.Ext
}

// mp4AudioCodecs can be stream-copied into an MP4. MOV accepts anything
// ffmpeg can mux, including PCM.
var mp4AudioCodecs = map[string]bool{
	"aac": true, "mp3": true, "ac3": true, "eac3": true, "alac": true,
	"opus": true, "flac": true, "mp2": true,
}

// EncodeSettings are the encoder knobs from config.
type EncodeSettings struct {
	CRF    int
	Preset string
}

// TranscodeArgs builds the ffmpeg command line for an HEVC re-encode of in
// to out. Progress is written as key=value lines to stdout.
func TranscodeArgs(in, out string, p Probe, c Container, s EncodeSettings) []string {
	args := []string{
		"-nostdin", "-hide_banner", "-v", "error", "-y",
		"-i", in,
		"-map", "0:v:0", "-map", "0:a?",
		"-c:v", "libx265", "-preset", s.Preset, "-crf", strconv.Itoa(s.CRF),
		"-x265-params", "log-level=error",
		"-tag:v", "hvc1",
	}
	if p.Video.FullRange() {
		args = append(args,
			"-vf", "scale=in_range=full:out_range=full,format=yuv420p",
			"-color_range", "pc")
	} else {
		args = append(args, "-pix_fmt", "yuv420p")
	}
	args = append(args, "-c:a", audioCodec(p, c))
	if audioCodec(p, c) == "aac" {
		args = append(args, "-b:a", "192k")
	}
	args = append(args,
		"-fps_mode", "passthrough",
		"-map_metadata", "0",
		"-movflags", "+faststart+use_metadata_tags",
		"-progress", "pipe:1", "-nostats",
		"-f", c.Format,
		out,
	)
	return args
}

// audioCodec is "copy" unless an audio stream cannot be carried in the
// target container, in which case audio is re-encoded to AAC.
func audioCodec(p Probe, c Container) string {
	if c == MP4 {
		for _, a := range p.Audio {
			if !mp4AudioCodecs[a.Codec] {
				return "aac"
			}
		}
	}
	return "copy"
}

// Progress is one update parsed from ffmpeg's -progress output.
type Progress struct {
	OutTime time.Duration
	Speed   string
	Done    bool
}

// Transcode runs ffmpeg with args, calling onProgress as it goes. Cancelling
// ctx kills ffmpeg; the caller removes the partial output.
func (t Tools) Transcode(ctx context.Context, args []string, onProgress func(Progress)) error {
	cmd := exec.CommandContext(ctx, t.FFmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	parseProgress(stdout, onProgress)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, tail(stderr.String(), 2000))
	}
	return nil
}

func parseProgress(r io.Reader, onProgress func(Progress)) {
	sc := bufio.NewScanner(r)
	var cur Progress
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "out_time_us", "out_time_ms": // both are microseconds
			if us, err := strconv.ParseInt(v, 10, 64); err == nil && us >= 0 {
				cur.OutTime = time.Duration(us) * time.Microsecond
			}
		case "speed":
			cur.Speed = strings.TrimSpace(v)
		case "progress":
			cur.Done = v == "end"
			if onProgress != nil {
				onProgress(cur)
			}
		}
	}
	_, _ = io.Copy(io.Discard, r)
}
