package audit

import (
	"sort"
	"time"
)

// MaterializedJob is a job reconstructed from a stream of audit
// events. The leader uses this to recall what was assigned and what
// came back — even across daemon restarts, since the audit log is the
// only durable record of jobs.
type MaterializedJob struct {
	JobID       string    `json:"job_id"`
	AgentID     string    `json:"agent_id"`
	Status      string    `json:"status"` // pending / done / error
	Prompt      string    `json:"prompt,omitempty"`
	PromptBytes int       `json:"prompt_bytes,omitempty"`
	Output      string    `json:"output,omitempty"`
	OutputBytes int       `json:"output_bytes,omitempty"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// Duration returns CompletedAt-StartedAt when both are set.
func (m MaterializedJob) Duration() time.Duration {
	if m.StartedAt.IsZero() || m.CompletedAt.IsZero() {
		return 0
	}
	return m.CompletedAt.Sub(m.StartedAt)
}

// MaterializeJobs joins the supplied events by job_id into materialized
// records. Lifecycle precedence:
//
//   - job_received → sets agent_id, prompt, started_at.
//   - job_complete → sets status="done", output, completed_at.
//   - job_error    → sets status="error", error message,
//     completed_at; output is captured if the worker
//     emitted one before the error.
//
// Events for unknown job_ids (empty) are skipped. Events arriving out
// of order are tolerated — completion just sets fields without
// requiring receipt first.
//
// Result is sorted by StartedAt ascending; jobs without a known start
// time sort before those that have one.
func MaterializeJobs(events []Event) []MaterializedJob {
	byID := map[string]*MaterializedJob{}
	for _, e := range events {
		if e.JobID == "" {
			continue
		}
		j, ok := byID[e.JobID]
		if !ok {
			j = &MaterializedJob{JobID: e.JobID, Status: "pending"}
			byID[e.JobID] = j
		}
		if j.AgentID == "" && e.AgentID != "" {
			j.AgentID = e.AgentID
		}
		switch e.Kind {
		case KindJobReceived:
			if j.StartedAt.IsZero() {
				j.StartedAt = e.Timestamp
			}
			if p, ok := stringFromMeta(e.Meta, "prompt"); ok {
				j.Prompt = p
			}
			if n, ok := intFromMeta(e.Meta, "prompt_bytes"); ok {
				j.PromptBytes = n
			}
		case KindJobComplete:
			j.Status = "done"
			j.CompletedAt = e.Timestamp
			if o, ok := stringFromMeta(e.Meta, "output"); ok {
				j.Output = o
			}
			if n, ok := intFromMeta(e.Meta, "output_bytes"); ok {
				j.OutputBytes = n
			}
		case KindJobError:
			j.Status = "error"
			j.CompletedAt = e.Timestamp
			if e.Message != "" {
				j.Error = e.Message
			}
			if o, ok := stringFromMeta(e.Meta, "output"); ok && j.Output == "" {
				j.Output = o
			}
			if n, ok := intFromMeta(e.Meta, "output_bytes"); ok && j.OutputBytes == 0 {
				j.OutputBytes = n
			}
		}
	}
	out := make([]MaterializedJob, 0, len(byID))
	for _, j := range byID {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// MaterializeJob returns the single materialized job for jobID, or
// (zero, false) if no events reference it.
func MaterializeJob(events []Event, jobID string) (MaterializedJob, bool) {
	scoped := make([]Event, 0)
	for _, e := range events {
		if e.JobID == jobID {
			scoped = append(scoped, e)
		}
	}
	jobs := MaterializeJobs(scoped)
	if len(jobs) == 0 {
		return MaterializedJob{}, false
	}
	return jobs[0], true
}

func stringFromMeta(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func intFromMeta(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		// JSON numbers decode as float64.
		return int(n), true
	}
	return 0, false
}
