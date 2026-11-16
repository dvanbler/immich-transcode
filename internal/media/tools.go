// Package media wraps the ffmpeg, ffprobe and exiftool subprocesses.
package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Tools holds resolved paths to the external programs.
type Tools struct {
	FFmpeg   string
	FFprobe  string
	ExifTool string

	// TimeBaseFlag is the -enc_time_base value meaning "keep the source's
	// time base": "demux" on ffmpeg 7+, which Check detects by asking
	// ffmpeg. Empty means the older spelling (see timeBase).
	TimeBaseFlag string
}

// SourceTimeBase returns the -enc_time_base value that keeps the source's
// time base.
func (t Tools) SourceTimeBase() string { return timeBase(t.TimeBaseFlag) }

// ToolInfo describes one resolved tool.
type ToolInfo struct {
	Name    string
	Path    string
	Version string
}

// Check resolves each tool, reads its version and confirms ffmpeg has the
// libx265 encoder. Paths are replaced with their resolved absolute form.
func (t *Tools) Check(ctx context.Context) ([]ToolInfo, error) {
	var infos []ToolInfo
	var errs []error
	for _, tool := range []struct {
		name string
		path *string
		args []string
	}{
		{"ffmpeg", &t.FFmpeg, []string{"-hide_banner", "-version"}},
		{"ffprobe", &t.FFprobe, []string{"-hide_banner", "-version"}},
		{"exiftool", &t.ExifTool, []string{"-ver"}},
	} {
		resolved, err := exec.LookPath(*tool.path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s not found (%q): %w", tool.name, *tool.path, err))
			continue
		}
		*tool.path = resolved
		out, err := command(ctx, resolved, tool.args...).Output()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s at %s failed to report a version: %w", tool.name, resolved, err))
			continue
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		infos = append(infos, ToolInfo{Name: tool.name, Path: resolved, Version: line})
	}
	if len(errs) > 0 {
		return infos, errors.Join(errs...)
	}
	out, err := command(ctx, t.FFmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		return infos, fmt.Errorf("list ffmpeg encoders: %w", err)
	}
	if !bytes.Contains(out, []byte("libx265")) {
		return infos, errors.New("this ffmpeg build has no libx265 encoder; a BtbN static build (https://github.com/BtbN/FFmpeg-Builds) includes it")
	}
	t.TimeBaseFlag = ""
	if command(ctx, t.FFmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "nullsrc=d=0.1",
		"-enc_time_base", "demux", "-f", "null", "-").Run() == nil {
		t.TimeBaseFlag = "demux"
	}
	return infos, nil
}

// ErrToolFailed marks an external tool that did not run to completion: it
// could not be started, or it was killed (for example by the kernel's
// out-of-memory killer). Unlike a non-zero exit, that says nothing about the
// file the tool was given, so callers must not treat it as a verdict.
var ErrToolFailed = errors.New("did not run to completion")

// exitedNonZero reports whether err is the tool itself exiting with a
// failure status, as opposed to failing to start or dying from a signal
// (ExitCode is -1 for those).
func exitedNonZero(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() > 0
}

// command is exec.CommandContext for every external tool: the child is
// detached from the terminal's Ctrl-C (see detach) and killed only when ctx
// is cancelled.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	detach(cmd)
	return cmd
}

// runCapture runs a command and returns stdout, folding stderr into errors.
func runCapture(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := command(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), stderr.Bytes(), ctx.Err()
		}
		if !exitedNonZero(err) {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s: %w: %w", baseName(name), ErrToolFailed, err)
		}
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s: %w: %s", baseName(name), err, tail(stderr.String(), 2000))
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

func head(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
