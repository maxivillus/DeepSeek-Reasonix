package control

// Фаза A (2026-08-11): авто-экстракция памяти по концу сессии. В
// Controller.Close после SessionEnd-хуков детачим child-процесс
// `reasonix memory-extract --session … --dir …`, который переживает выход
// родителя: читает транскрипт, дёргает LLM-sidecar и сохраняет факты через
// штатный memory.Store. Включение: env REASONIX_MEMORY_EXTRACT=1 (compose
// reasonix-daemon/multica-worker).

import (
	"os"
	"os/exec"
)

// memoryExtractSpawn is the Close-time seam. Tests replace it to assert the
// session-end trigger stays wired: the call site was silently lost in a rebase
// once already, and nothing else would have noticed.
var memoryExtractSpawn = spawnMemoryExtract

func spawnMemoryExtract(sessionPath, workspaceRoot string) {
	if os.Getenv("REASONIX_MEMORY_EXTRACT") != "1" {
		return
	}
	if sessionPath == "" || workspaceRoot == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "memory-extract", "--session", sessionPath, "--dir", workspaceRoot)
	// Detach the child from this process and its terminal. How that is spelled
	// out is platform-specific — see detachMemoryExtract.
	detachMemoryExtract(cmd)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Process.Release() // fire-and-forget: родитель выходит, init подберёт
}
