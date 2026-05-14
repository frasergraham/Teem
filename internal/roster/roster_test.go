package roster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAllocate_FreshWordlist(t *testing.T) {
	r, _ := Open("")
	id1 := r.Allocate("worker")
	id2 := r.Allocate("worker")
	if id1 == id2 {
		t.Fatalf("two consecutive allocations returned the same id: %s", id1)
	}
	if !strings.HasPrefix(id1, "worker-") || !strings.HasPrefix(id2, "worker-") {
		t.Fatalf("expected worker- prefix, got %q / %q", id1, id2)
	}
	// First wordlist entry is "ada" — the deterministic walk-order
	// gives "worker-ada" first.
	if id1 != "worker-ada" {
		t.Errorf("first allocation = %q, want worker-ada", id1)
	}
}

func TestAllocate_RoleScoping(t *testing.T) {
	r, _ := Open("")
	w := r.Allocate("worker")
	rev := r.Allocate("reviewer")
	if w != "worker-ada" || rev != "reviewer-ada" {
		t.Errorf("role scoping broken: worker=%q reviewer=%q", w, rev)
	}
}

func TestAllocate_ReincarnationLRU(t *testing.T) {
	r, _ := Open("")
	// Spawn three, retire all three with staggered timestamps.
	a := r.Allocate("worker") // worker-ada
	b := r.Allocate("worker") // worker-blake
	c := r.Allocate("worker") // worker-cleo
	// Force their LastUsedAt directly (Release uses time.Now and the
	// test would race on monotonic-time resolution).
	now := time.Now().UTC()
	r.mu.Lock()
	r.entries[a] = Entry{ID: a, Role: "worker", InUse: false, LastUsedAt: now.Add(-3 * time.Hour)}
	r.entries[b] = Entry{ID: b, Role: "worker", InUse: false, LastUsedAt: now.Add(-1 * time.Hour)}
	r.entries[c] = Entry{ID: c, Role: "worker", InUse: false, LastUsedAt: now.Add(-2 * time.Hour)}
	r.mu.Unlock()

	// Pin every other wordlist entry so only reincarnation is left.
	for _, base := range r.wordlist {
		id := "worker-" + base
		r.mu.Lock()
		if _, ok := r.entries[id]; !ok {
			r.entries[id] = Entry{ID: id, Role: "worker", InUse: true, LastUsedAt: now}
		}
		r.mu.Unlock()
	}

	// Allocate again; least-recently-used among a/b/c is a (3h ago).
	got := r.Allocate("worker")
	if got != a {
		t.Errorf("expected LRU reincarnation of %q, got %q", a, got)
	}
}

func TestAllocate_NumericFallback(t *testing.T) {
	r, _ := Open("")
	// Pin every wordlist entry for "worker" (in use, so reincarnation
	// can't pick them up either).
	now := time.Now().UTC()
	r.mu.Lock()
	for _, base := range r.wordlist {
		id := "worker-" + base
		r.entries[id] = Entry{ID: id, Role: "worker", InUse: true, LastUsedAt: now}
	}
	r.mu.Unlock()
	got := r.Allocate("worker")
	if got != "worker-1" {
		t.Errorf("numeric fallback id = %q, want worker-1", got)
	}
	got2 := r.Allocate("worker")
	if got2 != "worker-2" {
		t.Errorf("second numeric fallback id = %q, want worker-2", got2)
	}
}

func TestRelease_MarksAvailable(t *testing.T) {
	r, _ := Open("")
	id := r.Allocate("worker")
	r.Release(id)
	r.mu.Lock()
	e := r.entries[id]
	r.mu.Unlock()
	if e.InUse {
		t.Errorf("Release didn't clear InUse for %q", id)
	}
}

func TestMarkInUse_BumpsNumericCounter(t *testing.T) {
	r, _ := Open("")
	r.MarkInUse("worker-5", "worker")
	r.mu.Lock()
	n := r.nextNumeric["worker"]
	r.mu.Unlock()
	if n != 5 {
		t.Errorf("MarkInUse(worker-5) failed to bump counter; got %d want 5", n)
	}
}

func TestPersistence_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Allocate("worker")
	if id != "worker-ada" {
		t.Fatalf("got %q want worker-ada", id)
	}
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.IsKnown(id) {
		t.Errorf("reopened roster doesn't know %q", id)
	}
	// Next allocation for the same role must skip worker-ada and
	// give worker-blake (the next fresh wordlist entry).
	id2 := r2.Allocate("worker")
	if id2 != "worker-blake" {
		t.Errorf("post-reload allocation = %q, want worker-blake", id2)
	}
}

func TestMigrateLegacy_ArchetypeSeq(t *testing.T) {
	dir := t.TempDir()
	seqPath := filepath.Join(dir, "archetype-seq.json")
	body, _ := json.Marshal(map[string]int{"worker": 3, "reviewer": 1})
	if err := os.WriteFile(seqPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := Open(filepath.Join(dir, "roster.json"))
	added := r.MigrateLegacy(seqPath, "", []string{"worker", "reviewer"}, nil)
	if added != 4 {
		t.Errorf("migrated %d, want 4 (worker-1/2/3 + reviewer-1)", added)
	}
	for _, id := range []string{"worker-1", "worker-2", "worker-3", "reviewer-1"} {
		if !r.IsKnown(id) {
			t.Errorf("missing migrated entry %q", id)
		}
	}
	// Numeric counter must be high enough to avoid collision.
	r.mu.Lock()
	if r.nextNumeric["worker"] < 3 {
		t.Errorf("nextNumeric[worker] = %d, want ≥ 3", r.nextNumeric["worker"])
	}
	r.mu.Unlock()
}

func TestMigrateLegacy_TranscriptsDir(t *testing.T) {
	dir := t.TempDir()
	// Synthesize a legacy transcripts layout: transcripts/worker-3/<job>.jsonl
	tdir := filepath.Join(dir, "transcripts")
	for _, id := range []string{"worker-3", "reviewer-2"} {
		if err := os.MkdirAll(filepath.Join(tdir, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tdir, id, "j-abc.jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := Open(filepath.Join(dir, "roster.json"))
	added := r.MigrateLegacy("", tdir, []string{"worker", "reviewer"}, nil)
	if added != 2 {
		t.Errorf("migrated %d, want 2", added)
	}
	for _, id := range []string{"worker-3", "reviewer-2"} {
		if !r.IsKnown(id) {
			t.Errorf("missing migrated entry %q", id)
		}
	}
	// After migration, allocating "worker" should still prefer a
	// fresh wordlist entry over reincarnating worker-3.
	id := r.Allocate("worker")
	if id != "worker-ada" {
		t.Errorf("fresh allocation after legacy migration = %q, want worker-ada (migration shouldn't steal the fresh path)", id)
	}
}

func TestMigrateLegacy_Idempotent(t *testing.T) {
	dir := t.TempDir()
	tdir := filepath.Join(dir, "transcripts")
	_ = os.MkdirAll(filepath.Join(tdir, "worker-1"), 0o700)
	r, _ := Open(filepath.Join(dir, "roster.json"))
	first := r.MigrateLegacy("", tdir, []string{"worker"}, nil)
	second := r.MigrateLegacy("", tdir, []string{"worker"}, nil)
	if first != 1 || second != 0 {
		t.Errorf("non-idempotent: first=%d second=%d, want first=1 second=0", first, second)
	}
}

func TestRoleFromID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"worker-ada", "worker"},
		{"worker-3", "worker"},
		{"security-reviewer-cleo", "security-reviewer"},
		{"security-reviewer-12", "security-reviewer"},
		{"leader", ""},
		{"", ""},
		{"-bad", ""},
	}
	for _, c := range cases {
		got := RoleFromID(c.in)
		if got != c.want {
			t.Errorf("RoleFromID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
