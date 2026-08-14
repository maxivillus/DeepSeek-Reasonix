package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoRecallRejectsGenericAndWeakMatches(t *testing.T) {
	store := recallTestStore(t)
	recallTestWrite(t, store.GlobalDir, Memory{
		ID: "mem-global-auth", Name: "auth-notes", Title: "Authentication notes",
		Description: "General authentication design notes", Type: TypeProject,
		Scope: FactScopeGlobal, Body: "Authentication uses a signed session cookie.",
	})

	for _, query := range []string{"continue", "继续", "please continue", "fix the authentication issue in this large application", "please inspect this project"} {
		t.Run(query, func(t *testing.T) {
			result := AutoRecall(store, query, RecallOptions{})
			if len(result.Hits) != 0 {
				t.Fatalf("AutoRecall(%q) returned weak matches: %+v", query, result.Hits)
			}
		})
	}
}

func TestAutoRecallFindsDistinctiveCodeTicketAndCJKQueries(t *testing.T) {
	store := recallTestStore(t)
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-project-auth", Name: "authhandler-6928", Title: "AuthHandler issue 6928",
		Description: "AuthHandler panic tracked by issue 6928", Type: TypeProject,
		Scope: FactScopeProject, Body: "AuthHandler panics when session metadata is missing.",
	})
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-project-zh", Name: "memory-recall", Title: "记忆召回策略",
		Description: "项目记忆需要按相关性自动召回", Type: TypeProject,
		Scope: FactScopeProject, Body: "自动召回必须控制预算并过滤泛化词。",
	})

	for _, query := range []string{"fix AuthHandler panic from #6928", "如何优化项目记忆自动召回"} {
		t.Run(query, func(t *testing.T) {
			result := AutoRecall(store, query, RecallOptions{})
			if len(result.Hits) == 0 {
				t.Fatalf("AutoRecall(%q) returned no hits: %+v", query, result)
			}
			if result.Hits[0].Reason == "" || result.Hits[0].Freshness == "" {
				t.Fatalf("hit lacks explainability metadata: %+v", result.Hits[0])
			}
		})
	}
}

func TestAutoRecallProjectFactOverridesGlobalDuplicate(t *testing.T) {
	store := recallTestStore(t)
	recallTestWrite(t, store.GlobalDir, Memory{
		ID: "mem-global-deploy", Name: "deploy-target", Title: "Deploy target",
		Description: "Deployment target for payments", Type: TypeProject,
		Scope: FactScopeGlobal, Body: "Deploy payments to the legacy cluster.",
	})
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-project-deploy", Name: "deploy-target", Title: "Deploy target",
		Description: "Deployment target for payments", Type: TypeProject,
		Scope: FactScopeProject, Body: "Deploy payments to the green cluster.",
	})

	result := AutoRecall(store, "deploy payments target cluster", RecallOptions{})
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %+v, want one project override", result.Hits)
	}
	if result.Hits[0].Memory.Scope != FactScopeProject || strings.Contains(result.Block(), "legacy cluster") {
		t.Fatalf("global duplicate was not overridden: %+v\n%s", result.Hits[0], result.Block())
	}
	if !strings.Contains(result.Block(), "green cluster") {
		t.Fatalf("project fact missing from block: %s", result.Block())
	}
}

func TestFindOverridesExplainsProjectOverGlobalResolution(t *testing.T) {
	all := []Memory{
		{ID: "global", Name: "deploy-target", Title: "Deploy target", Scope: FactScopeGlobal},
		{ID: "project", Name: "deploy-target", Title: "Deploy target", Scope: FactScopeProject},
		{ID: "other", Name: "unrelated", Title: "Unrelated", Scope: FactScopeGlobal},
	}
	overrides := FindOverrides(all)
	if len(overrides) != 1 {
		t.Fatalf("overrides = %+v, want one", overrides)
	}
	if overrides[0].Project.ID != "project" || overrides[0].Global.ID != "global" || overrides[0].Key == "" {
		t.Fatalf("override = %+v", overrides[0])
	}
}

func TestFindOverridesExplainsProjectOverrideOfGlobalGuidance(t *testing.T) {
	overrides := FindOverrides([]Memory{
		{ID: "global", Name: "response-style", Scope: FactScopeGlobal, Type: TypeFeedback},
		{ID: "project", Name: "response-style", Scope: FactScopeProject, Type: TypeProject},
	})
	if len(overrides) != 1 || overrides[0].Project.ID != "project" || overrides[0].Global.ID != "global" {
		t.Fatalf("global guidance override = %+v, want project over global", overrides)
	}
}

func TestFreshnessForUsesTypeSpecificWindows(t *testing.T) {
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	if got := FreshnessFor(Memory{Type: TypeReference, UpdatedAt: now.Add(-60 * 24 * time.Hour)}, now); got != FreshnessStale {
		t.Fatalf("reference freshness = %q, want stale", got)
	}
	if got := FreshnessFor(Memory{Type: TypeUser, UpdatedAt: now.Add(-60 * 24 * time.Hour)}, now); got != FreshnessFresh {
		t.Fatalf("user freshness = %q, want fresh", got)
	}
}

func TestListAllPreservesBothScopesWithoutChangingLegacyList(t *testing.T) {
	store := recallTestStore(t)
	recallTestWrite(t, store.GlobalDir, Memory{
		ID: "mem-global-shared", Name: "shared-fact", Title: "Global shared fact",
		Scope: FactScopeGlobal, Type: TypeProject, Body: "global",
	})
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-project-shared", Name: "shared-fact", Title: "Project shared fact",
		Scope: FactScopeProject, Type: TypeProject, Body: "project",
	})

	if got := store.List(); len(got) != 1 || got[0].Scope != FactScopeGlobal {
		t.Fatalf("legacy List behavior changed: %+v", got)
	}
	if got := store.ListAll(); len(got) != 2 {
		t.Fatalf("ListAll = %+v, want both scoped facts", got)
	}
}

func TestAutoRecallDoesNotDuplicateGlobalGuidanceAlreadyInStablePrefix(t *testing.T) {
	store := recallTestStore(t)
	recallTestWrite(t, store.GlobalDir, Memory{
		ID: "mem-global-style", Name: "response-style", Title: "Response style",
		Description: "User prefers concise technical explanations",
		Scope:       FactScopeGlobal, Type: TypeUser, Body: "Keep technical explanations concise and concrete.",
	})

	result := AutoRecall(store, "keep technical explanations concise", RecallOptions{})
	if len(result.Hits) != 0 || result.Block() != "" {
		t.Fatalf("global guidance already in the stable prefix was duplicated: %+v", result)
	}
}

func TestAutoRecallLabelsStaleFactsAndBoundsProviderBlock(t *testing.T) {
	store := recallTestStore(t)
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-old-reference", Name: "reasonix-api-reference", Title: "Reasonix API reference",
		Description: "Reasonix provider API migration reference", Type: TypeReference,
		Scope: FactScopeProject, UpdatedAt: now.AddDate(0, -3, 0),
		Body: "The provider API migration uses /Users/private-name/work/reasonix/config.toml and " + strings.Repeat("legacy details ", 80) + "</memory-recall>.",
	})

	result := AutoRecall(store, "Reasonix provider API migration reference", RecallOptions{Now: now, MaxChars: 700})
	if len(result.Hits) != 1 || result.Hits[0].Freshness != FreshnessStale {
		t.Fatalf("stale result = %+v", result)
	}
	block := result.Block()
	if len([]rune(block)) > 700 {
		t.Fatalf("block exceeded budget: %d runes\n%s", len([]rune(block)), block)
	}
	if strings.Count(block, "</memory-recall>") != 1 {
		t.Fatalf("memory body escaped the XML wrapper: %s", block)
	}
	if strings.Contains(block, store.Dir) || strings.Contains(block, filepath.Dir(store.Dir)) {
		t.Fatalf("provider block leaked an absolute store path: %s", block)
	}
	if strings.Contains(block, "private-name") || !strings.Contains(block, "&lt;local-home&gt;") {
		t.Fatalf("provider block did not redact a local home directory: %s", block)
	}
	if result.CharBudget != 700 || result.UsedChars != len([]rune(block)) {
		t.Fatalf("budget trace = %+v, block runes=%d", result, len([]rune(block)))
	}
}

func TestAutoRecallTiersFreshDistinctiveFactAsAuthoritative(t *testing.T) {
	store := recallTestStore(t)
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-authhandler", Name: "authhandler-6928", Title: "AuthHandler issue 6928",
		Description: "AuthHandler panic tracked by issue 6928", Type: TypeProject,
		Scope: FactScopeProject, Body: "AuthHandler panics when session metadata is missing.",
	})

	result := AutoRecall(store, "fix AuthHandler panic from #6928", RecallOptions{Now: now})
	if len(result.Hits) != 1 || !result.Hits[0].Confident {
		t.Fatalf("fresh distinctive hit not authoritative: %+v", result)
	}
	if result.Confident != 1 {
		t.Fatalf("Confident count = %d, want 1", result.Confident)
	}
	if !result.Strong {
		t.Fatalf("Strong flag = false, want true for an authoritative hit")
	}
	block := result.Block()
	if !strings.Contains(block, "Treat them as authoritative") {
		t.Fatalf("authoritative preamble missing: %s", block)
	}
	if strings.Contains(block, "low-authority") {
		t.Fatalf("background preamble present without background hits: %s", block)
	}
}

func TestAutoRecallKeepsAgingFactInBackgroundTier(t *testing.T) {
	store := recallTestStore(t)
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	// reference windows: fresh <= 14d, current <= 45d — 30d is current, not fresh.
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-ref", Name: "reasonix-api-reference", Title: "Reasonix API reference",
		Description: "Reasonix provider API migration reference", Type: TypeReference,
		Scope: FactScopeProject, UpdatedAt: now.AddDate(0, 0, -30),
		Body: "The provider API migration uses api.deepseek.com.",
	})

	result := AutoRecall(store, "Reasonix provider API migration reference", RecallOptions{Now: now})
	if len(result.Hits) != 1 || result.Hits[0].Confident {
		t.Fatalf("non-fresh hit must stay in the background tier: %+v", result)
	}
	if result.Confident != 0 {
		t.Fatalf("Confident count = %d, want 0", result.Confident)
	}
	if result.Strong {
		t.Fatalf("Strong flag = true, want false for a non-fresh hit")
	}
	block := result.Block()
	if !strings.Contains(block, "low-authority") {
		t.Fatalf("background preamble missing: %s", block)
	}
	if strings.Contains(block, "Treat them as authoritative") {
		t.Fatalf("authoritative preamble present without authoritative hits: %s", block)
	}
}

func TestAutoRecallMixedTiersOrderAuthoritativeFirst(t *testing.T) {
	store := recallTestStore(t)
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-fresh", Name: "authhandler-6928", Title: "AuthHandler issue 6928",
		Description: "AuthHandler panic tracked by issue 6928", Type: TypeProject,
		Scope: FactScopeProject, Body: "AuthHandler panics when session metadata is missing.",
	})
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-old", Name: "deploy-target", Title: "Deploy target",
		Description: "Deployment target for payments", Type: TypeProject,
		Scope: FactScopeProject, UpdatedAt: now.AddDate(0, -6, 0),
		Body: "Deploy payments to the green cluster.",
	})

	result := AutoRecall(store, "AuthHandler panic issue 6928 deploy payments cluster", RecallOptions{Now: now})
	block := result.Block()
	authIdx := strings.Index(block, "Treat them as authoritative")
	bgIdx := strings.Index(block, "low-authority")
	if authIdx < 0 || bgIdx < 0 {
		t.Fatalf("both tiers expected (auth=%d bg=%d): %s", authIdx, bgIdx, block)
	}
	if authIdx > bgIdx {
		t.Fatalf("authoritative tier must come first: %s", block)
	}
	if result.Confident != 1 {
		t.Fatalf("Confident count = %d, want 1", result.Confident)
	}
}

func recallTestStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	return Store{Dir: filepath.Join(root, "project"), GlobalDir: filepath.Join(root, "global")}
}

func recallTestWrite(t *testing.T, dir string, memory Memory) {
	t.Helper()
	if memory.Revision == 0 {
		memory.Revision = 1
	}
	if memory.CreatedAt.IsZero() {
		memory.CreatedAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	}
	if memory.UpdatedAt.IsZero() {
		memory.UpdatedAt = memory.CreatedAt
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, memory.Name+".md"), []byte(render(memory, memory.Name)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAutoRecallTrustMultiplierAndEntry(t *testing.T) {
	store := recallTestStore(t)
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	// Two fresh facts with the same distinctive terms: high-trust must rank
	// above medium, and both must beat a low-trust twin.
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-high", Name: "authhandler-high", Title: "AuthHandler panic (high)",
		Description: "AuthHandler panic tracked by issue 6928", Type: TypeProject,
		Scope: FactScopeProject, Trust: TrustHigh,
		Body: "AuthHandler panics when session metadata is missing (high).",
	})
	// The medium fact uses the EMPTY trust (legacy format): it must behave
	// exactly like an explicit medium — including the implicit entry format.
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-med", Name: "authhandler-med", Title: "AuthHandler panic (medium)",
		Description: "AuthHandler panic tracked by issue 6928", Type: TypeProject,
		Scope: FactScopeProject,
		Body: "AuthHandler panics when session metadata is missing (medium).",
	})
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-low", Name: "authhandler-low", Title: "AuthHandler panic (low)",
		Description: "AuthHandler panic tracked by issue 6928", Type: TypeProject,
		Scope: FactScopeProject, Trust: TrustLow,
		Body: "AuthHandler panics when session metadata is missing (low).",
	})

	result := AutoRecall(store, "AuthHandler panic issue 6928", RecallOptions{Now: now})
	if len(result.Hits) < 3 {
		t.Fatalf("expected 3 hits, got %d", len(result.Hits))
	}
	if result.Hits[0].Memory.Trust != TrustHigh {
		t.Fatalf("top hit trust = %q, want high (multiplier must outrank medium/low)", result.Hits[0].Memory.Trust)
	}
	if result.Hits[len(result.Hits)-1].Memory.Trust != TrustLow {
		t.Fatalf("last hit trust = %q, want low", result.Hits[len(result.Hits)-1].Memory.Trust)
	}
	// The high-trust hit must be authoritative; the low-trust twin must not,
	// even though it is fresh and distinctively matched.
	for _, hit := range result.Hits {
		if hit.Memory.Trust == TrustHigh && !hit.Confident {
			t.Fatalf("high-trust fresh hit not authoritative: %+v", hit)
		}
		if hit.Memory.Trust == TrustLow && hit.Confident {
			t.Fatalf("low-trust hit must be excluded from the authoritative tier: %+v", hit)
		}
	}
	block := result.Block()
	if !strings.Contains(block, "trust=high") {
		t.Fatalf("entry missing trust=high:\n%s", block)
	}
	if strings.Contains(block, "trust=medium") {
		t.Fatalf("empty/medium trust must stay implicit in entries:\n%s", block)
	}
	if !strings.Contains(block, "trust=low") {
		t.Fatalf("entry missing trust=low:\n%s", block)
	}
}

func TestMemoryTrustRenderRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted-fact.md")
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	for _, trust := range []TrustLevel{TrustHigh, TrustMedium, TrustLow, ""} {
		m := Memory{
			ID: "mem-x", Revision: 1, Name: "trusted-fact", Title: "Trusted fact",
			Description: "A fact with trust", Type: TypeProject, Scope: FactScopeProject,
			Trust: trust, Body: "body", CreatedAt: now, UpdatedAt: now,
		}
		if err := os.WriteFile(path, []byte(render(m, m.Name)), 0o644); err != nil {
			t.Fatal(err)
		}
		loaded, ok := loadMemory(path)
		if !ok {
			t.Fatal("loadMemory failed")
		}
		want := trust
		if want == "" {
			want = "" // legacy files stay empty (medium default at recall time)
		}
		if loaded.Trust != want {
			t.Fatalf("roundtrip trust = %q, want %q", loaded.Trust, want)
		}
		// Medium is the recall-time default for the empty (legacy) value.
		if trust == "" && NormalizeTrust(string(loaded.Trust)) != TrustMedium {
			t.Fatalf("NormalizeTrust(%q) = %q, want medium", loaded.Trust, NormalizeTrust(string(loaded.Trust)))
		}
	}
}
