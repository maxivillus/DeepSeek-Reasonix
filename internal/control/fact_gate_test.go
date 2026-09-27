package control

// fact gate on the session-engine path: a goal whose ground a fresh memory
// fact already covers is created with a bounded round limit instead of an
// unbounded autonomous loop, and the user is told why.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/memory"
	"reasonix/internal/session"
)

// factGateGoal classifies as research and shares enough distinctive terms with
// the fixture fact below for the authoritative recall tier.
const factGateGoal = "investigate the make rebuild pipeline for the custom toolchain, implement the fix, verify it with tests and document the findings"

const factGateFactBody = "The custom toolchain make binary rebuild pipeline rebuilds make and documents the custom toolchain."

func factGateMemory(t *testing.T, withFact bool) *memory.Set {
	t.Helper()
	userDir := t.TempDir()
	cwd := t.TempDir()
	if withFact {
		store := memory.StoreFor(userDir, cwd)
		if _, err := store.Save(memory.Memory{
			Name:        "make-rebuild-pipeline",
			Title:       "make rebuild pipeline",
			Description: "custom toolchain make binary rebuild pipeline documented",
			Body:        factGateFactBody,
			Type:        memory.TypeProject,
			Scope:       memory.FactScopeProject,
			Trust:       memory.TrustHigh,
			UpdatedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("save fact: %v", err)
		}
	}
	return memory.Load(memory.Options{CWD: cwd, UserDir: userDir})
}

type noticeRecorder struct {
	mu   sync.Mutex
	text []string
}

func (r *noticeRecorder) emit(ev event.Event) {
	if ev.Kind != event.Notice {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text = append(r.text, ev.Text)
}

func (r *noticeRecorder) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.text, "\n")
}

func factGateController(t *testing.T, withFact bool) (*Controller, *noticeRecorder) {
	t.Helper()
	ctx := context.Background()
	service, err := session.NewService("fact-gate", session.NewFilesystemPersistence(t.TempDir()))
	if err != nil {
		t.Fatalf("session service: %v", err)
	}
	runtime, err := service.Create(ctx, session.CreateOptions{SessionID: "fact-gate"})
	if err != nil {
		t.Fatalf("session runtime: %v", err)
	}
	rec := &noticeRecorder{}
	c := newOwnedTestController(t, Options{
		Sink:             event.FuncSink(rec.emit),
		SessionService:   service,
		SessionRuntime:   runtime,
		ExclusiveSession: true,
		Memory:           factGateMemory(t, withFact),
	})
	return c, rec
}

func TestFactGateLimitsEngineGoalRounds(t *testing.T) {
	if got := ClassifyGoalBudget(factGateGoal); got != budgetClassResearch {
		t.Fatalf("fixture goal class = %q, want %q", got, budgetClassResearch)
	}
	c, rec := factGateController(t, true)
	if !c.sessionEngineEnabled() {
		t.Fatal("fixture must use the session engine")
	}
	if !c.factCoversGoal(factGateGoal, GoalResearchAuto) {
		t.Fatal("fixture fact does not cover the goal")
	}
	if err := c.SetGoalDurable(factGateGoal); err != nil {
		t.Fatalf("SetGoalDurable: %v", err)
	}
	view, err := c.GetGoal(context.Background())
	if err != nil {
		t.Fatalf("GetGoal: %v", err)
	}
	if view == nil || view.MaxGoalRounds == nil {
		t.Fatalf("covered goal should carry a round limit, got %+v", view)
	}
	if *view.MaxGoalRounds != uint64(factGateRoundLimit) {
		t.Fatalf("MaxGoalRounds = %d, want %d", *view.MaxGoalRounds, factGateRoundLimit)
	}
	if !strings.Contains(rec.joined(), "autoresearch skipped") {
		t.Fatalf("gate notice missing: %q", rec.joined())
	}
}

func TestFactGateLeavesUncoveredGoalUnlimited(t *testing.T) {
	c, rec := factGateController(t, false)
	if c.factCoversGoal(factGateGoal, GoalResearchAuto) {
		t.Fatal("gate fired without a covering fact")
	}
	if err := c.SetGoalDurable(factGateGoal); err != nil {
		t.Fatalf("SetGoalDurable: %v", err)
	}
	view, err := c.GetGoal(context.Background())
	if err != nil {
		t.Fatalf("GetGoal: %v", err)
	}
	if view == nil || view.MaxGoalRounds != nil {
		t.Fatalf("goal without a covering fact must stay unlimited, got %+v", view)
	}
	if strings.Contains(rec.joined(), "autoresearch skipped") {
		t.Fatalf("unexpected gate notice: %q", rec.joined())
	}
}

func TestFactGateApplies(t *testing.T) {
	researchGoal := "系统性地研究并分析 make 问题，修复并验证 custom/bin/make 方案，总结文档"
	plainGoal := "fix the typo in the README"
	cases := []struct {
		name   string
		goal   string
		mode   GoalResearchMode
		strong bool
		want   bool
	}{
		{"auto+research-heuristic+strong → gate", researchGoal, GoalResearchAuto, true, true},
		{"auto+research-heuristic+no-fact → no gate", researchGoal, GoalResearchAuto, false, false},
		{"auto+plain+strong → no gate (no research anyway)", plainGoal, GoalResearchAuto, true, false},
		{"explicit --research never gated", researchGoal, GoalResearchOn, true, false},
		{"explicit --no-research never gated", researchGoal, GoalResearchOff, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := factGateApplies(tc.goal, tc.mode, tc.strong); got != tc.want {
				t.Fatalf("factGateApplies(%q, %v, %v) = %v, want %v", tc.goal, tc.mode, tc.strong, got, tc.want)
			}
		})
	}
}

func TestActiveGoalBlockFactGateMarker(t *testing.T) {
	goal := "系统性地研究并分析 make 问题，修复并验证 custom/bin/make 方案，总结文档"

	gated := activeGoalBlock(goal, true)
	if !strings.Contains(gated, "Auto-research was skipped for this goal") {
		t.Fatalf("gated block missing fact-covers marker:\n%s", gated)
	}
	if strings.Contains(gated, "AutoResearch protocol") {
		t.Fatalf("gated block still carries AutoResearch protocol instructions:\n%s", gated)
	}

	ungated := activeGoalBlock(goal, false)
	if strings.Contains(ungated, "Auto-research was skipped") {
		t.Fatalf("ungated block has marker without gate:\n%s", ungated)
	}
}
