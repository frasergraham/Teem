package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/team"
)

// TestSpawnAgent_NameIdempotent verifies that asking the spawner for
// the same name twice while a worker is still running returns the
// same id without re-provisioning.
func TestSpawnAgent_NameIdempotent(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 3, WorkingDir: t.TempDir()},
		},
	}
	sp := archetypeTestSpawner(t, tm)

	id1, err := sp.Spawn(context.Background(), "worker", "ada")
	if err != nil {
		t.Fatalf("spawn 1: %v", err)
	}
	if id1 != "ada" {
		t.Errorf("first spawn id = %q, want ada", id1)
	}
	swapExecutor(t, sp, id1)

	id2, err := sp.Spawn(context.Background(), "worker", "ada")
	if err != nil {
		t.Fatalf("spawn 2: %v", err)
	}
	if id2 != id1 {
		t.Errorf("second spawn returned %q, want idempotent %q", id2, id1)
	}
	// Sanity: no extra worker created.
	sp.mu.Lock()
	count := len(sp.workers)
	sp.mu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 worker after idempotent spawn, got %d", count)
	}
}

// TestSpawnAgent_NameReincarnates spawns under a name, stops it, then
// spawns under the same name again. The id and the worktree branch
// `teem/ada` must come back instead of a fresh entry being allocated.
func TestSpawnAgent_NameReincarnates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := initGitRepo(t)

	worktreeBase := t.TempDir()
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			// Empty WorkingDir → spawner builds a per-agent worktree
			// at worktreeBase/<id> on branch teem/<id>.
			{Role: "worker", Placement: "local", MaxConcurrent: 2},
		},
	}
	bs := bus.NewMemBus()
	t.Cleanup(func() { bs.Close() })
	sp := NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{
		RepoRoot:     repo,
		WorktreeBase: worktreeBase,
	})

	id1, err := sp.Spawn(context.Background(), "worker", "ada")
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if id1 != "ada" {
		t.Fatalf("first spawn id = %q, want ada", id1)
	}
	swapExecutor(t, sp, id1)
	if !branchExists(t, repo, "teem/ada") {
		t.Fatalf("branch teem/ada should exist after first spawn")
	}

	if err := sp.StopAgent(context.Background(), id1); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Branch survives the stop — that's what makes reincarnation
	// meaningful.
	if !branchExists(t, repo, "teem/ada") {
		t.Fatalf("branch teem/ada should survive stop_agent (reincarnation depends on it)")
	}

	id2, err := sp.Spawn(context.Background(), "worker", "ada")
	if err != nil {
		t.Fatalf("second spawn: %v", err)
	}
	if id2 != id1 {
		t.Errorf("reincarnated id = %q, want %q (same name → same id)", id2, id1)
	}
	swapExecutor(t, sp, id2)

	// And the worktree is registered against teem/ada again.
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "branch refs/heads/teem/ada") {
		t.Errorf("worktree list missing branch teem/ada after reincarnation:\n%s", out)
	}
}

// TestSpawnAgent_RejectsCrossRole pre-registers a name under reviewer
// role and verifies that spawn_agent for worker with the same name
// fails with a clear error.
func TestSpawnAgent_RejectsCrossRole(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
			{Role: "reviewer", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
		},
	}
	bs := bus.NewMemBus()
	t.Cleanup(func() { bs.Close() })
	rost, _ := roster.Open("")
	// Pre-register bob as a reviewer (not in use).
	rost.Register("bob", "reviewer", time.Time{})
	sp := NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{Roster: rost})

	_, err := sp.Spawn(context.Background(), "worker", "bob")
	if err == nil {
		t.Fatal("expected cross-role spawn to fail")
	}
	if !strings.Contains(err.Error(), "already a reviewer") {
		t.Errorf("error %q should mention reviewer", err)
	}
}

// TestSpawnAgent_RejectsReservedName covers the validation table.
func TestSpawnAgent_RejectsReservedName(t *testing.T) {
	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1, WorkingDir: t.TempDir()},
		},
	}
	sp := archetypeTestSpawner(t, tm)

	cases := []struct {
		name string
		want string
	}{
		{"leader", "reserved"},
		{"daemon", "reserved"},
		{"teem", "reserved"},
		{"system", "reserved"},
		{"worker1", "legacy"},
		{"reviewer7", "legacy"},
		{"pm1", "legacy"},
		{"integrator5", "legacy"},
		{"Ada", "invalid"},
		{"ada/x", "invalid"},
		{"ada.b", "invalid"},
		{"with-hyphen", "invalid"},
		{"9starts", "invalid"},
		{"toolongname1234567890123456789012", "invalid"}, // 33 chars
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sp.Spawn(context.Background(), "worker", tc.name)
			if err == nil {
				t.Fatalf("expected error for name %q", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q for name %q should contain %q", err, tc.name, tc.want)
			}
		})
	}
}

// initGitRepo creates a fresh git repo with one commit. Skips when
// git isn't on PATH.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, stderr.String())
		}
	}
	return dir
}

func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", filepath.Join("refs/heads", branch)).CombinedOutput()
	return err == nil && len(out) > 0
}
