//go:build !windows

package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestCtrlCDoesNotReachChildren reproduces a terminal Ctrl-C, which is sent
// to every process in the foreground process group. A helper process (in its
// own group) starts an ffmpeg job, then signals its whole group. ffmpeg must
// not receive the signal; only cancelling the context may stop it.
func TestCtrlCDoesNotReachChildren(t *testing.T) {
	if os.Getenv("CTRLC_HELPER") == "1" {
		ctrlCHelper()
		return
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCtrlCDoesNotReachChildren$")
	cmd.Env = append(os.Environ(), "CTRLC_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg did not survive a process-group Ctrl-C: %v\n%s", err, out)
	}
}

func ctrlCHelper() {
	// Like the CLI: catch Ctrl-C instead of dying from it.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)

	done := make(chan error, 1)
	go func() {
		// About 3 s of work on a single thread, so the signal arrives mid-job.
		done <- Tools{FFmpeg: "ffmpeg"}.Transcode(context.Background(), []string{
			"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=duration=20:size=640x360:rate=30",
			"-c:v", "libx264", "-preset", "slower", "-threads", "1", "-progress", "pipe:1", "-f", "null", "-",
		}, nil)
	}()
	time.Sleep(700 * time.Millisecond)
	syscall.Kill(0, syscall.SIGINT) // the whole process group, as a terminal does
	select {
	case <-sigs:
	case <-time.After(5 * time.Second):
		fmt.Println("helper never saw its own SIGINT")
		os.Exit(2)
	}
	if err := <-done; err != nil {
		fmt.Println("transcode failed:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
