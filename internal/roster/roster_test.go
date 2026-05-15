package roster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// TestRoster_DuplicateInsertIsIdempotent re-registers the same
// (canonical id, role) pair twice and confirms the second call
// doesn't create a second entry — the roster keys on canonical id,
// so the second insert merges onto the first.
func TestRoster_DuplicateInsertIsIdempotent(t *testing.T) {
	r, _ := Open("")
	first, err := r.ReserveNamed("worker-ada", "worker")
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if first.ID != "worker-ada" {
		t.Errorf("first.ID = %q, want worker-ada", first.ID)
	}
	second, err := r.ReserveNamed("worker-ada", "worker")
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second.ID = %q, want %q", second.ID, first.ID)
	}
	// Snapshot should have exactly one entry for (worker, ada).
	count := 0
	for _, e := range r.Snapshot() {
		if e.Role == "worker" && e.ID == "worker-ada" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate insert produced %d worker-ada entries, want 1", count)
	}
}

// TestRoster_DuplicateAcrossRolesIsAllowed confirms the scope key is
// (name, role), not name alone — `ada` can be both a worker and a
// reviewer without conflict.
func TestRoster_DuplicateAcrossRolesIsAllowed(t *testing.T) {
	r, _ := Open("")
	w, err := r.ReserveNamed("worker-ada", "worker")
	if err != nil {
		t.Fatalf("worker reserve: %v", err)
	}
	rev, err := r.ReserveNamed("reviewer-ada", "reviewer")
	if err != nil {
		t.Fatalf("reviewer reserve: %v", err)
	}
	if w.ID == rev.ID {
		t.Errorf("worker and reviewer collided on id %q", w.ID)
	}
	if w.ID != "worker-ada" || rev.ID != "reviewer-ada" {
		t.Errorf("ids = %q / %q, want worker-ada / reviewer-ada", w.ID, rev.ID)
	}
}

// TestRoster_DedupOnOpen verifies that a roster persisted in the old
// shape (bare named entry + role-prefixed wordlist entry, same name +
// role) is collapsed onto its canonical form at load time, preferring
// the older first_seen and OR-ing in_use.
func TestRoster_DedupOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")
	older := time.Date(2026, 5, 14, 6, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 14, 14, 0, 0, 0, time.UTC)
	preseed := onDisk{
		Entries: map[string]Entry{
			"ada": {
				ID: "ada", Role: "worker", InUse: false,
				FirstSeen: newer, LastUsedAt: newer, Source: SourceNamed,
			},
			"worker-ada": {
				ID: "worker-ada", Role: "worker", InUse: false,
				FirstSeen: older, LastUsedAt: older, Source: SourceWordlist,
			},
		},
	}
	body, _ := json.Marshal(preseed)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("after dedup got %d entries, want 1: %+v", len(snap), snap)
	}
	got := snap[0]
	if got.ID != "worker-ada" {
		t.Errorf("dedup id = %q, want worker-ada", got.ID)
	}
	if !got.FirstSeen.Equal(older) {
		t.Errorf("first_seen = %s, want older %s", got.FirstSeen, older)
	}
	if !got.LastUsedAt.Equal(newer) {
		t.Errorf("last_used_at = %s, want newer %s", got.LastUsedAt, newer)
	}
	if got.Source != SourceWordlist {
		t.Errorf("source = %q, want %q (wordlist wins over named)", got.Source, SourceWordlist)
	}
}

// TestRoster_DedupOnOpen_InUseORMerge covers the OR-merge branch:
// when the bare entry and canonical sibling disagree on InUse, the
// merged result must reflect "either is live" so a still-running
// worker isn't silently retired by dedup.
func TestRoster_DedupOnOpen_InUseORMerge(t *testing.T) {
	for _, tc := range []struct {
		name            string
		bareInUse       bool
		canonicalInUse  bool
		wantMergedInUse bool
	}{
		{"bare-live", true, false, true},
		{"canonical-live", false, true, true},
		{"both-live", true, true, true},
		{"neither-live", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "roster.json")
			ts := time.Date(2026, 5, 14, 14, 0, 0, 0, time.UTC)
			preseed := onDisk{
				Entries: map[string]Entry{
					"ada": {
						ID: "ada", Role: "worker", InUse: tc.bareInUse,
						FirstSeen: ts, LastUsedAt: ts, Source: SourceNamed,
					},
					"worker-ada": {
						ID: "worker-ada", Role: "worker", InUse: tc.canonicalInUse,
						FirstSeen: ts, LastUsedAt: ts, Source: SourceWordlist,
					},
				},
			}
			body, _ := json.Marshal(preseed)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			snap := r.Snapshot()
			if len(snap) != 1 {
				t.Fatalf("after dedup got %d entries, want 1: %+v", len(snap), snap)
			}
			if snap[0].InUse != tc.wantMergedInUse {
				t.Errorf("merged InUse = %v, want %v (bare=%v, canonical=%v)",
					snap[0].InUse, tc.wantMergedInUse, tc.bareInUse, tc.canonicalInUse)
			}
		})
	}
}

// TestRoster_DedupOnOpen_BareOnly verifies that a bare-name entry
// with no canonical sibling is renamed (not just deleted) so legacy
// data isn't lost.
func TestRoster_DedupOnOpen_BareOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster.json")
	ts := time.Date(2026, 5, 14, 14, 0, 0, 0, time.UTC)
	preseed := onDisk{
		Entries: map[string]Entry{
			"bob": {
				ID: "bob", Role: "reviewer", InUse: false,
				FirstSeen: ts, LastUsedAt: ts, Source: SourceNamed,
			},
		},
	}
	body, _ := json.Marshal(preseed)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsKnown("reviewer-bob") {
		t.Error("bare bob should have been renamed to reviewer-bob")
	}
	if r.IsKnown("bob") {
		t.Error("bare bob should no longer exist")
	}
}

// TestRoster_FindByBareName guards the helper that powers bare-name
// socket adoption. The reconcile path treats len(matches)!=1 as
// "can't safely adopt", so the multi-role case must be exercised
// directly here.
func TestRoster_FindByBareName(t *testing.T) {
	cases := []struct {
		name     string
		seed     []struct{ id, role string }
		bareName string
		wantIDs  []string
	}{
		{
			name:     "zero matches",
			seed:     []struct{ id, role string }{{"worker-ada", "worker"}},
			bareName: "ghost",
			wantIDs:  nil,
		},
		{
			name:     "one match",
			seed:     []struct{ id, role string }{{"worker-ada", "worker"}},
			bareName: "ada",
			wantIDs:  []string{"worker-ada"},
		},
		{
			name:     "two matches across roles",
			seed:     []struct{ id, role string }{{"worker-ada", "worker"}, {"reviewer-ada", "reviewer"}},
			bareName: "ada",
			wantIDs:  []string{"reviewer-ada", "worker-ada"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := Open("")
			for _, s := range tc.seed {
				if _, err := r.ReserveNamed(s.id, s.role); err != nil {
					t.Fatalf("seed %q/%q: %v", s.id, s.role, err)
				}
			}
			got := r.FindByBareName(tc.bareName)
			gotIDs := make([]string, 0, len(got))
			for _, e := range got {
				gotIDs = append(gotIDs, e.ID)
			}
			sort.Strings(gotIDs)
			if len(gotIDs) != len(tc.wantIDs) {
				t.Fatalf("got %d matches %v, want %d %v", len(gotIDs), gotIDs, len(tc.wantIDs), tc.wantIDs)
			}
			for i, id := range gotIDs {
				if id != tc.wantIDs[i] {
					t.Errorf("match[%d] = %q, want %q", i, id, tc.wantIDs[i])
				}
			}
		})
	}
}

func TestCanonicalID(t *testing.T) {
	cases := []struct {
		role, name, want string
	}{
		{"worker", "ada", "worker-ada"},
		{"worker", "worker-ada", "worker-ada"},
		{"reviewer", "ada", "reviewer-ada"},
		{"reviewer", "worker-ada", "reviewer-worker-ada"}, // wrong-role prefix isn't stripped
		{"", "ada", ""},
		{"worker", "", ""},
	}
	for _, c := range cases {
		got := CanonicalID(c.role, c.name)
		if got != c.want {
			t.Errorf("CanonicalID(%q, %q) = %q, want %q", c.role, c.name, got, c.want)
		}
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
