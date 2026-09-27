//go:build windows

package control

import (
	"os/exec"

	"reasonix/internal/proc"
)

// memoryExtractCommand builds the detached child command. proc.Command keeps the
// child's console hidden, and a Windows child does not die with its parent, so
// no extra detachment flag is needed.
func memoryExtractCommand(exe string, args []string) *exec.Cmd {
	return proc.Command(exe, args...)
}
