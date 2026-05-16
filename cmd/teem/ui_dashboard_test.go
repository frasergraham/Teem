package main

import (
	"fmt"
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
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
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
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageCoding})
	doneTask, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Earlier delivery"})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageCoding})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageReviewing})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageIntegrating})
	_, _ = rt.plan.UpdateTask(doneTask.ID, plan.UpdateInput{Stage: plan.StageVerified, Status: plan.StatusDone})

	_ = rt.leaderStatus.Set("leader", "Reviewing T1+T6 diff", []string{task.ID})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		"Reviewing T1+T6 diff",
		"Build the thing",
		"Earlier delivery",
		"coding",
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
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageCoding, AddEvidence: []string{"j-aaa"}})

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
		"coding",
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
// (planning/coding/reviewing/integrating) with an AssignedTo that names a worker
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
	_, _ = rt.plan.UpdateTask(taskA.ID, plan.UpdateInput{AssignedTo: &live, Stage: plan.StageCoding})
	rt.registry.Add(mcpsrv.AgentEntry{ID: live, Role: "worker", State: mcpsrv.StateBusy})

	// Task B: assigned to a worker that is GONE (never registered) →
	// stage is coding → must surface as STALE + gone.
	taskB, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "orphaned"})
	_, _ = rt.plan.UpdateTask(taskB.ID, plan.UpdateInput{AssignedTo: &ghost, Stage: plan.StageCoding})

	// Task C: assignee gone but stage is 'proposed' — not in an
	// active-work stage, so we mute the assignee but do NOT mark stale.
	taskC, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "pre-work"})
	_, _ = rt.plan.UpdateTask(taskC.ID, plan.UpdateInput{AssignedTo: &vanished})

	// Task-level styling lives on the per-team detail page; the summary
	// index only carries counters and the leader status snippet.
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
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

// TestDashboard_AssigneeExplicit locks in the simplest case: a task
// whose assigned_to is set renders that value as-is — no italic
// "derived" class, no inferred-from-evidence note.
func TestDashboard_AssigneeExplicit(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	owner := "worker-foo"
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Explicit owner"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{AssignedTo: &owner, Stage: plan.StageCoding})
	rt.registry.Add(mcpsrv.AgentEntry{ID: owner, Role: "worker", State: mcpsrv.StateBusy})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	row := extractTaskRow(t, w.Body.String(), task.ID)
	if !strings.Contains(row, owner) {
		t.Errorf("expected explicit assignee %q in row:\n%s", owner, row)
	}
	if strings.Contains(row, "assignee derived") || strings.Contains(row, " derived\"") {
		t.Errorf("explicit assignee must not be rendered as derived:\n%s", row)
	}
}

// TestDashboard_AssigneeDerivedFromEvidence covers the bug this task
// was filed against: link_task_to_job only appends to evidence and
// leaves assigned_to empty. The dashboard must infer the assignee
// from the latest evidence job's agent_id and mark it derived.
func TestDashboard_AssigneeDerivedFromEvidence(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Evidence-linked"})
	// Two evidence entries; the dashboard walks newest-last, so the
	// later one (j-new / worker-bar) is the assignee that wins.
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:       plan.StageCoding,
		AddEvidence: []string{"j-old", "j-new"},
	})
	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-bar", Role: "worker", State: mcpsrv.StateBusy})

	now := time.Now()
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-10 * time.Minute),
		AgentID:   "worker-old",
		JobID:     "j-old",
		Kind:      audit.KindJobReceived,
	})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-2 * time.Minute),
		AgentID:   "worker-bar",
		JobID:     "j-new",
		Kind:      audit.KindJobReceived,
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	row := extractTaskRow(t, w.Body.String(), task.ID)
	if !strings.Contains(row, "worker-bar") {
		t.Errorf("expected derived assignee 'worker-bar' (from latest evidence job):\n%s", row)
	}
	if !strings.Contains(row, "derived") {
		t.Errorf("derived assignee must carry the 'derived' class:\n%s", row)
	}
}

// TestDashboard_AssigneeBlankWhenNoneAvailable covers the fall-through
// case: a task with neither an explicit assignee nor any resolvable
// evidence renders the empty placeholder, not a stray value.
func TestDashboard_AssigneeBlankWhenNoneAvailable(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Unowned"})
	// Evidence references a job the audit log has no record of; the
	// lookup must fail and the assignee column stay blank.
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:       plan.StageCoding,
		AddEvidence: []string{"j-missing"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	row := extractTaskRow(t, w.Body.String(), task.ID)
	// Open-tasks template renders "—" when the assignee is empty.
	if !strings.Contains(row, "—") {
		t.Errorf("expected blank-assignee em-dash placeholder:\n%s", row)
	}
	if strings.Contains(row, "derived") {
		t.Errorf("no resolvable evidence: row must not be marked derived:\n%s", row)
	}
}

// extractTaskRow returns the HTML for the <tr>...</tr> row that
// contains the given task id. Best-effort string slicing — good enough
// for asserting per-row classes in the dashboard tests.
func extractTaskRow(t *testing.T, body, taskID string) string {
	t.Helper()
	// Task ids can appear in non-row contexts now (e.g. the hero
	// stage bar's title= tooltip lists today's task ids). Walk the
	// body looking for the occurrence that's actually inside a <tr>.
	pos := 0
	for {
		off := strings.Index(body[pos:], taskID)
		if off < 0 {
			break
		}
		idx := pos + off
		start := strings.LastIndex(body[:idx], "<tr")
		if start >= 0 {
			end := strings.Index(body[idx:], "</tr>")
			if end < 0 {
				t.Fatalf("no </tr> after task %q", taskID)
			}
			row := body[start : idx+end+len("</tr>")]
			// Skip "occurrences" outside an actual row: if there's a
			// closing </tr> between the candidate <tr and the id,
			// the id isn't inside that row.
			if !strings.Contains(body[start:idx], "</tr>") {
				return row
			}
		}
		pos = idx + len(taskID)
	}
	t.Fatalf("task %q not found inside any <tr> row", taskID)
	return ""
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
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`<details id="details-branches"`,
		"2 branches",
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `<details id="details-branches"`) {
		t.Errorf("collapsed branches section missing")
	}
	if !strings.Contains(body, "(no repo)") {
		t.Errorf("expected '(no repo)' placeholder for repo-less team")
	}
}

// TestTeamPage_BranchesCollapsedByDefault asserts the branches section
// renders as a closed <details> at the bottom of the team page (below
// the recent-activity section), with the count + name peek in the
// summary and the existing branches table inside the body.
func TestTeamPage_BranchesCollapsedByDefault(t *testing.T) {
	dir := seedRepoWithBranches(t)

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.repoRoot = dir
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// <details id="details-branches"> must be present and NOT pre-opened.
	idx := strings.Index(body, `<details id="details-branches"`)
	if idx < 0 {
		t.Fatalf("missing collapsed branches <details> wrapper")
	}
	// Slice forward to the closing > of the opening tag and assert no
	// open attribute slipped in (default state must be collapsed).
	openTagEnd := strings.Index(body[idx:], ">")
	if openTagEnd < 0 {
		t.Fatalf("unterminated <details> opening tag")
	}
	openTag := body[idx : idx+openTagEnd+1]
	if strings.Contains(openTag, " open") {
		t.Errorf("branches <details> opened by default: %q", openTag)
	}

	// Summary carries the count text and the name peek (two seeded branches).
	if !strings.Contains(body, "2 branches") {
		t.Errorf("summary missing branch count")
	}
	if !strings.Contains(body, "teem/worker-1") || !strings.Contains(body, "teem/worker-2") {
		t.Errorf("summary peek missing seeded branch names")
	}

	// Branches section must come AFTER the Recent activity section.
	activityIdx := strings.Index(body, "Recent activity")
	if activityIdx < 0 {
		t.Fatalf("Recent activity heading not found")
	}
	if idx < activityIdx {
		t.Errorf("branches section at offset %d should come after Recent activity at %d", idx, activityIdx)
	}
}

// TestTeamPage_BranchesPeek_Truncates asserts the comma-joined peek is
// capped at branchNamePeekLimit names and appends a "+N more" tail.
func TestTeamPage_BranchesPeek_Truncates(t *testing.T) {
	rows := make([]dashboardBranch, 10)
	for i := range rows {
		rows[i].Name = fmt.Sprintf("teem/worker-%c", 'a'+i)
	}
	got := branchNamePeek(rows, branchNamePeekLimit)
	want := "teem/worker-a, teem/worker-b, teem/worker-c, teem/worker-d, teem/worker-e +5 more"
	if got != want {
		t.Errorf("peek mismatch:\n  got  %q\n  want %q", got, want)
	}
}

// TestTeamPage_NoBranches_StillRenders asserts the team page is happy
// with a repo that has zero teem/* branches: the collapsed wrapper is
// there, the summary reads "0 branches", and the empty body renders the
// existing placeholder rather than panicking.
func TestTeamPage_NoBranches_StillRenders(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.repoRoot = t.TempDir() // empty dir → listTeemBranches yields nil
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `<details id="details-branches"`) {
		t.Errorf("collapsed branches wrapper missing")
	}
	if !strings.Contains(body, "0 branches") {
		t.Errorf("expected '0 branches' in summary")
	}
	if !strings.Contains(body, "no teem/* branches in this working tree") {
		t.Errorf("empty-state placeholder missing")
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
	_, _ = rtA.plan.UpdateTask(openT.ID, plan.UpdateInput{Stage: plan.StageCoding})
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
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageCoding})
	_ = rt.leaderStatus.Set("leader", "Looking at the diff", []string{task.ID})

	// Second team must NOT bleed into the detail page.
	rtOther := newFullTestTeam(t, "beta")
	otherTask, _ := rtOther.plan.AddTask(plan.NewTaskInput{Title: "Other team's task"})
	_ = otherTask
	d.teams["alpha-team"] = rt
	d.teams["beta"] = rtOther

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha-team/legacy", nil)
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

// TestResolveTeam_IDAndNameAlias verifies the URL key resolver
// accepts both the canonical team id and the team's display Name.
// Long-lived clients (Claude Code's MCP transport, the teem-channel
// SSE shim) captured a `/teams/<name>/...` URL before TI1 minted a
// separate id; the alias keeps that handshake alive across daemon
// restart.
func TestResolveTeam_IDAndNameAlias(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "ignored-by-test")
	rt.team.ID = "t-abc1234567890def"
	rt.team.Name = "example-team"
	d.teams[rt.team.ID] = rt

	// id match.
	if got := d.resolveTeam("t-abc1234567890def"); got != rt {
		t.Errorf("id lookup: got %p want %p", got, rt)
	}
	// name alias.
	if got := d.resolveTeam("example-team"); got != rt {
		t.Errorf("name alias: got %p want %p", got, rt)
	}
	// miss.
	if got := d.resolveTeam("nonesuch"); got != nil {
		t.Errorf("unknown key should return nil, got %p", got)
	}

	// Bare-team page must render for both URL forms.
	for _, key := range []string{"t-abc1234567890def", "example-team"} {
		req := httptest.NewRequest(http.MethodGet, "/teams/"+key+"/legacy", nil)
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET /teams/%s: code=%d body=%s", key, w.Code, w.Body.String())
			continue
		}
		// Both URLs should render the same team — display name appears in body.
		if !strings.Contains(w.Body.String(), "example-team") {
			t.Errorf("GET /teams/%s: missing team name in body", key)
		}
	}
}

// TestResolveTeam_IDTakesPrecedence verifies that when one team's
// Name happens to match another team's canonical id, the id-keyed
// team wins. Pathological case; documented in resolveTeam's comment.
func TestResolveTeam_IDTakesPrecedence(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	idTeam := newFullTestTeam(t, "id-team")
	idTeam.team.ID = "t-collidewithname"
	idTeam.team.Name = "id-team"
	d.teams[idTeam.team.ID] = idTeam

	nameTeam := newFullTestTeam(t, "other-id")
	nameTeam.team.ID = "other-id"
	// Pathological: this team's Name shadows idTeam's id.
	nameTeam.team.Name = "t-collidewithname"
	d.teams[nameTeam.team.ID] = nameTeam

	if got := d.resolveTeam("t-collidewithname"); got != idTeam {
		t.Errorf("id match must win over name alias: got %p want %p", got, idTeam)
	}
}

// TestTeamDetail_HeroSection seeds a mixed task population + several
// archetypes and asserts the hero band renders:
//   - big hero numerals (active-agents total, open-tasks total),
//   - one chip per archetype declared in the team's roster
//     (alphabetical, including zero-count entries),
//   - one stage-bar segment per stage with ≥ 1 transition today.
func TestTeamDetail_HeroSection(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	// Add an extra archetype so the chip strip exercises sorting +
	// zero-count rendering.
	rt.team.Archetypes = append(rt.team.Archetypes,
		team.ArchetypeSpec{Role: "integrator", Placement: "local", MaxConcurrent: 1},
		team.ArchetypeSpec{Role: "project_manager", Placement: "local", MaxConcurrent: 1},
	)
	d.teams["alpha"] = rt

	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-1", Role: "worker", State: mcpsrv.StateBusy})
	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-2", Role: "worker", State: mcpsrv.StateRunning})
	rt.registry.Add(mcpsrv.AgentEntry{ID: "reviewer-1", Role: "reviewer", State: mcpsrv.StateRunning})

	// Seed today's pipeline: one task per stage, all with
	// StageEnteredAt = now (so they count as "today").
	t1, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "fresh proposal"})
	t2, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "in coding"})
	_, _ = rt.plan.UpdateTask(t2.ID, plan.UpdateInput{Stage: plan.StageCoding})
	t3, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "awaiting"})
	_, _ = rt.plan.UpdateTask(t3.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Hero numerals
	if !strings.Contains(body, `class="hero status-panel"`) && !strings.Contains(body, `class="hero"`) {
		t.Fatalf("hero section not rendered:\n%s", body)
	}
	if !strings.Contains(body, `class="stat big"`) {
		t.Errorf("hero big numerals missing")
	}
	// active-agents total = 3, open-tasks total = 3 (3 open tasks).
	// Markup wraps across lines; check the numeral and label both
	// appear inside the big-stat div.
	bigIdx := strings.Index(body, `class="stat big"`)
	if bigIdx < 0 {
		t.Fatalf("big stat div missing")
	}
	bigChunk := body[bigIdx : bigIdx+300]
	if !strings.Contains(bigChunk, `>3</span>`) || !strings.Contains(bigChunk, "active agents") {
		t.Errorf("expected active-agents total = 3 in hero big chunk:\n%s", bigChunk)
	}

	// Archetype chips, alphabetical: integrator, project_manager,
	// reviewer, worker. integrator + project_manager have zero count.
	for _, role := range []string{"integrator", "project_manager", "reviewer", "worker"} {
		if !strings.Contains(body, role) {
			t.Errorf("chip for archetype %q missing in body", role)
		}
	}
	// Zero chip rendered with the .zero modifier.
	if !strings.Contains(body, `class="chip zero"`) {
		t.Errorf("zero-count chip class missing")
	}

	// Stage bar: at least the three seeded stages must appear as
	// segments (case-insensitive — the template lowercases the stage
	// constant string and uppercases via CSS).
	if !strings.Contains(body, `class="stage-bar"`) {
		t.Fatalf("stage bar not rendered:\n%s", body)
	}
	for _, stage := range []string{"proposed", "coding", "awaiting_approval"} {
		if !strings.Contains(body, `>`+stage+`</span>`) {
			t.Errorf("stage bar missing segment for %q", stage)
		}
	}
	// Tooltip should list task ids for hover-inspection.
	if !strings.Contains(body, `title="awaiting_approval (1): `+t3.ID+`"`) {
		t.Errorf("stage segment title= missing task id:\n%s", body)
	}
	// Color: at least the AWAITING_APPROVAL amber should be in the
	// inline style.
	if !strings.Contains(body, "#f59e0b") {
		t.Errorf("expected amber colour for awaiting_approval segment")
	}
	_ = t1
}

// TestTeamPage_StatusPanel_HasBridgeConsoleLook asserts the chunk-1
// dashboard redesign baseline: the hero gets the `status-panel` modifier,
// renders a breathing amber lamp (`.status-lamp`), and includes a serif
// status-headline pulled from `LeaderStatus.Text`.
func TestTeamPage_StatusPanel_HasBridgeConsoleLook(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	headline := "Cutting the T20 release; reviewing T17 diff"
	_ = rt.leaderStatus.Set("leader", headline, nil)

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="hero status-panel"`,
		`class="status-lamp"`,
		`class="status-headline`,
		headline,
		`team-detail-page`, // body class scoping the new theme
		`Fraunces`,         // font preload reference in <head>
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in status-panel markup:\n%s", want, body)
		}
	}
}

// TestTeamPage_WorkersPanel_PositionedAfterStatus asserts DOM order:
// the workers-panel section sits between the status-panel hero and the
// awaiting-approval section, so the operator's eye lands on "who is
// doing what" before "what needs approval".
func TestTeamPage_WorkersPanel_PositionedAfterStatus(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-uma", Role: "worker", State: mcpsrv.StateBusy})
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Refactor flow"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval, AddEvidence: []string{"j-1"}})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	statusIdx := strings.Index(body, `class="hero status-panel"`)
	workersIdx := strings.Index(body, `class="workers-panel"`)
	approvalIdx := strings.Index(body, `class="decisions-section"`)
	if statusIdx < 0 {
		t.Fatalf("status-panel section missing")
	}
	if workersIdx < 0 {
		t.Fatalf("workers-panel section missing")
	}
	if approvalIdx < 0 {
		t.Fatalf("decisions-section missing despite awaiting-approval task being seeded")
	}
	if workersIdx <= statusIdx {
		t.Errorf("workers-panel at %d should come AFTER status-panel at %d", workersIdx, statusIdx)
	}
	if workersIdx >= approvalIdx {
		t.Errorf("workers-panel at %d should come BEFORE decisions-section at %d", workersIdx, approvalIdx)
	}
}

// TestTeamPage_WorkersPanel_UsesPersonaNames asserts the manifest
// renders agents via team.PersonaName ("worker-uma" → "Coder Uma") and
// tags them with the matching role-colour modifier (.role-tag.coder).
func TestTeamPage_WorkersPanel_UsesPersonaNames(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	rt.registry.Add(mcpsrv.AgentEntry{ID: "worker-uma", Role: "worker", State: mcpsrv.StateBusy})
	rt.registry.Add(mcpsrv.AgentEntry{ID: "reviewer-bex", Role: "reviewer", State: mcpsrv.StateRunning})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Slice the workers section so we don't pick up matches from the
	// existing "Active agents" mini-chip strip lower on the page.
	start := strings.Index(body, `class="workers-panel"`)
	if start < 0 {
		t.Fatalf("workers-panel missing")
	}
	end := strings.Index(body[start:], `</section>`)
	if end < 0 {
		t.Fatalf("workers-panel close tag missing")
	}
	panel := body[start : start+end]

	for _, want := range []string{
		"Coder Uma",
		"Reviewer Bex",
		`class="role-tag coder"`,
		`class="role-tag reviewer"`,
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("missing %q inside workers-panel:\n%s", want, panel)
		}
	}
}

// TestTeamPage_WorkersPanel_ZeroWorkersStillRenders asserts the panel
// still renders without crashing when no agents are active. The "All
// idle" placeholder shows so the operator sees the empty state.
func TestTeamPage_WorkersPanel_ZeroWorkersStillRenders(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="workers-panel"`) {
		t.Errorf("workers-panel missing on empty team:\n%s", body)
	}
	if !strings.Contains(body, "All idle") {
		t.Errorf("expected 'All idle' placeholder for zero-workers team")
	}
}

// TestTeamDetail_HeroEmptyDay confirms the hero's "no activity today"
// placeholder kicks in when no task transitioned today and that the
// chip strip still renders every archetype.
func TestTeamDetail_HeroEmptyDay(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	// No agents, no tasks → 0 hero numerals, empty stage bar.

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "no stage activity today") {
		t.Errorf("expected empty-state line in hero:\n%s", body)
	}
	// Archetypes from the seed (worker + reviewer) still chip in.
	for _, role := range []string{"worker", "reviewer"} {
		if !strings.Contains(body, role) {
			t.Errorf("chip for archetype %q missing from empty-day hero", role)
		}
	}
}

// --- Decisions panel tests (t-40aae1ad) ---

// putTaskInBlocked moves a fresh task into stage=blocked via the
// plan store so blocker-row tests can render against a real task.
func putTaskInBlocked(t *testing.T, rt *registeredTeam, title string) plan.Task {
	t.Helper()
	task, err := rt.plan.AddTask(plan.NewTaskInput{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:  plan.StageBlocked,
		Status: plan.StatusBlocked,
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestDecisionsList_AwaitingApproval(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Sign off on plan"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="decisions-section"`,
		`class="decision-stripe approval"`,
		`background:#ffb347`,
		`class="decision-type approval">APPROVAL`,
		`APPROVE`,
		`REJECT`,
		`COMMENT`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestDecisionsList_DecisionWithQuestionSeverity(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "What pattern should we use?"})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "worker-uma",
		Kind:      audit.KindDecisionNote,
		Message:   "Should we use sync.Map or a regular map+mutex here?",
		Meta:      map[string]any{"task_id": task.ID, "severity": "question"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="decision-stripe question"`,
		`background:#7dd3fc`,
		`class="decision-type question">QUESTION`,
		`Should we use sync.Map`,
		`REPLY`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Info-severity decision notes must NOT show up as a QUESTION row.
	infoTask, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Recorded decision"})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "worker-uma",
		Kind:      audit.KindDecisionNote,
		Message:   "Went with sync.Map after benchmarking.",
		Meta:      map[string]any{"task_id": infoTask.ID, "severity": "info"},
	})
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req)
	if strings.Contains(decisionsSection(t, w2.Body.String()), "Went with sync.Map after benchmarking") {
		t.Errorf("info-severity decision should not appear in decisions panel")
	}
}

// decisionsSection slices the rendered HTML down to the
// .decisions-section block so panel-only assertions (e.g. "this text
// must NOT appear here") aren't fooled by matches in the recent-
// activity audit stream lower on the page.
func decisionsSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `class="decisions-section"`)
	if start < 0 {
		return ""
	}
	rest := body[start:]
	end := strings.Index(rest, `</section>`)
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func TestDecisionsList_OpenBlocker(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task := putTaskInBlocked(t, rt, "Migration is stuck")
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "worker-uma",
		Kind:      audit.KindBlockerNote,
		Message:   "Waiting on schema confirmation from product",
		Meta:      map[string]any{"task_id": task.ID},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="decision-stripe blocker"`,
		`background:#a78bfa`,
		`class="decision-type blocker">BLOCKER`,
		`Waiting on schema confirmation`,
		`UNBLOCK`,
		`/teams/alpha/decisions/` + task.ID + `/unblock`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Once the operator unblocks (stage leaves blocked), the row drops.
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageProposed})
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req)
	if strings.Contains(decisionsSection(t, w2.Body.String()), "Waiting on schema confirmation") {
		t.Errorf("blocker row should drop once task leaves stage=blocked")
	}
}

func TestDecisionsList_SortedNewestFirst(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	now := time.Now().UTC()
	// Oldest: an approval (StageEnteredAt now-10m).
	apprTask, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Older approval"})
	_, _ = rt.plan.UpdateTask(apprTask.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(apprTask.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})
	// Override the stage-entered timestamp by writing a backdated event
	// for sort comparison via timestamp-only signal. Approval rows use
	// the task's StageEnteredAt which we cannot easily override, so the
	// approval row's age will be ~"just now". Question + blocker we can
	// time-stamp explicitly to verify the mixed sort.
	qTask, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Middle question"})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-5 * time.Minute),
		AgentID:   "worker-uma",
		Kind:      audit.KindDecisionNote,
		Message:   "MIDDLE_QUESTION_MARKER",
		Meta:      map[string]any{"task_id": qTask.ID, "severity": "question"},
	})
	bTask := putTaskInBlocked(t, rt, "Newest blocker")
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: now.Add(-1 * time.Minute),
		AgentID:   "worker-uma",
		Kind:      audit.KindBlockerNote,
		Message:   "NEWEST_BLOCKER_MARKER",
		Meta:      map[string]any{"task_id": bTask.ID},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()

	// All three should appear; order: BLOCKER (1m), APPROVAL (now-ish),
	// QUESTION (5m). The APPROVAL StageEnteredAt is set by UpdateTask
	// just above, so it's the most recent of all three. Therefore the
	// expected order is APPROVAL > BLOCKER > QUESTION (newest first).
	apprIdx := strings.Index(body, "Older approval")
	blockerIdx := strings.Index(body, "NEWEST_BLOCKER_MARKER")
	questionIdx := strings.Index(body, "MIDDLE_QUESTION_MARKER")
	if apprIdx < 0 || blockerIdx < 0 || questionIdx < 0 {
		t.Fatalf("missing one of the three decisions: appr=%d blocker=%d question=%d", apprIdx, blockerIdx, questionIdx)
	}
	if !(apprIdx < blockerIdx && blockerIdx < questionIdx) {
		t.Errorf("expected APPROVAL < BLOCKER < QUESTION (by position); got appr=%d blocker=%d question=%d",
			apprIdx, blockerIdx, questionIdx)
	}
}

func TestDecisionsList_QuestionDismissedAfterReply(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "What pattern?"})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC().Add(-2 * time.Minute),
		AgentID:   "worker-uma",
		Kind:      audit.KindDecisionNote,
		Message:   "QUESTION_BODY_MARKER",
		Meta:      map[string]any{"task_id": task.ID, "severity": "question"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if !strings.Contains(decisionsSection(t, w.Body.String()), "QUESTION_BODY_MARKER") {
		t.Fatalf("expected QUESTION row present before reply")
	}

	// Operator REPLY lands as a newer decision_note (any severity).
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "operator",
		Kind:      audit.KindDecisionNote,
		Message:   "Use sync.Map.",
		Meta:      map[string]any{"task_id": task.ID, "severity": "info"},
	})

	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req)
	if strings.Contains(decisionsSection(t, w2.Body.String()), "QUESTION_BODY_MARKER") {
		t.Errorf("QUESTION row should be dismissed after operator REPLY lands")
	}
}

func TestDecisionsList_UnansweredQuestionStillRenders(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Still open"})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "worker-uma",
		Kind:      audit.KindDecisionNote,
		Message:   "UNANSWERED_QUESTION_MARKER",
		Meta:      map[string]any{"task_id": task.ID, "severity": "question"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if !strings.Contains(decisionsSection(t, w.Body.String()), "UNANSWERED_QUESTION_MARKER") {
		t.Errorf("unanswered QUESTION row should still render")
	}
}

func TestDecisionsList_ReplyForDifferentTask_DoesNotDismiss(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	taskA, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Task A"})
	taskB, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Task B"})
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC().Add(-2 * time.Minute),
		AgentID:   "worker-uma",
		Kind:      audit.KindDecisionNote,
		Message:   "TASK_A_QUESTION_MARKER",
		Meta:      map[string]any{"task_id": taskA.ID, "severity": "question"},
	})
	// A reply, but for task B — must not dismiss task A's question.
	_ = rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "operator",
		Kind:      audit.KindDecisionNote,
		Message:   "Reply for B.",
		Meta:      map[string]any{"task_id": taskB.ID, "severity": "info"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if !strings.Contains(decisionsSection(t, w.Body.String()), "TASK_A_QUESTION_MARKER") {
		t.Errorf("task A's question should still render — reply was for task B")
	}
}

func TestDecisionsList_EmptyState(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="decisions-section"`) {
		t.Fatalf("decisions-section missing on empty state")
	}
	if !strings.Contains(body, "All clear.") {
		t.Errorf("empty-state placeholder 'All clear.' missing")
	}
}

func TestDecisionsList_PreservesApprovalActions(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Preserve approve route"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`/teams/alpha/tasks/` + task.ID + `/approve`,
		`/teams/alpha/tasks/` + task.ID + `/reject`,
		`/teams/alpha/tasks/` + task.ID + `/comment`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("approval form URL %q missing — preservation regression for t-dfb9554b", want)
		}
	}
}
