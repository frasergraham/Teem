package provisioner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a fresh git repo in dir with one commit so HEAD points
// at something real. Returns the repo path. Skips the test if git is not
// on PATH.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
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

func TestResolveRepoRoot(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRepoRoot(sub)
	if err != nil {
		t.Fatalf("ResolveRepoRoot: %v", err)
	}
	// macOS prefixes /private/ on some tempdir paths; compare via EvalSymlinks.
	if real1, _ := filepath.EvalSymlinks(got); real1 != "" {
		got = real1
	}
	if real2, _ := filepath.EvalSymlinks(repo); real2 != "" {
		repo = real2
	}
	if got != repo {
		t.Fatalf("got %q want %q", got, repo)
	}
}

func TestResolveRepoRoot_NotRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	_, err := ResolveRepoRoot(dir)
	if !errors.Is(err, ErrNoRepo) {
		t.Fatalf("want ErrNoRepo, got %v", err)
	}
}

func TestEnsureWorktree_CreatesAndIsIdempotent(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "agent-1")
	ctx := context.Background()

	if err := EnsureWorktree(ctx, repo, wt, "teem/agent-1"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("worktree marker missing: %v", err)
	}

	// Second call must be idempotent.
	if err := EnsureWorktree(ctx, repo, wt, "teem/agent-1"); err != nil {
		t.Fatalf("idempotent EnsureWorktree: %v", err)
	}

	list, err := listWorktrees(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := list[wt]; !ok {
		// Worktree paths may resolve through symlinks on macOS; compare by
		// EvalSymlinks-normalised values.
		realWT, _ := filepath.EvalSymlinks(wt)
		found := false
		for p := range list {
			real, _ := filepath.EvalSymlinks(p)
			if real == realWT {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("worktree %q not listed; got %v", wt, list)
		}
	}
}

func TestEnsureWorktree_ReusesExistingBranch(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// Create the branch directly.
	if err := exec.Command("git", "-C", repo, "branch", "teem/be-1").Run(); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	wt := filepath.Join(t.TempDir(), "be-1")
	if err := EnsureWorktree(ctx, repo, wt, "teem/be-1"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	// The worktree should be on the pre-existing branch, not a new one.
	out, err := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "teem/be-1" {
		t.Fatalf("worktree branch = %q, want teem/be-1", got)
	}
}

func TestRemoveWorktree_KeepsBranch(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "agent-2")
	ctx := context.Background()
	if err := EnsureWorktree(ctx, repo, wt, "teem/agent-2"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if err := RemoveWorktree(ctx, repo, wt); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree path still present: %v", err)
	}
	// Branch must survive teardown — that's the whole point.
	if !branchExists(ctx, repo, "teem/agent-2") {
		t.Fatalf("branch teem/agent-2 was deleted on teardown")
	}
}

// TestAdoptOrphanedWorktree_RenamesBareToCanonical covers the
// pre-canonicalisation orphan path: a worktree at `<base>/ada/` on
// branch `teem/ada` should be renamed in-place to `<base>/worker-ada/`
// on `teem/worker-ada` when a Provision call comes in for the
// canonical id `worker-ada`. Best-effort, so failures should not
// surface — but the happy path must rename both the dir and the
// branch.
func TestAdoptOrphanedWorktree_RenamesBareToCanonical(t *testing.T) {
	repo := initRepo(t)
	base := t.TempDir()
	ctx := context.Background()

	// Pre-canonicalisation state: bare worktree on a bare branch.
	bareDir := filepath.Join(base, "ada")
	if err := EnsureWorktree(ctx, repo, bareDir, "teem/ada"); err != nil {
		t.Fatalf("seed bare worktree: %v", err)
	}

	p := &LocalProvisioner{RepoRoot: repo, WorktreeBase: base}
	canonicalDir := filepath.Join(base, "worker-ada")
	canonicalBranch := "teem/worker-ada"
	p.adoptOrphanedWorktree(ctx, AgentSpec{ID: "worker-ada", Role: "worker"}, canonicalDir, canonicalBranch)

	if _, err := os.Stat(bareDir); err == nil {
		t.Errorf("bare worktree %q should have been moved", bareDir)
	}
	if _, err := os.Stat(canonicalDir); err != nil {
		t.Errorf("canonical worktree missing after adopt: %v", err)
	}
	if branchExists(ctx, repo, "teem/ada") {
		t.Errorf("bare branch teem/ada should have been renamed")
	}
	if !branchExists(ctx, repo, canonicalBranch) {
		t.Errorf("canonical branch %q should exist after rename", canonicalBranch)
	}
}

// TestAdoptOrphanedWorktree_NoOpWhenCanonicalExists guards against
// the helper clobbering an already-canonical worktree.
func TestAdoptOrphanedWorktree_NoOpWhenCanonicalExists(t *testing.T) {
	repo := initRepo(t)
	base := t.TempDir()
	ctx := context.Background()

	canonicalDir := filepath.Join(base, "worker-ada")
	if err := EnsureWorktree(ctx, repo, canonicalDir, "teem/worker-ada"); err != nil {
		t.Fatalf("seed canonical worktree: %v", err)
	}
	// Also create a bare orphan to confirm it's left alone when
	// canonical already exists.
	bareDir := filepath.Join(base, "ada")
	if err := EnsureWorktree(ctx, repo, bareDir, "teem/ada"); err != nil {
		t.Fatalf("seed bare worktree: %v", err)
	}

	p := &LocalProvisioner{RepoRoot: repo, WorktreeBase: base}
	p.adoptOrphanedWorktree(ctx, AgentSpec{ID: "worker-ada", Role: "worker"}, canonicalDir, "teem/worker-ada")

	if _, err := os.Stat(bareDir); err != nil {
		t.Errorf("bare orphan was unexpectedly removed: %v", err)
	}
	if _, err := os.Stat(canonicalDir); err != nil {
		t.Errorf("canonical worktree disappeared: %v", err)
	}
}

func TestRemoveWorktree_MissingIsOK(t *testing.T) {
	repo := initRepo(t)
	if err := RemoveWorktree(context.Background(), repo, filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("missing worktree should not error: %v", err)
	}
}
