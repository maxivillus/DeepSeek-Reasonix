package control

// Фаза B (2026-08-11): инкрементальная экстракция памяти ВО ВРЕМЯ сессии +
// рефид. Каждые REASONIX_MEMORY_INCREMENTAL_MINUTES минут (default 15)
// обрабатывается НОВЫЙ сегмент транскрипта (вырос ≥ REASONIX_MEMORY_INCREMENTAL_MIN,
// default 8k символов) — факты сохраняются через memoryManager.saveMemory:
//   - заметка «Saved memory …» уходит в очередь → следующий ход модели её видит
//     (рефид, не трогая cache-stable префикс);
//   - новые факты сразу доступны встроенному auto-recall (focused-query retrieval).
// Мастер-выключатель — тот же REASONIX_MEMORY_EXTRACT=1; INCREMENTAL_MINUTES=0
// отключает инкрементальную часть (session-end экстракция остаётся).

import (
	"context"
	"os"
	"strconv"
	"time"

	"reasonix/internal/memory"
	"reasonix/internal/provider"
)

const (
	incrementalDefaultMinutes = 15
	incrementalMinGrowth      = 8_000
)

// startIncrementalExtraction запускает фоновый тикер (отменяется через ctx).
func (c *Controller) startIncrementalExtraction(ctx context.Context) {
	if os.Getenv("REASONIX_MEMORY_EXTRACT") != "1" {
		return
	}
	minutes := envPositiveInt("REASONIX_MEMORY_INCREMENTAL_MINUTES", incrementalDefaultMinutes)
	if minutes <= 0 {
		return
	}
	growth := envPositiveInt("REASONIX_MEMORY_INCREMENTAL_MIN", incrementalMinGrowth)
	go func() {
		ticker := time.NewTicker(time.Duration(minutes) * time.Minute)
		defer ticker.Stop()
		lastChars := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.incrementalExtractOnce(ctx, growth, &lastChars)
			}
		}
	}()
}

// incrementalExtractOnce обрабатывает новый сегмент транскрипта. Позиция
// lastChars двигается только после обработки сегмента (append-only jsonl →
// префиксы стабильны); при малом приросте позиция не двигается, и следующий
// тик возьмёт больше.
func (c *Controller) incrementalExtractOnce(ctx context.Context, growth int, lastChars *int) {
	path := c.SessionPath()
	if path == "" {
		return
	}
	transcript := memory.ReadSessionTranscript(path)
	if len(transcript) < memory.MinExtractTranscript() {
		return
	}
	segment := transcript
	if *lastChars > 0 && len(transcript) > *lastChars {
		segment = transcript[*lastChars:]
	}
	if len(segment) < growth {
		return
	}
	model := os.Getenv("REASONIX_MEMORY_EXTRACT_MODEL")
	if model == "" {
		model = c.modelRef
	}
	if model == "" {
		return
	}
	p, err := c.providerResolver.Resolve(provider.Selection{Ref: model})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	facts := memory.ExtractWithProvider(ctx, p, segment)
	*lastChars = len(transcript) // сегмент обработан (даже если 0 фактов)
	if len(facts) == 0 {
		return
	}
	set := c.memory.current()
	if set == nil {
		return
	}
	filtered := memory.FilterExtractedFacts(facts, set.Store, memory.ExtractMax())
	mode := memory.GlobalWriteMode()
	var synced []memory.Memory
	for _, f := range filtered {
		fact := memory.Memory{
			Name:        memory.ExtractFactName(f),
			Title:       f.Title,
			Description: f.Description,
			Type:        memory.NormalizeType(f.Type),
			// Auto-extracted (incremental ticker, Фаза B): medium-trust.
			Trust: memory.TrustMedium,
			Body:  f.Body,
		}
		if mode == "global" || mode == "duplicate" {
			fact.Scope = memory.FactScopeGlobal
			_, _ = c.memory.saveMemory(fact)
			synced = append(synced, fact)
		}
		if mode == "project" || mode == "duplicate" {
			fact.Scope = memory.FactScopeProject
			_, _ = c.memory.saveMemory(fact)
			synced = append(synced, fact)
		}
	}
	// Фаза 2 (2026-08-15): dual-write в общий memory-mcp (env REASONIX_MEMORY_MCP=1).
	_ = memory.SyncFactsToMCP(synced, "incremental")
}

func envPositiveInt(name string, def int) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n < 0 {
		return def
	}
	return n
}
