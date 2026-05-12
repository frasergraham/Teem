package provisioner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/frasergraham/teem/internal/transport"
)

// LocalProvisioner places workers on the current host.
//
// By default each agent runs in its own git worktree branched off the
// leader's repo HEAD. The worktree lives at WorktreeBase/<agent-id> on
// branch teem/<agent-id>. This gives every agent isolation and a
// reviewable branch.
//
// Two opt-outs:
//   - If the team spec's WorkingDir is non-empty, the agent runs there raw
//     (no worktree). Old behavior; useful for advanced users.
//   - If RepoRoot is empty (leader was not run inside a git repo), the
//     provisioner falls back to the raw WorkingDir path. With both empty,
//     Provision returns an error.
type LocalProvisioner struct {
	// RepoRoot is the leader's git repo root. Empty disables auto-worktree.
	RepoRoot string
	// WorktreeBase is the directory under which agent worktrees are
	// created. Empty disables auto-worktree.
	WorktreeBase string

	// mu serializes git worktree mutations. Concurrent SpawnByRole calls
	// must not race the git index.
	mu sync.Mutex
}

// NewLocalProvisioner constructs a LocalProvisioner with worktree support.
// Pass empty strings to disable.
func NewLocalProvisioner(repoRoot, worktreeBase string) *LocalProvisioner {
	return &LocalProvisioner{RepoRoot: repoRoot, WorktreeBase: worktreeBase}
}

// ErrLocalNoWorkingDir is returned when neither the YAML supplies a
// working_dir nor the leader is running inside a git repo.
var ErrLocalNoWorkingDir = errors.New("local agent requires either working_dir in YAML or that the leader is running inside a git repo")

func (p *LocalProvisioner) Provision(ctx context.Context, spec AgentSpec) (*Agent, error) {
	// Persistent local agents are not "provisioned" — the operator runs
	// teem-worker themselves on this host (or another) at tailnet
	// hostname teem-<id>. We just return enough metadata for the
	// spawner to build an HTTPExecutor pointing at it.
	if spec.IsPersistent() {
		return &Agent{
			ID:          spec.ID,
			Role:        spec.Role,
			Backend:     BackendLocal,
			Lifecycle:   spec.Lifecycle,
			TailnetHost: "teem-" + spec.ID,
			MCPs:        spec.MCPs,
		}, nil
	}

	workDir := spec.WorkingDir
	var branch string

	if workDir == "" {
		if p.RepoRoot == "" || p.WorktreeBase == "" {
			return nil, ErrLocalNoWorkingDir
		}
		workDir = filepath.Join(p.WorktreeBase, spec.ID)
		branch = "teem/" + spec.ID
		p.mu.Lock()
		err := EnsureWorktree(ctx, p.RepoRoot, workDir, branch)
		p.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("local: prepare worktree for %s: %w", spec.ID, err)
		}
	}

	a := &Agent{
		ID:         spec.ID,
		Role:       spec.Role,
		WorkingDir: workDir,
		Backend:    BackendLocal,
		Lifecycle:  spec.Lifecycle,
		Transport:  transport.LocalTransport{},
		MCPs:       spec.MCPs,
	}
	if branch != "" {
		// We stash the worktree path on Cloud so Teardown knows what to
		// remove. CloudPlacement is named for the cloud case; we reuse it
		// rather than introduce another struct for a single string.
		a.Cloud = &CloudPlacement{TaskARN: workDir}
	}
	return a, nil
}

func (p *LocalProvisioner) Teardown(ctx context.Context, a *Agent) error {
	if a == nil || a.Cloud == nil || a.Cloud.TaskARN == "" {
		return nil
	}
	if a.IsPersistent() {
		return nil
	}
	if p.RepoRoot == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return RemoveWorktree(ctx, p.RepoRoot, a.Cloud.TaskARN)
}
