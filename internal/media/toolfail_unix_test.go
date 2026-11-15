//go:build !windows

package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// script writes an executable shell script standing in for a tool.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A tool that cannot start or is killed says nothing about the file, so it
// must surface as ErrToolFailed rather than as a verdict on the file.
func TestToolFailureIsNotAVerdict(t *testing.T) {
	ctx := context.Background()
	broken := map[string]string{
		"missing": filepath.Join(t.TempDir(), "no-such-tool"),
		"killed":  script(t, "kill -9 $$"),
	}
	for name, tool := range broken {
		t.Run(name, func(t *testing.T) {
			tools := Tools{FFmpeg: tool, FFprobe: tool}
			problems, err := tools.DecodeCheck(ctx, "in.mp4")
			if !errors.Is(err, ErrToolFailed) || problems != "" {
				t.Errorf("DecodeCheck = %q, %v; want ErrToolFailed and no problems", problems, err)
			}
			if _, err := tools.ProbeFile(ctx, "in.mp4"); !errors.Is(err, ErrToolFailed) {
				t.Errorf("ProbeFile error = %v; want ErrToolFailed", err)
			}
		})
	}
}

// A tool that runs and exits non-zero has judged the file.
func TestToolRejectionIsAVerdict(t *testing.T) {
	ctx := context.Background()
	tool := script(t, "echo 'Invalid data found' >&2; exit 1")
	tools := Tools{FFmpeg: tool, FFprobe: tool}
	problems, err := tools.DecodeCheck(ctx, "in.mp4")
	if err != nil || problems != "Invalid data found" {
		t.Errorf("DecodeCheck = %q, %v; want the stderr text and no error", problems, err)
	}
	if _, err := tools.ProbeFile(ctx, "in.mp4"); err == nil || errors.Is(err, ErrToolFailed) {
		t.Errorf("ProbeFile error = %v; want a rejection, not ErrToolFailed", err)
	}
}
