package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/transport"
)

// fakeExecutor records calls so the test can assert that SpawnByRole
// followed by AssignJob actually drives the worker goroutine. The real
// ProcessExecutor would shell out to `claude`, which we don't have in
// CI; here we just return the prompt back.
type fakeExecutor struct {
	mu   sync.Mutex
	jobs []executor.Job
}

func (f *fakeExecutor) Execute(_ context.Context, job executor.Job) (string, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.mu.Unlock()
	return "echo: " + job.Prompt, nil
}

func (f *fakeExecutor) seen() []executor.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]executor.Job, len(f.jobs))
	copy(out, f.jobs)
	return out
}

// TestSpawnAndAssign_WorkerOutlivesRequest is the regression test for
// the bug where Worker.Start used the MCP request ctx and died the
// instant spawn_agent returned. We simulate that by canceling the ctx
// passed to SpawnByRole *before* AssignJob, then assert the job still
// runs.
func TestSpawnAndAssign_WorkerOutlivesRequest(t *testing.T) {
	tm := &team.Team{
		Name:   "ctx-test",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
		},
	}
	bs := bus.NewMemBus()
	defer bs.Close()
	reg := mcpsrv.NewRegistry()

	cfg := Config{
		HTTPClient:   nil,
		WorkerToken:  "tok",
		RepoRoot:     "",
		WorktreeBase: "",
	}

	// Build a real Spawner with a long-lived base ctx (daemon
	// lifetime). This is the fix under test.
	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()
	sp := NewSpawner(daemonCtx, tm, bs, reg, cfg)

	// Call SpawnByRole with a short-lived "request" ctx that we
	// cancel immediately after spawn returns — this is what MCP does
	// in practice.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	id, err := sp.SpawnByRole(reqCtx, "worker")
	if err != nil {
		t.Fatalf("SpawnByRole: %v", err)
	}
	if id != "worker-ada" {
		t.Fatalf("agent id = %q", id)
	}
	cancelReq() // pretend the MCP framework cancelled the request

	// Replace the Worker's Executor with our fake so we can observe
	// the goroutine actually receiving a job. The real executor would
	// try to exec `claude`.
	sp.mu.Lock()
	w := sp.workers["worker-ada"]
	sp.mu.Unlock()
	if w == nil {
		t.Fatal("worker missing from spawner.workers after spawn")
	}
	fake := &fakeExecutor{}
	w.Executor = fake

	// Give the request-ctx cancellation a moment to propagate to any
	// goroutine that might (incorrectly) be tied to it.
	time.Sleep(100 * time.Millisecond)

	// Now assign a job with a fresh request ctx.
	jobReqCtx, cancelAssign := context.WithCancel(context.Background())
	defer cancelAssign()
	jobID, err := sp.AssignJob(jobReqCtx, "worker-ada", "hello", "")
	if err != nil {
		t.Fatalf("AssignJob: %v", err)
	}
	if jobID == "" {
		t.Fatal("empty job id")
	}

	// Wait up to 2s for the worker goroutine to receive and process
	// the job. With the bug, the goroutine has exited and the job
	// sits in the bus forever.
	deadline := time.After(2 * time.Second)
	for {
		if len(fake.seen()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker goroutine never picked up the job — context lifetime bug")
		case <-time.After(20 * time.Millisecond):
		}
	}

	jobs := fake.seen()
	if jobs[0].Prompt != "hello" {
		t.Errorf("job prompt = %q", jobs[0].Prompt)
	}
}

// suppress unused-import warnings on toolchains that strip them.
var (
	_ = provisioner.Backend("")
	_ = transport.LocalTransport{}
)
