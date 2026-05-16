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
// Each entry also records the agent_id the job was assigned to (when
// known) so worker_stopped events — which carry agent_id but no
// job_id — can clear in-flight entries for that worker. Entries
// rehydrated from plan.Task.Evidence after a daemon bounce have an
// empty agent_id; those still clear on the per-job terminal kinds.
//
// Restart safety: the daemon rehydrates the index from
// plan.Task.Evidence on startup, so worker events landing on the new
// process still attribute to the right task even when the job
// out-lived a daemon bounce.
type JobTaskIndex struct {
	mu sync.RWMutex
	m  map[string]entry
}

type entry struct {
	taskID  string
	agentID string
}

// NewJobTaskIndex returns an empty index. Safe for concurrent use.
func NewJobTaskIndex() *JobTaskIndex {
	return &JobTaskIndex{m: map[string]entry{}}
}

// Set records that jobID is the work for taskID, assigned to
// agentID. agentID may be empty when the caller doesn't know it
// (e.g. rehydration from plan.Task.Evidence). Empty jobID/taskID are
// ignored so callers do not have to guard the assign path with a nil
// check.
func (i *JobTaskIndex) Set(jobID, taskID, agentID string) {
	if i == nil || jobID == "" || taskID == "" {
		return
	}
	i.mu.Lock()
	i.m[jobID] = entry{taskID: taskID, agentID: agentID}
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
	if !ok {
		return "", false
	}
	return v.taskID, true
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

// ClearByAgent drops every entry whose agentID matches. Returns the
// number of entries removed. Called by the injecting Sink on
// worker_stopped (SIGKILL/crash/OOM paths where no per-job terminal
// event will fire). Entries with empty agentID — rehydrated from
// plan evidence — are not matched.
func (i *JobTaskIndex) ClearByAgent(agentID string) int {
	if i == nil || agentID == "" {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	n := 0
	for jid, e := range i.m {
		if e.agentID == agentID {
			delete(i.m, jid)
			n++
		}
	}
	return n
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
