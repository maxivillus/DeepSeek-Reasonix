package memory

// mcp_read_test.go — shared-store (memory-mcp) read path: summarize_index into
// the prefix, search_facts into per-turn recall, dual-read dedup, and the
// disabled fallback. Uses testdata/fake-memory-mcp.sh as the server.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeMCPFact = "Fake shared fact about quantum widgets"

func fakeMCPEnv(t *testing.T) {
	t.Helper()
	t.Setenv("REASONIX_MEMORY_MCP", "1")
	t.Setenv("MEMORY_MCP_CMD", filepath.Join("testdata", "fake-memory-mcp.sh"))
	t.Setenv("MEMORY_MCP_DB", filepath.Join(t.TempDir(), "facts.db"))
}

func TestMCPSummarizeIndex(t *testing.T) {
	fakeMCPEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), mcpReadTimeout)
	defer cancel()
	idx, err := mcpSummarizeIndex(ctx)
	if err != nil {
		t.Fatalf("mcpSummarizeIndex: %v", err)
	}
	if want := "#42 high! [project] Fake shared fact about quantum widgets"; idx != want {
		t.Fatalf("index = %q, want %q", idx, want)
	}
}

func TestMCPRecallFacts(t *testing.T) {
	fakeMCPEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), mcpReadTimeout)
	defer cancel()
	facts, err := mcpRecallFacts(ctx, "quantum widgets", 4)
	if err != nil {
		t.Fatalf("mcpRecallFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	f := facts[0]
	if f.ID != "mcp-42" || f.Body != fakeMCPFact {
		t.Fatalf("fact = %+v, want id mcp-42 with body %q", f, fakeMCPFact)
	}
	if f.Trust != TrustHigh || f.Scope != FactScopeProject || f.Type != TypeProject {
		t.Fatalf("fact mapping wrong: trust=%s scope=%s type=%s", f.Trust, f.Scope, f.Type)
	}
	if want := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC); !f.UpdatedAt.Equal(want) {
		t.Fatalf("UpdatedAt = %v, want %v", f.UpdatedAt, want)
	}
}

// TestLoadPrefersMCPIndex verifies Load swaps the native index for the capped
// shared-store index when MCP read is enabled.
func TestLoadPrefersMCPIndex(t *testing.T) {
	fakeMCPEnv(t)
	set := Load(Options{CWD: t.TempDir(), UserDir: t.TempDir()})
	if !set.MCPIndex {
		t.Fatal("MCPIndex flag not set")
	}
	if !strings.Contains(set.Index, "#42 high!") || !strings.Contains(set.Index, "quantum widgets") {
		t.Fatalf("Index does not carry the shared index: %q", set.Index)
	}
	if bg := set.BackgroundBlock(); !strings.Contains(bg, "Shared cross-runtime index") {
		t.Fatalf("BackgroundBlock missing shared-index note: %q", bg)
	}
}

// TestLoadFallsBackToNative verifies the native index survives when MCP read is
// disabled (and the store has facts).
func TestLoadFallsBackToNative(t *testing.T) {
	userDir := t.TempDir()
	cwd := t.TempDir()
	store := StoreFor(userDir, cwd)
	if _, err := store.Save(Memory{
		Name: "native-only", Title: "Native Only",
		Description: "a local fact", Type: TypeProject, Scope: FactScopeProject,
		Body: "local fact body", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	t.Setenv("REASONIX_MEMORY_MCP", "")
	set := Load(Options{CWD: cwd, UserDir: userDir})
	if set.MCPIndex {
		t.Fatal("MCPIndex should be false when MCP read is disabled")
	}
	if !strings.Contains(set.Index, "native-only") {
		t.Fatalf("native index missing: %q", set.Index)
	}
}

// TestAutoRecallMergesMCPFacts: with an empty native store, a query matching
// only the shared-store fact still recalls it (cross-runtime recall).
func TestAutoRecallMergesMCPFacts(t *testing.T) {
	fakeMCPEnv(t)
	set := Load(Options{CWD: t.TempDir(), UserDir: t.TempDir()})
	result := set.AutoRecall("quantum widgets", RecallOptions{})
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %d, want 1 (block: %s)", len(result.Hits), result.Block())
	}
	hit := result.Hits[0]
	if hit.Memory.ID != "mcp-42" {
		t.Fatalf("hit id = %q, want mcp-42", hit.Memory.ID)
	}
	if !hit.Confident {
		t.Fatal("cross-runtime hit should be in the authoritative tier (fresh + distinctive match)")
	}
	if !strings.Contains(result.Block(), "quantum widgets") {
		t.Fatalf("recall block missing the fact: %s", result.Block())
	}
}

// TestAutoRecallDedupsMCPAgainstNative: the same fact in both stores must be
// recalled once, from the native side.
func TestAutoRecallDedupsMCPAgainstNative(t *testing.T) {
	fakeMCPEnv(t)
	userDir := t.TempDir()
	cwd := t.TempDir()
	store := StoreFor(userDir, cwd)
	if _, err := store.Save(Memory{
		Name: "quantum-widgets", Title: "Quantum Widgets",
		Description: "shared fact", Type: TypeProject, Scope: FactScopeProject,
		Body: fakeMCPFact, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	set := Load(Options{CWD: cwd, UserDir: userDir})
	result := set.AutoRecall("quantum widgets", RecallOptions{})
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %d, want 1 (block: %s)", len(result.Hits), result.Block())
	}
	if result.Hits[0].Memory.ID == "mcp-42" {
		t.Fatalf("MCP duplicate was not deduped against the native fact: %s", result.Block())
	}
}

// TestAutoRecallMCPDisabled: without the env flag the native-only behavior is
// unchanged (an empty store yields no hits, not an MCP fetch).
func TestAutoRecallMCPDisabled(t *testing.T) {
	t.Setenv("REASONIX_MEMORY_MCP", "")
	set := Load(Options{CWD: t.TempDir(), UserDir: t.TempDir()})
	result := set.AutoRecall("quantum widgets", RecallOptions{})
	if len(result.Hits) != 0 {
		t.Fatalf("hits = %d, want 0 with MCP disabled and empty store", len(result.Hits))
	}
	if result.Suppressed != "memory store is empty" {
		t.Fatalf("suppressed = %q, want %q", result.Suppressed, "memory store is empty")
	}
}

// TestMCPServerDownFallback: an unavailable server must not break recall or
// Load (best-effort, native fallback).
func TestMCPServerDownFallback(t *testing.T) {
	t.Setenv("REASONIX_MEMORY_MCP", "1")
	t.Setenv("MEMORY_MCP_CMD", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("MEMORY_MCP_DB", filepath.Join(t.TempDir(), "facts.db"))
	set := Load(Options{CWD: t.TempDir(), UserDir: t.TempDir()})
	if set.MCPIndex {
		t.Fatal("MCPIndex should stay false when the server is unavailable")
	}
	if result := set.AutoRecall("quantum widgets", RecallOptions{}); len(result.Hits) != 0 {
		t.Fatalf("hits = %d, want 0 (fallback on empty store)", len(result.Hits))
	}
	_ = os.Getenv("MEMORY_MCP_DB") // keep the import; env is exercised by t.Setenv
}

// TestRealServerSmoke exercises the read path against a REAL memory-mcp server
// (skipped unless MEMORY_MCP_REAL_SMOKE=1). Run from the repo root with the
// production env, e.g.:
//
//	REASONIX_MEMORY_MCP=1 MEMORY_MCP_REAL_SMOKE=1 \
//	  MEMORY_MCP_CMD=<path to memory-mcp> MEMORY_MCP_DB=<path to facts.db> \
//	  go test -run TestRealServerSmoke -v ./internal/memory/
func TestRealServerSmoke(t *testing.T) {
	if os.Getenv("MEMORY_MCP_REAL_SMOKE") != "1" {
		t.Skip("set MEMORY_MCP_REAL_SMOKE=1 to run against a real server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpReadTimeout)
	defer cancel()
	idx, err := mcpSummarizeIndex(ctx)
	if err != nil {
		t.Fatalf("mcpSummarizeIndex: %v", err)
	}
	if strings.TrimSpace(idx) == "" {
		t.Fatal("real server returned an empty index")
	}
	t.Logf("real summarize_index: %d chars, first line: %s", len(idx), firstLine(idx))
	facts, err := mcpRecallFacts(ctx, "memory", 3)
	if err != nil {
		t.Fatalf("mcpRecallFacts: %v", err)
	}
	t.Logf("real search_facts('memory'): %d facts", len(facts))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestAutoRecallServerCompose: with REASONIX_MEMORY_MCP_COMPOSE=1 the recall
// block comes from the server's compose_recall (one scoring pipeline).
func TestAutoRecallServerCompose(t *testing.T) {
	fakeMCPEnv(t)
	t.Setenv("REASONIX_MEMORY_MCP_COMPOSE", "1")
	set := Load(Options{CWD: t.TempDir(), UserDir: t.TempDir()})
	result := set.AutoRecall("quantum widgets", RecallOptions{})
	if result.Block() == "" {
		t.Fatal("expected a server-composed block")
	}
	if !strings.Contains(result.Block(), "Fake shared fact about quantum widgets") {
		t.Fatalf("block does not carry the server content: %s", result.Block())
	}
	if result.Source != "memory-mcp compose_recall" {
		t.Fatalf("source = %q, want memory-mcp compose_recall", result.Source)
	}
	if len(result.Hits) != 0 {
		t.Fatalf("server-composed block must not fabricate local hits, got %d", len(result.Hits))
	}
}

// TestAutoRecallServerComposeFallback: when the server is unavailable the
// compose path falls through to the native recall.
func TestAutoRecallServerComposeFallback(t *testing.T) {
	t.Setenv("REASONIX_MEMORY_MCP", "1")
	t.Setenv("REASONIX_MEMORY_MCP_COMPOSE", "1")
	t.Setenv("MEMORY_MCP_CMD", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("MEMORY_MCP_DB", filepath.Join(t.TempDir(), "facts.db"))
	userDir := t.TempDir()
	cwd := t.TempDir()
	store := StoreFor(userDir, cwd)
	if _, err := store.Save(Memory{
		Name: "quantum-widgets", Title: "Quantum Widgets",
		Description: "shared fact", Type: TypeProject, Scope: FactScopeProject,
		Body: fakeMCPFact, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	set := Load(Options{CWD: cwd, UserDir: userDir})
	result := set.AutoRecall("quantum widgets", RecallOptions{})
	if len(result.Hits) != 1 {
		t.Fatalf("native fallback should recall the local fact, got %d hits (block: %s)",
			len(result.Hits), result.Block())
	}
	if result.Source != "" {
		t.Fatalf("fallback source = %q, want native (empty)", result.Source)
	}
}
