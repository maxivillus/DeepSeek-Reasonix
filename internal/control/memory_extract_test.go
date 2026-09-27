package control

// The session-end memory extraction trigger was silently dropped during a
// rebase (the function survived, the call site did not), so the host stored no
// extracted facts for six weeks. These tests pin the wiring.

import (
	"path/filepath"
	"sync"
	"testing"

	"reasonix/internal/event"
)

func TestCloseSpawnsMemoryExtraction(t *testing.T) {
	t.Setenv("REASONIX_MEMORY_EXTRACT", "1")

	var mu sync.Mutex
	var calls [][2]string
	prev := memoryExtractSpawn
	memoryExtractSpawn = func(sessionPath, workspaceRoot string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, [2]string{sessionPath, workspaceRoot})
	}
	t.Cleanup(func() { memoryExtractSpawn = prev })

	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.jsonl")
	c := newOwnedTestController(t, Options{
		Sink:          event.Discard,
		SessionPath:   sessionPath,
		WorkspaceRoot: dir,
	})
	if c.incrementalStop == nil {
		t.Fatal("mid-session extraction ticker was not started for a session path")
	}
	// Fire the SessionEnd hook without running a real turn: this test owns the
	// trigger, not the session-start machinery.
	c.mu.Lock()
	c.startedOnce = true
	c.mu.Unlock()

	c.Close()
	<-c.closeFinalized

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("memory extraction spawned %d times, want exactly 1 (calls=%v)", len(calls), calls)
	}
	if calls[0][0] != sessionPath || calls[0][1] != dir {
		t.Fatalf("spawn args = %v, want [%s %s]", calls[0], sessionPath, dir)
	}
}

func TestNoIncrementalTickerWithoutSessionPath(t *testing.T) {
	t.Setenv("REASONIX_MEMORY_EXTRACT", "1")
	c := newOwnedTestController(t, Options{Sink: event.Discard})
	if c.incrementalStop != nil {
		t.Fatal("ticker started without a session path")
	}
}
