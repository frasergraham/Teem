package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// putTaskInAwaiting moves a fresh task through proposed → specced →
// awaiting_approval so tests can exercise the decide paths.
func putTaskInAwaiting(t *testing.T, rt *registeredTeam, title string) plan.Task {
	t.Helper()
	task, err := rt.plan.AddTask(plan.NewTaskInput{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced}); err != nil {
		t.Fatal(err)
	}
	updated, err := rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestDecideTask_ApproveMovesToBuilding(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	task := putTaskInAwaiting(t, rt, "approve me")

	got, err := decideTask(rt, task.ID, decisionApprove, "ship it")
	if err != nil {
		t.Fatalf("decideTask: %v", err)
	}
	if got.Stage != plan.StageCoding {
		t.Errorf("stage after approve = %q want building", got.Stage)
	}
	if got.Status != plan.StatusInProgress {
		t.Errorf("status after approve = %q want in_progress", got.Status)
	}
	if !strings.Contains(got.Notes, "APPROVED") || !strings.Contains(got.Notes, "ship it") {
		t.Errorf("approve notes missing decision/comment: %q", got.Notes)
	}
	// Audit captured the decision.
	events, _ := rt.auditSink.Query("", got.UpdatedAt.Add(-1), 10)
	found := false
	for _, e := range events {
		if e.Kind == audit.KindDecisionNote && e.Meta["task_id"] == task.ID && e.Meta["decision"] == "approve" {
			found = true
			break
		}
	}
	if !found {
		t.Error("approve did not write a decision_note audit event")
	}
}

func TestDecideTask_RejectMovesToShelved(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	task := putTaskInAwaiting(t, rt, "reject me")

	got, err := decideTask(rt, task.ID, decisionReject, "off scope")
	if err != nil {
		t.Fatalf("decideTask: %v", err)
	}
	if got.Stage != plan.StageShelved {
		t.Errorf("stage after reject = %q want shelved", got.Stage)
	}
	if got.Status != plan.StatusShelved {
		t.Errorf("status after reject = %q want shelved", got.Status)
	}
	if !strings.Contains(got.Notes, "REJECTED") {
		t.Errorf("reject notes missing marker: %q", got.Notes)
	}
}

func TestDecideTask_CommentStaysInPlace(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	task := putTaskInAwaiting(t, rt, "comment me")

	got, err := decideTask(rt, task.ID, decisionComment, "have you considered X?")
	if err != nil {
		t.Fatalf("decideTask: %v", err)
	}
	if got.Stage != plan.StageAwaitingApproval {
		t.Errorf("stage after comment = %q want awaiting_approval", got.Stage)
	}
	if !strings.Contains(got.Notes, "COMMENT") || !strings.Contains(got.Notes, "have you considered X?") {
		t.Errorf("comment notes missing marker: %q", got.Notes)
	}
}

func TestDecideTask_WrongStageIs409(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "still proposed"})
	if _, err := decideTask(rt, task.ID, decisionApprove, ""); err != errNotAwaitingApproval {
		t.Errorf("want errNotAwaitingApproval, got %v", err)
	}
}

func TestDecideTask_UnknownTaskIsErr(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	if _, err := decideTask(rt, "t-nope", decisionApprove, ""); err != plan.ErrTaskNotFound {
		t.Errorf("want ErrTaskNotFound, got %v", err)
	}
}

// --- HTTP routing tests ---

func TestControlTaskAction_Approve_JSON(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task := putTaskInAwaiting(t, rt, "approve via API")

	body, _ := json.Marshal(map[string]string{"comment": "lgtm"})
	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/alpha/tasks/"+task.ID+"/approve",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got plan.Task
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v / %s", err, w.Body.String())
	}
	if got.Stage != plan.StageCoding {
		t.Errorf("stage in response = %q want building", got.Stage)
	}
}

func TestControlTaskAction_Reject_RequiresReason(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task := putTaskInAwaiting(t, rt, "reject empty")

	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/alpha/tasks/"+task.ID+"/reject",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code=%d want 400 (missing reason) body=%s", w.Code, w.Body.String())
	}
}

func TestControlTaskAction_NotInAwaitingIs409(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "still proposed"})

	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/alpha/tasks/"+task.ID+"/approve",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("code=%d want 409 (not in awaiting_approval) body=%s", w.Code, w.Body.String())
	}
}

func TestControlTaskAction_UnknownTeamIs404(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/no-such-team/tasks/t-xxx/approve",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", w.Code)
	}
}

func TestControlTaskAction_RequiresAuth(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task := putTaskInAwaiting(t, rt, "auth check")

	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/alpha/tasks/"+task.ID+"/approve",
		bytes.NewReader([]byte(`{}`)))
	// no auth header
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", w.Code)
	}
}

func TestTaskActionForm_RedirectsWithFlash(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task := putTaskInAwaiting(t, rt, "form approve")

	form := url.Values{}
	form.Set("comment", "lgtm via form")
	req := httptest.NewRequest(http.MethodPost,
		"/teams/alpha/tasks/"+task.ID+"/approve",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "flash=task_approved") {
		t.Errorf("redirect location missing flash: %q", loc)
	}
	got, _ := rt.plan.Get(task.ID)
	if got.Stage != plan.StageCoding {
		t.Errorf("post-redirect stage = %q want building", got.Stage)
	}
}

// --- Dashboard rendering tests ---

func TestDashboard_RendersAwaitingApprovalSection(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{
		Title: "Review the PM doc",
		Notes: "Please skim docs/project-manager.md and confirm scope.",
	})
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
		"Decisions",
		"APPROVAL",
		"Review the PM doc",
		"APPROVE",
		"REJECT",
		"COMMENT",
		"/teams/alpha/tasks/" + task.ID + "/approve",
		"/teams/alpha/tasks/" + task.ID + "/reject",
		"/teams/alpha/tasks/" + task.ID + "/comment",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestDashboard_AwaitingApprovalDetailsPersistsAcrossReload locks in
// the per-details expand-state preservation: the awaiting-approval
// brief <details> must carry a stable id matching
// details-task-<taskid>-notes, and the team-detail page must include
// the inline restore/persist script that reads/writes
// localStorage('teem.ui.collapse.<id>'). Together those keep an
// operator's expanded brief expanded across the 10s auto-refresh and
// across tab close (post t-b83b9936 migration from sessionStorage).
func TestDashboard_AwaitingApprovalDetailsPersistsAcrossReload(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Notes must exceed the 200-char preview threshold so the renderer
	// emits the collapsible <details> path (not the plain preview div).
	long := strings.Repeat("please skim and confirm scope. ", 20)
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Review the PM doc", Notes: long})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()

	wantID := `id="details-task-` + task.ID + `-notes"`
	if !strings.Contains(body, wantID) {
		t.Errorf("awaiting-approval <details> missing stable id %q; body=%s", wantID, body)
	}
	if !strings.Contains(body, "localStorage.getItem(key)") {
		t.Errorf("team page missing localStorage restore script; body=%s", body)
	}
	if !strings.Contains(body, "localStorage.setItem(key,") {
		t.Errorf("team page missing localStorage persist script; body=%s", body)
	}
	if !strings.Contains(body, "'teem.ui.collapse.'") {
		t.Errorf("team page missing collapse key prefix; body=%s", body)
	}
}

func TestDashboard_SummaryShowsAwaitingApprovalCount(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	for _, title := range []string{"A", "B"} {
		task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: title})
		_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
		_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "2 awaiting approval") {
		t.Errorf("summary tile missing awaiting-approval count: body=%s", body)
	}
}

// TestControlTaskAction_ConcurrentApproveOneWinsOne409 fires two
// approvals at the same awaiting_approval task simultaneously. Exactly
// one must succeed (200, stage=building); the other must lose the
// race and get 409. Without UpdateTaskIfStage both writers would pass
// the read-then-write check and the second would clobber the first's
// notes.
func TestControlTaskAction_ConcurrentApproveOneWinsOne409(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task := putTaskInAwaiting(t, rt, "race for approval")

	const N = 2
	var ok, conflict int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"comment": "lgtm"})
			req := httptest.NewRequest(http.MethodPost,
				"/control/teams/alpha/tasks/"+task.ID+"/approve",
				bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			d.handler().ServeHTTP(w, req)
			switch w.Code {
			case http.StatusOK:
				atomic.AddInt32(&ok, 1)
			case http.StatusConflict:
				atomic.AddInt32(&conflict, 1)
			default:
				t.Errorf("racer %d unexpected code %d body=%s", i, w.Code, w.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if ok != 1 || conflict != 1 {
		t.Fatalf("want 1 OK + 1 409, got ok=%d conflict=%d", ok, conflict)
	}
	got, _ := rt.plan.Get(task.ID)
	if got.Stage != plan.StageCoding {
		t.Errorf("post-race stage = %q want building", got.Stage)
	}
	// The winner's notes line should be intact (one APPROVED entry, not
	// truncated). The loser must not have appended anything.
	if c := strings.Count(got.Notes, "APPROVED"); c != 1 {
		t.Errorf("notes should contain exactly one APPROVED line, got %d (notes=%q)", c, got.Notes)
	}
}

// TestControlTaskAction_ConcurrentCommentsBothLand fires two COMMENT
// actions at the same awaiting_approval task simultaneously. Both must
// succeed (200, stage stays awaiting_approval), and both comment bodies
// must appear in the final notes. Without the read-modify-write under
// the plan lock the second writer would clobber the first's appended
// COMMENT line — the lost-update race that t-dfb9554b round 2 missed
// (UpdateTaskIfStage caught APPROVE/REJECT because those change stage,
// but COMMENT stays in awaiting_approval so both writers pass).
func TestControlTaskAction_ConcurrentCommentsBothLand(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task := putTaskInAwaiting(t, rt, "race for comment")

	const (
		bodyA = "first concurrent comment — unique-marker-aaa"
		bodyB = "second concurrent comment — unique-marker-bbb"
	)
	comments := []string{bodyA, bodyB}
	var ok int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, c := range comments {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"comment": c})
			req := httptest.NewRequest(http.MethodPost,
				"/control/teams/alpha/tasks/"+task.ID+"/comment",
				bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			d.handler().ServeHTTP(w, req)
			if w.Code == http.StatusOK {
				atomic.AddInt32(&ok, 1)
			} else {
				t.Errorf("comment %q got code=%d body=%s", c, w.Code, w.Body.String())
			}
		}(c)
	}
	close(start)
	wg.Wait()

	if ok != 2 {
		t.Fatalf("want 2 OK comments, got %d", ok)
	}
	got, _ := rt.plan.Get(task.ID)
	if got.Stage != plan.StageAwaitingApproval {
		t.Errorf("post-comment stage = %q want awaiting_approval", got.Stage)
	}
	if !strings.Contains(got.Notes, "unique-marker-aaa") {
		t.Errorf("notes missing first comment: %q", got.Notes)
	}
	if !strings.Contains(got.Notes, "unique-marker-bbb") {
		t.Errorf("notes missing second comment: %q", got.Notes)
	}
	if c := strings.Count(got.Notes, "COMMENT"); c != 2 {
		t.Errorf("notes should contain exactly two COMMENT lines, got %d (notes=%q)", c, got.Notes)
	}
}

// TestDashboard_AwaitingApprovalSortedNewestFirst asserts the
// awaiting-approval section orders rows by StageEnteredAt descending,
// not by random task id.
func TestDashboard_AwaitingApprovalSortedNewestFirst(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Create three tasks and walk each through specced →
	// awaiting_approval, with a sleep between so StageEnteredAt
	// strictly orders them. Titles are letter-tagged so we can find
	// them in the rendered HTML regardless of (random) id ordering.
	titles := []string{"first-into-awaiting", "second-into-awaiting", "third-into-awaiting"}
	for _, title := range titles {
		task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: title})
		_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
		_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})
		// Time.Now is millisecond-resolution on macOS; sleep a hair so
		// StageEnteredAt strictly increases between tasks.
		time.Sleep(2 * time.Millisecond)
	}

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	// Each title must appear in the rendered HTML; the indices in the
	// body string then tell us the render order.
	idx := make([]int, len(titles))
	for i, title := range titles {
		idx[i] = strings.Index(body, title)
		if idx[i] < 0 {
			t.Fatalf("title %q missing from rendered body", title)
		}
	}
	// Newest-first means the *last* created task's title appears first
	// in the body. So we want idx[2] < idx[1] < idx[0].
	if !(idx[2] < idx[1] && idx[1] < idx[0]) {
		t.Errorf("awaiting-approval render order wrong; want newest-first (titles index: third<second<first), got %v", idx)
	}
}

// TestAwaitingApprovalCard_PlanShapedBranch wires an awaiting-approval
// task to a worker whose branch touches only docs/foo.md. The card
// must surface the "Plan artifact" header, list the docs/foo.md path,
// and render the worker + branch link. The deep-link branch URL
// goes to the per-agent jobs page (the closest existing branch view).
func TestAwaitingApprovalCard_PlanShapedBranch(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.repoRoot = seedRepoWithDocsBranch(t, "worker-una", "docs/foo.md")
	d.teams["alpha"] = rt

	// Seed an audit event linking job j1 → worker-una.
	writeAuditJobReceived(t, rt, "worker-una", "j1", "go do the thing")

	task, _ := rt.plan.AddTask(plan.NewTaskInput{
		Title: "Review the PM doc",
		Notes: "Please skim docs/foo.md and confirm scope.",
	})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:       plan.StageAwaitingApproval,
		AddEvidence: []string{"j1"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Plan artifact",
		"docs/foo.md",
		"worker-una",
		"teem/worker-una",
		`href="/teams/alpha/agents/worker-una/jobs"`,
		`href="/teams/alpha/jobs/j1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan-shaped card missing %q in body", want)
		}
	}
}

// TestAwaitingApprovalCard_MixedBranch asserts that when the worker's
// branch touches Go files alongside (or instead of) docs, the card
// still shows the worker + branch + job links but does NOT claim the
// task is plan-shaped (no "Plan artifact" header).
func TestAwaitingApprovalCard_MixedBranch(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.repoRoot = seedRepoWithMixedBranch(t, "worker-pax")
	d.teams["alpha"] = rt

	writeAuditJobReceived(t, rt, "worker-pax", "j2", "ship the feature")

	task, _ := rt.plan.AddTask(plan.NewTaskInput{
		Title: "Review the feature",
		Notes: "Code change — confirm shape before merge.",
	})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:       plan.StageAwaitingApproval,
		AddEvidence: []string{"j2"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	// Worker / branch / job links present.
	for _, want := range []string{
		"worker-pax",
		"teem/worker-pax",
		`href="/teams/alpha/agents/worker-pax/jobs"`,
		`href="/teams/alpha/jobs/j2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mixed-branch card missing %q in body", want)
		}
	}
	// Mixed branch is NOT plan-shaped; the "Plan artifact" header must
	// not appear for this card. The header is wrapped in its own div so
	// substring search is enough — it's not used elsewhere on the page.
	if strings.Contains(body, "Plan artifact") {
		t.Errorf("mixed-branch card should not claim plan-shaped; body contained 'Plan artifact'")
	}
}

// TestAwaitingApprovalCard_BriefDeEmphasized asserts the leader brief
// stays accessible behind a stable-id <details> (so the sessionStorage
// preserve-open-state script still finds it) AND is rendered with the
// de-emphasized "brief-deemph" class so it visually retreats behind
// the work-product block above it.
func TestAwaitingApprovalCard_BriefDeEmphasized(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, _ := rt.plan.AddTask(plan.NewTaskInput{
		Title: "Operator-action task",
		Notes: "this is the leader brief — secondary context",
	})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAwaitingApproval})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()

	wantID := `id="details-task-` + task.ID + `-notes"`
	if !strings.Contains(body, wantID) {
		t.Errorf("brief <details> missing stable id %q", wantID)
	}
	if !strings.Contains(body, `class="brief-deemph"`) {
		t.Errorf("brief <details> missing de-emphasized class; body=%s", body)
	}
	if !strings.Contains(body, "Brief from leader") {
		t.Errorf("brief summary label missing")
	}
	if !strings.Contains(body, "this is the leader brief") {
		t.Errorf("brief body content missing")
	}
}

// seedRepoWithDocsBranch builds a temp git repo whose main branch
// holds an initial empty commit, and creates teem/<agentID> with a
// single commit that adds docFile. The repo path is returned for
// rt.repoRoot.
func seedRepoWithDocsBranch(t *testing.T, agentID, docFile string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--initial-branch=main", ".")
	runGit(t, dir, "commit", "--allow-empty", "-m", "initial commit on main")
	runGit(t, dir, "checkout", "-b", "teem/"+agentID)
	// Create the doc file inside the worktree, then commit.
	full := filepath.Join(dir, docFile)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("# the design doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", docFile)
	runGit(t, dir, "commit", "-m", agentID+": add "+docFile)
	runGit(t, dir, "checkout", "main")
	return dir
}

// seedRepoWithMixedBranch builds a temp git repo with a teem/<agentID>
// branch that touches BOTH a Go file and a markdown file — i.e. not
// plan-shaped (code + doc). isPlanShaped requires every file to be
// docs/**/*.md so a single .go file disqualifies the branch.
func seedRepoWithMixedBranch(t *testing.T, agentID string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--initial-branch=main", ".")
	runGit(t, dir, "commit", "--allow-empty", "-m", "initial commit on main")
	runGit(t, dir, "checkout", "-b", "teem/"+agentID)
	files := map[string]string{
		"docs/note.md": "# note\n",
		"main.go":      "package main\n\nfunc main() {}\n",
	}
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", path)
	}
	runGit(t, dir, "commit", "-m", agentID+": mixed code + doc")
	runGit(t, dir, "checkout", "main")
	return dir
}

// writeAuditJobReceived stamps the minimum audit-event row needed for
// resolveEvidenceRows to map jobID → agentID: a single job_received
// event with the agent id populated.
func writeAuditJobReceived(t *testing.T, rt *registeredTeam, agentID, jobID, prompt string) {
	t.Helper()
	err := rt.auditSink.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   agentID,
		JobID:     jobID,
		Kind:      audit.KindJobReceived,
		Message:   prompt,
		Meta:      map[string]any{"prompt": prompt},
	})
	if err != nil {
		t.Fatalf("audit write: %v", err)
	}
}

// seedRepoWithDocsBranchContent is like seedRepoWithDocsBranch but
// lets the caller pick the markdown content — used by the inline-
// render tests to seed `# Heading\n\nbody\n`, an oversize file for
// truncation, and a hostile `<script>` payload for the
// no-unsafe-HTML check.
func seedRepoWithDocsBranchContent(t *testing.T, agentID, docFile, content string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--initial-branch=main", ".")
	runGit(t, dir, "commit", "--allow-empty", "-m", "initial commit on main")
	runGit(t, dir, "checkout", "-b", "teem/"+agentID)
	full := filepath.Join(dir, docFile)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", docFile)
	runGit(t, dir, "commit", "-m", agentID+": add "+docFile)
	runGit(t, dir, "checkout", "main")
	return dir
}

// TestPlanArtifact_RenderMarkdownInline_DocsBranch asserts that an
// awaiting-approval card whose evidence-branch carries a markdown
// plan doc renders that doc's HTML inline (not just the file path).
// The stable <details id="details-task-...-plan-..."> wrapper must
// be present so the sessionStorage script can persist its expanded
// state across the 10s auto-refresh.
func TestPlanArtifact_RenderMarkdownInline_DocsBranch(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	rt.repoRoot = seedRepoWithDocsBranchContent(t, "worker-una", "docs/foo.md",
		"# Heading\n\nbody\n")
	d.teams["alpha"] = rt

	writeAuditJobReceived(t, rt, "worker-una", "j1", "go do the thing")

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Review the plan"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:       plan.StageAwaitingApproval,
		AddEvidence: []string{"j1"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, "<h1>Heading</h1>") {
		t.Errorf("rendered HTML missing <h1>Heading</h1>; body=%s", body)
	}
	wantID := `id="details-task-` + task.ID + `-plan-docs-foo-md"`
	if !strings.Contains(body, wantID) {
		t.Errorf("plan-artifact <details> missing stable id %q", wantID)
	}
	// The raw markdown source must not leak through outside of the
	// rendered HTML — the literal "# Heading" line should not appear.
	if strings.Contains(body, "# Heading") {
		t.Errorf("raw markdown leaked into rendered body; got %q", body)
	}
}

// TestPlanArtifact_RenderMarkdown_TruncateLargeFile seeds a >50KB
// markdown file on the worker branch and asserts the dashboard
// surfaces a "(truncated)" indicator in the summary and clips the
// rendered body at the cap.
func TestPlanArtifact_RenderMarkdown_TruncateLargeFile(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	// 60KB of body content; well over the 50KB planFileSizeCap. The
	// content is mostly the unique marker "BLOATBLOATBLOAT" so the
	// truncation note's "more bytes" hint stays accurate.
	huge := "# Big doc\n\n" + strings.Repeat("BLOATBLOATBLOAT\n", 4000)
	rt.repoRoot = seedRepoWithDocsBranchContent(t, "worker-big", "docs/big.md", huge)
	d.teams["alpha"] = rt

	writeAuditJobReceived(t, rt, "worker-big", "jbig", "ship the big doc")

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Review the big doc"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:       plan.StageAwaitingApproval,
		AddEvidence: []string{"jbig"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, "(truncated)") {
		t.Errorf("summary missing (truncated) marker; body=%s", body)
	}
	if !strings.Contains(body, "[truncated") {
		t.Errorf("rendered body missing inline truncation note")
	}
	// Sanity: the rendered HTML for this card should be smaller than
	// the source (~60KB markdown shouldn't survive intact). Worst case
	// the response carries up to planFileSizeCap of source ≈ 50KB.
	if len(body) > 200*1024 {
		t.Errorf("dashboard body too large (%d bytes) — file was not clipped", len(body))
	}
}

// TestPlanArtifact_RenderMarkdown_NoUnsafeHTML asserts that goldmark
// is configured with WithUnsafe=false: a literal <script> tag in the
// markdown source must be escaped, not passed through to the
// dashboard HTML. The markdown comes from worker-controlled branches
// so trusting raw HTML would be an XSS hole on the unauthenticated
// localhost dashboard.
func TestPlanArtifact_RenderMarkdown_NoUnsafeHTML(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	hostile := "# Plan\n\n<script>alert(1)</script>\n\nbody\n"
	rt.repoRoot = seedRepoWithDocsBranchContent(t, "worker-evil", "docs/evil.md", hostile)
	d.teams["alpha"] = rt

	writeAuditJobReceived(t, rt, "worker-evil", "jevil", "hostile payload")

	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "Review the evil doc"})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageSpecced})
	_, _ = rt.plan.UpdateTask(task.ID, plan.UpdateInput{
		Stage:       plan.StageAwaitingApproval,
		AddEvidence: []string{"jevil"},
	})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("raw <script> leaked through goldmark; body=%s", body)
	}
	// Goldmark with WithUnsafe=false omits raw HTML blocks entirely
	// and substitutes a comment marker — the marker's presence is the
	// positive signal that the unsafe-HTML guard fired.
	if !strings.Contains(body, "raw HTML omitted") {
		t.Errorf("expected goldmark's raw-HTML-omitted marker in output; body=%s", body)
	}
	// The literal payload string must not appear anywhere in the
	// rendered output, escaped or not — goldmark drops the tag, so
	// "alert(1)" should not survive.
	if strings.Contains(body, "alert(1)") {
		t.Errorf("script payload leaked through; body=%s", body)
	}
}

// TestTaskActionForm_RejectFromCommentField asserts the dashboard form
// can reject a task using the single `comment` input — the form has
// only one text field that doubles as the optional approve comment AND
// the required reject reason. The action ends in stage=shelved with
// the reason captured in notes.
func TestTaskActionForm_RejectFromCommentField(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task := putTaskInAwaiting(t, rt, "form reject")

	form := url.Values{}
	form.Set("comment", "wrong direction, redo")
	req := httptest.NewRequest(http.MethodPost,
		"/teams/alpha/tasks/"+task.ID+"/reject",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := rt.plan.Get(task.ID)
	if got.Stage != plan.StageShelved {
		t.Errorf("stage = %q want shelved", got.Stage)
	}
	if !strings.Contains(got.Notes, "REJECTED") {
		t.Errorf("notes missing REJECTED marker: %q", got.Notes)
	}
	if !strings.Contains(got.Notes, "wrong direction, redo") {
		t.Errorf("reject reason from comment field not captured in notes: %q", got.Notes)
	}
}
