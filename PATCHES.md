# PATCHES.md — branch `main-v2-v1251`

This fork branch tracks upstream `main-v2` (rebase on v1.25.1) plus **7 local
commits** that add a memory subsystem and read_file improvements, developed
and verified for a long-running autonomous agent stack. Everything is opt-in
via environment variables; default behavior is unchanged.

## Commits (oldest → newest)

| Commit | Area | What |
|---|---|---|
| `28846fd5c` | read_file | clamp + JPEG q80 compression and tesseract OCR for raster images |
| `52dba55a5` | read_file | OCR psm 11 + preserve_interword_spaces (experiment) |
| `168a43c2c` | read_file | drop tesseract OCR, keep clamp+JPEG compression |
| `8ff9463fe` | read_file | vision-proxy transcript for text-only models |
| `c5f6cec3d` | memory | rebase: port memory extract/recall-tiers/fact-gate/trust/index-cap |
| `520eb44d0` | memory | dual-write extracted facts to shared memory-mcp (Phase 2) |
| `541b390dc` | memory | drop host-specific defaults in mcp_sync (portable) |
| `f55069cf82` | memory | read shared memory-mcp store: prefix index (summarize_index, 4000 cap) + per-turn recall (search_facts), dual-read with native-wins dedup (step a) |
| `f7d5cb3eb` | memory | server-side recall assembly: REASONIX_MEMORY_MCP_COMPOSE=1 uses compose_recall block (RRF lexical+semantic+graph, sessions, tiers); native fallback; RecallResult.Source |

## read_file improvements

- **Raster image budget**: images are clamped to 1568px and re-encoded as
  JPEG q80 before entering context (screen ~1.4–1.6 MB raw → a few hundred KB
  base64). Vector formats are skipped. Tesseract OCR was removed from the
  stack by decision (2026-08-08); only clamp + compression remain.
- **Text-only model transcript**: when the active provider is text-only,
  `read_file` falls back to a local vision-model transcript so image content
  is still described.

## Memory subsystem (auto-extraction + retrieval quality)

Six opt-in features, each gated by its own env var:

### 1. Auto-extraction (`REASONIX_MEMORY_EXTRACT=1`)

- **Session-end child process** (`memory-extract` CLI): extracts durable facts
  from the transcript with the configured provider; threshold guard — only
  transcripts ≥ 8k tokens are worth extracting; JSON-mode prompt with 3
  retries on parse failure.
- **Incremental in-session ticker**: a background ticker extracts facts from
  the accumulated conversation mid-session. Facts are saved through the normal
  store path, so the "Saved memory" notice enters the queue and the next model
  turn sees it (refeed).
- `REASONIX_MEMORY_GLOBAL_WRITE=project|global|duplicate` controls where
  facts land.

### 2. Recall tiers (`REASONIX_MEMORY_RECALL_TIERS`)

Two-tier recall: **authoritative** and **background**. Background facts are
still searchable on demand but are not injected into the cache-stable prefix.

### 3. Fact gate (`REASONIX_MEMORY_FACT_GATE`)

Facts marked **Strong** (user-confirmed) persist a `researchSkippedByFact`
marker across turns and emit a marker in the goal block, so the model does not
re-research what memory already answers authoritatively.

### 4. Storage trust (`REASONIX_MEMORY_TRUST`)

Facts carry `TrustLevel` high/medium/low with retrieval multiplier 1.5/1.0/0.7;
`low`-trust facts are excluded from the authoritative tier.

### 5. Index cap (`REASONIX_MEMORY_INDEX_CAP`)

The background memory index in the cache-stable prefix is capped at
`IndexMaxChars = 4000`, one line per memory, description clipped at 120
graphemes, freshest-first, so a growing store cannot bloat the prompt prefix.

### 6. memory-mcp dual-write (`REASONIX_MEMORY_MCP=1`)

After native save, extracted facts are also synced (best-effort, 60s timeout)
to a shared SQLite+FTS5 MCP fact store via stdio `remember_fact` calls —
see [github.com/maxivillus/memory-mcp](https://github.com/maxivillus/memory-mcp).

- `MEMORY_MCP_CMD` — server command (default `memory-mcp` via PATH)
- `MEMORY_MCP_DB` — target DB (default `~/.local/share/memory-mcp/facts.db`,
  XDG-style; set explicitly by the stack)

The sync never blocks extraction: native write happens first, MCP errors are
logged and ignored.

## Upstream feedback

Feature material was posted upstream as issues: reasonix
[#8617–#8620](https://github.com/esengine/DeepSeek-Reasonix/issues/8617)
(memory suite), plus ACP diagnostics
[#8238/#8240/#8241](https://github.com/esengine/DeepSeek-Reasonix/issues/8238).
