package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/team"
)

// capturingExecutor records the Job it last saw so tests can assert on
// the Context the worker passed in.
type capturingExecutor struct {
	mu  sync.Mutex
	job executor.Job
	got chan struct{}
}

func newCapturingExecutor() *capturingExecutor {
	return &capturingExecutor{got: make(chan struct{}, 1)}
}

func (c *capturingExecutor) Execute(_ context.Context, j executor.Job) (string, error) {
	c.mu.Lock()
	c.job = j
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
	return "ok", nil
}

func (c *capturingExecutor) lastJob() executor.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.job
}

func TestSpawnInjectsArchetypeMemory(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
		},
	}
	bs := bus.NewMemBus()
	t.Cleanup(func() { bs.Close() })
	reg := mcpsrv.NewRegistry()

	const baseline = "# Digest\n\nThe worker role always uses gofmt."
	sp := NewSpawner(context.Background(), tm, bs, reg, Config{
		LoadArchetypeMemory: func(role string) (string, error) {
			if role != "worker" {
				return "", nil
			}
			return baseline, nil
		},
	})

	id, err := sp.Spawn(context.Background(), "worker", "")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Replace the executor with our capturing one before we assign
	// the job (the worker is already running but it pulls Executor
	// on every job).
	exec := newCapturingExecutor()
	sp.mu.Lock()
	w := sp.workers[id]
	sp.mu.Unlock()
	if w == nil {
		t.Fatalf("worker %s missing after SpawnByRole", id)
	}
	if w.BaselineContext != baseline {
		t.Errorf("BaselineContext = %q, want %q", w.BaselineContext, baseline)
	}
	w.Executor = exec

	if _, err := sp.AssignJob(context.Background(), id, "do the thing", "extra context"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	select {
	case <-exec.got:
	case <-time.After(2 * time.Second):
		t.Fatal("executor never saw the job")
	}
	jb := exec.lastJob()
	if !strings.Contains(jb.Context, baseline) {
		t.Errorf("Job.Context should include baseline; got %q", jb.Context)
	}
	if !strings.Contains(jb.Context, "extra context") {
		t.Errorf("Job.Context should include caller context too; got %q", jb.Context)
	}
}

func TestSpawnWithoutArchetypeMemoryHook(t *testing.T) {
	// Sanity: when no loader is configured the worker has an empty
	// BaselineContext and the job's Context is unchanged.
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
		},
	}
	bs := bus.NewMemBus()
	t.Cleanup(func() { bs.Close() })
	reg := mcpsrv.NewRegistry()
	sp := NewSpawner(context.Background(), tm, bs, reg, Config{})

	id, err := sp.Spawn(context.Background(), "worker", "")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	exec := newCapturingExecutor()
	sp.mu.Lock()
	w := sp.workers[id]
	sp.mu.Unlock()
	w.Executor = exec

	if _, err := sp.AssignJob(context.Background(), id, "do it", "ctx"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	select {
	case <-exec.got:
	case <-time.After(2 * time.Second):
		t.Fatal("executor never saw job")
	}
	if got := exec.lastJob().Context; got != "ctx" {
		t.Errorf("Job.Context = %q, want %q", got, "ctx")
	}
}
