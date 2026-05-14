package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/team"
)

// recordingProvisioner remembers every Teardown call so the test can
// assert HandleWorkerStopped flagged the agent Stopped before tearing
// it down.
type recordingProvisioner struct {
	mu        sync.Mutex
	teardowns []*provisioner.Agent
}

func (r *recordingProvisioner) Provision(context.Context, provisioner.AgentSpec) (*provisioner.Agent, error) {
	return nil, nil
}
func (r *recordingProvisioner) Teardown(_ context.Context, a *provisioner.Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Snapshot so the test sees whatever Stopped value was set at
	// teardown time, not whatever it gets mutated to afterwards.
	cp := *a
	r.teardowns = append(r.teardowns, &cp)
	return nil
}
func (r *recordingProvisioner) seen() []*provisioner.Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*provisioner.Agent, len(r.teardowns))
	copy(out, r.teardowns)
	return out
}

// TestHandleWorkerStopped_FullReconciliation drives the happy path:
// spawn a local agent (via in-process LocalProvisioner with no
// SocketDir, which yields a Transport-backed agent), then simulate
// the leader receiving a worker_stopped audit event. After the call,
// the registry should report Stopped, the spawner should have
// dropped the worker, and Teardown should have been invoked with
// Stopped=true.
func TestHandleWorkerStopped_FullReconciliation(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
		},
	}
	bs := bus.NewMemBus()
	defer bs.Close()
	reg := mcpsrv.NewRegistry()
	sp := NewSpawner(context.Background(), tm, bs, reg, Config{WorkerToken: "tok"})

	// Manually wire a tracked worker so we don't have to depend on
	// real provisioning. This mirrors what startWorker would have set
	// up after a successful spawn.
	rp := &recordingProvisioner{}
	agentID := "worker-1"
	ag := &provisioner.Agent{ID: agentID, Role: "worker", Backend: provisioner.BackendLocal}
	sp.mu.Lock()
	sp.workers[agentID] = &Worker{Agent: ag, Bus: bs}
	sp.provisioned[agentID] = provisionedAgent{provisioner: rp, agent: ag}
	cancelled := false
	sp.subs[agentID] = func() { cancelled = true }
	sp.mu.Unlock()
	reg.Add(mcpsrv.AgentEntry{ID: agentID, Role: "worker", State: mcpsrv.StateRunning})

	sp.HandleWorkerStopped(context.Background(), agentID)

	// Registry flipped.
	if e, ok := reg.Get(agentID); !ok || e.State != mcpsrv.StateStopped {
		t.Errorf("registry state = %+v, want stopped", e)
	}
	// Internal maps cleaned.
	sp.mu.Lock()
	_, hasWorker := sp.workers[agentID]
	_, hasProv := sp.provisioned[agentID]
	sp.mu.Unlock()
	if hasWorker || hasProv {
		t.Errorf("spawner still has worker=%v prov=%v after stop", hasWorker, hasProv)
	}
	if !cancelled {
		t.Error("results subscription was not cancelled")
	}

	// Teardown got Stopped=true.
	seen := rp.seen()
	if len(seen) != 1 {
		t.Fatalf("Teardown called %d times, want 1", len(seen))
	}
	if !seen[0].Stopped {
		t.Errorf("Teardown received Agent with Stopped=false; want true so /shutdown POST is skipped")
	}
}

// TestHandleWorkerStopped_UnknownAgent is the idempotency check:
// duplicate worker_stopped events (audit POSTs are at-least-once)
// should not panic, blow up, or fire teardown a second time.
func TestHandleWorkerStopped_UnknownAgent(t *testing.T) {
	tm := &team.Team{
		Name:       "x",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()}},
	}
	bs := bus.NewMemBus()
	defer bs.Close()
	sp := NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{})

	// Should not panic, should not block, should return promptly.
	done := make(chan struct{})
	go func() {
		sp.HandleWorkerStopped(context.Background(), "ghost-1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleWorkerStopped on unknown agent blocked")
	}
}
