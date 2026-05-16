package audit

import "sync"

// JobTaskIndex is the in-memory map from in-flight job_id to the
// task_id it was assigned for. It is populated synchronously by the
// assign_job MCP tool (every job is task-scoped — there are no
// standalone jobs); consulted by a decorating Sink to stamp
// meta.task_id onto every event from that job; and cleared when the
// job reaches a terminal kind (job_complete / job_error /
// job_interrupted) so a long-running daemon does not accumulate
// entries indefinitely.
//
// Restart safety: the daemon rehydrates the index from
// plan.Task.Evidence on startup, so worker events landing on the new
// process still attribute to the right task even when the job
// out-lived a daemon bounce.
type JobTaskIndex struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewJobTaskIndex returns an empty index. Safe for concurrent use.
func NewJobTaskIndex() *JobTaskIndex {
	return &JobTaskIndex{m: map[string]string{}}
}

// Set records that jobID is the work for taskID. Empty arguments are
// ignored so callers do not have to guard the assign path with a nil
// check.
func (i *JobTaskIndex) Set(jobID, taskID string) {
	if i == nil || jobID == "" || taskID == "" {
		return
	}
	i.mu.Lock()
	i.m[jobID] = taskID
	i.mu.Unlock()
}

// Get returns the recorded task_id for jobID, or "", false.
func (i *JobTaskIndex) Get(jobID string) (string, bool) {
	if i == nil || jobID == "" {
		return "", false
	}
	i.mu.RLock()
	v, ok := i.m[jobID]
	i.mu.RUnlock()
	return v, ok
}

// Clear drops jobID from the index. Called by the injecting Sink on
// terminal events; safe to call repeatedly for the same id.
func (i *JobTaskIndex) Clear(jobID string) {
	if i == nil || jobID == "" {
		return
	}
	i.mu.Lock()
	delete(i.m, jobID)
	i.mu.Unlock()
}

// Size reports the current number of tracked job→task mappings.
// Exposed for tests and operator diagnostics.
func (i *JobTaskIndex) Size() int {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	n := len(i.m)
	i.mu.RUnlock()
	return n
}
