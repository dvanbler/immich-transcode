//go:build windows

package media

import (
	"os/exec"
	"syscall"
)

// detach starts the child in a new process group so a console Ctrl-C (which
// Windows sends to every process attached to the console) is not delivered
// to it. Cancelling the command's context still kills the child.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
