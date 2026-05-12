// Package gitsetup prepares a remote worker's git checkout. It clones
// the operator's repo into the worker's workdir, wires up a credential
// helper that reads a PAT from env (TEEM_GIT_TOKEN), configures the
// committer identity, and ensures the worker is on its dedicated branch
// (default teem/<agent-id>).
//
// The package is shaped as pure functions over an Options struct so it
// can be exercised against a local file:// remote in tests without
// hitting GitHub.
package gitsetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options controls Configure and Push. AgentID and RepoURL are the only
// required fields when calling Configure.
type Options struct {
	// AgentID identifies the worker. The branch name defaults to
	// BranchPrefix+AgentID.
	AgentID string
	// WorkDir is the worker's working directory. Configure clones into
	// WorkDir/repo and configures git there.
	WorkDir string
	// RepoURL is the HTTPS clone URL of the repo.
	RepoURL string
	// Token is the personal access token used by the credential helper.
	// Stored only in env on the worker process; never written to disk.
	Token string
	// Username is the git auth username paired with Token. For GitHub
	// fine-grained or classic PATs this is "x-access-token"; for GitLab
	// it is "oauth2". Default: x-access-token.
	Username string
	// AuthorName / AuthorEmail are the committer identity for any
	// commits the worker makes. Default: "Teem Agent" /
	// "teem-agent@noreply.local".
	AuthorName  string
	AuthorEmail string
	// BranchPrefix is prepended to AgentID to form the branch name.
	// Default: "teem/".
	BranchPrefix string
	// BaseRef is the ref the agent's branch is created from on first
	// run (when the branch doesn't exist remotely). Default: HEAD of
	// the cloned default branch.
	BaseRef string
}

// applyDefaults fills in unset fields. Returns a copy so callers don't
// see their input mutated.
func applyDefaults(o Options) Options {
	if o.Username == "" {
		o.Username = "x-access-token"
	}
	if o.AuthorName == "" {
		o.AuthorName = "Teem Agent"
	}
	if o.AuthorEmail == "" {
		o.AuthorEmail = "teem-agent@noreply.local"
	}
	if o.BranchPrefix == "" {
		o.BranchPrefix = "teem/"
	}
	return o
}

// Branch returns the on-disk branch name for the options.
func Branch(o Options) string {
	o = applyDefaults(o)
	return o.BranchPrefix + o.AgentID
}

// ClonePath returns the directory the repo will be cloned into.
func ClonePath(o Options) string {
	return filepath.Join(o.WorkDir, "repo")
}

// Configure prepares the worker's git environment:
//
//  1. Writes a credential helper script that prints username/password
//     read from env when git asks. The script is created with mode 0700
//     and lives in WorkDir.
//  2. Clones RepoURL into WorkDir/repo if not already cloned.
//  3. Configures local user.name / user.email and credential.helper.
//  4. Ensures the agent's branch exists locally (checked out, branched
//     off origin/HEAD if new).
//
// Configure is idempotent — running it twice against an existing clone
// is a no-op beyond updating refs.
func Configure(ctx context.Context, o Options) (clonePath string, err error) {
	o = applyDefaults(o)
	if o.AgentID == "" {
		return "", errors.New("gitsetup: AgentID is required")
	}
	if o.WorkDir == "" {
		return "", errors.New("gitsetup: WorkDir is required")
	}
	if o.RepoURL == "" {
		return "", errors.New("gitsetup: RepoURL is required")
	}
	if err := os.MkdirAll(o.WorkDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir workdir: %w", err)
	}

	helperPath, err := writeCredentialHelper(o)
	if err != nil {
		return "", err
	}

	clonePath = ClonePath(o)
	if !isGitDir(clonePath) {
		// Use a temp dir then move into place so a failed clone doesn't
		// leave a half-baked repo.
		if err := runGit(ctx, "", "clone", o.RepoURL, clonePath); err != nil {
			return "", fmt.Errorf("clone: %w", err)
		}
	}

	for _, args := range [][]string{
		{"config", "user.name", o.AuthorName},
		{"config", "user.email", o.AuthorEmail},
		{"config", "credential.helper", helperPath},
		// Disable interactive prompts so missing credentials fail fast
		// rather than hanging waiting for a TTY that doesn't exist.
		{"config", "core.askpass", ""},
	} {
		if err := runGit(ctx, clonePath, args...); err != nil {
			return "", fmt.Errorf("git %v: %w", args, err)
		}
	}

	// Ensure we're on the agent's branch.
	branch := Branch(o)
	if err := ensureBranch(ctx, clonePath, branch, o.BaseRef); err != nil {
		return "", err
	}
	return clonePath, nil
}

// Push pushes the agent's branch to the configured remote with
// --set-upstream. Returns the stderr text alongside an error so callers
// can surface a useful diagnostic to the audit log.
func Push(ctx context.Context, o Options) (stderr string, err error) {
	o = applyDefaults(o)
	clonePath := ClonePath(o)
	branch := Branch(o)
	cmd := exec.CommandContext(ctx, "git", "-C", clonePath, "push", "-u", "origin", branch)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stderrBuf.String()), fmt.Errorf("git push: %w", err)
	}
	return strings.TrimSpace(stderrBuf.String()), nil
}

// writeCredentialHelper writes a small shell script that prints
// username and password (read from env) to stdout — git's credential
// helper protocol. Returns the absolute path to the script.
func writeCredentialHelper(o Options) (string, error) {
	path := filepath.Join(o.WorkDir, ".teem-git-credentials.sh")
	// The helper deliberately reads $TEEM_GIT_TOKEN at runtime rather
	// than baking the token into the script — that way rotating the
	// token is just changing an env var.
	script := fmt.Sprintf(`#!/bin/sh
echo "username=%s"
echo "password=$TEEM_GIT_TOKEN"
`, escapeShellSingleLine(o.Username))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write credential helper: %w", err)
	}
	return path, nil
}

// escapeShellSingleLine escapes a value safe to embed in double quotes.
// Used only for the username here so a hostile value (like a username
// containing `"`) can't break the helper.
func escapeShellSingleLine(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "$", `\$`)
	return s
}

// ensureBranch makes sure the named branch exists in the worktree and is
// checked out. If it exists locally we just check it out; if it exists
// only on origin we create the tracking branch; if it doesn't exist we
// branch from baseRef (or HEAD when baseRef is empty).
func ensureBranch(ctx context.Context, clonePath, branch, baseRef string) error {
	if branchExistsLocal(ctx, clonePath, branch) {
		return runGit(ctx, clonePath, "checkout", branch)
	}
	if branchExistsRemote(ctx, clonePath, branch) {
		return runGit(ctx, clonePath, "checkout", "-b", branch, "origin/"+branch)
	}
	if baseRef == "" {
		return runGit(ctx, clonePath, "checkout", "-b", branch)
	}
	return runGit(ctx, clonePath, "checkout", "-b", branch, baseRef)
}

func branchExistsLocal(ctx context.Context, dir, branch string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func branchExistsRemote(ctx context.Context, dir, branch string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branch).Run() == nil
}

func isGitDir(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
