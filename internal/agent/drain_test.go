package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
	"github.com/frasergraham/teem/internal/inflight"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/team"
)

// blockingExecutor lets the test gate when a job completes — exactly
// the shape Drain needs to exercise.
type blockingExecutor struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func newBlockingExec() *blockingExecutor {
	return &blockingExecutor{release: make(chan struct{})}
}
func (b *blockingExecutor) Execute(ctx context.Context, _ executor.Job) (string, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	select {
	case <-b.release:
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestDrain_BlocksUntilJobsFinish(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
		},
	}
	dir := t.TempDir()
	sink, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	ifl, err := inflight.Open(filepath.Join(dir, "in-flight.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer ifl.Close()

	bs := bus.NewMemBus()
	defer bs.Close()
	sp := NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{
		AuditSink: sink,
		InFlight:  ifl,
	})
	if _, err := sp.Spawn(context.Background(), "worker", ""); err != nil {
		t.Fatal(err)
	}

	// Swap in our blocking executor so we can gate completion.
	sp.mu.Lock()
	w := sp.workers["worker-ada"]
	sp.mu.Unlock()
	be := newBlockingExec()
	w.Executor = be

	// Assign a job; the executor will block on be.release.
	if _, err := sp.AssignJob(context.Background(), "worker-ada", "do stuff", ""); err != nil {
		t.Fatal(err)
	}
	// Wait for the worker to actually pick up the job and start
	// blocking (in-flight should be 1).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && sp.TotalInFlight() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if sp.TotalInFlight() != 1 {
		t.Fatalf("expected in-flight=1, got %d", sp.TotalInFlight())
	}

	// Drain with a short timeout: should hit deadline because the
	// job is still blocked.
	drainCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := sp.Drain(drainCtx); err == nil {
		t.Fatalf("expected Drain to time out while job is blocked")
	}

	// Outstanding should still show the job pre-cleanup.
	out, _ := ifl.Outstanding()
	if len(out) != 1 {
		t.Errorf("expected 1 outstanding in-flight record, got %d", len(out))
	}

	// Release the executor; drain should now succeed.
	close(be.release)
	drainCtx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := sp.Drain(drainCtx2); err != nil {
		t.Errorf("expected clean drain after release, got %v", err)
	}
	if sp.TotalInFlight() != 0 {
		t.Errorf("post-drain in-flight: %d", sp.TotalInFlight())
	}

	// And the in-flight log's outstanding set is empty after job_complete.
	out, _ = ifl.Outstanding()
	if len(out) != 0 {
		t.Errorf("expected 0 outstanding after completion, got %d (%+v)", len(out), out)
	}
}
