package provisioner

import (
	"context"
	"testing"

	"github.com/frasergraham/teem/internal/team"
)

// TestSSHProvision_PropagatesSkill exercises the FromTeamSpec →
// SSHProvisioner.Provision path and asserts the Skill field round-trips
// onto the resulting Agent. Regression guard for the bug where the SSH
// and Fargate Agent{} constructions silently dropped spec.Skill while
// the local path had been wired through.
func TestSSHProvision_PropagatesSkill(t *testing.T) {
	teamSpec := team.AgentSpec{
		ID:        "worker-indi",
		Role:      "worker",
		SSHTarget: "user@host",
		Skill:     "linear",
	}
	pSpec := FromTeamSpec(teamSpec)
	if pSpec.Skill != "linear" {
		t.Fatalf("FromTeamSpec dropped Skill: got %q, want %q", pSpec.Skill, "linear")
	}

	agent, err := SSHProvisioner{}.Provision(context.Background(), pSpec)
	if err != nil {
		t.Fatalf("SSH Provision: %v", err)
	}
	if agent.Skill != "linear" {
		t.Errorf("SSHProvisioner.Provision dropped Skill: got %q, want %q", agent.Skill, "linear")
	}
}
