package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/team"
)

// TestArchetypeSeq_PersistsAcrossSpawner exercises the core fix:
// after a "daemon restart" (re-creating the Spawner with the same
// state file), a fresh SpawnByRole produces the NEXT id, not the
// first one again.
func TestArchetypeSeq_PersistsAcrossSpawner(t *testing.T) {
	dir := t.TempDir()
	seqPath := filepath.Join(dir, "archetype-seq.json")

	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 5, WorkingDir: t.TempDir()},
		},
	}
	makeSpawner := func() *Spawner {
		bs := bus.NewMemBus()
		t.Cleanup(func() { bs.Close() })
		return NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{
			ArchetypeSeqPath: seqPath,
		})
	}

	sp := makeSpawner()
	id1, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn 1: %v", err)
	}
	if id1 != "worker-1" {
		t.Errorf("id1 = %q, want worker-1", id1)
	}
	id2, _ := sp.SpawnByRole(context.Background(), "worker")
	if id2 != "worker-2" {
		t.Errorf("id2 = %q, want worker-2", id2)
	}

	// "Restart": drop the spawner, build a new one against the
	// same persisted file. The counter must survive.
	sp.Stop()
	sp = makeSpawner()
	id3, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn after restart: %v", err)
	}
	if id3 != "worker-3" {
		t.Errorf("post-restart id = %q, want worker-3 (state file must persist counter)", id3)
	}
}

// TestArchetypeSeq_AuditFallback verifies that even when the state
// file is missing/corrupt, the audit log's historical ids reseed the
// counter so we never reuse a number.
func TestArchetypeSeq_AuditFallback(t *testing.T) {
	dir := t.TempDir()

	sink, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	// Seed a "historical" worker-5 — as if a previous daemon run had
	// spawned five workers and is now gone, along with the seq file.
	if err := sink.Write(audit.Event{AgentID: "worker-5", Kind: audit.KindHeartbeat}); err != nil {
		t.Fatal(err)
	}

	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 10, WorkingDir: t.TempDir()},
		},
	}
	bs := bus.NewMemBus()
	defer bs.Close()
	sp := NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{
		AuditSink: sink,
		// ArchetypeSeqPath intentionally blank — exercise the audit
		// fallback in isolation.
	})

	id, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id != "worker-6" {
		t.Errorf("with worker-5 in audit, next id should be worker-6, got %q", id)
	}
}

func TestParseInstanceID(t *testing.T) {
	cases := []struct {
		in   string
		role string
		n    int
		ok   bool
	}{
		{"worker-1", "worker", 1, true},
		{"reviewer-12", "reviewer", 12, true},
		{"security-reviewer-3", "security-reviewer", 3, true},
		{"worker", "", 0, false},
		{"worker-", "", 0, false},
		{"-3", "", 0, false},
		{"worker-abc", "", 0, false},
	}
	for _, c := range cases {
		role, n, ok := parseInstanceID(c.in)
		if ok != c.ok || (ok && (role != c.role || n != c.n)) {
			t.Errorf("parseInstanceID(%q) = (%q, %d, %v), want (%q, %d, %v)", c.in, role, n, ok, c.role, c.n, c.ok)
		}
	}
}
