package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
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
type Worker struct {
	Agent    *provisioner.Agent
	Bus      bus.Bus
	Executor executor.Executor

	jobsTopic   string
	resultTopic string
	logTopic    string
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
	return nil
}

func (w *Worker) runJob(ctx context.Context, job jobMessage) {
	output, err := w.Executor.Execute(ctx, executor.Job{
		ID:      job.JobID,
		Prompt:  job.Prompt,
		Context: job.Context,
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
