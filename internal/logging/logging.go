// Package logging sets up the structured log file and the console printer.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// OpenFile returns a JSON logger appending to path.
func OpenFile(path string) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}
	return slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})), f, nil
}

// Console prints concise human output. When attached to a terminal it keeps
// a single updating status line below the regular output.
type Console struct {
	mu       sync.Mutex
	w        io.Writer
	tty      bool
	status   string
	disabled bool // status line disabled (e.g. several workers)
}

// NewConsole writes to stdout.
func NewConsole() *Console {
	fi, err := os.Stdout.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	return &Console{w: os.Stdout, tty: tty}
}

// DisableStatus turns off the live status line.
func (c *Console) DisableStatus() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
	c.disabled = true
}

// Printf prints a line above the status line.
func (c *Console) Printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
	fmt.Fprintf(c.w, format, args...)
	if !strings.HasSuffix(format, "\n") {
		fmt.Fprintln(c.w)
	}
	c.drawLocked()
}

// Status replaces the live status line. An empty string clears it.
func (c *Console) Status(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tty || c.disabled {
		return
	}
	c.clearLocked()
	c.status = s
	c.drawLocked()
}

func (c *Console) clearLocked() {
	if c.tty && c.status != "" {
		fmt.Fprint(c.w, "\r\033[2K")
	}
}

func (c *Console) drawLocked() {
	if c.tty && !c.disabled && c.status != "" {
		fmt.Fprint(c.w, c.status)
	}
}

// Writer returns the underlying writer for tables.
func (c *Console) Writer() io.Writer { return c.w }
