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

	id1, err := sp.Spawn(context.Background(), "worker", "")
	if err != nil {
		t.Fatalf("spawn 1: %v", err)
	}
	if id1 != "worker-ada" {
		t.Errorf("id1 = %q want worker-ada", id1)
	}
	// Swap executor so the worker doesn't try to exec claude on the
	// next assignment. (The Worker is started inside SpawnByRole.)
	swapExecutor(t, sp, id1)

	id2, err := sp.Spawn(context.Background(), "worker", "")
	if err != nil {
		t.Fatalf("spawn 2: %v", err)
	}
	if id2 != "worker-blake" {
		t.Errorf("id2 = %q want worker-blake", id2)
	}
	swapExecutor(t, sp, id2)

	id3, err := sp.Spawn(context.Background(), "worker", "")
	if err != nil {
		t.Fatalf("spawn 3: %v", err)
	}
	if id3 != "worker-cleo" {
		t.Errorf("id3 = %q want worker-cleo", id3)
	}
	swapExecutor(t, sp, id3)

	// Fourth spawn should refuse — at capacity.
	_, err = sp.Spawn(context.Background(), "worker", "")
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

	id1, _ := sp.Spawn(context.Background(), "worker", "")
	swapExecutor(t, sp, id1)
	id2, _ := sp.Spawn(context.Background(), "worker", "")
	swapExecutor(t, sp, id2)

	// Stopping the first worker releases its name back to the pool,
	// but the next spawn still goes to a fresh wordlist entry — the
	// wordlist is far from exhausted, so we don't reincarnate yet.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sp.StopAgent(ctx, id1); err != nil {
		t.Fatalf("stop %s: %v", id1, err)
	}

	id3, err := sp.Spawn(context.Background(), "worker", "")
	if err != nil {
		t.Fatalf("spawn after stop: %v", err)
	}
	if id3 == id1 || id3 == id2 {
		t.Errorf("id3 = %q reused a still-in-use or just-released name (id1=%q id2=%q); fresh wordlist still has entries", id3, id1, id2)
	}
	if id3 != "worker-cleo" {
		t.Errorf("id3 = %q want worker-cleo (next fresh wordlist entry)", id3)
	}
}

func TestSpawnByRole_UnknownRole(t *testing.T) {
	tm := &team.Team{
		Name:       "x",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()}},
	}
	sp := archetypeTestSpawner(t, tm)
	_, err := sp.Spawn(context.Background(), "unknown", "")
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
