//go:build !windows

package control

import (
	"os/exec"

	"reasonix/internal/proc"
)

// memoryExtractCommand builds the detached child command. Off Windows the child
// runs in its own session, so it survives the parent process and terminal and
// never picks up the parent's controlling terminal.
func memoryExtractCommand(exe string, args []string) *exec.Cmd {
	cmd := proc.Command(exe, args...)
	proc.SetProcessGroupKill(cmd)
	return cmd
}
