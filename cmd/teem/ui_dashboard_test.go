package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/leaderstatus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

// newFullTestTeam returns a registeredTeam populated with plan,
// leaderstatus, and an audit sink so the dashboard/task-flow routes
// can render against real stores.
func newFullTestTeam(t *testing.T, name string) *registeredTeam {
	t.Helper()
	dir := t.TempDir()
	sink, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	planStore, err := plan.Open(filepath.Join(dir, "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = planStore.Close() })
	ls, err := leaderstatus.Open(filepath.Join(dir, "leader_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	tm := &team.Team{
		Name: name,
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 4},
			{Role: "reviewer", Placement: "fargate", MaxConcurrent: 2},
		},
	}
	return &registeredTeam{
		team:           tm,
		auditSink:      sink,
		plan:           planStore,
		leaderStatus:   ls,
		registry:       mcpsrv.NewRegistry(),
		transcriptsDir: filepath.Join(dir, "transcripts"),
		registered:     time.Now().Add(-2 * time.Hour),
	}
}

func TestDashboard_FiltersStoppedWorkers(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-1", Role: "worker", State: mcpsrv.StateRunning})
	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-2", Role: "worker", State: mcpsrv.StateBusy})
	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-3", Role: "worker", State: mcpsrv.StateStopped})
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "worker-1") {
		t.Errorf("running worker missing from dashboard")
	}
	if !strings.Contains(body, "worker-2") {
		t.Errorf("busy worker missing from dashboard")
	}
	if strings.Contains(body, "worker-3") {
		t.Errorf("stopped worker should be filtered out of dashboard, got: %s", body)
	}
}

func TestDashboard_ShowsPlacement(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.registry.Add(mcpsrv.AgentEntry{ID: "reviewer-1", Role: "reviewer", State: mcpsrv.StateRunning})
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "fargate") {
		t.Errorf("placement (fargate) not rendered for reviewer-1: %s", body)
	}
}

func TestDashboard_ShowsLeaderStatusAndTasks(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// One open task in 'building', one done.
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Build the thing"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageBuilding})
	doneTask, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Earlier delivery"})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageBuilding})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageInReview})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageMerging})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageVerified, Status: plan.StatusDone})

	_ = rt.leaderStatus.Set("leader", "Reviewing T1+T6 diff", []string{task.ID})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		"Reviewing T1+T6 diff",
		"Build the thing",
		"Earlier delivery",
		"building",
		"verified",
		task.ID,
		doneTask.ID,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dashboard body", want)
		}
	}
}

func TestTaskFlow_RendersBannerAndDecisions(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Refactor auth"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageBuilding, AddEvidence: []string{"j-aaa"}})

	now := time.Now()
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-5 * time.Minute),
		AgentID:   "leader",
		Kind:      audit.KindDecisionNote,
		Message:   "Kept old API around so mobile team can ship",
		Meta:      map[string]any{"task_id": task.ID},
	})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-3 * time.Minute),
		AgentID:   "worker-1",
		Kind:      audit.KindBlockerNote,
		Message:   "Need creds from ops",
		Meta:      map[string]any{"task_id": task.ID},
	})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-2 * time.Minute),
		AgentID:   "worker-1",
		JobID:     "j-aaa",
		Kind:      audit.KindJobReceived,
		Meta:      map[string]any{"prompt": "do the refactor"},
	})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-1 * time.Minute),
		AgentID:   "worker-1",
		JobID:     "j-aaa",
		Kind:      audit.KindJobComplete,
		Meta:      map[string]any{"output": "refactor done"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/tasks/"+task.ID, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Refactor auth",
		"building",
		"Kept old API around so mobile team can ship",
		"Need creds from ops",
		"j-aaa",
		"do the refactor",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in task flow body", want)
		}
	}
}

func TestTaskFlow_LongPromptCollapsesIntoDetails(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "X"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{AddEvidence: []string{"j-long"}})

	long := strings.Repeat("supercalifragilisticexpialidocious ", 30)
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().Add(-2 * time.Minute),
		AgentID:   "worker-1",
		JobID:     "j-long",
		Kind:      audit.KindJobReceived,
		Meta:      map[string]any{"prompt": long},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/tasks/"+task.ID, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "<details class=\"expandable\"") {
		t.Errorf("expected long prompt to collapse into <details>; body=%s", body)
	}
}

// TestDashboard_MarksOrphanedAssigneeStale locks in the visual signal
// for the situation the user hit: tasks sit in an active pipeline stage
// (building/in_review/merging) with an AssignedTo that names a worker
// no longer in the registry. The dashboard should:
//   - mute/strike the assignee (class="assignee gone")
//   - show a STALE pill so the leader sees they need to act.
func TestDashboard_MarksOrphanedAssigneeStale(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	live := "worker-live"
	ghost := "worker-ghost"
	vanished := "worker-vanished"

	// Task A: assigned to a worker that IS active → no stale, no gone.
	taskA, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "live-handoff"})
	_, _ = rt.plan.UpdateTask(taskA.ID, plan.UpdateInput{AssignedTo: &live, Stage: plan.StageBuilding})
	rt.registry.Add(mcpsrv.AgentEntry{ID: live, Role: "worker", State: mcpsrv.StateBusy})

	// Task B: assigned to a worker that is GONE (never registered) →
	// stage is building → must surface as STALE + gone.
	taskB, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "orphaned"})
	_, _ = rt.plan.UpdateTask(taskB.ID, plan.UpdateInput{AssignedTo: &ghost, Stage: plan.StageBuilding})

	// Task C: assignee gone but stage is 'proposed' — not in an
	// active-work stage, so we mute the assignee but do NOT mark stale.
	taskC, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "pre-work"})
	_, _ = rt.plan.UpdateTask(taskC.ID, plan.UpdateInput{AssignedTo: &vanished})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Task A's row should not carry the gone class or STALE pill.
	rowA := extractTaskRow(t, body, taskA.ID)
	if strings.Contains(rowA, "assignee gone") {
		t.Errorf("task A assignee is live; should not be marked gone:\n%s", rowA)
	}
	if strings.Contains(rowA, "STALE") {
		t.Errorf("task A is not stale (live worker):\n%s", rowA)
	}

	// Task B: orphaned in an active stage → gone class + STALE pill.
	rowB := extractTaskRow(t, body, taskB.ID)
	if !strings.Contains(rowB, "assignee gone") {
		t.Errorf("task B has an orphaned assignee — assignee cell should carry the gone class:\n%s", rowB)
	}
	if !strings.Contains(rowB, "STALE") {
		t.Errorf("task B is in an active stage with a vanished worker — STALE pill missing:\n%s", rowB)
	}

	// Task C: gone but stage is proposed — mute the assignee but no STALE.
	rowC := extractTaskRow(t, body, taskC.ID)
	if !strings.Contains(rowC, "assignee gone") {
		t.Errorf("task C: assignee is gone, cell should be muted:\n%s", rowC)
	}
	if strings.Contains(rowC, "STALE") {
		t.Errorf("task C is in 'proposed' stage; STALE is reserved for active work stages:\n%s", rowC)
	}
}

// extractTaskRow returns the HTML for the <tr>...</tr> row that
// contains the given task id. Best-effort string slicing — good enough
// for asserting per-row classes in the dashboard tests.
func extractTaskRow(t *testing.T, body, taskID string) string {
	t.Helper()
	idx := strings.Index(body, taskID)
	if idx < 0 {
		t.Fatalf("task %q not found in body", taskID)
	}
	start := strings.LastIndex(body[:idx], "<tr")
	if start < 0 {
		t.Fatalf("no <tr before task %q", taskID)
	}
	end := strings.Index(body[idx:], "</tr>")
	if end < 0 {
		t.Fatalf("no </tr> after task %q", taskID)
	}
	return body[start : idx+end+len("</tr>")]
}

func TestResolveTaskFlowRoute(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"/tasks/t-aa", "t-aa", true},
		{"/tasks/", "", false},
		{"/tasks/t-aa/extra", "", false},
		{"/jobs/t-aa", "", false},
	}
	for _, tc := range cases {
		got, ok := resolveTaskFlowRoute(tc.in)
		if got != tc.wantID || ok != tc.wantOK {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", tc.in, got, ok, tc.wantID, tc.wantOK)
		}
	}
}
