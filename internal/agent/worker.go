package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
	"github.com/frasergraham/teem/internal/inflight"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/provisioner"
)

// jobMessage is the on-bus payload published to agent.<id>.jobs.
type jobMessage struct {
	JobID   string `json:"job_id"`
	Prompt  string `json:"prompt"`
	Context string `json:"context,omitempty"`
}

// resultMessage is the on-bus payload published to agent.<id>.results.
type resultMessage struct {
	JobID  string `json:"job_id"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// Worker drives a single provisioned agent: it pulls jobs from the bus,
// hands each one to its Executor, and publishes the final result back to
// the bus.
//
// When Audit + Registry are set, the worker also emits the same audit
// signals as the remote teem-worker daemon (job_received,
// job_complete, job_error, periodic heartbeats), and updates the
// registry's LastSeen on each event. This makes local + SSH workers
// observable to `list_agents` / `query_audit` / `recall_jobs` /
// `teem audit` the same way Fargate workers already are.
type Worker struct {
	Agent    *provisioner.Agent
	Bus      bus.Bus
	Executor executor.Executor

	// Optional observability hookups. nil = no-op.
	Audit             audit.Sink
	Registry          *mcpsrv.Registry
	InFlight          *inflight.Log // durability: records start/end so daemon restarts can mark interrupted
	HeartbeatInterval time.Duration // 0 disables; spawner picks 60s default
	BodyCap           int           // truncation cap for prompt/output meta (default 64 KiB)

	// BaselineContext is the archetype-memory markdown the spawner
	// snapshot at construction time. When non-empty it's prepended to
	// each job's Context so the worker carries "what this role has
	// been doing" into every Claude run.
	BaselineContext string

	jobsTopic   string
	resultTopic string
	logTopic    string

	startedAt time.Time
	inFlight  atomic.Int64
}

// JobsTopic returns the bus topic this worker listens on for jobs.
func JobsTopic(agentID string) string { return "agent." + agentID + ".jobs" }

// ResultsTopic returns the bus topic this worker publishes results to.
func ResultsTopic(agentID string) string { return "agent." + agentID + ".results" }

// LogsTopic returns the bus topic this worker streams logs/status on.
func LogsTopic(agentID string) string { return "agent." + agentID + ".log" }

// Start runs the worker loop in a goroutine. It returns immediately. The
// goroutine exits when ctx is cancelled or the jobs channel closes.
func (w *Worker) Start(ctx context.Context) error {
	if w.Executor == nil {
		return fmt.Errorf("agent %s: worker has no executor", w.Agent.ID)
	}
	w.jobsTopic = JobsTopic(w.Agent.ID)
	w.resultTopic = ResultsTopic(w.Agent.ID)
	w.logTopic = LogsTopic(w.Agent.ID)
	w.startedAt = time.Now()
	if w.BodyCap == 0 {
		w.BodyCap = 64 * 1024
	}
	ch, err := w.Bus.Subscribe(ctx, w.jobsTopic)
	if err != nil {
		return fmt.Errorf("agent %s: subscribe: %w", w.Agent.ID, err)
	}
	go func() {
		for msg := range ch {
			var job jobMessage
			if err := json.Unmarshal(msg.Payload, &job); err != nil {
				w.publishLog(ctx, fmt.Sprintf("decode job: %v", err))
				continue
			}
			w.runJob(ctx, job)
		}
	}()
	if w.Audit != nil && w.HeartbeatInterval > 0 {
		go w.runHeartbeat(ctx)
	}
	return nil
}

func (w *Worker) runJob(ctx context.Context, job jobMessage) {
	w.inFlight.Add(1)
	defer w.inFlight.Add(-1)

	// Durability: record this job as in-flight before doing any
	// work. If the daemon dies between start and end, restart
	// reconcile picks up the orphan.
	if w.InFlight != nil {
		_ = w.InFlight.RecordStart(job.JobID, w.Agent.ID, truncate(job.Prompt, 200))
	}

	w.emit(audit.Event{
		AgentID: w.Agent.ID,
		JobID:   job.JobID,
		Kind:    audit.KindJobReceived,
		Meta: map[string]any{
			"prompt":       truncate(job.Prompt, w.BodyCap),
			"prompt_bytes": len(job.Prompt),
			"role":         w.Agent.Role,
		},
	})

	jobCtx := job.Context
	if w.BaselineContext != "" {
		if jobCtx != "" {
			jobCtx = w.BaselineContext + "\n\n---\n\n" + jobCtx
		} else {
			jobCtx = w.BaselineContext
		}
	}
	output, err := w.Executor.Execute(ctx, executor.Job{
		ID:      job.JobID,
		Prompt:  job.Prompt,
		Context: jobCtx,
		MCPs:    w.Agent.MCPs,
	})
	res := resultMessage{JobID: job.JobID, Output: output}
	if err != nil {
		res.Error = err.Error()
	}
	body, _ := json.Marshal(res)
	_ = w.Bus.Publish(ctx, bus.Message{
		Topic:   w.resultTopic,
		Kind:    bus.KindResult,
		From:    w.Agent.ID,
		Payload: body,
	})

	if err != nil {
		w.emit(audit.Event{
			AgentID: w.Agent.ID,
			JobID:   job.JobID,
			Kind:    audit.KindJobError,
			Message: err.Error(),
			Meta: map[string]any{
				"output":       truncate(output, w.BodyCap),
				"output_bytes": len(output),
			},
		})
		if w.InFlight != nil {
			_ = w.InFlight.RecordEnd(job.JobID)
		}
		return
	}
	w.emit(audit.Event{
		AgentID: w.Agent.ID,
		JobID:   job.JobID,
		Kind:    audit.KindJobComplete,
		Meta: map[string]any{
			"output":       truncate(output, w.BodyCap),
			"output_bytes": len(output),
		},
	})
	if w.InFlight != nil {
		_ = w.InFlight.RecordEnd(job.JobID)
	}
}

// runHeartbeat emits a heartbeat audit event on a fixed interval while
// ctx is alive. Matches the teem-worker daemon's behavior so local
// workers show up in list_agents.last_seen the same way Fargate ones
// do.
func (w *Worker) runHeartbeat(ctx context.Context) {
	t := time.NewTicker(w.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.emit(audit.Event{
			AgentID: w.Agent.ID,
			Kind:    audit.KindHeartbeat,
			Meta: map[string]any{
				"in_flight": w.inFlight.Load(),
				"uptime_s":  int(time.Since(w.startedAt).Seconds()),
				"role":      w.Agent.Role,
				"backend":   string(w.Agent.Backend),
			},
		})
	}
}

// emit writes an event to the audit sink AND bumps the registry's
// LastSeen. Mirrors the daemon's audit-handler-wrapper behavior so
// in-process worker events are observable the same way HTTP-posted
// events are. Best-effort; nil deps are silently skipped.
func (w *Worker) emit(ev audit.Event) {
	if w.Audit == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	_ = w.Audit.Write(ev)
	if w.Registry != nil && ev.AgentID != "" {
		w.Registry.SetLastSeen(ev.AgentID, ev.Timestamp)
	}
}

func (w *Worker) publishLog(ctx context.Context, line string) {
	_ = w.Bus.Publish(ctx, bus.Message{
		Topic:   w.logTopic,
		Kind:    bus.KindLog,
		From:    w.Agent.ID,
		Payload: []byte(line),
	})
}

// EnsureDir creates dir if it does not exist; used by spawner to set up
// worker WorkingDirs ahead of time.
func EnsureDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Clean(dir), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return nil
}

// truncate clamps s to cap bytes with a "<truncated>" marker when it
// trims something. cap <= 0 disables.
func truncate(s string, cap int) string {
	if cap <= 0 || len(s) <= cap {
		return s
	}
	return s[:cap] + "\n…<truncated>"
}
