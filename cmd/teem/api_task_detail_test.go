package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// TestAPITaskDetail_NoEvidence checks the happy path for a brand-new
// task: the plan record round-trips, timeline / agents / jobs are
// empty (not null) so the SPA's `.map` and `.length` calls don't
// blow up.
func TestAPITaskDetail_NoEvidence(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, err := rt.plan.AddTask(plan.NewTaskInput{Title: "no evidence yet"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/tasks/"+task.ID, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var p apiTaskDetailPayload
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if p.Task.ID != task.ID {
		t.Errorf("task.id=%q want %q", p.Task.ID, task.ID)
	}
	if p.Timeline == nil {
		t.Error("timeline must be [], not null")
	}
	if p.Agents == nil {
		t.Error("agents must be [], not null")
	}
	if p.Jobs == nil {
		t.Error("jobs must be [], not null")
	}
	if len(p.Timeline) != 0 || len(p.Agents) != 0 || len(p.Jobs) != 0 {
		t.Errorf("expected empty slices, got %d timeline / %d agents / %d jobs",
			len(p.Timeline), len(p.Agents), len(p.Jobs))
	}
}

// TestAPITaskDetail_FullJoin wires plan + audit + a fake transcript
// event and asserts the timeline pulls both task-tagged events and
// evidence-job events, the agent rollup tallies per-status counts,
// and the transcript URL is the unauth /api path.
func TestAPITaskDetail_FullJoin(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, err := rt.plan.AddTask(plan.NewTaskInput{Title: "join task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.plan.UpdateTask(task.ID, plan.UpdateInput{AddEvidence: []string{"j-1"}}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// Two task-tagged decision notes (task source) + one job lifecycle
	// (job source). The job_received → job_transcript_ready →
	// job_complete trio drives the MaterializeJobs path.
	for _, ev := range []audit.Event{
		{Timestamp: now.Add(-3 * time.Minute), AgentID: "ada", Kind: audit.KindDecisionNote, Message: "picked branch X", Meta: map[string]any{"task_id": task.ID}},
		{Timestamp: now.Add(-2 * time.Minute), AgentID: "ada", JobID: "j-1", Kind: audit.KindJobReceived, Meta: map[string]any{"prompt": "go!"}},
		{Timestamp: now.Add(-1 * time.Minute), AgentID: "ada", JobID: "j-1", Kind: audit.KindJobTranscriptReady, Meta: map[string]any{"path": "/tmp/x", "bytes": 420, "summary": "did the thing"}},
		{Timestamp: now.Add(-30 * time.Second), AgentID: "ada", JobID: "j-1", Kind: audit.KindJobComplete, Message: "ok"},
	} {
		if err := rt.auditSink.Write(ev); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/tasks/"+task.ID, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var p apiTaskDetailPayload
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Timeline newest-first; task-tagged decision must appear.
	if len(p.Timeline) < 4 {
		t.Fatalf("timeline len=%d want ≥4", len(p.Timeline))
	}
	var sawTaskSource, sawJobSource bool
	for _, ev := range p.Timeline {
		if ev.Source == "task" && ev.Kind == "decision_note" {
			sawTaskSource = true
		}
		if ev.Source == "job" && ev.JobID == "j-1" {
			sawJobSource = true
		}
	}
	if !sawTaskSource {
		t.Error("timeline missing source=task decision_note")
	}
	if !sawJobSource {
		t.Error("timeline missing source=job event for j-1")
	}
	// Newest first.
	for i := 1; i < len(p.Timeline); i++ {
		if p.Timeline[i].Timestamp.After(p.Timeline[i-1].Timestamp) {
			t.Errorf("timeline not newest-first at i=%d", i)
		}
	}

	// Agent rollup: one agent, one done job.
	if len(p.Agents) != 1 || p.Agents[0].AgentID != "ada" {
		t.Fatalf("agents=%+v want single ada rollup", p.Agents)
	}
	if p.Agents[0].JobCount != 1 || p.Agents[0].Done != 1 {
		t.Errorf("ada rollup=%+v want JobCount=1 Done=1", p.Agents[0])
	}

	// Job row carries the unauth transcript URL.
	if len(p.Jobs) != 1 {
		t.Fatalf("jobs len=%d", len(p.Jobs))
	}
	wantURL := "/teams/alpha/transcripts/ada/j-1"
	if p.Jobs[0].TranscriptURL != wantURL {
		t.Errorf("transcript_url=%q want %q", p.Jobs[0].TranscriptURL, wantURL)
	}
	if p.Jobs[0].Status != "done" {
		t.Errorf("job status=%q want done", p.Jobs[0].Status)
	}
}

// TestAPITaskDetail_UnknownTaskAndTeam ensures 404 for both missing
// team and missing task — distinct from a 500 plan-unavailable path,
// which is reserved for the broken-internal-state case.
func TestAPITaskDetail_UnknownTaskAndTeam(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Unknown team.
	req := httptest.NewRequest(http.MethodGet, "/api/teams/zzz/tasks/t-1", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown team: code=%d want 404", w.Code)
	}

	// Unknown task on a known team.
	req = httptest.NewRequest(http.MethodGet, "/api/teams/alpha/tasks/t-missing", nil)
	w = httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown task: code=%d want 404", w.Code)
	}
}

// TestAPITeamTranscript_UnauthGet covers the unauth read path: GET
// without a bearer must serve NDJSON. The existing bearer-gated SSR
// POST path (/teams/<id>/transcripts/...) is unchanged — the modal
// link uses the new /api/ path.
func TestAPITeamTranscript_UnauthGet(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	dir := filepath.Join(rt.transcriptsDir, "ada")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":"system","message":"hello"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "j-1.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/transcripts/ada/j-1", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type=%q want application/x-ndjson", ct)
	}
	if w.Body.String() != string(body) {
		t.Errorf("body=%q want %q", w.Body.String(), string(body))
	}
}

// TestAPITeamTranscript_RejectsPOST keeps the /api/ transcript path
// strictly read-only — operators must continue using the bearer-gated
// SSR endpoint to upload.
func TestAPITeamTranscript_RejectsPOST(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodPost, "/api/teams/alpha/transcripts/ada/j-1", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("code=%d want 405 (POST not exposed on /api/)", w.Code)
	}
}

// TestSummarizeTaskEvent locks the per-kind composition rules for
// the timeline `summary` field. Covers task_stage_changed both ways
// (full meta vs missing from/to), job_complete with full meta vs
// missing meta (no duration / no tool_calls), decision_note, and an
// unrecognised kind that falls back to Message → kind name.
func TestSummarizeTaskEvent(t *testing.T) {
	cases := []struct {
		name string
		e    audit.Event
		want string
	}{
		{
			name: "task_stage_changed_full",
			e: audit.Event{
				Kind: audit.KindTaskStageChanged,
				Meta: map[string]any{"from": "proposed", "to": "ready"},
			},
			want: "stage proposed → ready",
		},
		{
			name: "task_stage_changed_empty_meta_falls_back",
			e: audit.Event{
				Kind:    audit.KindTaskStageChanged,
				Message: "fallback",
			},
			want: "fallback",
		},
		{
			name: "task_stage_changed_empty_meta_no_message",
			e:    audit.Event{Kind: audit.KindTaskStageChanged},
			want: "task_stage_changed",
		},
		{
			name: "job_complete_full_meta",
			e: audit.Event{
				AgentID: "ada",
				JobID:   "abcdef1234567890",
				Kind:    audit.KindJobComplete,
				Meta:    map[string]any{"duration_ms": float64(125000), "tool_calls": float64(7)},
			},
			want: "ada finished job abcdef12 (2m5s, 7 tool calls)",
		},
		{
			name: "job_complete_missing_meta",
			e: audit.Event{
				AgentID: "ada",
				JobID:   "abcdef1234567890",
				Kind:    audit.KindJobComplete,
			},
			want: "ada finished job abcdef12",
		},
		{
			name: "decision_note_uses_message",
			e: audit.Event{
				Kind:    audit.KindDecisionNote,
				Message: "picked branch X",
			},
			want: "picked branch X",
		},
		{
			name: "unknown_kind_falls_back_to_message",
			e: audit.Event{
				Kind:    audit.Kind("custom_thing"),
				Message: "hi",
			},
			want: "hi",
		},
		{
			name: "unknown_kind_no_message_returns_kind",
			e:    audit.Event{Kind: audit.Kind("custom_thing")},
			want: "custom_thing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeTaskEvent(tc.e)
			if got != tc.want {
				t.Errorf("summarizeTaskEvent = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAPITaskDetail_OriginAndParentSurfaced verifies the task-detail
// API exposes Origin + ParentID so the SPA can render the synthetic
// "<Origin> created this task" row at the head of the participation
// log. Also covers the legacy-default-to-operator path: tasks loaded
// from disk without an Origin should arrive at the SPA as operator.
func TestAPITaskDetail_OriginAndParentSurfaced(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	parent, _ := rt.plan.AddTask(plan.NewTaskInput{Title: "parent"})
	child, err := rt.plan.AddTask(plan.NewTaskInput{
		Title:    "child",
		ParentID: parent.ID,
		Origin:   plan.OriginLeader,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/tasks/"+child.ID, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var p apiTaskDetailPayload
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Task.Origin != string(plan.OriginLeader) {
		t.Errorf("origin = %q want leader", p.Task.Origin)
	}
	if p.Task.ParentID != parent.ID {
		t.Errorf("parent_id = %q want %s", p.Task.ParentID, parent.ID)
	}

	// Parent itself had no Origin supplied — AddTask's library-level
	// fallback should land it on operator (and the wire field reflects
	// that, so the SPA's legacy fallback never has to kick in for
	// freshly-created tasks).
	req2 := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/tasks/"+parent.ID, nil)
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req2)
	var p2 apiTaskDetailPayload
	_ = json.Unmarshal(w2.Body.Bytes(), &p2)
	if p2.Task.Origin != string(plan.OriginOperator) {
		t.Errorf("default origin = %q want operator", p2.Task.Origin)
	}
}

// TestAPITeamTranscript_Missing returns 404 when the file doesn't
// exist on disk (e.g. transcript event was emitted but the file was
// rotated). Distinct from "transcripts not configured" → 500.
func TestAPITeamTranscript_Missing(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/transcripts/ada/never-was", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("code=%d want 404", w.Code)
	}
}
