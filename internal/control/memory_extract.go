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
	"syscall"
)

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
	// Отдельная сессия (setsid): child не умирает вместе с родительским
	// процессом/терминалом и не получает его stdin/stdout/stderr.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Process.Release() // fire-and-forget: родитель выходит, init подберёт
}
