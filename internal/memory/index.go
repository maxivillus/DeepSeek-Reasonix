// The provider-visible memory index: scope-qualified references for every
// active fact, override annotations for shadowed global guidance.
package memory

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// IndexMaxChars caps the provider-visible memory index so a growing
	// store cannot bloat the cache-stable prompt prefix. The shared
	// memory-mcp summarize_index applies the same budget (mcpIndexMaxChars).
	IndexMaxChars = 4000
	// indexDescMaxRunes clips each entry's description in the index; bodies
	// never enter the prefix.
	indexDescMaxRunes = 120
)

// Index returns the provider-visible index that loads into the cached
// prefix: every active fact from both scopes with a scope-qualified
// reference, shadowed global facts annotated rather than hidden — the index
// agrees with the project-over-global rule recall enforces (#7995). The
// per-directory MEMORY.md files keep their unqualified format.
//
// The index is bounded: freshest facts first (so the cap keeps what the
// model is most likely to need), descriptions clipped, and the whole block
// cut at a line boundary once it exceeds IndexMaxChars.
func (s Store) Index() string {
	memories := s.ListAll()
	if len(memories) == 0 {
		return ""
	}
	shadowed := map[string]string{} // global fact ID -> winning project reference
	for _, o := range FindOverrides(memories) {
		shadowed[o.Global.ID] = providerMemoryReference(o.Project)
	}
	// Freshest first: UpdatedAt, then CreatedAt, then name for determinism.
	sort.Slice(memories, func(i, j int) bool {
		a, b := memories[i], memories[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.Name < b.Name
	})
	lines := make([]string, 0, len(memories))
	seen := map[string]bool{} // collapse legacy migration duplicates (same qualified ref)
	for _, memory := range memories {
		if ref := providerMemoryReference(memory); seen[ref] {
			continue
		} else {
			seen[ref] = true
		}
		line := renderQualifiedIndexLine(memory)
		if winner, ok := shadowed[memory.ID]; ok &&
			NormalizeFactScope(string(memory.Scope)) == FactScopeGlobal {
			line += " (overridden by " + winner + ")"
		}
		lines = append(lines, line)
	}
	joined := strings.Join(lines, "\n") + "\n"
	if r := []rune(joined); len(r) > IndexMaxChars {
		cut := r[:IndexMaxChars]
		// Cut at a line boundary so a partial line can never masquerade as a
		// full entry or dangle an override annotation. The newline position
		// must be counted in runes: strings.LastIndex reports a byte offset,
		// and slicing a rune slice by a byte offset both mis-cuts multibyte
		// text and — because r[:IndexMaxChars] retains the backing array's
		// capacity — silently extends the cut past the cap.
		lastNL := -1
		for j, ch := range cut {
			if ch == '\n' {
				lastNL = j
			}
		}
		if lastNL > 0 {
			cut = cut[:lastNL]
		}
		joined = string(cut) + fmt.Sprintf("\n… (truncated %d chars)", len(r)-len(cut))
	}
	return joined
}

// renderQualifiedIndexLine is the provider-index variant of renderIndexLine:
// the link is the scope-qualified reference every memory tool accepts, so a
// name collision across scopes can never be misread.
func renderQualifiedIndexLine(m Memory) string {
	marker := ""
	if ResolveActivation(m) == ActivationPinned {
		marker = " pinned"
	}
	ref := providerMemoryReference(m)
	return fmt.Sprintf("- [%s](%s) — [%s/%s%s] %s",
		displayTitle(m.Title, m.Name), ref,
		NormalizeFactScope(string(m.Scope)), NormalizeType(string(m.Type)), marker, oneLine(clipRunes(m.Description, indexDescMaxRunes)))
}
