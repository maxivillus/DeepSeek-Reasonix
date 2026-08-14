package memory

// Auto-extraction (Phase A, 2026-08-11) — «память между сессиями» в стиле
// jcode (JCODE_MEMORY_GLOBAL_WRITE): по концу сессии child-процесс
// `reasonix memory-extract` извлекает durable-факты из транскрипта через
// LLM-sidecar и сохраняет их через штатный Store (та же точка, что
// remember-инструмент): ревизии, скоупы, индекс — без ручных write.
//
// Решения пользователя (2026-08-11): авто-экстракция пишет ТОЛЬКО
// project/reference (user/feedback — только явный remember агента);
// global-write управляется env REASONIX_MEMORY_GLOBAL_WRITE.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"reasonix/internal/provider"
)

const (
	extractTranscriptCap = 200_000 // символов транскрипта на экстракцию
	extractMinTranscript = 8_000   // меньше — экстракцию пропускаем (тривиальные сессии;
	//                             // 50k оказалось высоко: task-сессии daemon'а — 5-30k)
	extractDefaultMax = 10 // кап фактов за сессию
)

// MinExtractTranscript — минимальный размер транскрипта для экстракции.
func MinExtractTranscript() int { return extractMinTranscript }

// ExtractFact — один извлечённый факт (JSON из sidecar'а).
type ExtractFact struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Body        string `json:"body"`
}

// GlobalWriteMode возвращает режим записи из env
// (REASONIX_MEMORY_GLOBAL_WRITE=project|global|duplicate; default project).
func GlobalWriteMode() string {
	switch os.Getenv("REASONIX_MEMORY_GLOBAL_WRITE") {
	case "global":
		return "global"
	case "duplicate":
		return "duplicate"
	default:
		return "project"
	}
}

// ExtractMax возвращает кап фактов за сессию (REASONIX_MEMORY_EXTRACT_MAX).
func ExtractMax() int {
	n, err := strconv.Atoi(os.Getenv("REASONIX_MEMORY_EXTRACT_MAX"))
	if err != nil || n <= 0 {
		return extractDefaultMax
	}
	return n
}

// ReadSessionTranscript собирает текст транскрипта из <path>.jsonl:
// user/assistant content (без reasoning_content), tool-результаты — имя + 200
// символов. Путь может быть как стебом, так и полным <stem>.jsonl.
func ReadSessionTranscript(path string) string {
	if !strings.HasSuffix(path, ".jsonl") {
		path += ".jsonl"
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var m struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Name    string `json:"name"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		switch m.Role {
		case "user":
			b.WriteString("USER: " + content + "\n")
		case "assistant":
			b.WriteString("ASSISTANT: " + content + "\n")
		case "tool":
			b.WriteString("TOOL")
			if m.Name != "" {
				b.WriteString("(" + m.Name + ")")
			}
			if len(content) > 200 {
				content = content[:200]
			}
			b.WriteString(": " + content + "\n")
		}
		if b.Len() > extractTranscriptCap {
			b.WriteString("\n[transcript truncated]")
			break
		}
	}
	return b.String()
}

// ExtractionPrompt — system-промпт sidecar'а (гайдлайны remember-инструмента).
func ExtractionPrompt() string {
	return "You are a memory curator for a coding agent. Extract durable, reusable facts from the conversation transcript. " +
		"Emit ONLY a JSON array, no prose: [{\"title\": ..., \"description\": ..., \"type\": ..., \"body\": ...}, ...]\n" +
		"- title: short slug (lowercase, hyphens, <=64 chars)\n" +
		"- description: one-line hook (<=120 chars), unique across facts\n" +
		"- type: \"project\" (ongoing work, goals, constraints, stack quirks, environment facts) or \"reference\" (pointers to external resources: URLs, tickets, repos). Do NOT use \"user\" or \"feedback\".\n" +
		"- body: the fact itself, 1-4 sentences; for actionable guidance include \"**Why:**\" and \"**How to apply:**\" lines; link related facts as [[name]].\n" +
		"Rules:\n" +
		"- Save only facts that remain useful across sessions: architecture decisions, stack/environment quirks, credentials locations (never the values), preferences stated explicitly.\n" +
		"- Do NOT save: facts already recorded in code/git/repo docs, transient task state, literal secrets/tokens, placeholder content, statements about missing environment variables or absent tokens.\n" +
		"- At most 10 facts; if nothing durable, emit [].\n" +
		"Transcript:\n"
}

// junkPatterns — грабли jcode (CONTENT-placeholder, ложные факты про токены).
var junkPatterns = []string{
	"content",
	"does not exist",
	"not found",
	"not present in the environment",
	"no process",
	"no such token",
}

// FilterExtractedFacts применяет фильтры мусора, нормализует типы
// (всё не-reference → project — решение пользователя), дедуплицирует по
// description против существующего индекса и ограничивает капом.
func FilterExtractedFacts(facts []ExtractFact, store Store, max int) []ExtractFact {
	if max <= 0 {
		max = extractDefaultMax
	}
	seen := map[string]bool{}
	for _, m := range store.List() {
		seen[strings.ToLower(strings.TrimSpace(m.Description))] = true
	}
	out := make([]ExtractFact, 0, len(facts))
	for _, f := range facts {
		if len(out) >= max {
			break
		}
		desc := strings.TrimSpace(f.Description)
		body := strings.TrimSpace(f.Body)
		if desc == "" || body == "" || len(body) < 40 {
			continue
		}
		low := strings.ToLower(desc + " " + body)
		junk := false
		for _, p := range junkPatterns {
			if strings.Contains(low, p) {
				junk = true
				break
			}
		}
		if junk {
			continue
		}
		key := strings.ToLower(desc)
		if seen[key] {
			continue
		}
		seen[key] = true
		t := NormalizeType(f.Type)
		if t != TypeReference {
			t = TypeProject
		}
		f.Description = desc
		f.Body = body
		f.Type = string(t)
		f.Title = strings.TrimSpace(f.Title)
		if f.Title == "" {
			f.Title = desc
		}
		out = append(out, f)
	}
	return out
}

// SaveExtractedFacts сохраняет факты по mode (project|global|duplicate) через
// штатный SaveWithOptions (тот же путь, что remember). Возвращает число
// сохранённых записей.
func SaveExtractedFacts(store Store, facts []ExtractFact, mode string) (int, error) {
	saved := 0
	for _, f := range facts {
		m := Memory{
			Name:        slug(firstNonEmpty(f.Title, f.Description)),
			Title:       f.Title,
			Description: f.Description,
			Type:        NormalizeType(f.Type),
			// Auto-extracted facts are medium-trust: judged by the sidecar LLM
			// from a transcript, not confirmed by the user.
			Trust: TrustMedium,
			Body:  f.Body,
		}
		if mode == "global" || mode == "duplicate" {
			m.Scope = FactScopeGlobal
			if _, err := store.SaveWithOptions(m, SaveOptions{}); err == nil {
				saved++
			}
		}
		if mode == "project" || mode == "duplicate" {
			m.Scope = FactScopeProject
			if _, err := store.SaveWithOptions(m, SaveOptions{}); err == nil {
				saved++
			}
		}
	}
	return saved, nil
}

// ParseExtractJSON толерантно достаёт JSON-массив фактов из ответа sidecar'а:
// сначала прямой Unmarshal, затем первый '[' … последний ']'.
func ParseExtractJSON(raw string) []ExtractFact {
	raw = strings.TrimSpace(raw)
	var facts []ExtractFact
	if json.Unmarshal([]byte(raw), &facts) == nil {
		return facts
	}
	// обрезать ```json ... ``` и любой прозаический мусор
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start >= 0 && end > start {
		if json.Unmarshal([]byte(raw[start:end+1]), &facts) == nil {
			return facts
		}
	}
	return nil
}

// ExtractFactName — имя факта для store (slug из title/description).
func ExtractFactName(f ExtractFact) string { return slug(firstNonEmpty(f.Title, f.Description)) }

// ExtractWithProvider — вызов LLM-sidecar (общий для session-end child-процесса
// и инкрементальной экстракции Фазы B): JSON-режим (json_object) + до 3 попыток
// при пустом/непарсящемся ответе (модель иногда отвечает прозой — deepseek-flash
// вариативен: 0 vs 8 фактов на одном и том же транскрипте).
func ExtractWithProvider(ctx context.Context, p provider.Provider, transcript string) []ExtractFact {
	var raw string
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		ch, err := p.Stream(ctx, provider.Request{
			Messages: []provider.Message{
				{Role: provider.RoleSystem, Content: ExtractionPrompt()},
				{Role: provider.RoleUser, Content: transcript},
			},
			MaxTokens:      2000,
			ResponseFormat: &provider.ResponseFormat{Type: "json_object"},
		})
		if err != nil {
			continue
		}
		var sb strings.Builder
		for chunk := range ch {
			if chunk.Type == provider.ChunkText {
				sb.WriteString(chunk.Text)
			}
		}
		raw = sb.String()
		if len(ParseExtractJSON(raw)) > 0 {
			break
		}
	}
	return ParseExtractJSON(raw)
}
