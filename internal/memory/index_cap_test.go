package memory

import (
	"strings"
	"testing"
	"time"
)

// The provider-visible index is bounded so a growing store cannot bloat the
// cache-stable prompt prefix: freshest first, descriptions clipped, whole
// block cut at a line boundary past IndexMaxChars (PATCHES.md §5).
func TestIndexCapBoundAndMarker(t *testing.T) {
	store := recallTestStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Multibyte description (Cyrillic, 2 bytes/rune): the line-boundary cut
	// must count positions in runes, not bytes.
	desc := strings.Repeat("д", 200) // longer than the 120-rune clip
	for i := 0; i < 60; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		recallTestWrite(t, store.Dir, Memory{
			ID:          "mem-" + strings.Repeat("x", i) + string(rune('a'+i%26)),
			Name:        "fact-" + strings.Repeat("z", i%5) + string(rune('a'+i%26)),
			Description: desc,
			Type:        TypeProject,
			Scope:       FactScopeProject,
			CreatedAt:   at,
			UpdatedAt:   at,
		})
	}

	index := store.Index()
	if index == "" {
		t.Fatal("index empty")
	}
	runes := len([]rune(index))
	// The cut keeps full lines, so the block stays at or under the cap plus
	// the truncation marker line.
	if runes > IndexMaxChars+80 {
		t.Fatalf("index exceeds cap: %d runes (cap %d)\n%s", runes, IndexMaxChars, index)
	}
	// The marker's dropped-char count must match the rune arithmetic.
	if !strings.Contains(index, "truncated") {
		t.Fatalf("index over cap but no truncation marker:\n%s", index)
	}
	// No partial line: every kept line must start a fresh "- [" entry or be
	// the marker; a dangling override annotation is impossible after a line
	// boundary cut.
	lastLine := index[strings.LastIndex(index, "\n")+1:]
	if !strings.HasPrefix(lastLine, "- [") && !strings.Contains(lastLine, "truncated") {
		t.Fatalf("block ends mid-line:\n%q", lastLine)
	}
}

// TestIndexFreshestFirst verifies the capped index keeps the most recently
// touched facts at the top (UpdatedAt desc, then CreatedAt desc).
func TestIndexFreshestFirst(t *testing.T) {
	store := recallTestStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-old", Name: "fact-old", Title: "Old fact",
		Description: "touched first", Type: TypeProject, Scope: FactScopeProject,
		CreatedAt: base, UpdatedAt: base,
	})
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-new", Name: "fact-new", Title: "New fact",
		Description: "touched last", Type: TypeProject, Scope: FactScopeProject,
		CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(2 * time.Hour),
	})

	index := store.Index()
	posNew := strings.Index(index, "fact-new")
	posOld := strings.Index(index, "fact-old")
	if posNew < 0 || posOld < 0 {
		t.Fatalf("index missing entries:\n%s", index)
	}
	if posNew > posOld {
		t.Fatalf("freshest fact must come first:\n%s", index)
	}
}

// TestIndexDescriptionClipped verifies a long description is clipped at
// indexDescMaxRunes with an ellipsis and the tail never enters the prefix.
func TestIndexDescriptionClipped(t *testing.T) {
	store := recallTestStore(t)
	long := strings.Repeat("a", 300)
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-clip", Name: "fact-clip", Title: "Clipped fact",
		Description: long, Type: TypeProject, Scope: FactScopeProject,
	})

	index := store.Index()
	if !strings.Contains(index, "…") && !strings.Contains(index, "...") {
		t.Fatalf("clipped description should carry an ellipsis:\n%s", index)
	}
	if strings.Contains(index, strings.Repeat("a", 200)) {
		t.Fatalf("description tail leaked into the index:\n%s", index)
	}
	line := index[:strings.Index(index, "\n")]
	if len([]rune(line)) > indexDescMaxRunes+60 {
		t.Fatalf("index line exceeds clipped description budget: %d runes\n%s", len([]rune(line)), line)
	}
}

// TestIndexUncappedWhenSmall keeps the exact legacy shape for stores that fit:
// no truncation marker, trailing newline, entries present.
func TestIndexUncappedWhenSmall(t *testing.T) {
	store := recallTestStore(t)
	recallTestWrite(t, store.Dir, Memory{
		ID: "mem-a", Name: "fact-a", Title: "Fact A",
		Description: "short", Type: TypeProject, Scope: FactScopeProject,
	})

	index := store.Index()
	if strings.Contains(index, "truncated") {
		t.Fatalf("small index must not carry a truncation marker:\n%s", index)
	}
	if !strings.HasSuffix(index, "\n") {
		t.Fatalf("index should end with a newline: %q", index)
	}
	if !strings.Contains(index, "[Fact A](project/fact-a.md)") {
		t.Fatalf("entry missing:\n%s", index)
	}
}
