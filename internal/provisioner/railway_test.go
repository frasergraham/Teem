package provisioner

import (
	"context"
	"errors"
	"testing"
)

func TestRailwayProvisioner_StubbedUntilFollowUp(t *testing.T) {
	p := RailwayProvisioner{}
	if _, err := p.Provision(context.Background(), AgentSpec{ID: "x", Role: "y", Backend: BackendRailway}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Provision: got %v, want ErrNotImplemented", err)
	}
	if err := p.Teardown(context.Background(), &Agent{ID: "x"}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Teardown: got %v, want ErrNotImplemented", err)
	}
}
