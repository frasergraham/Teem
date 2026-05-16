package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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

// --- Task "ready" endpoint -------------------------------------------

// TestControlTaskReady_FlipsProposedToReady covers the happy path: a
// POST against a proposed task transitions it to `ready` and the JSON
// response carries the updated task.
func TestControlTaskReady_FlipsProposedToReady(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task, err := rt.plan.AddTask(plan.NewTaskInput{Title: "ready me"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/alpha/tasks/"+task.ID+"/ready",
		bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got plan.Task
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v / %s", err, w.Body.String())
	}
	if got.Stage != plan.StageReady {
		t.Errorf("stage = %q want ready", got.Stage)
	}
	stored, _ := rt.plan.Get(task.ID)
	if stored.Stage != plan.StageReady {
		t.Errorf("stored stage = %q want ready", stored.Stage)
	}
	t.Logf("smoke: POST /tasks/%s/ready → %d %s", task.ID, w.Code, w.Body.String())
}

// TestControlTaskReady_IsIdempotent re-posts against a task that's
// already in `ready`. The second call must still return 200 + the
// task (no body change, no error) so a double-click stays a no-op.
func TestControlTaskReady_IsIdempotent(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "double-click"})

	url := "/control/teams/alpha/tasks/" + task.ID + "/ready"
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: code=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	stored, _ := rt.plan.Get(task.ID)
	if stored.Stage != plan.StageReady {
		t.Errorf("stored stage = %q want ready", stored.Stage)
	}
}

// TestControlTaskReady_TerminalIs409 covers the only two stages the
// endpoint refuses outright: verified and abandoned. Both must return
// 409 (operator can't dispatch something that's done or won't-do).
func TestControlTaskReady_TerminalIs409(t *testing.T) {
	for _, terminal := range []plan.Stage{plan.StageVerified, plan.StageAbandoned} {
		t.Run(string(terminal), func(t *testing.T) {
			d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
			rt := newFullTestTeam(t, "alpha")
			d.teams["alpha"] = rt
			task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "terminal"})
			// Walk the matrix into the terminal stage.
			switch terminal {
			case plan.StageVerified:
				for _, s := range []plan.Stage{plan.StageCoding, plan.StageReviewing, plan.StageIntegrating, plan.StageVerified} {
					if _, err := rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: s}); err != nil {
						t.Fatalf("walk to %q: %v", s, err)
					}
				}
			case plan.StageAbandoned:
				if _, err := rt.plan.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageAbandoned}); err != nil {
					t.Fatalf("walk to abandoned: %v", err)
				}
			}
			req := httptest.NewRequest(http.MethodPost,
				"/control/teams/alpha/tasks/"+task.ID+"/ready",
				bytes.NewReader([]byte(`{}`)))
			w := httptest.NewRecorder()
			d.handler().ServeHTTP(w, req)
			if w.Code != http.StatusConflict {
				t.Errorf("code=%d want 409 (%s) body=%s", w.Code, terminal, w.Body.String())
			}
			stored, _ := rt.plan.Get(task.ID)
			if stored.Stage != terminal {
				t.Errorf("stage moved despite 409: %q != %q", stored.Stage, terminal)
			}
		})
	}
}

// TestControlTaskReady_UnauthOK locks in the tailnet-boundary auth
// model: the endpoint must accept a POST with no Authorization header.
// The dashboard's SPA fetch can't carry the bearer token, so this gate
// is the load-bearing one (paired with isDashboardTaskReadyAction's
// suffix check).
func TestControlTaskReady_UnauthOK(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	task, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "no auth"})

	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/alpha/tasks/"+task.ID+"/ready",
		bytes.NewReader([]byte(`{}`)))
	// no Authorization header
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("unauth POST got code=%d want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestControlTaskReady_UnknownTaskIs404 — sanity check on the not-found path.
func TestControlTaskReady_UnknownTaskIs404(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, token: "test-token"}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodPost,
		"/control/teams/alpha/tasks/t-does-not-exist/ready",
		bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", w.Code)
	}
}

// seedRepoWithDocsBranchContent builds a temp git repo with a
// teem/<agentID> branch holding the supplied markdown file. Used by
// the renderBranchMarkdown unit tests (security + truncation guards).
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

// TestRenderBranchMarkdown_NoUnsafeHTML asserts goldmark is configured
// with WithUnsafe=false so worker-controlled <script> tags never reach
// the rendered output. The SPA uses dangerouslySetInnerHTML on this
// field, so this guard is the only thing keeping a hostile plan-doc
// branch from becoming an XSS hole.
func TestRenderBranchMarkdown_NoUnsafeHTML(t *testing.T) {
	repo := seedRepoWithDocsBranchContent(t, "worker-evil", "docs/evil.md",
		"# Plan\n\n<script>alert(1)</script>\n\nbody\n")
	rendered, _, ok := renderBranchMarkdown(repo, "teem/worker-evil", "docs/evil.md")
	if !ok {
		t.Fatalf("renderBranchMarkdown returned ok=false")
	}
	if strings.Contains(rendered, "<script>alert(1)</script>") {
		t.Errorf("raw <script> leaked through goldmark; rendered=%s", rendered)
	}
	if strings.Contains(rendered, "alert(1)") {
		t.Errorf("script payload leaked through; rendered=%s", rendered)
	}
	if !strings.Contains(rendered, "raw HTML omitted") {
		t.Errorf("expected goldmark's raw-HTML-omitted marker; rendered=%s", rendered)
	}
}

// TestRenderBranchMarkdown_TruncatesLargeFile asserts that markdown
// larger than planFileSizeCap is clipped to the cap with a trailing
// "[truncated …]" note so the rendered HTML still closes its tags and
// the card stays a reasonable size in the operator's browser.
func TestRenderBranchMarkdown_TruncatesLargeFile(t *testing.T) {
	huge := "# Big doc\n\n" + strings.Repeat("BLOATBLOATBLOAT\n", 4000)
	repo := seedRepoWithDocsBranchContent(t, "worker-big", "docs/big.md", huge)
	rendered, truncated, ok := renderBranchMarkdown(repo, "teem/worker-big", "docs/big.md")
	if !ok {
		t.Fatalf("renderBranchMarkdown returned ok=false")
	}
	if !truncated {
		t.Errorf("truncated flag should be true for >50KB source")
	}
	if !strings.Contains(rendered, "[truncated") {
		t.Errorf("rendered body missing inline truncation note; rendered first 200=%s", rendered[:min(200, len(rendered))])
	}
}
