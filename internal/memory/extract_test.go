package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// Фильтры мусора (грабли jcode): CONTENT-placeholder, ложные факты про токены,
// дедуп по description, кап, нормализация типов (всё не-reference → project).
func TestFilterExtractedFactsJunk(t *testing.T) {
	store := Store{Dir: t.TempDir(), GlobalDir: t.TempDir()}
	facts := []ExtractFact{
		{Title: "ok-fact", Description: "stack quirk", Body: "The stack uses bind-mounts for the CLI binary so updates need no rebuild."},
		{Title: "junk-placeholder", Description: "placeholder", Body: "CONTENT placeholder text that should never be stored anywhere at all."},
		{Title: "junk-token", Description: "token missing", Body: "The GITHUB_TOKEN does not exist in the environment at all."},
		{Title: "short", Description: "too short", Body: "tiny"},
		{Title: "type-user", Description: "user prefs", Type: "user", Body: "The user prefers conservative defaults and explicit confirmations."},
		{Title: "type-ref", Description: "docs pointer", Type: "reference", Body: "The headroom CCR docs live at headroom-docs.vercel.app/docs/ccr."},
	}
	out := FilterExtractedFacts(facts, store, 10)
	if len(out) != 3 {
		t.Fatalf("want 3 facts, got %d: %+v", len(out), out)
	}
	// user → project (решение пользователя); reference остаётся
	for _, f := range out {
		if f.Type != "project" && f.Type != "reference" {
			t.Errorf("unexpected type %q", f.Type)
		}
	}
	if out[0].Type != "project" || out[1].Type != "project" || out[2].Type != "reference" {
		t.Errorf("type normalization wrong: %+v", out)
	}
}

func TestFilterExtractedFactsDedupeAndCap(t *testing.T) {
	store := Store{Dir: t.TempDir(), GlobalDir: t.TempDir()}
	// уже существующий факт в индексе
	if _, err := store.SaveWithOptions(Memory{
		Name: "existing", Title: "existing", Description: "existing fact",
		Type: TypeProject, Scope: FactScopeProject,
		Body: "An existing fact about the stack that already lives in the index store.",
	}, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	facts := []ExtractFact{
		{Title: "dup", Description: "existing fact", Body: "An existing fact about the stack that already lives in the index store."},
		{Title: "a", Description: "fact a", Body: "First extracted durable fact about the deployment layout of the stack."},
		{Title: "b", Description: "fact b", Body: "Second extracted durable fact about the deployment layout of the stack."},
		{Title: "c", Description: "fact c", Body: "Third extracted durable fact about the deployment layout of the stack."},
	}
	out := FilterExtractedFacts(facts, store, 2)
	if len(out) != 2 {
		t.Fatalf("cap 2, got %d: %+v", len(out), out)
	}
	for _, f := range out {
		if f.Description == "existing fact" {
			t.Errorf("dedupe failed: %+v", f)
		}
	}
}

// Толерантный парсинг JSON: чистый массив и массив с ```-обёрткой/мусором.
func TestParseExtractJSON(t *testing.T) {
	clean := `[{"title":"a","description":"desc a","type":"project","body":"body a"}]`
	if got := ParseExtractJSON(clean); len(got) != 1 {
		t.Fatalf("clean parse failed: %+v", got)
	}
	fenced := "Here are the facts:\n```json\n[{\"title\":\"b\",\"description\":\"desc b\",\"type\":\"reference\",\"body\":\"body b\"}]\n```\n"
	if got := ParseExtractJSON(fenced); len(got) != 1 || got[0].Title != "b" {
		t.Fatalf("fenced parse failed: %+v", got)
	}
	if got := ParseExtractJSON("no json here"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// Чтение транскрипта: user/assistant без reasoning, tool — имя+обрезанный вывод.
func TestReadSessionTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess")
	content := `{"role":"user","content":"hello"}
{"role":"assistant","reasoning_content":"hidden thinking"}
{"role":"assistant","content":"hi there"}
{"role":"tool","name":"bash","content":"some very long output that should be truncated to two hundred characters for the transcript summary"}
`
	if err := os.WriteFile(path+".jsonl", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := ReadSessionTranscript(path)
	for _, want := range []string{"USER: hello", "ASSISTANT: hi there", "TOOL(bash):"} {
		if !contains(tr, want) {
			t.Errorf("transcript missing %q:\n%s", want, tr)
		}
	}
	if contains(tr, "hidden thinking") {
		t.Error("reasoning_content leaked into transcript")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSaveExtractedFactsMarksMediumTrust(t *testing.T) {
	store := Store{Dir: t.TempDir(), GlobalDir: t.TempDir()}
	facts := []ExtractFact{
		{Title: "extracted one", Description: "an extracted fact about the stack layout", Body: "The extracted fact body with enough detail to survive the junk filter."},
	}
	n, err := SaveExtractedFacts(store, facts, "project")
	if err != nil || n != 1 {
		t.Fatalf("SaveExtractedFacts = %d, %v; want 1, nil", n, err)
	}
	got := store.List()
	if len(got) != 1 {
		t.Fatalf("want 1 memory, got %d", len(got))
	}
	if got[0].Trust != TrustMedium {
		t.Fatalf("auto-extracted fact trust = %q, want medium", got[0].Trust)
	}
	// The persisted file must round-trip the trust marker.
	loaded, ok := loadMemory(store.Path(got[0].Name))
	if !ok || loaded.Trust != TrustMedium {
		t.Fatalf("persisted trust roundtrip = %q (ok=%v), want medium", loaded.Trust, ok)
	}
}
