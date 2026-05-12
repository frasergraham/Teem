package provisioner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// canonicalize resolves symlinks so equality checks survive platform-level
// path rewriting (e.g. macOS `/var/folders/...` → `/private/var/...`). Falls
// back to the input on error so missing files don't break callers.
func canonicalize(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// ResolveRepoRoot walks up from startDir looking for the enclosing git
// repository and returns its top-level path. Returns ErrNoRepo if startDir
// is not inside a git working tree.
func ResolveRepoRoot(startDir string) (string, error) {
	if startDir == "" {
		var err error
		startDir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	cmd := exec.Command("git", "-C", startDir, "rev-parse", "--show-toplevel")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "not a git repository") {
			return "", ErrNoRepo
		}
		return "", fmt.Errorf("git rev-parse: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ErrNoRepo signals that ResolveRepoRoot was called outside a git working
// tree.
var ErrNoRepo = errors.New("not inside a git repository")

// EnsureWorktree makes sure path is a git worktree of repoRoot checked out
// on branch. It is idempotent across teem sessions:
//
//   - If path is already registered as a worktree, nothing happens.
//   - If path exists on disk but is not registered, the stale entry is
//     pruned and the worktree is re-added.
//   - If branch already exists but no worktree references it, the
//     existing branch is reused (the agent picks up where it left off).
//   - Otherwise a fresh branch is created from the current HEAD.
func EnsureWorktree(ctx context.Context, repoRoot, path, branch string) error {
	existing, err := listWorktrees(ctx, repoRoot)
	if err != nil {
		return err
	}
	want := canonicalize(path)
	for p := range existing {
		if canonicalize(p) == want {
			return nil
		}
	}
	if _, err := os.Stat(path); err == nil {
		// Path exists but isn't a registered worktree — prune so the next
		// add doesn't fail with "already exists".
		if err := runGit(ctx, repoRoot, "worktree", "prune"); err != nil {
			return fmt.Errorf("worktree prune: %w", err)
		}
		if _, err := os.Stat(path); err == nil {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove stale path %s: %w", path, err)
			}
		}
	}
	args := []string{"worktree", "add"}
	if branchExists(ctx, repoRoot, branch) {
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path)
	}
	if err := runGit(ctx, repoRoot, args...); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	return nil
}

// RemoveWorktree removes the worktree at path from repoRoot, keeping the
// branch intact. Missing worktrees are not an error.
func RemoveWorktree(ctx context.Context, repoRoot, path string) error {
	existing, err := listWorktrees(ctx, repoRoot)
	if err != nil {
		return err
	}
	want := canonicalize(path)
	var registered bool
	for p := range existing {
		if canonicalize(p) == want {
			registered = true
			break
		}
	}
	if !registered {
		// Not registered — maybe the user already removed it. Best-effort
		// rm on the directory, then prune so git forgets it.
		if _, err := os.Stat(path); err == nil {
			_ = os.RemoveAll(path)
		}
		_ = runGit(ctx, repoRoot, "worktree", "prune")
		return nil
	}
	if err := runGit(ctx, repoRoot, "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("git worktree remove: %w", err)
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func branchExists(ctx context.Context, repoRoot, branch string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// listWorktrees returns a map keyed by worktree path (the value is the
// HEAD ref or empty for the main worktree). Output of
// `git worktree list --porcelain`:
//
//	worktree /path/to/main
//	HEAD abcdef...
//	branch refs/heads/main
//
//	worktree /path/to/other
//	HEAD ...
//	branch refs/heads/teem/be-1
func listWorktrees(ctx context.Context, repoRoot string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git worktree list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	out := map[string]string{}
	var current string
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			current = strings.TrimPrefix(line, "worktree ")
			out[current] = ""
		case strings.HasPrefix(line, "branch ") && current != "":
			out[current] = strings.TrimPrefix(line, "branch ")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
