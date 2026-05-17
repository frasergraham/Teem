package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// TestAssignJob_PropagatesTaskIDToAudit verifies the end-to-end
// attribution path: when an audit event lands for a job_id registered
// in the JobTaskIndex (i.e. one that came in via assign_job), the
// injectingSink decorator stamps Meta["task_id"] before writing.
// Mirrors the production daemon wiring: hookedSink wraps the file
// sink, injectingSink wraps that, and the index lives next to the
// outermost decorator.
func TestAssignJob_PropagatesTaskIDToAudit(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	idx := audit.NewJobTaskIndex()
	inj := newInjectingSink(rt.auditSink, idx)

	// Simulate what handleAssignJob does: register the (job, task)
	// pair before any audit event for that job lands. From this
	// point on, anything tagged with JobID = "job-1" picks up the
	// task_id automatically.
	idx.Set("job-1", "t-12345678", "worker-ada")

	if err := inj.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "worker-ada",
		JobID:     "job-1",
		Kind:      audit.KindJobReceived,
		Message:   "starting",
	}); err != nil {
		t.Fatal(err)
	}

	events, err := rt.auditSink.Query("", time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("event count=%d", len(events))
	}
	got, _ := events[0].Meta["task_id"].(string)
	if got != "t-12345678" {
		t.Errorf("meta.task_id=%q, want t-12345678 (meta=%v)", got, events[0].Meta)
	}

	// Terminal kinds clear the index entry so the daemon doesn't leak
	// memory over long uptimes.
	if err := inj.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "worker-ada",
		JobID:     "job-1",
		Kind:      audit.KindJobComplete,
	}); err != nil {
		t.Fatal(err)
	}
	if idx.Size() != 0 {
		t.Errorf("index must clear on job_complete; got size=%d", idx.Size())
	}
}

// TestInjectingSink_WorkerStoppedClearsByAgent asserts that a
// worker_stopped event — which carries agent_id but no job_id —
// sweeps every in-flight index entry for that agent. Catches the
// SIGKILL/crash/OOM leak where no per-job terminal event fires.
// A second agent's entries must survive.
func TestInjectingSink_WorkerStoppedClearsByAgent(t *testing.T) {
	rt := newFullTestTeam(t, "alpha")
	idx := audit.NewJobTaskIndex()
	inj := newInjectingSink(rt.auditSink, idx)

	// Two in-flight jobs on worker-ada, one on worker-ben.
	idx.Set("job-a1", "t-aaaaaaaa", "worker-ada")
	idx.Set("job-a2", "t-bbbbbbbb", "worker-ada")
	idx.Set("job-b1", "t-cccccccc", "worker-ben")
	if idx.Size() != 3 {
		t.Fatalf("setup: size=%d", idx.Size())
	}

	// worker-ada dies (no per-job terminal kind, no job_id on the
	// event — the index must still sweep ada's two rows).
	if err := inj.Write(audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "worker-ada",
		Kind:      audit.KindWorkerStopped,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Get("job-a1"); ok {
		t.Errorf("job-a1 must clear on worker-ada stop")
	}
	if _, ok := idx.Get("job-a2"); ok {
		t.Errorf("job-a2 must clear on worker-ada stop")
	}
	if v, ok := idx.Get("job-b1"); !ok || v != "t-cccccccc" {
		t.Errorf("worker-ben entry must survive: got (%q, %v)", v, ok)
	}
}

// TestBuildTaskAgentRollups_UnionsTimelineEvents asserts the
// belt-and-braces widening: an agent that appears only via
// timeline-tagged events (meta.task_id) — never as an Evidence job —
// still surfaces in the rollup with a sensible LastSeenAt. This
// catches the regression where audit events that drift loose of
// task.Evidence (pruned jobs, race-on-link, manual operator audit
// writes) would silently disappear from the dashboard.
func TestBuildTaskAgentRollups_UnionsTimelineEvents(t *testing.T) {
	now := time.Now().UTC()
	// One agent with an Evidence job (the conventional path).
	jobs := []apiTaskJob{
		{
			JobID:       "j-1",
			AgentID:     "worker-ada",
			Status:      "done",
			StartedAt:   now.Add(-10 * time.Minute),
			CompletedAt: now.Add(-8 * time.Minute),
		},
	}
	// Two timeline events: one for the Evidence agent (should NOT
	// double-count because j-1 is in seenJobs), and one for an
	// agent whose work never landed in Evidence.
	timeline := []apiTaskTimelineEvent{
		{
			Timestamp: now.Add(-9 * time.Minute),
			Kind:      string(audit.KindJobComplete),
			AgentID:   "worker-ada",
			JobID:     "j-1", // already counted in jobs
			Source:    "job",
		},
		{
			Timestamp: now.Add(-2 * time.Minute),
			Kind:      string(audit.KindDecisionNote),
			AgentID:   "reviewer-blake",
			Source:    "task",
			Meta:      map[string]any{"task_id": "t-abc12345"},
		},
	}
	rollups := buildTaskAgentRollups(jobs, timeline)
	if len(rollups) != 2 {
		t.Fatalf("rollup count=%d, want 2 (ada + blake) — %+v", len(rollups), rollups)
	}
	byAgent := map[string]apiTaskAgentRollup{}
	for _, r := range rollups {
		byAgent[r.AgentID] = r
	}
	ada, ok := byAgent["worker-ada"]
	if !ok {
		t.Fatal("worker-ada missing from rollup")
	}
	if ada.JobCount != 1 || ada.Done != 1 {
		t.Errorf("ada JobCount=%d Done=%d, want 1/1 (timeline must not double-count)", ada.JobCount, ada.Done)
	}
	blake, ok := byAgent["reviewer-blake"]
	if !ok {
		t.Fatal("reviewer-blake missing — union pass did not surface a timeline-only agent")
	}
	if blake.JobCount != 0 {
		t.Errorf("blake JobCount=%d, want 0 (no evidence jobs)", blake.JobCount)
	}
	if blake.LastSeenAt.IsZero() {
		t.Errorf("blake LastSeenAt must come from timeline event")
	}
	// Sort order: most recently active first. Blake's last event is
	// newer (t-2m) than Ada's (t-8m), so Blake should lead.
	if rollups[0].AgentID != "reviewer-blake" {
		t.Errorf("sort order: first=%q, want reviewer-blake (most recent)", rollups[0].AgentID)
	}
}

// TestTaskDetailLog_RendersChronologically is the end-to-end
// participation-log shape test. Builds a task with audit events from
// multiple agents (worker, reviewer, leader, operator), drives the
// API endpoint, and asserts the returned timeline carries everything
// the SPA needs to render the rebuilt TaskDetailModal: per-event
// agent_id (for persona derivation), kind (for verb selection),
// job_id (for short-id rendering and transcript link join), and
// chronological coverage (the SPA flips the API's newest-first order
// to oldest-first; both orderings must produce a stable monotonic
// sequence).
func TestTaskDetailLog_RendersChronologically(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	task, err := rt.plan.AddTask(plan.NewTaskInput{Title: "traceability check"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.plan.UpdateTask(task.ID, plan.UpdateInput{AddEvidence: []string{"job-coder", "job-reviewer"}}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	// Build a participation arc: leader proposes → ready, ada (worker)
	// codes, blake (reviewer) reviews, operator approves. Events are
	// written in arc order; the API returns them newest-first.
	for _, ev := range []audit.Event{
		{Timestamp: now.Add(-5 * time.Minute), AgentID: "leader", Kind: audit.KindTaskStageChanged, Meta: map[string]any{"task_id": task.ID, "from": "proposed", "stage": "ready"}},
		{Timestamp: now.Add(-4 * time.Minute), AgentID: "worker-ada", JobID: "job-coder", Kind: audit.KindJobReceived, Meta: map[string]any{}},
		{Timestamp: now.Add(-3*time.Minute - 30*time.Second), AgentID: "worker-ada", JobID: "job-coder", Kind: audit.KindJobTranscriptReady, Meta: map[string]any{"path": "/tmp/x", "bytes": 612 * 1024, "summary": "coded"}},
		{Timestamp: now.Add(-3 * time.Minute), AgentID: "worker-ada", JobID: "job-coder", Kind: audit.KindJobComplete, Message: "did the thing"},
		{Timestamp: now.Add(-2 * time.Minute), AgentID: "reviewer-blake", JobID: "job-reviewer", Kind: audit.KindJobReceived, Meta: map[string]any{}},
		{Timestamp: now.Add(-90 * time.Second), AgentID: "reviewer-blake", JobID: "job-reviewer", Kind: audit.KindJobComplete, Message: "lgtm"},
		{Timestamp: now.Add(-1 * time.Minute), AgentID: "operator", Kind: audit.KindDecisionNote, Message: "approved", Meta: map[string]any{"task_id": task.ID, "decision": "approve"}},
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
		t.Fatal(err)
	}

	// The seven events above all live on this task (three via
	// meta.task_id, four via Evidence join). Every one should show up
	// in the timeline.
	if len(p.Timeline) != 7 {
		t.Fatalf("timeline len=%d want 7, got %+v", len(p.Timeline), p.Timeline)
	}

	// API returns newest-first; SPA flips. Either ordering must be
	// strictly monotonic — assert the API's invariant here.
	for i := 1; i < len(p.Timeline); i++ {
		if p.Timeline[i].Timestamp.After(p.Timeline[i-1].Timestamp) {
			t.Errorf("timeline must be newest-first; i=%d ts=%s > prev=%s",
				i, p.Timeline[i].Timestamp, p.Timeline[i-1].Timestamp)
		}
	}

	// Persona derivation source: every job event needs an agent_id;
	// stage-change and decision_note rows need agent_id too so the
	// SPA can label them "Leader" / "Operator". Quick sweep.
	byKind := map[string][]apiTaskTimelineEvent{}
	for _, ev := range p.Timeline {
		byKind[ev.Kind] = append(byKind[ev.Kind], ev)
		if ev.AgentID == "" {
			t.Errorf("event kind=%q missing agent_id (persona source)", ev.Kind)
		}
	}
	if _, ok := byKind[string(audit.KindTaskStageChanged)]; !ok {
		t.Error("timeline missing task_stage_changed (operator-pre-flight signal)")
	}
	if _, ok := byKind[string(audit.KindDecisionNote)]; !ok {
		t.Error("timeline missing decision_note (operator approval row)")
	}
	if rows := byKind[string(audit.KindJobComplete)]; len(rows) != 2 {
		t.Errorf("expected 2 job_complete rows (coder + reviewer), got %d", len(rows))
	}

	// Transcript URL shape: the coder job's transcript_ready event
	// fired, so the SPA-visible URL must point at the unauth /api
	// path with both agent and job in place. The reviewer job did
	// not emit transcript_ready, so its job has no URL.
	var coderJob, reviewerJob *apiTaskJob
	for i := range p.Jobs {
		switch p.Jobs[i].JobID {
		case "job-coder":
			coderJob = &p.Jobs[i]
		case "job-reviewer":
			reviewerJob = &p.Jobs[i]
		}
	}
	if coderJob == nil {
		t.Fatal("coder job missing from materialised jobs")
	}
	if coderJob.AgentID != "worker-ada" {
		t.Errorf("coder job agent=%q want worker-ada", coderJob.AgentID)
	}
	wantURL := "/teams/alpha/transcripts/worker-ada/job-coder"
	if coderJob.TranscriptURL != wantURL {
		t.Errorf("coder transcript_url=%q want %q", coderJob.TranscriptURL, wantURL)
	}
	if reviewerJob == nil {
		t.Fatal("reviewer job missing")
	}
	if reviewerJob.TranscriptURL != "" {
		t.Errorf("reviewer transcript_url=%q want empty (no transcript_ready)", reviewerJob.TranscriptURL)
	}

	// Sanity: every kind the SPA renders as a participation row is
	// present, and every row has the timestamp + agent_id the
	// persona/verb mapping needs.
	requiredKinds := []string{
		string(audit.KindTaskStageChanged),
		string(audit.KindJobReceived),
		string(audit.KindJobComplete),
		string(audit.KindDecisionNote),
	}
	for _, k := range requiredKinds {
		if len(byKind[k]) == 0 {
			t.Errorf("required kind %q absent from API payload", k)
		}
	}
}
