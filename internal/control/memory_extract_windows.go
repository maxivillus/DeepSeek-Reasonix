//go:build windows

package control

import (
	"os/exec"
	"syscall"

	"reasonix/internal/proc"
)

// detachMemoryExtract gives the child its own process group so the parent's
// exit does not take it down, and keeps it from flashing a console window.
// Windows has no setsid; CREATE_NEW_PROCESS_GROUP is the closest equivalent.
func detachMemoryExtract(cmd *exec.Cmd) {
	proc.HideWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}
