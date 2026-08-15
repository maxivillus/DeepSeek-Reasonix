package cli

// memory-extract — скрытая команда авто-экстракции памяти (Фаза A, 2026-08-11).
// Вызывается как child-процесс из Controller.Close (internal/control) и
// переживает выход родителя: читает транскрипт сессии, дёргает LLM-sidecar,
// фильтрует мусор и сохраняет факты через штатный memory.Store.
//
//	reasonix memory-extract --session <stem> --dir <workspace> [--model REF] [--mode project|global|duplicate]

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/memory"
)

func memoryExtractCommand(args []string) int {
	var session, dir, model, mode string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			if i+1 < len(args) {
				session = args[i+1]
				i++
			}
		case "--dir":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case "--model":
			if i+1 < len(args) {
				model = args[i+1]
				i++
			}
		case "--mode":
			if i+1 < len(args) {
				mode = args[i+1]
				i++
			}
		}
	}
	if session == "" || dir == "" {
		return 0
	}
	if mode == "" {
		mode = memory.GlobalWriteMode()
	}

	// Родитель (CLI/daemon) закрывает store при выходе — даём ему 2 секунды,
	// чтобы освободить файлы до нашей записи.
	time.Sleep(2 * time.Second)

	cfg, err := config.Load()
	if err != nil {
		return 0
	}
	if model == "" {
		model = os.Getenv("REASONIX_MEMORY_EXTRACT_MODEL")
	}
	if model == "" {
		model = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(model)
	if !ok || entry == nil {
		return 0
	}
	p, err := boot.NewProvider(entry)
	if err != nil {
		return 0
	}

	transcript := memory.ReadSessionTranscript(session)
	if len(transcript) < memory.MinExtractTranscript() {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	facts := memory.ExtractWithProvider(ctx, p, transcript)
	if len(facts) == 0 {
		memoryExtractAudit(session, dir, mode, 0, 0, "no facts parsed")
		return 0
	}

	set := memory.Load(memory.Options{CWD: dir, UserDir: config.MemoryUserDir()})
	filtered := memory.FilterExtractedFacts(facts, set.Store, memory.ExtractMax())
	saved, _ := memory.SaveExtractedFacts(set.Store, filtered, mode)
	// Фаза 2 (2026-08-15): dual-write в общий memory-mcp (env REASONIX_MEMORY_MCP=1).
	_ = memory.SyncExtractedFactsToMCP(filtered, mode, session)
	memoryExtractAudit(session, dir, mode, len(facts), saved, "")
	return 0
}

// memoryExtractAudit пишет одну JSON-строку в
// <userDir>/logs/memory-extract-<YYYYMMDD>.jsonl (аудит качества пула).
func memoryExtractAudit(session, dir, mode string, parsed, saved int, note string) {
	userDir := config.MemoryUserDir()
	if userDir == "" {
		return
	}
	logDir := filepath.Join(userDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	line, _ := json.Marshal(map[string]any{
		"ts":      time.Now().Format(time.RFC3339),
		"session": filepath.Base(session),
		"dir":     dir,
		"mode":    mode,
		"parsed":  parsed,
		"saved":   saved,
		"note":    note,
	})
	path := filepath.Join(logDir, "memory-extract-"+time.Now().Format("20060102")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, string(line))
}
