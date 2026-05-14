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
		// Use the test name as both display name and routing id so
		// existing assertions on URL paths like `/teams/alpha/...` keep
		// working. The daemon's teams map is keyed by id, not name.
		ID:   name,
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

	// Worker identity and placement only render on the per-team detail
	// page; the summary index at "/" carries counters, not chip rows.
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
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

	// Task-level styling lives on the per-team detail page; the summary
	// index only carries counters and the leader status snippet.
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
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

// TestTeamDetail_RendersBranchesSection seeds a real temp git repo with
// teem/* branches, points a registered team at it, and verifies that
// the per-team detail page shows the new "Active branches" rows and
// the summary index tile carries the branch counter.
func TestTeamDetail_RendersBranchesSection(t *testing.T) {
	dir := seedRepoWithBranches(t)

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.repoRoot = dir
	// Mark worker-1 live so its row links to the jobs page; worker-2
	// stays orphaned (no registry entry).
	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-1", Role: "worker", State: mcpsrv.StateRunning})
	d.teams["alpha"] = rt

	// Per-team detail page renders the full section.
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Active branches",
		"teem/worker-1",
		"teem/worker-2",
		"did the thing",
		"left over branch",
		`href="/teams/alpha/agents/worker-1/jobs"`,
		"orphan", // class added when no registry entry
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in detail body", want)
		}
	}
	// feature/x is not a teem/ branch and must not appear.
	if strings.Contains(body, "feature/x") {
		t.Errorf("non-teem branch leaked into Active branches list")
	}

	// Summary tile shows the branch counter.
	reqI := httptest.NewRequest(http.MethodGet, "/", nil)
	wI := httptest.NewRecorder()
	d.handler().ServeHTTP(wI, reqI)
	bodyI := wI.Body.String()
	if !strings.Contains(bodyI, "Branches") {
		t.Errorf("summary tile missing branches counter label: %s", bodyI)
	}
}

// TestTeamDetail_NoRepoShowsPlaceholder asserts a repo-less team
// (Fargate-only) renders the section header with a "(no repo)" hint
// instead of attempting to shell out and 500-ing.
func TestTeamDetail_NoRepoShowsPlaceholder(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.repoRoot = ""
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Active branches") {
		t.Errorf("section header missing")
	}
	if !strings.Contains(body, "(no repo)") {
		t.Errorf("expected '(no repo)' placeholder for repo-less team")
	}
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

// TestSummaryIndex_RendersTilePerTeam verifies the / route renders one
// tile per registered team, with the four headline counters and a deep
// link to the per-team detail page. The numbers are exercised end-to-end
// so a regression in teamTileSnapshot is visible.
func TestSummaryIndex_RendersTilePerTeam(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rtA := newFullTestTeam(t, "alpha")
	rtA.registry.Add(mcpsrv.AgentEntry{ID: "worker-1", Role: "worker", State: mcpsrv.StateRunning})
	rtA.registry.Add(mcpsrv.AgentEntry{ID: "worker-2", Role: "worker", State: mcpsrv.StateBusy})
	// One open task and one completed-today task (UpdatedAt == now).
	openT, _ := rtA.plan.AddTask(plan.NewTaskInput{Title: "Build something"})
	_, _ = rtA.plan.UpdateTask(openT.ID, plan.UpdateInput{Stage: plan.StageBuilding})
	doneT, _ := rtA.plan.AddTask(plan.NewTaskInput{Title: "Already shipped"})
	_, _ = rtA.plan.UpdateTask(doneT.ID, plan.UpdateInput{Status: plan.StatusDone})
	_ = rtA.leaderStatus.Set("leader", "Cutting the T20 release", nil)
	d.teams["alpha"] = rtA

	rtB := newFullTestTeam(t, "beta")
	d.teams["beta"] = rtB

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Each team has a tile that deep-links to /teams/<slug>.
	for _, want := range []string{
		`href="/teams/alpha"`,
		`href="/teams/beta"`,
		"alpha",
		"beta",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in summary body", want)
		}
	}

	// Headline labels are present (counters render even when zero).
	for _, want := range []string{
		"Open task",
		"Active worker",
		"In flight",
		"Completed today",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing counter label %q in summary body", want)
		}
	}

	// The leader-status one-liner shows up.
	if !strings.Contains(body, "Cutting the T20 release") {
		t.Errorf("missing leader status on tile: %s", body)
	}

	// The detail-page only sections are NOT inlined on the index.
	if strings.Contains(body, "worker-1") || strings.Contains(body, "Status board") {
		t.Errorf("detail-page content leaked into summary index: %s", body)
	}
}

// TestTeamDetail_RendersSingleTeam verifies /teams/<slug> renders the
// deep view for that team and the deep view alone. The team is keyed
// by its slug; the display name can still contain spaces or
// capitals without affecting routing.
func TestTeamDetail_RendersSingleTeam(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha-team")
	rt.team.Name = "Alpha Team"
	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-1", Role: "worker", State: mcpsrv.StateBusy})
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Wire up the thing"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageBuilding})
	_ = rt.leaderStatus.Set("leader", "Looking at the diff", []string{task.ID})

	// Second team must NOT bleed into the detail page.
	rtOther := newFullTestTeam(t, "beta")
	otherTask, _ := rtOther.plan.AddTask(plan.NewTaskInput{Title: "Other team's task"})
	_ = otherTask
	d.teams["alpha-team"] = rt
	d.teams["beta"] = rtOther

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha-team", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, want := range []string{
		"Alpha Team",
		"Status board",
		"Looking at the diff",
		"Wire up the thing",
		"worker-1",
		"Open tasks",
		"Active agents",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in team detail body", want)
		}
	}

	// Other team's content must not leak in.
	if strings.Contains(body, "Other team's task") {
		t.Errorf("other team leaked into detail page: %s", body)
	}

	// Unknown slug → 404.
	req2 := httptest.NewRequest(http.MethodGet, "/teams/nonesuch", nil)
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("unknown slug should 404, got %d", w2.Code)
	}
}
