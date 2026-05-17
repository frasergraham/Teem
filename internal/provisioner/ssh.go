package provisioner

import (
	"context"
	"fmt"

	"github.com/frasergraham/teem/internal/transport"
)

// SSHProvisioner places workers on a remote host reachable by SSH.
//
// The host is expected to have `claude` already installed and on PATH.
// Auth uses the local ssh-agent (SSH_AUTH_SOCK).
type SSHProvisioner struct{}

func (SSHProvisioner) Provision(_ context.Context, spec AgentSpec) (*Agent, error) {
	if spec.SSHTarget == "" {
		return nil, fmt.Errorf("ssh: agent %q has no ssh_target", spec.ID)
	}
	return &Agent{
		ID:         spec.ID,
		Role:       spec.Role,
		WorkingDir: spec.WorkingDir,
		Backend:    BackendSSH,
		Transport:  transport.SSHTransport{Target: spec.SSHTarget},
		MCPs:       spec.MCPs,
		Skill:      spec.Skill,
		Model:      spec.Model,
	}, nil
}

func (SSHProvisioner) Teardown(_ context.Context, _ *Agent) error { return nil }
