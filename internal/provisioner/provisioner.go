package provisioner

import (
	"context"
	"errors"
	"fmt"

	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/transport"
)

// Backend names the placement strategy for an agent.
type Backend string

const (
	BackendLocal   Backend = "local"
	BackendSSH     Backend = "ssh"
	BackendFargate Backend = "fargate"
)

// AgentSpec is the input to Provision — the subset of team.AgentSpec the
// provisioner needs to place a worker, plus the chosen Backend.
type AgentSpec struct {
	ID         string
	Role       string
	WorkingDir string
	SSHTarget  string
	Backend    Backend
	// Lifecycle is "ephemeral" (default) or "persistent". Persistent
	// agents are not torn down on Spawner.Stop and may be reconciled
	// across `teem chat` sessions.
	Lifecycle string
	MCPs      []team.MCPRef
}

// IsPersistent reports whether the spec carries a persistent lifecycle.
func (s AgentSpec) IsPersistent() bool { return s.Lifecycle == "persistent" }

// Agent is the result of Provision: a placed worker, ready to run
// subprocesses via Transport.
type Agent struct {
	ID         string
	Role       string
	WorkingDir string
	Backend    Backend
	Lifecycle  string
	// TailnetHost is the worker's hostname on the tailnet, if any. Empty
	// for local backends.
	TailnetHost string
	// SocketPath is the unix-socket path the spawner should dial when
	// the worker speaks HTTP locally (ephemeral local backend after
	// the teem-worker subprocess migration). Mutually exclusive with
	// Transport.
	SocketPath string
	// Transport is set for SSH agents; nil for cloud agents and
	// socket-based local agents (the spawner builds an HTTPExecutor
	// instead).
	Transport transport.Transport
	MCPs      []team.MCPRef
	// Cloud holds backend-specific identifiers needed for Teardown.
	Cloud *CloudPlacement
	// WorktreeBranch, when non-empty, names the local branch the
	// LocalProvisioner created for this agent's worktree (always
	// `teem/<id>`). Used by spawner cleanup hooks to delete the branch
	// when the worker stops; empty for backends that don't own a
	// per-agent local branch (Fargate, SSH, persistent local).
	WorktreeBranch string
	// Stopped is true when the worker has already terminated under
	// its own steam (typically the self-exit-after-idle path). The
	// LocalProvisioner uses this to skip the /shutdown POST and
	// SIGTERM-by-pid fallback during Teardown and only clean up the
	// on-disk artefacts (socket, pidfile, worktree).
	Stopped bool
}

// IsPersistent reports whether the agent carries a persistent lifecycle.
func (a *Agent) IsPersistent() bool { return a != nil && a.Lifecycle == "persistent" }

// CloudPlacement carries identifiers a cloud provisioner needs to find or
// stop a previously-launched agent.
type CloudPlacement struct {
	// TaskARN is the ECS task ARN for fargate-backed agents.
	TaskARN string
}

// Provisioner places agents on hosts.
type Provisioner interface {
	Provision(ctx context.Context, spec AgentSpec) (*Agent, error)
	Teardown(ctx context.Context, a *Agent) error
}

// ErrAgentStopped signals that the underlying placement has stopped (e.g.
// ECS task moved to STOPPED). Returned by Watcher.CheckLiveness.
var ErrAgentStopped = errors.New("provisioner: agent stopped")

// Watcher is an optional capability for backends whose underlying compute
// can die out-of-band (Fargate task killed, spot interruption, …). The
// spawner type-asserts and runs a slow polling goroutine per agent.
type Watcher interface {
	// CheckLiveness returns nil if the agent is still alive on the
	// backend, ErrAgentStopped if it has stopped, or another error for
	// transient lookup failures.
	CheckLiveness(ctx context.Context, a *Agent) error
}

// FromTeamSpec converts a team.AgentSpec into a provisioner AgentSpec,
// inferring Backend from local/ssh_target/backend. The team package has
// already validated that exactly one placement marker is set.
func FromTeamSpec(a team.AgentSpec) AgentSpec {
	spec := AgentSpec{
		ID:         a.ID,
		Role:       a.Role,
		WorkingDir: a.WorkingDir,
		SSHTarget:  a.SSHTarget,
		Lifecycle:  a.LifecycleOrDefault(),
		MCPs:       a.MCPs,
	}
	switch {
	case a.Backend != "":
		spec.Backend = Backend(a.Backend)
	case a.Local:
		spec.Backend = BackendLocal
	case a.SSHTarget != "":
		spec.Backend = BackendSSH
	default:
		spec.Backend = BackendLocal
	}
	return spec
}

// Select returns a Provisioner appropriate for spec.Backend. Unknown
// backends produce an error so callers don't silently fall back.
func Select(spec AgentSpec) (Provisioner, error) {
	switch spec.Backend {
	case BackendLocal:
		// Fallback constructor with no worktree support. Spawners that
		// want auto-worktree should build LocalProvisioner via
		// NewLocalProvisioner with the leader's repo root.
		return &LocalProvisioner{}, nil
	case BackendSSH:
		return SSHProvisioner{}, nil
	case BackendFargate:
		// FargateProvisioner needs runtime deps (AWS client + leader-side
		// tailnet HTTP client + worker token). It is constructed by the
		// spawner via NewFargateProvisioner, not here.
		return nil, fmt.Errorf("provisioner: fargate must be constructed via NewFargateProvisioner")
	default:
		return nil, fmt.Errorf("provisioner: unknown backend %q", spec.Backend)
	}
}
