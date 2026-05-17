package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

const (
	// orphanJobSweepInterval is how often the daemon scans the audit
	// log for stale job_received events with no terminal partner.
	orphanJobSweepInterval = 5 * time.Minute
	// orphanJobStaleAfter is the age past which an unmatched
	// job_received is considered orphaned and gets a synthetic
	// job_interrupted emitted on its behalf.
	//
	// sweepOnce queries the audit log over a 2*orphanJobStaleAfter (4h)
	// window, so jobs orphaned more than 4h ago fall outside the query
	// and are silently skipped. Prior-shutdown orphans are caught at
	// startup by inFlightLog.Outstanding() reconcile; this 4h ceiling
	// only matters for jobs orphaned during a long-running daemon.
	orphanJobStaleAfter = 2 * time.Hour
)

// runOrphanJobSweep ticks every orphanJobSweepInterval and emits a
// synthetic job_interrupted for any job_received that never got a
// terminal partner within orphanJobStaleAfter. The synthetic terminal
// itself terminates the job in the audit log, so subsequent sweeps
// over the same state emit nothing new.
//
// Exits when ctx is cancelled (daemon shutdown).
//
// Bound to d.baseCtx (daemon-scoped): if a team is unregistered while
// the daemon keeps running, this ticker would keep firing against a
// closed sink. Team-unregister is currently a no-op path so this
// never fires in practice — documented, not refactored.
//
// Single-goroutine-per-team: buildTeamServices is the only caller and
// handleRegister/restoreTeams never both fire for the same team_id.
func runOrphanJobSweep(ctx context.Context, teamID string, sink audit.Sink) {
	t := time.NewTicker(orphanJobSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepOnce(sink, teamID, time.Now().UTC(), orphanJobStaleAfter)
		}
	}
}

// sweepOnce performs a single sweep pass and emits synthetic
// job_interrupted events for stale orphans. Exposed so tests can
// drive a sweep without spinning the ticker for 5 minutes.
func sweepOnce(sink audit.Sink, teamID string, now time.Time, staleAfter time.Duration) {
	since := now.Add(-2 * staleAfter)
	events, err := sink.Query("", since, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] orphan-sweep %s: query: %v\n", teamID, err)
		return
	}
	orphans := findOrphans(events, now, staleAfter)
	for _, o := range orphans {
		_ = sink.Write(audit.Event{
			Timestamp: now,
			AgentID:   o.AgentID,
			JobID:     o.JobID,
			Kind:      audit.KindJobInterrupted,
			Message:   "orphan-job sweep marked stale received job interrupted",
			Meta: map[string]any{
				"reason":               "stale_sweep",
				"original_received_at": o.ReceivedAt.Format(time.RFC3339),
			},
		})
	}
	if len(orphans) > 0 {
		fmt.Fprintf(os.Stderr, "[teemd] %s: orphan-sweep marked %d stale job(s) interrupted\n", teamID, len(orphans))
	}
}

// orphanJob is one unmatched job_received event found by the sweep.
type orphanJob struct {
	JobID      string
	AgentID    string
	ReceivedAt time.Time
}

// findOrphans walks events in order, tracking job_received entries
// without a matching terminal partner (job_complete, job_error, or
// job_interrupted). Returns the subset whose received timestamp is
// older than now - staleAfter. Pure — no I/O, easy to unit-test.
//
// Assumes events arrive in timestamp order; an out-of-order terminal
// seen before its job_received would false-positive an orphan.
func findOrphans(events []audit.Event, now time.Time, staleAfter time.Duration) []orphanJob {
	type received struct {
		agentID string
		ts      time.Time
	}
	open := map[string]received{}
	for _, e := range events {
		if e.JobID == "" {
			continue
		}
		switch e.Kind {
		case audit.KindJobReceived:
			if _, ok := open[e.JobID]; !ok {
				open[e.JobID] = received{agentID: e.AgentID, ts: e.Timestamp}
			}
		case audit.KindJobComplete, audit.KindJobError, audit.KindJobInterrupted:
			delete(open, e.JobID)
		}
	}
	cutoff := now.Add(-staleAfter)
	var out []orphanJob
	for jobID, r := range open {
		if r.ts.Before(cutoff) {
			out = append(out, orphanJob{
				JobID:      jobID,
				AgentID:    r.agentID,
				ReceivedAt: r.ts,
			})
		}
	}
	return out
}
