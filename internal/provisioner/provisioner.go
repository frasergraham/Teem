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
	BackendRailway Backend = "railway"
)

// AgentSpec is the input to Provision — the subset of team.AgentSpec the
// provisioner needs to place a worker, plus the chosen Backend.
type AgentSpec struct {
	ID         string
	Role       string
	WorkingDir string
	SSHTarget  string
	Backend    Backend
	MCPs       []team.MCPRef
}

// Agent is the result of Provision: a placed worker, ready to run
// subprocesses via Transport.
type Agent struct {
	ID         string
	Role       string
	WorkingDir string
	Backend    Backend
	// TailnetHost is the worker's hostname on the tailnet, if any. Empty
	// for local backends.
	TailnetHost string
	Transport   transport.Transport
	MCPs        []team.MCPRef
}

// Provisioner places agents on hosts.
type Provisioner interface {
	Provision(ctx context.Context, spec AgentSpec) (*Agent, error)
	Teardown(ctx context.Context, a *Agent) error
}

// ErrNotImplemented is returned by stubbed backends (currently Railway).
var ErrNotImplemented = errors.New("provisioner: not implemented")

// FromTeamSpec converts a team.AgentSpec into a provisioner AgentSpec,
// inferring Backend from local/ssh_target. Future cloud backends will be
// selected by an explicit field on team.AgentSpec.
func FromTeamSpec(a team.AgentSpec) AgentSpec {
	spec := AgentSpec{
		ID:         a.ID,
		Role:       a.Role,
		WorkingDir: a.WorkingDir,
		SSHTarget:  a.SSHTarget,
		MCPs:       a.MCPs,
	}
	switch {
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
		return LocalProvisioner{}, nil
	case BackendSSH:
		return SSHProvisioner{}, nil
	case BackendRailway:
		return RailwayProvisioner{}, nil
	default:
		return nil, fmt.Errorf("provisioner: unknown backend %q", spec.Backend)
	}
}
