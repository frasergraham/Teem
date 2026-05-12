package provisioner

import (
	"context"

	"github.com/frasergraham/teem/internal/transport"
)

// LocalProvisioner places workers on the current host.
type LocalProvisioner struct{}

func (LocalProvisioner) Provision(_ context.Context, spec AgentSpec) (*Agent, error) {
	return &Agent{
		ID:         spec.ID,
		Role:       spec.Role,
		WorkingDir: spec.WorkingDir,
		Backend:    BackendLocal,
		Transport:  transport.LocalTransport{},
		MCPs:       spec.MCPs,
	}, nil
}

func (LocalProvisioner) Teardown(_ context.Context, _ *Agent) error { return nil }
