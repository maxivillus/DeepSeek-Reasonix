// The session recall index: the recall pool read and tokenized once per
// memory snapshot, so each user turn's automatic recall costs zero disk IO.
// Markdown files stay the source of truth — every write path reloads the
// snapshot through memory.Load, which rebuilds this index with it.
package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"reasonix/internal/retrieval"
)

// RecallIndex is the prebuilt retrieval state for one immutable Set snapshot.
type RecallIndex struct {
	docs    []autoRecallDoc
	fielded []retrieval.FieldedDoc // V2 shadow pool, prebuilt field split
}

// BuildRecallIndex reads and tokenizes the recall pool once. A zero store
// yields nil, which recall reports as an empty memory store.
func BuildRecallIndex(store Store) *RecallIndex {
	return buildRecallIndexFromMemories(recallMemories(store.ListAll()))
}

// AutoRecall runs automatic recall against this snapshot's prebuilt index —
// the per-turn path. Semantics are identical to the package-level AutoRecall.
// When the shared memory-mcp store is enabled (REASONIX_MEMORY_MCP=1) the
// matches are merged with a per-turn search_facts recall over the shared
// store, so facts written by other runtimes surface here too (dual-read;
// native hits win on duplicate text).
func (s *Set) AutoRecall(query string, opts RecallOptions) RecallResult {
	result := RecallResult{Query: strings.TrimSpace(query), CharBudget: recallCharBudget(opts.MaxChars)}
	if genericRecallQuery(result.Query) {
		result.Suppressed = "generic user turn"
		return result
	}
	if s == nil {
		result.Suppressed = "memory store is empty"
		return result
	}
	// Server-side recall assembly (REASONIX_MEMORY_MCP_COMPOSE=1): the shared
	// store's compose_recall builds the whole block (RRF lexical+semantic+
	// graph, session expansion, tiers) — one scoring pipeline for every
	// runtime. Best-effort: any failure falls through to the native path.
	if mcpComposeEnabled() {
		ctx, cancel := context.WithTimeout(context.Background(), mcpReadTimeout)
		defer cancel()
		if block, err := mcpComposeRecall(ctx, result.Query,
			recallLimit(opts.Limit), result.CharBudget); err == nil && block != "" {
			result.block = block
			result.UsedChars = utf8.RuneCountInString(block)
			result.Source = "memory-mcp compose_recall"
			return result
		} else if err != nil {
			mcpWarn(fmt.Errorf("compose_recall unavailable (native recall used): %w", err))
		}
	}
	index := s.recall
	if index == nil {
		// Hand-built sets (tests, embedders) carry no prebuilt index; the
		// Load path always does, so per-turn recall stays disk-free there.
		index = BuildRecallIndex(s.Store)
	}
	queryTerms, err := retrieval.QueryTerms(result.Query)
	if err != nil {
		result.Suppressed = "no searchable terms"
		return result
	}
	var hits []RecallHit
	if index != nil && len(index.docs) > 0 {
		result.ShadowHits = shadowRankV2(result.Query, index.fielded)
		hits = scoreRecallDocs(index.docs, result.Query, queryTerms, opts.Now)
	}
	if mcpSyncEnabled() {
		hits = append(hits, mcpRecallHits(result.Query, queryTerms, opts.Now, hits)...)
	}
	if len(hits) == 0 {
		if index == nil || len(index.docs) == 0 {
			result.Suppressed = "memory store is empty"
		} else {
			result.Suppressed = "no sufficiently distinctive match"
		}
		return result
	}
	return finalizeRecall(result, hits, opts)
}

// buildRecallIndexFromMemories builds a recall index from an ad-hoc fact list
// (e.g. memory-mcp search results). Returns nil when nothing is searchable.
func buildRecallIndexFromMemories(memories []Memory) *RecallIndex {
	if len(memories) == 0 {
		return nil
	}
	index := &RecallIndex{}
	for _, memory := range memories {
		index.fielded = append(index.fielded, retrieval.FieldedDoc{ID: memory.ID, Fields: map[string]string{
			"name": memory.Name, "title": memory.Title, "keywords": memory.Keywords,
			"subject": memory.SubjectKey, "description": memory.Description, "body": memory.Body,
		}})
		text := autoRecallSearchText(memory)
		terms := retrieval.Tokens(text)
		if len(terms) == 0 {
			continue
		}
		index.docs = append(index.docs, autoRecallDoc{
			memory: memory,
			text:   text,
			counts: retrieval.Counts(terms),
			length: len(terms),
		})
	}
	if len(index.docs) == 0 {
		return nil
	}
	return index
}
