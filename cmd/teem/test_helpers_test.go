package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/leaderstatus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

// newFullTestTeam returns a registeredTeam populated with plan,
// leaderstatus, and an audit sink so the various handler tests can
// exercise real stores. Used across the cmd/teem test suite; lives in
// this dedicated helper file so deleting feature-specific test files
// (e.g. the SSR dashboard tests removed in Phase 4) doesn't strand the
// other tests that depend on it.
func newFullTestTeam(t *testing.T, name string) *registeredTeam {
	t.Helper()
	dir := t.TempDir()
	sink, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	planStore, err := plan.Open(filepath.Join(dir, "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = planStore.Close() })
	ls, err := leaderstatus.Open(filepath.Join(dir, "leader_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	tm := &team.Team{
		ID:   name,
		Name: name,
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 4},
			{Role: "reviewer", Placement: "fargate", MaxConcurrent: 2},
		},
	}
	return &registeredTeam{
		team:           tm,
		auditSink:      sink,
		plan:           planStore,
		leaderStatus:   ls,
		registry:       mcpsrv.NewRegistry(),
		transcriptsDir: filepath.Join(dir, "transcripts"),
		registered:     time.Now().Add(-2 * time.Hour),
	}
}
