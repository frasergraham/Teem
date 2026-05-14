package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/team"
)

// TestRoster_PersistsAcrossSpawner exercises the post-T9 equivalent
// of "the counter survives a daemon restart": after re-creating the
// Spawner with the same persisted roster, a fresh SpawnByRole skips
// previously-allocated wordlist entries and lands on the next fresh
// one.
func TestRoster_PersistsAcrossSpawner(t *testing.T) {
	dir := t.TempDir()
	rosterPath := filepath.Join(dir, "roster.json")

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
		rost, err := roster.Open(rosterPath)
		if err != nil {
			t.Fatalf("roster open: %v", err)
		}
		return NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{
			Roster: rost,
		})
	}

	sp := makeSpawner()
	id1, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn 1: %v", err)
	}
	if id1 != "worker-ada" {
		t.Errorf("id1 = %q, want worker-ada", id1)
	}
	id2, _ := sp.SpawnByRole(context.Background(), "worker")
	if id2 != "worker-blake" {
		t.Errorf("id2 = %q, want worker-blake", id2)
	}

	// "Restart": drop the spawner, build a new one against the
	// same persisted roster. Allocation must skip the names
	// already on disk.
	sp.Stop()
	sp = makeSpawner()
	id3, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn after restart: %v", err)
	}
	if id3 != "worker-cleo" {
		t.Errorf("post-restart id = %q, want worker-cleo (roster must persist allocations)", id3)
	}
}

// TestRoster_LegacyAuditMigration verifies that historical
// `<role>-N` ids surfaced via the legacy archetype-seq.json file
// land in the roster as reincarnation candidates — they don't get
// reused as fresh ids, but they're not lost either.
func TestRoster_LegacyAuditMigration(t *testing.T) {
	dir := t.TempDir()
	rosterPath := filepath.Join(dir, "roster.json")
	legacySeqPath := filepath.Join(dir, "archetype-seq.json")
	// Seed a legacy archetype-seq.json claiming five historical workers.
	if err := os.WriteFile(legacySeqPath, []byte(`{"worker": 5}`), 0o600); err != nil {
		t.Fatal(err)
	}

	rost, err := roster.Open(rosterPath)
	if err != nil {
		t.Fatalf("roster open: %v", err)
	}
	added := rost.MigrateLegacy(legacySeqPath, "", []string{"worker"}, nil)
	if added != 5 {
		t.Errorf("migrated %d, want 5", added)
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
		Roster: rost,
	})
	// First spawn should be a fresh wordlist entry — legacy ids
	// don't steal the fresh path.
	id, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id != "worker-ada" {
		t.Errorf("after migration, first spawn = %q, want worker-ada (legacy ids must NOT be preferred over fresh wordlist)", id)
	}
}

// TestRoster_LegacyTranscriptsMigration verifies that a synthesized
// pre-T9 transcripts layout (transcripts/worker-3/<job>.jsonl) feeds
// the roster so the historical id is preserved for reincarnation and
// the numeric counter is bumped past it.
func TestRoster_LegacyTranscriptsMigration(t *testing.T) {
	dir := t.TempDir()
	rosterPath := filepath.Join(dir, "roster.json")
	transcriptsDir := filepath.Join(dir, "transcripts")
	// Synthesize legacy worker-3 layout.
	if err := os.MkdirAll(filepath.Join(transcriptsDir, "worker-3"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcriptsDir, "worker-3", "j-legacy.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rost, err := roster.Open(rosterPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := rost.MigrateLegacy("", transcriptsDir, []string{"worker"}, nil); n != 1 {
		t.Errorf("MigrateLegacy returned %d, want 1", n)
	}
	if !rost.IsKnown("worker-3") {
		t.Errorf("worker-3 not registered in roster after transcripts migration")
	}

	tm := &team.Team{
		Name:   "x",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 5, WorkingDir: t.TempDir()},
		},
	}
	bs := bus.NewMemBus()
	defer bs.Close()
	sp := NewSpawner(context.Background(), tm, bs, mcpsrv.NewRegistry(), Config{Roster: rost})

	id, err := sp.SpawnByRole(context.Background(), "worker")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id == "worker-3" {
		t.Errorf("first post-migration spawn reused legacy id %q — wordlist should win", id)
	}
	if id != "worker-ada" {
		t.Errorf("first post-migration spawn = %q, want worker-ada", id)
	}
}
