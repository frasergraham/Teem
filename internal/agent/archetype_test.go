package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/team"
)

// archetypeTestSpawner builds a Spawner with the team set up so the
// returned spawn path doesn't try to actually exec a process. We swap
// each worker's Executor with a fake so we can confirm the worker
// goroutine started and is wired correctly.
func archetypeTestSpawner(t *testing.T, tm *team.Team) *Spawner {
	t.Helper()
	bs := bus.NewMemBus()
	t.Cleanup(func() { bs.Close() })
	reg := mcpsrv.NewRegistry()
	ctx := context.Background()
	// Disable worktree by giving every spec a working dir up front.
	return NewSpawner(ctx, tm, bs, reg, Config{})
}

func TestSpawnByRole_GeneratesArchetypeIDs(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 3, WorkingDir: t.TempDir()},
		},
	}
	sp := archetypeTestSpawner(t, tm)

	id1, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn 1: %v", err)
	}
	if id1 != "worker-1" {
		t.Errorf("id1 = %q want worker-1", id1)
	}
	// Swap executor so the worker doesn't try to exec claude on the
	// next assignment. (The Worker is started inside SpawnByRole.)
	swapExecutor(t, sp, id1)

	id2, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn 2: %v", err)
	}
	if id2 != "worker-2" {
		t.Errorf("id2 = %q want worker-2", id2)
	}
	swapExecutor(t, sp, id2)

	id3, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn 3: %v", err)
	}
	if id3 != "worker-3" {
		t.Errorf("id3 = %q want worker-3", id3)
	}
	swapExecutor(t, sp, id3)

	// Fourth spawn should refuse — at capacity.
	_, err = sp.SpawnByRole(context.Background(), "worker")
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Errorf("expected capacity error, got: %v", err)
	}
}

func TestSpawnByRole_MonotonicAfterStop(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 2, WorkingDir: t.TempDir()},
		},
	}
	sp := archetypeTestSpawner(t, tm)

	id1, _ := sp.SpawnByRole(context.Background(), "worker")
	swapExecutor(t, sp, id1)
	id2, _ := sp.SpawnByRole(context.Background(), "worker")
	swapExecutor(t, sp, id2)

	// Stopping worker-1 frees a slot. The next spawn must NOT reuse
	// the id — it gets worker-3.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sp.StopAgent(ctx, id1); err != nil {
		t.Fatalf("stop %s: %v", id1, err)
	}

	id3, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn after stop: %v", err)
	}
	if id3 != "worker-3" {
		t.Errorf("id3 = %q want worker-3 (no id reuse)", id3)
	}
}

func TestSpawnByRole_UnknownRole(t *testing.T) {
	tm := &team.Team{
		Name:       "x",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()}},
	}
	sp := archetypeTestSpawner(t, tm)
	_, err := sp.SpawnByRole(context.Background(), "unknown")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected unknown role error, got: %v", err)
	}
}

// swapExecutor replaces the worker's executor with a no-op fake so the
// test doesn't try to exec `claude`. Has to happen after SpawnByRole
// because Worker is created inside that call.
func swapExecutor(t *testing.T, sp *Spawner, agentID string) {
	t.Helper()
	sp.mu.Lock()
	w := sp.workers[agentID]
	sp.mu.Unlock()
	if w == nil {
		t.Fatalf("worker %s missing from spawner after SpawnByRole", agentID)
	}
	w.Executor = &noopExecutor{}
}

type noopExecutor struct{}

func (*noopExecutor) Execute(context.Context, executor.Job) (string, error) { return "", nil }
