package main

import (
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// injectingSink wraps an audit.Sink and stamps Meta["task_id"] onto
// every Event whose JobID is registered in a JobTaskIndex. Terminal
// kinds (job_complete / job_error / job_interrupted / worker_stopped)
// clear the index entry after the underlying Write succeeds so a
// long-running daemon does not leak entries — worker_stopped catches
// the SIGKILL / crash / OOM path where no job_complete fires.
//
// The decorator sits OUTSIDE the hookedSink so injection happens
// before the inner Write and the hook fan-out — every consumer
// (channel hook, messaging hook, archmem hook, the on-disk JSONL,
// and any future hook) sees a uniform event shape with task_id
// already in place.
//
// Idempotent: if an event already carries Meta["task_id"] the
// existing value is left alone, so MCP tools that emit
// task-scoped events directly (set_task_stage, record_decision,
// record_blocker) keep their explicit attribution.
type injectingSink struct {
	inner audit.Sink
	idx   *audit.JobTaskIndex
}

func newInjectingSink(inner audit.Sink, idx *audit.JobTaskIndex) *injectingSink {
	return &injectingSink{inner: inner, idx: idx}
}

func (s *injectingSink) Write(e audit.Event) error {
	if s.idx != nil && e.JobID != "" {
		if taskID, ok := s.idx.Get(e.JobID); ok {
			if !hasTaskID(e.Meta) {
				e.Meta = withTaskID(e.Meta, taskID)
			}
		}
	}
	if err := s.inner.Write(e); err != nil {
		return err
	}
	if s.idx != nil {
		switch e.Kind {
		case audit.KindJobComplete, audit.KindJobError, audit.KindJobInterrupted:
			if e.JobID != "" {
				s.idx.Clear(e.JobID)
			}
		case audit.KindWorkerStopped:
			// worker_stopped carries agent_id but no job_id —
			// sweep every entry assigned to that agent so a
			// SIGKILL'd worker doesn't leak in-flight rows.
			if e.AgentID != "" {
				s.idx.ClearByAgent(e.AgentID)
			}
		}
	}
	return nil
}

func (s *injectingSink) Query(agentID string, since time.Time, limit int) ([]audit.Event, error) {
	return s.inner.Query(agentID, since, limit)
}

func (s *injectingSink) Close() error { return s.inner.Close() }

func hasTaskID(m map[string]any) bool {
	if m == nil {
		return false
	}
	v, ok := m["task_id"].(string)
	return ok && v != ""
}

// withTaskID returns a fresh map containing the original keys plus
// task_id. Copying avoids mutating the caller's literal-initialised
// meta map — several callers reuse the same map for adjacent writes.
func withTaskID(m map[string]any, taskID string) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["task_id"] = taskID
	return out
}
