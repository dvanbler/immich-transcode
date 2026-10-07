//go:build !windows

package media

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own process group. A terminal Ctrl-C is sent
// to the whole foreground process group; without this, ffmpeg would receive
// it directly and quit mid-step, defeating the "finish the current step"
// first Ctrl-C. Cancelling the command's context still kills the child.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
