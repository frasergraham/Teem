package audit

import (
	"testing"
	"time"
)

func TestMaterializeJobs_HappyPath(t *testing.T) {
	t0 := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Timestamp: t0, AgentID: "wk-1", JobID: "j1", Kind: KindJobReceived,
			Meta: map[string]any{"prompt": "find the bug", "prompt_bytes": 13}},
		{Timestamp: t0.Add(2 * time.Second), AgentID: "wk-1", JobID: "j1", Kind: KindJobComplete,
			Meta: map[string]any{"output": "no bug found", "output_bytes": 12}},
	}
	jobs := MaterializeJobs(events)
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.JobID != "j1" || j.AgentID != "wk-1" {
		t.Errorf("ids: %+v", j)
	}
	if j.Status != "done" {
		t.Errorf("status = %q, want done", j.Status)
	}
	if j.Prompt != "find the bug" || j.Output != "no bug found" {
		t.Errorf("bodies: prompt=%q output=%q", j.Prompt, j.Output)
	}
	if j.PromptBytes != 13 || j.OutputBytes != 12 {
		t.Errorf("byte counts: %+v", j)
	}
	if j.Duration() != 2*time.Second {
		t.Errorf("duration = %v", j.Duration())
	}
}

func TestMaterializeJobs_ErrorPath(t *testing.T) {
	t0 := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Timestamp: t0, AgentID: "wk-1", JobID: "j2", Kind: KindJobReceived,
			Meta: map[string]any{"prompt": "do thing"}},
		{Timestamp: t0.Add(1 * time.Second), AgentID: "wk-1", JobID: "j2", Kind: KindJobError,
			Message: "claude exit 1",
			Meta:    map[string]any{"output": "partial", "output_bytes": 7}},
	}
	jobs := MaterializeJobs(events)
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.Status != "error" {
		t.Errorf("status = %q, want error", j.Status)
	}
	if j.Error != "claude exit 1" {
		t.Errorf("error = %q", j.Error)
	}
	if j.Output != "partial" {
		t.Errorf("output not captured from error event: %q", j.Output)
	}
}

func TestMaterializeJobs_PendingWhenOnlyReceipt(t *testing.T) {
	events := []Event{
		{Timestamp: time.Now(), AgentID: "wk-1", JobID: "j3", Kind: KindJobReceived,
			Meta: map[string]any{"prompt": "still running"}},
	}
	jobs := MaterializeJobs(events)
	if len(jobs) != 1 || jobs[0].Status != "pending" {
		t.Fatalf("expected one pending job, got %+v", jobs)
	}
}

func TestMaterializeJobs_FiltersUnscopedEvents(t *testing.T) {
	// Heartbeats and notes without a JobID shouldn't create job entries.
	events := []Event{
		{Timestamp: time.Now(), AgentID: "wk-1", Kind: KindHeartbeat, Meta: map[string]any{"uptime_s": 5}},
		{Timestamp: time.Now(), AgentID: "wk-1", JobID: "j4", Kind: KindJobReceived},
		{Timestamp: time.Now(), AgentID: "wk-1", JobID: "j4", Kind: KindJobComplete},
	}
	jobs := MaterializeJobs(events)
	if len(jobs) != 1 {
		t.Errorf("non-job events leaked into materialized output: %+v", jobs)
	}
}

func TestMaterializeJob_SingleLookup(t *testing.T) {
	t0 := time.Now()
	events := []Event{
		{Timestamp: t0, AgentID: "a1", JobID: "x", Kind: KindJobReceived,
			Meta: map[string]any{"prompt": "p1"}},
		{Timestamp: t0, AgentID: "a2", JobID: "y", Kind: KindJobReceived,
			Meta: map[string]any{"prompt": "p2"}},
	}
	j, ok := MaterializeJob(events, "y")
	if !ok {
		t.Fatal("expected to find j=y")
	}
	if j.AgentID != "a2" || j.Prompt != "p2" {
		t.Errorf("wrong job: %+v", j)
	}
	if _, ok := MaterializeJob(events, "missing"); ok {
		t.Error("missing job should not be found")
	}
}
