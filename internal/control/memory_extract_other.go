//go:build !windows

package control

import (
	"os/exec"
	"syscall"
)

// detachMemoryExtract runs the child in its own session, so it survives the
// parent process and terminal and never picks up the parent's controlling
// terminal.
func detachMemoryExtract(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
