package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunPruneBranches_UsesIDKeyedPaths is the regression test for the
// TI1 follow-up (t-c717772d + t-2f8c2915): the prune CLI builds the
// roster + worktree paths from team.ID (canonical t-<hex>), not the
// historical team.Name slug. Before the fix, runPruneBranches opened
// `~/.teem/state/<name>/roster.json` (which is empty after the daemon's
// migration to id-keyed paths), so every retired worker was classified
// as `orphan`; and `~/.teem/worktrees/<name>/` (which doesn't exist), so
// the `git worktree remove` step silently no-op'd on every row.
func TestRunPruneBranches_UsesIDKeyedPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo := t.TempDir()
	runGit(t, repo, "init", "--initial-branch=main", ".")
	runGit(t, repo, "commit", "--allow-empty", "-m", "init")
	runGit(t, repo, "checkout", "-b", "teem/worker-test")
	runGit(t, repo, "commit", "--allow-empty", "-m", "wip on worker-test")
	runGit(t, repo, "checkout", "main")

	teamID := "t-deadbeef00112233"
	yamlBody := `team:
  id: ` + teamID + `
  name: example-team
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	yamlPath := filepath.Join(repo, "teem.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed the roster at the id-keyed path. Hand-written JSON (rather
	// than constructing via roster.Open) keeps the test pinned to the
	// on-disk shape — if a future change reshapes the file, this test
	// fails loudly instead of silently passing through an in-memory
	// shim.
	rosterDir := filepath.Join(home, ".teem", "state", teamID)
	if err := os.MkdirAll(rosterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rosterJSON := `{"entries":{"worker-test":{"id":"worker-test","role":"worker","in_use":false,"last_used_at":"2020-01-01T00:00:00Z"}},"next_numeric":{}}`
	if err := os.WriteFile(filepath.Join(rosterDir, "roster.json"), []byte(rosterJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	worktreeBase := filepath.Join(home, ".teem", "worktrees", teamID)
	if err := os.MkdirAll(worktreeBase, 0o700); err != nil {
		t.Fatal(err)
	}
	worktreeDir := filepath.Join(worktreeBase, "worker-test")
	runGit(t, repo, "worktree", "add", worktreeDir, "teem/worker-test")

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}

	// Dry run first: classification table must call this branch
	// `retired` (the roster entry was read) rather than `orphan` (the
	// pre-fix symptom — pruner opened the wrong file).
	out := captureStdout(t, func() {
		if err := runPruneBranches([]string{"--team", yamlPath, "--retired-age=1ns"}); err != nil {
			t.Fatalf("dry-run: %v", err)
		}
	})
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "teem/worker-test") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("no row for teem/worker-test in output:\n%s", out)
	}
	if strings.Contains(row, "orphan") {
		t.Errorf("worker-test classified as orphan — roster path not id-keyed: %q", row)
	}
	if !strings.Contains(row, "retired") {
		t.Errorf("worker-test row missing retired tag: %q", row)
	}

	// Now do the destructive sweep with --force: both the branch and
	// the worktree dir must vanish. If WorktreeBase were name-keyed,
	// the os.Stat probe inside Apply would miss the worktree and leave
	// it behind even after the branch deletes.
	if _, err := captureStdoutAndErr(t, func() error {
		return runPruneBranches([]string{"--team", yamlPath, "--yes", "--force", "--retired-age=1ns"})
	}); err != nil {
		t.Fatalf("--yes --force: %v", err)
	}
	if branchExistsInRepo(t, repo, "teem/worker-test") {
		t.Errorf("branch teem/worker-test still present after --yes --force prune")
	}
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir %q should be removed (id-keyed WorktreeBase resolved); stat err=%v", worktreeDir, err)
	}
}

func branchExistsInRepo(t *testing.T, repo, name string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	return cmd.Run() == nil
}

// captureStdoutAndErr is captureStdout's twin for the destructive
// branch: it swallows stdout while also surfacing the inner error.
// captureStdout's fn signature is `func()`, which forces a t.Fatalf
// inside the closure for any error; for the prune sweep we want to
// inspect the error from outside.
func captureStdoutAndErr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = fn() })
	return out, err
}

// captureStderr swaps os.Stderr for a pipe for the duration of fn and
// returns whatever the function wrote there. Used by prune tests that
// want to assert on the operator-facing "skipped live" message.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	_ = w.Close()
	<-done
	os.Stderr = orig
	return buf.String()
}

// TestRunPruneBranches_ForceSkipsLiveWorker is the regression test for
// t-0a16821c: even with `--yes --force`, a branch whose owner is on
// the roster with in_use=true must NOT be deleted, and the CLI must
// print the operator-facing "stop first" hint to stderr. The bug was
// that the CLI passed no LiveCheck callback to pruner.Apply, so a
// stale Classify snapshot (or a spawn during the sweep) could let a
// live worker's branch through.
func TestRunPruneBranches_ForceSkipsLiveWorker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo := t.TempDir()
	runGit(t, repo, "init", "--initial-branch=main", ".")
	runGit(t, repo, "commit", "--allow-empty", "-m", "init")
	// Live worker branch with unmerged work — without the live check
	// this would be eligible for `branch -D` under --force.
	runGit(t, repo, "checkout", "-b", "teem/worker-una")
	runGit(t, repo, "commit", "--allow-empty", "-m", "wip on una")
	runGit(t, repo, "checkout", "main")

	teamID := "t-cafe000011112222"
	yamlBody := `team:
  id: ` + teamID + `
  name: example-team
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	yamlPath := filepath.Join(repo, "teem.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed the roster with the live worker. in_use=true is the signal
	// that this worker is currently spawned — the same flag the spawner
	// writes in production.
	rosterDir := filepath.Join(home, ".teem", "state", teamID)
	if err := os.MkdirAll(rosterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rosterJSON := `{"entries":{"worker-una":{"id":"worker-una","role":"worker","in_use":true,"last_used_at":"2026-05-15T00:00:00Z"}},"next_numeric":{}}`
	if err := os.WriteFile(filepath.Join(rosterDir, "roster.json"), []byte(rosterJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}

	var runErr error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			runErr = runPruneBranches([]string{"--team", yamlPath, "--yes", "--force", "--retired-age=1ns"})
		})
	})
	if runErr != nil {
		t.Fatalf("--yes --force: %v", runErr)
	}
	if !branchExistsInRepo(t, repo, "teem/worker-una") {
		t.Errorf("live worker's branch was deleted under --yes --force (work would be lost)")
	}
	if !strings.Contains(stderr, "refusing to delete teem/worker-una") {
		t.Errorf("stderr missing 'refusing to delete teem/worker-una' line; got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "agent is live") {
		t.Errorf("stderr missing 'agent is live' rationale; got:\n%s", stderr)
	}
}
