package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

func tempOrphanSink(t *testing.T) *audit.FileSink {
	t.Helper()
	s, err := audit.OpenFile(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeEvent(t *testing.T, sink audit.Sink, e audit.Event) {
	t.Helper()
	if err := sink.Write(e); err != nil {
		t.Fatalf("sink.Write: %v", err)
	}
}

func countInterrupted(t *testing.T, sink audit.Sink, jobID string) int {
	t.Helper()
	events, err := sink.Query("", time.Time{}, 0)
	if err != nil {
		t.Fatalf("sink.Query: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == audit.KindJobInterrupted && e.JobID == jobID {
			n++
		}
	}
	return n
}

func TestOrphanJobSweep_EmitsInterruptedForStaleReceived(t *testing.T) {
	sink := tempOrphanSink(t)
	now := time.Now().UTC()
	receivedAt := now.Add(-3 * time.Hour)

	writeEvent(t, sink, audit.Event{
		Timestamp: receivedAt,
		AgentID:   "worker-ada",
		JobID:     "job-stale-1",
		Kind:      audit.KindJobReceived,
	})

	sweepOnce(sink, "team-x", now, orphanJobStaleAfter)

	events, err := sink.Query("", time.Time{}, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var interrupted *audit.Event
	for i := range events {
		if events[i].Kind == audit.KindJobInterrupted && events[i].JobID == "job-stale-1" {
			interrupted = &events[i]
			break
		}
	}
	if interrupted == nil {
		t.Fatalf("expected synthetic job_interrupted for job-stale-1, got %d events", len(events))
	}
	if interrupted.AgentID != "worker-ada" {
		t.Errorf("agent_id = %q, want worker-ada", interrupted.AgentID)
	}
	if got, _ := interrupted.Meta["reason"].(string); got != "stale_sweep" {
		t.Errorf("meta.reason = %v, want stale_sweep", interrupted.Meta["reason"])
	}
	gotRecv, _ := interrupted.Meta["original_received_at"].(string)
	if gotRecv == "" {
		t.Errorf("meta.original_received_at missing; meta=%v", interrupted.Meta)
	} else if parsed, perr := time.Parse(time.RFC3339, gotRecv); perr != nil {
		t.Errorf("meta.original_received_at not RFC3339: %v", perr)
	} else if !parsed.Equal(receivedAt.Truncate(time.Second)) {
		t.Errorf("meta.original_received_at = %v, want ~%v", parsed, receivedAt)
	}
}

func TestOrphanJobSweep_LeavesFreshJobsAlone(t *testing.T) {
	sink := tempOrphanSink(t)
	now := time.Now().UTC()

	writeEvent(t, sink, audit.Event{
		Timestamp: now.Add(-30 * time.Minute),
		AgentID:   "worker-ben",
		JobID:     "job-fresh-1",
		Kind:      audit.KindJobReceived,
	})

	sweepOnce(sink, "team-x", now, orphanJobStaleAfter)

	if n := countInterrupted(t, sink, "job-fresh-1"); n != 0 {
		t.Fatalf("fresh job got %d synthetic interrupts, want 0", n)
	}
}

func TestOrphanJobSweep_LeavesAlreadyTerminatedAlone(t *testing.T) {
	sink := tempOrphanSink(t)
	now := time.Now().UTC()
	pairedTS := now.Add(-3 * time.Hour)

	writeEvent(t, sink, audit.Event{
		Timestamp: pairedTS,
		AgentID:   "worker-cleo",
		JobID:     "job-paired-1",
		Kind:      audit.KindJobReceived,
	})
	writeEvent(t, sink, audit.Event{
		Timestamp: pairedTS.Add(1 * time.Second),
		AgentID:   "worker-cleo",
		JobID:     "job-paired-1",
		Kind:      audit.KindJobComplete,
	})

	sweepOnce(sink, "team-x", now, orphanJobStaleAfter)

	if n := countInterrupted(t, sink, "job-paired-1"); n != 0 {
		t.Fatalf("already-terminated job got %d synthetic interrupts, want 0", n)
	}
}

func TestOrphanJobSweep_IdempotentOnSecondRun(t *testing.T) {
	sink := tempOrphanSink(t)
	now := time.Now().UTC()

	writeEvent(t, sink, audit.Event{
		Timestamp: now.Add(-4 * time.Hour),
		AgentID:   "worker-dax",
		JobID:     "job-stale-2",
		Kind:      audit.KindJobReceived,
	})

	sweepOnce(sink, "team-x", now, orphanJobStaleAfter)
	if n := countInterrupted(t, sink, "job-stale-2"); n != 1 {
		t.Fatalf("after first sweep: %d interrupts, want 1", n)
	}

	// Second tick over identical (well, augmented-with-the-synthetic)
	// state must not emit a second interrupt — the first synthetic
	// terminates the job, so findOrphans sees it as terminated.
	sweepOnce(sink, "team-x", now.Add(orphanJobSweepInterval), orphanJobStaleAfter)
	if n := countInterrupted(t, sink, "job-stale-2"); n != 1 {
		t.Fatalf("after second sweep: %d interrupts, want 1 (idempotent)", n)
	}
}

func TestFindOrphans_HandlesJobErrorAsTerminal(t *testing.T) {
	now := time.Now().UTC()
	receivedAt := now.Add(-3 * time.Hour)
	events := []audit.Event{
		{Timestamp: receivedAt, AgentID: "w", JobID: "j-err", Kind: audit.KindJobReceived},
		{Timestamp: receivedAt.Add(time.Second), AgentID: "w", JobID: "j-err", Kind: audit.KindJobError},
	}
	if got := findOrphans(events, now, orphanJobStaleAfter); len(got) != 0 {
		t.Fatalf("job_error should terminate; got orphans = %+v", got)
	}
}

func TestFindOrphans_HandlesJobInterruptedAsTerminal(t *testing.T) {
	now := time.Now().UTC()
	receivedAt := now.Add(-3 * time.Hour)
	events := []audit.Event{
		{Timestamp: receivedAt, AgentID: "w", JobID: "j-int", Kind: audit.KindJobReceived},
		{Timestamp: receivedAt.Add(time.Second), AgentID: "w", JobID: "j-int", Kind: audit.KindJobInterrupted},
	}
	if got := findOrphans(events, now, orphanJobStaleAfter); len(got) != 0 {
		t.Fatalf("job_interrupted should terminate; got orphans = %+v", got)
	}
}
