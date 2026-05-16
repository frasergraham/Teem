package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestDashboardTask_RendersCostColumn locks in the wiring: with
// pricing.yaml present and a KindUsageEvent linked to a task's
// evidence, the rendered HTML shows the Cost header, the formatted
// dollar amount, and the drill-in <details> structure.
func TestDashboardTask_RendersCostColumn(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"<th class=\"cost-col\"",
		"Cost",
		"$52.50",
		"details-task-" + task.ID + "-cost",
		"job-42",
		"claude-opus-4-7",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestDashboard_HeroSpend_RendersWhenPricingPresent verifies the hero
// shows "today's spend" with the formatted total when pricing is
// loaded and at least one usage event is in scope.
func TestDashboard_HeroSpend_RendersWhenPricingPresent(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	writePricingFile(t, tmpHome)

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Two distinct jobs so the daily total is unambiguous: each is 1M
	// input on opus = $15, total $30.
	now := time.Now()
	writeUsageEvent(t, rt.auditSink, "w1", "j1", "claude-opus-4-7", 1_000_000, 0, 0, 0, now)
	writeUsageEvent(t, rt.auditSink, "w2", "j2", "claude-opus-4-7", 1_000_000, 0, 0, 0, now)

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "hero-spend") {
		t.Errorf("hero-spend element missing; body=%s", body)
	}
	if !strings.Contains(body, "$30.00") {
		t.Errorf("hero spend total $30.00 missing; body=%s", body)
	}
	if !strings.Contains(body, "today's spend") {
		t.Errorf("hero spend label missing")
	}
}

// TestDashboard_HeroSpend_HiddenWhenPricingAbsent: with no
// pricing.yaml, the hero spend line MUST NOT render — even though
// usage events still arrive in the audit. The cost UI is hidden, not
// zero-rendered.
func TestDashboard_HeroSpend_HiddenWhenPricingAbsent(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, "hero-spend") {
		t.Errorf("hero-spend rendered without pricing.yaml; body=%s", body)
	}
	if strings.Contains(body, "today's spend") {
		t.Errorf("hero spend label rendered without pricing.yaml")
	}
	// And the Cost column header must be absent too.
	if strings.Contains(body, "<th class=\"cost-col\"") {
		t.Errorf("Cost column rendered without pricing.yaml")
	}
}

// TestDashboardTask_TodaysSpend_SeparateFromPerTaskSum guards the
// design invariant: a job linked to two tasks counts twice in the
// per-task column but only once in Today's spend.
func TestDashboardTask_TodaysSpend_SeparateFromPerTaskSum(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()
	// Per-task cells over-attribute — both rows show $15.
	if strings.Count(body, "$15.00") < 2 {
		t.Errorf("expected $15.00 to appear at least twice (once per task); body=%s", body)
	}
	// Hero spend uses raw stream — total stays $15, NOT $30.
	if !strings.Contains(body, "$15.00") {
		t.Errorf("hero spend missing; body=%s", body)
	}
	if strings.Contains(body, "$30.00") {
		t.Errorf("hero spend leaked over-attribution into the daily total")
	}
}
