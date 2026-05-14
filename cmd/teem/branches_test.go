package main

import (
	"os/exec"
	"strings"
	"testing"

	mcpsrv "github.com/frasergraham/teem/internal/mcp"
)

// runGit runs `git <args...>` inside dir and fatals on failure. Tests
// only call this for setup, so a hard failure is the right signal.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=teem-test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=teem-test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"HOME="+t.TempDir(), // shield against the operator's ~/.gitconfig
		"PATH="+pathEnv(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// pathEnv is a minimal PATH adequate to find git in CI/dev. Kept tiny
// so the test doesn't drag in surprises from the caller's environment.
func pathEnv() string {
	return "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"
}

// seedRepoWithBranches builds a temp git repo containing one commit on
// each of: main, teem/worker-1, teem/worker-2, feature/x. Returns the
// repo path.
func seedRepoWithBranches(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--initial-branch=main", ".")
	runGit(t, dir, "commit", "--allow-empty", "-m", "initial commit on main")

	// teem/worker-1: distinct subject so we can assert on it.
	runGit(t, dir, "checkout", "-b", "teem/worker-1")
	runGit(t, dir, "commit", "--allow-empty", "-m", "worker-1: did the thing")

	// teem/worker-2: orphan (no registry entry) once we drive the test.
	runGit(t, dir, "checkout", "-b", "teem/worker-2", "main")
	runGit(t, dir, "commit", "--allow-empty", "-m", "worker-2: left over branch")

	// feature/x: should NOT be picked up (no teem/ prefix).
	runGit(t, dir, "checkout", "-b", "feature/x", "main")
	runGit(t, dir, "commit", "--allow-empty", "-m", "feature work")

	runGit(t, dir, "checkout", "main")
	return dir
}

func TestListTeemBranches_FiltersAndMaps(t *testing.T) {
	dir := seedRepoWithBranches(t)
	reg := mcpsrv.NewRegistry()
	// Only worker-1 is "live"; worker-2 is an orphan from a stopped agent.
	reg.Add(mcpsrv.AgentEntry{ID: "worker-1", Role: "worker", State: mcpsrv.StateRunning})

	rows := listTeemBranches(dir, reg, "alpha")
	if len(rows) != 2 {
		t.Fatalf("want 2 teem/ branches, got %d: %#v", len(rows), rows)
	}
	for _, r := range rows {
		if r.AgentID == "" {
			t.Errorf("row with empty AgentID leaked through: %#v", r)
		}
	}

	byName := map[string]dashboardBranch{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	w1, ok := byName["teem/worker-1"]
	if !ok {
		t.Fatalf("teem/worker-1 missing")
	}
	if !w1.Live {
		t.Errorf("worker-1 is in the registry; expected Live=true")
	}
	if w1.JobsURL != "/teams/alpha/agents/worker-1/jobs" {
		t.Errorf("jobs url: got %q", w1.JobsURL)
	}
	if w1.AgentID != "worker-1" {
		t.Errorf("agentID: got %q", w1.AgentID)
	}
	if !strings.Contains(w1.Subject, "did the thing") {
		t.Errorf("subject not captured: %q", w1.Subject)
	}
	if w1.SHA == "" {
		t.Errorf("short SHA missing")
	}
	if w1.AgeAgo == "" {
		t.Errorf("AgeAgo should be set for a fresh commit")
	}

	w2 := byName["teem/worker-2"]
	if w2.Live {
		t.Errorf("worker-2 has no registry entry; expected Live=false")
	}
	if w2.JobsURL != "" {
		t.Errorf("orphan branch should have no jobs link, got %q", w2.JobsURL)
	}
}

func TestListTeemBranches_EmptyRepoRoot(t *testing.T) {
	reg := mcpsrv.NewRegistry()
	if got := listTeemBranches("", reg, "alpha"); got != nil {
		t.Errorf("empty repoRoot: want nil, got %#v", got)
	}
}

func TestListTeemBranches_NoTeemBranches(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--initial-branch=main", ".")
	runGit(t, dir, "commit", "--allow-empty", "-m", "initial")
	reg := mcpsrv.NewRegistry()
	got := listTeemBranches(dir, reg, "alpha")
	if len(got) != 0 {
		t.Errorf("repo without teem/ refs should return 0 rows, got %#v", got)
	}
}

func TestListTeemBranches_GitMissing(t *testing.T) {
	reg := mcpsrv.NewRegistry()
	// Pointing at a path that isn't a repo at all should swallow the
	// error and return an empty slice (dashboard must not 500).
	if got := listTeemBranches(t.TempDir(), reg, "alpha"); got != nil {
		t.Errorf("non-repo path: want nil/empty, got %#v", got)
	}
}

func TestTruncateSubject(t *testing.T) {
	if got := truncateSubject("short", 80); got != "short" {
		t.Errorf("short subject mangled: %q", got)
	}
	long := strings.Repeat("a", 200)
	got := truncateSubject(long, 80)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long subject should end with ellipsis: %q", got)
	}
	if len(got) > 80+len("…") {
		t.Errorf("long subject not trimmed: len=%d", len(got))
	}
}
