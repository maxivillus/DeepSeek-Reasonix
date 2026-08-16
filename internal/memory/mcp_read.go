package memory

// mcp_read.go — чтение общего memory-mcp store (кросс-рантайм память, Фаза А
// шаг (а)): префикс-индекс через summarize_index (кап 4000 символов) и
// per-turn реколл через search_facts. Работает при том же флаге, что и
// dual-write (REASONIX_MEMORY_MCP=1); чтение best-effort — при любой ошибке
// вызывающие молча возвращаются к нативной памяти (fallback).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// mcpReadTimeout bounds one read RPC (index fetch at Load, search per turn).
const mcpReadTimeout = 10 * time.Second

// mcpIndexMaxChars mirrors the server's summarize_index default cap — the
// index-cap pattern for the shared store, so the prefix stays bounded.
const mcpIndexMaxChars = 4000

// mcpReadWarned keeps error logging to one line per process: an unavailable
// server is a persistent condition, not a per-turn event.
var mcpReadWarned atomic.Bool

func mcpWarn(err error) {
	if mcpReadWarned.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "memory-mcp read unavailable (native memory used): %v\n", err)
	}
}

// mcpToolResult is the JSON-RPC tools/call result envelope:
// {"content":[{"type":"text","text":"<json>"}]}.
type mcpToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// mcpSummarizeIndex fetches the shared cross-runtime fact index (one line per
// fact, freshest first, capped at mcpIndexMaxChars) via summarize_index.
// Returns "" when the server is unavailable or the index is empty.
func mcpSummarizeIndex(ctx context.Context) (string, error) {
	sess, err := startMCPSession(ctx)
	if err != nil {
		return "", err
	}
	defer sess.close()
	res, err := sess.call(ctx, "tools/call", map[string]any{
		"name":      "summarize_index",
		"arguments": map[string]any{"limit": 200, "max_chars": mcpIndexMaxChars},
	})
	if err != nil {
		return "", err
	}
	var tool mcpToolResult
	if err := json.Unmarshal(res, &tool); err != nil || len(tool.Content) == 0 {
		return "", fmt.Errorf("unexpected summarize_index response")
	}
	var out struct {
		Index string `json:"index"`
	}
	if err := json.Unmarshal([]byte(tool.Content[0].Text), &out); err != nil {
		return "", fmt.Errorf("parse summarize_index: %w", err)
	}
	return strings.TrimSpace(out.Index), nil
}

// mcpFactRow mirrors one search_facts fact row. project holds the native fact
// scope ("project"|"global"), domain the native type — mcp_sync writes them
// that way, so the mapping below is the inverse. Strong is a tolerant bool:
// SQLite exposes it as 0/1, JSON fixtures may use true/false.
type mcpFactRow struct {
	ID        int64   `json:"id"`
	Text      string  `json:"text"`
	Source    string  `json:"source"`
	Project   string  `json:"project"`
	Domain    string  `json:"domain"`
	Trust     string  `json:"trust"`
	Strong    mcpBool `json:"strong"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// mcpBool accepts both JSON booleans and SQLite 0/1 integers.
type mcpBool bool

func (b *mcpBool) UnmarshalJSON(data []byte) error {
	var v bool
	if err := json.Unmarshal(data, &v); err == nil {
		*b = mcpBool(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = mcpBool(n != 0)
		return nil
	}
	return fmt.Errorf("mcpBool: cannot parse %s", data)
}

// mcpRecallFacts runs a full-text search over the shared memory-mcp store and
// returns the matches as Memory values. Best-effort: callers fall back to
// native-only recall on error.
func mcpRecallFacts(ctx context.Context, query string, limit int) ([]Memory, error) {
	sess, err := startMCPSession(ctx)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	res, err := sess.call(ctx, "tools/call", map[string]any{
		"name":      "search_facts",
		"arguments": map[string]any{"query": query, "limit": limit},
	})
	if err != nil {
		return nil, err
	}
	var tool mcpToolResult
	if err := json.Unmarshal(res, &tool); err != nil || len(tool.Content) == 0 {
		return nil, fmt.Errorf("unexpected search_facts response")
	}
	var out struct {
		Count int          `json:"count"`
		Facts []mcpFactRow `json:"facts"`
		Error string       `json:"error"`
	}
	if err := json.Unmarshal([]byte(tool.Content[0].Text), &out); err != nil {
		return nil, fmt.Errorf("parse search_facts: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("search_facts: %s", out.Error)
	}
	memories := make([]Memory, 0, len(out.Facts))
	for _, row := range out.Facts {
		if m, ok := memoryFromMCPRow(row); ok {
			memories = append(memories, m)
		}
	}
	return memories, nil
}

// memoryFromMCPRow maps a shared-store fact row onto the native Memory shape so
// the existing recall pipeline (BM25, freshness, trust tiers) can score it
// identically to local facts.
func memoryFromMCPRow(row mcpFactRow) (Memory, bool) {
	text := strings.TrimSpace(row.Text)
	if text == "" {
		return Memory{}, false
	}
	scope := FactScopeProject
	if strings.EqualFold(strings.TrimSpace(row.Project), string(FactScopeGlobal)) {
		scope = FactScopeGlobal
	}
	now := time.Now().UTC()
	id := "mcp-" + strconv.FormatInt(row.ID, 10)
	return Memory{
		ID:          id,
		Name:        id,
		Title:       mcpTitle(text),
		Description: oneLine(clipRunes(text, 120)),
		Type:        NormalizeType(row.Domain),
		Scope:       scope,
		Trust:       NormalizeTrust(row.Trust),
		Body:        text,
		CreatedAt:   parseMCPTime(row.CreatedAt, now),
		UpdatedAt:   parseMCPTime(row.UpdatedAt, now),
	}, true
}

// mcpTitle is the recall-block title for a shared-store fact: the first line
// of the fact text, clipped so one long fact can't dominate the block.
func mcpTitle(text string) string {
	first := text
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	return clipRunes(strings.TrimSpace(first), 80)
}

func clipRunes(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

// parseMCPTime parses the server's RFC3339 timestamps; on failure the fact is
// treated as fresh (just fetched) rather than expired.
func parseMCPTime(value string, fallback time.Time) time.Time {
	if when, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
		return when.UTC()
	}
	return fallback
}

// mcpRecallHits searches the shared store for the query and scores the matches
// with the same pipeline as native facts (scoreRecallDocs). Native hits win on
// duplicate text; MCP adds cross-runtime facts the local store never saw.
func mcpRecallHits(query string, queryTerms []string, now time.Time, native []RecallHit) []RecallHit {
	ctx, cancel := context.WithTimeout(context.Background(), mcpReadTimeout)
	defer cancel()
	memories, err := mcpRecallFacts(ctx, query, maxAutoRecallLimit)
	if err != nil {
		mcpWarn(err)
		return nil
	}
	index := buildRecallIndexFromMemories(memories)
	if index == nil || len(index.docs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, hit := range native {
		if key := recallDedupKey(hit.Memory.Body); key != "" {
			seen[key] = true
		}
	}
	var out []RecallHit
	for _, hit := range scoreRecallDocs(index.docs, query, queryTerms, now) {
		if key := recallDedupKey(hit.Memory.Body); key != "" && seen[key] {
			continue
		}
		out = append(out, hit)
	}
	return out
}

// recallDedupKey normalizes a fact body for cross-store dedup: the shared store
// carries the same extracted facts as the native files (dual-write), so a text
// match means the same fact, whichever side it came from.
func recallDedupKey(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// mcpComposeEnabled gates the server-side recall assembly: with
// REASONIX_MEMORY_MCP_COMPOSE=1 (and MCP read enabled), AutoRecall asks the
// server for a ready-to-inject <memory-recall> block instead of scoring
// locally — one scoring pipeline for every runtime.
func mcpComposeEnabled() bool {
	return mcpSyncEnabled() && os.Getenv("REASONIX_MEMORY_MCP_COMPOSE") == "1"
}

// mcpComposeRecall builds the provider-visible recall block server-side
// (compose_recall tool: RRF over lexical + semantic + entity graph, session
// expansion, authoritative/background tiers).
func mcpComposeRecall(ctx context.Context, query string, limit, chars int) (string, error) {
	sess, err := startMCPSession(ctx)
	if err != nil {
		return "", err
	}
	defer sess.close()
	args := map[string]any{"turn_text": query, "limit": limit, "chars": chars}
	// REASONIX_MEMORY_MCP_WORKSPACE scopes server-side recall to one project
	// (multica project_id); empty = shared pool only.
	if ws := os.Getenv("REASONIX_MEMORY_MCP_WORKSPACE"); ws != "" {
		args["workspace"] = ws
	}
	res, err := sess.call(ctx, "tools/call", map[string]any{
		"name": "compose_recall",
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var tool mcpToolResult
	if err := json.Unmarshal(res, &tool); err != nil || len(tool.Content) == 0 {
		return "", fmt.Errorf("unexpected compose_recall response")
	}
	var out struct {
		Block string `json:"block"`
	}
	if err := json.Unmarshal([]byte(tool.Content[0].Text), &out); err != nil {
		return "", fmt.Errorf("parse compose_recall: %w", err)
	}
	return strings.TrimSpace(out.Block), nil
}
