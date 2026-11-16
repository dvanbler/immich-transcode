//go:build !windows

package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/dvanbler/immich-transcode/internal/logging"
)

func interrupt(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
}

// The first Ctrl-C asks for a graceful stop and leaves the context alone;
// the second cancels it.
func TestInterruptibleTwoStage(t *testing.T) {
	a := &app{console: logging.NewConsole()}
	first := make(chan struct{})
	ctx, stop := a.interruptible(context.Background(), func() { close(first) })
	defer stop()

	interrupt(t)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("first Ctrl-C did not call onFirst")
	}
	if ctx.Err() != nil {
		t.Fatal("first Ctrl-C cancelled the context")
	}
	interrupt(t)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("second Ctrl-C did not cancel the context")
	}
}

// Without a graceful stage, the first Ctrl-C cancels.
func TestInterruptibleCancelsAtOnce(t *testing.T) {
	a := &app{console: logging.NewConsole()}
	ctx, stop := a.interruptible(context.Background(), nil)
	defer stop()
	interrupt(t)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not cancel the context")
	}
}

// stop cancels the context and is safe to call twice, and a parent's
// cancellation is passed on.
func TestInterruptibleStopAndParent(t *testing.T) {
	a := &app{console: logging.NewConsole()}
	ctx, stop := a.interruptible(context.Background(), nil)
	stop()
	stop()
	if ctx.Err() == nil {
		t.Error("stop did not cancel the context")
	}
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop = a.interruptible(parent, nil)
	defer stop()
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("cancelling the parent did not cancel the context")
	}
}
