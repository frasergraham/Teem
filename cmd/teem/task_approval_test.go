package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Awaiting approval",
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
// the sessionStorage-based expand-state preservation: the awaiting-
// approval brief <details> must carry a stable id matching
// details-task-<taskid>-notes, and the team-detail page must include
// the inline restore/persist script that reads/writes
// sessionStorage('expanded:<id>'). Together those keep an operator's
// expanded brief expanded across the 10s auto-refresh.
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
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
	if !strings.Contains(body, "sessionStorage.getItem('expanded:'") {
		t.Errorf("team page missing sessionStorage restore script; body=%s", body)
	}
	if !strings.Contains(body, "sessionStorage.setItem('expanded:'") {
		t.Errorf("team page missing sessionStorage persist script; body=%s", body)
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

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
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
