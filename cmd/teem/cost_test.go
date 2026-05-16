package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// writePricingFile drops the test pricing.yaml under tmpHome/.teem so
// teamSnapshot's DefaultPricingPath() picks it up. Tests must
// t.Setenv("HOME", tmpHome) before calling.
func writePricingFile(t *testing.T, tmpHome string) {
	t.Helper()
	dir := filepath.Join(tmpHome, ".teem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`pricing:
  claude-opus-4-7:
    input_per_million: 15.00
    output_per_million: 75.00
    cache_read_per_million: 1.50
    cache_create_per_million: 18.75
`)
	if err := os.WriteFile(filepath.Join(dir, "pricing.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeUsageEvent appends one KindUsageEvent to the team's audit sink so
// the dashboard's per-render projection picks it up.
func writeUsageEvent(t *testing.T, sink audit.Sink, agentID, jobID, model string, in, out, cw, cr int64, when time.Time) {
	t.Helper()
	if err := sink.Write(audit.Event{
		Timestamp: when,
		AgentID:   agentID,
		JobID:     jobID,
		Kind:      audit.KindUsageEvent,
		Meta: map[string]any{
			"agent_id":            agentID,
			"job_id":              jobID,
			"model":               model,
			"input_tokens":        in,
			"output_tokens":       out,
			"cache_create_tokens": cw,
			"cache_read_tokens":   cr,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// findTaskByID walks a dashboardTask slice for the matching ID.
func findTaskByID(tasks []dashboardTask, id string) (dashboardTask, bool) {
	for _, t := range tasks {
		if t.ID == id {
			return t, true
		}
	}
	return dashboardTask{}, false
}

// TestTeamSnapshot_PerTaskCost locks in the wiring: with pricing.yaml
// present and a KindUsageEvent linked to a task's evidence, the
// per-task cost cell carries the formatted dollar amount and the
// per-job breakdown the SPA renders inside the drill-in <details>.
func TestTeamSnapshot_PerTaskCost(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	writePricingFile(t, tmpHome)

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Run the worker"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageCoding, AddEvidence: []string{"job-42"}})

	// 1M input + 0.5M output on opus = $15 + $37.50 = $52.50
	writeUsageEvent(t, rt.auditSink, "worker-uma", "job-42", "claude-opus-4-7",
		1_000_000, 500_000, 0, 0, time.Now())

	snap := teamSnapshot(d.snapshotTeam(rt))
	got, ok := findTaskByID(snap.OpenTasks, task.ID)
	if !ok {
		t.Fatalf("task %s missing from OpenTasks", task.ID)
	}
	if !got.Cost.HasCost {
		t.Errorf("Cost.HasCost = false, want true")
	}
	if got.Cost.Display != "$52.50" {
		t.Errorf("Cost.Display = %q, want $52.50", got.Cost.Display)
	}
	if len(got.Cost.Jobs) != 1 || got.Cost.Jobs[0].JobID != "job-42" || got.Cost.Jobs[0].Model != "claude-opus-4-7" {
		t.Errorf("Cost.Jobs = %+v, want one job-42/claude-opus-4-7 row", got.Cost.Jobs)
	}
}

// TestTeamSnapshot_HeroSpend_PresentWithPricing verifies the hero
// "today's spend" field carries the formatted total when pricing is
// loaded and at least one usage event is in scope.
func TestTeamSnapshot_HeroSpend_PresentWithPricing(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	writePricingFile(t, tmpHome)

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	now := time.Now()
	writeUsageEvent(t, rt.auditSink, "w1", "j1", "claude-opus-4-7", 1_000_000, 0, 0, 0, now)
	writeUsageEvent(t, rt.auditSink, "w2", "j2", "claude-opus-4-7", 1_000_000, 0, 0, 0, now)

	snap := teamSnapshot(d.snapshotTeam(rt))
	if !snap.HasPricing {
		t.Errorf("HasPricing = false, want true")
	}
	if snap.HeroSpendDisplay != "$30.00" {
		t.Errorf("HeroSpendDisplay = %q, want $30.00", snap.HeroSpendDisplay)
	}
}

// TestTeamSnapshot_HeroSpend_HiddenWithoutPricing: with no pricing.yaml,
// HasPricing must be false and HeroSpendDisplay must stay empty even
// though usage events are arriving in the audit.
func TestTeamSnapshot_HeroSpend_HiddenWithoutPricing(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// No writePricingFile — file is absent.

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Anything"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageCoding, AddEvidence: []string{"job-42"}})
	writeUsageEvent(t, rt.auditSink, "w1", "job-42", "claude-opus-4-7",
		1_000_000, 500_000, 0, 0, time.Now())

	snap := teamSnapshot(d.snapshotTeam(rt))
	if snap.HasPricing {
		t.Errorf("HasPricing = true without pricing.yaml")
	}
	if snap.HeroSpendDisplay != "" {
		t.Errorf("HeroSpendDisplay = %q, want empty", snap.HeroSpendDisplay)
	}
	if got, _ := findTaskByID(snap.OpenTasks, task.ID); got.Cost.HasCost {
		t.Errorf("task Cost.HasCost = true without pricing.yaml")
	}
}

// TestTeamSnapshot_TodaysSpend_SeparateFromPerTaskSum guards the
// design invariant: a job linked to two tasks counts twice in each
// task's per-task cost cell but only once in HeroSpendDisplay.
func TestTeamSnapshot_TodaysSpend_SeparateFromPerTaskSum(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	writePricingFile(t, tmpHome)

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	taskA, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Task A"})
	_, _ = rt.plan.UpdateTask(taskA.ID, plan.UpdateInput{Stage: plan.StageCoding, AddEvidence: []string{"shared"}})
	taskB, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Task B"})
	_, _ = rt.plan.UpdateTask(taskB.ID, plan.UpdateInput{Stage: plan.StageCoding, AddEvidence: []string{"shared"}})

	// shared job: $15. Both tasks list it in evidence.
	writeUsageEvent(t, rt.auditSink, "w1", "shared", "claude-opus-4-7", 1_000_000, 0, 0, 0, time.Now())

	snap := teamSnapshot(d.snapshotTeam(rt))
	a, _ := findTaskByID(snap.OpenTasks, taskA.ID)
	b, _ := findTaskByID(snap.OpenTasks, taskB.ID)
	if a.Cost.Display != "$15.00" || b.Cost.Display != "$15.00" {
		t.Errorf("per-task displays = %q/%q, want both $15.00", a.Cost.Display, b.Cost.Display)
	}
	// Hero spend uses raw stream — total stays $15, NOT $30.
	if snap.HeroSpendDisplay != "$15.00" {
		t.Errorf("HeroSpendDisplay = %q, want $15.00 (raw stream — not over-attributed)", snap.HeroSpendDisplay)
	}
}
