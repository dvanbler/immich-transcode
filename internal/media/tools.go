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
}

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
	return infos, nil
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
