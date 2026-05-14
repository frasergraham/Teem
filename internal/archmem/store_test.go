package archmem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T, roles ...string) *Store {
	t.Helper()
	dir := t.TempDir()
	rf := func() []string { return roles }
	return New(dir, rf)
}

func TestAppendThenLoad(t *testing.T) {
	s := newStore(t, "worker")
	for i := 0; i < 3; i++ {
		err := s.AppendEntry("worker", Entry{
			Timestamp: time.Date(2026, 5, 13, 12, i, 0, 0, time.UTC),
			AgentID:   "worker-12",
			JobID:     "abc",
			Status:    "done",
			Summary:   "touched executor.go",
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	body, err := s.Load("worker")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(body, "# Recent entries") {
		t.Errorf("missing Recent entries header in body:\n%s", body)
	}
	entries, err := s.LoadEntries("worker")
	if err != nil {
		t.Fatalf("load entries: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3 (body: %q)", len(entries), body)
	}
	if entries[0].AgentID != "worker-12" {
		t.Errorf("entry[0].AgentID = %q", entries[0].AgentID)
	}
}

func TestAtomicRewrite(t *testing.T) {
	s := newStore(t, "worker")
	for i := 0; i < 5; i++ {
		_ = s.AppendEntry("worker", Entry{
			Timestamp: time.Now().UTC().Add(time.Duration(-i*30) * 24 * time.Hour),
			AgentID:   "worker-1",
			JobID:     "j",
			Status:    "done",
			Summary:   "old",
		})
	}
	digest := "Worked on executor and provisioner this fortnight."
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	entries, _ := s.LoadEntries("worker")
	kept := EntriesNewerThan(entries, cutoff)
	fm := Frontmatter{
		Role:             "worker",
		DigestUpdated:    time.Now().UTC(),
		DigestWindowDays: 7,
	}
	if err := s.Rewrite("worker", fm, digest, kept); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	body, _ := s.Load("worker")
	if !strings.Contains(body, "Worked on executor") {
		t.Errorf("digest text missing from body:\n%s", body)
	}
	if !strings.Contains(body, "digest_window_days: 7") {
		t.Errorf("digest_window_days frontmatter missing:\n%s", body)
	}
	got, _ := s.LoadFrontmatter("worker")
	if got.DigestWindowDays != 7 {
		t.Errorf("frontmatter.DigestWindowDays = %d, want 7", got.DigestWindowDays)
	}
	if got.DigestUpdated.IsZero() {
		t.Errorf("digest_updated should be set after rewrite")
	}
	gotEntries, _ := s.LoadEntries("worker")
	if len(gotEntries) != len(kept) {
		t.Errorf("entries after rewrite: got %d want %d", len(gotEntries), len(kept))
	}
}

func TestConcurrentAppend(t *testing.T) {
	s := newStore(t, "worker")
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_ = s.AppendEntry("worker", Entry{
				Timestamp: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
				AgentID:   "worker-1",
				JobID:     "j",
				Status:    "done",
				Summary:   "go",
			})
		}(i)
	}
	wg.Wait()
	entries, err := s.LoadEntries("worker")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != N {
		t.Errorf("got %d entries, want %d", len(entries), N)
	}
}

func TestCorruptFileTolerated(t *testing.T) {
	s := newStore(t, "worker")
	// Pre-seed a half-written file: valid header + frontmatter + two
	// good lines + one junk line.
	path := filepath.Join(s.Dir(), "worker.md")
	_ = os.MkdirAll(s.Dir(), 0o700)
	body := "---\nrole: worker\n---\n# Digest\n\n# Recent entries\n" +
		"- 2026-05-13T10:00:00Z worker-1 job=a done — first\n" +
		"- 2026-05-13T10:01:00Z worker-1 job=b done — second\n" +
		"- not-a-timestamp garbage\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entries, err := s.LoadEntries("worker")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2 (rest dropped)", len(entries))
	}
}

// TestLeaderRoleAlwaysValid pins the rule that "leader" is accepted by
// AppendEntry even when it isn't part of the team's archetype set —
// per-team leader memory is parallel to per-archetype worker memory,
// not a member of it.
func TestLeaderRoleAlwaysValid(t *testing.T) {
	s := newStore(t, "worker") // RolesFunc returns only "worker"
	if err := s.AppendEntry(LeaderRole, Entry{
		Timestamp: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		AgentID:   "leader",
		JobID:     "",
		Status:    "note",
		Summary:   "spawned reviewer-blake to look at T4",
	}); err != nil {
		t.Fatalf("append leader: %v", err)
	}
	body, err := s.Load(LeaderRole)
	if err != nil {
		t.Fatalf("load leader: %v", err)
	}
	if !strings.Contains(body, "role: leader") {
		t.Errorf("frontmatter role missing:\n%s", body)
	}
	if !strings.Contains(body, "spawned reviewer-blake") {
		t.Errorf("entry summary missing:\n%s", body)
	}
	// The file must be co-located with worker.md, not somewhere else.
	if _, err := os.Stat(filepath.Join(s.Dir(), "leader.md")); err != nil {
		t.Errorf("leader.md should exist: %v", err)
	}
}

// TestRoleNameRegex pins the path-traversal guard. Anything that
// doesn't match ^[a-z][a-z0-9_-]*$ must be rejected by AppendEntry
// without writing a file.
func TestRoleNameRegex(t *testing.T) {
	s := newStore(t, "worker", "leader")
	bad := []string{"", "../etc", "Worker", "1abc", "with space", "a/b", "a\\b"}
	for _, role := range bad {
		err := s.AppendEntry(role, Entry{
			Timestamp: time.Now().UTC(),
			AgentID:   "x",
			JobID:     "y",
			Status:    "done",
			Summary:   "z",
		})
		if err == nil {
			t.Errorf("expected error for role %q", role)
		}
	}
}

// TestSummarizerLeaderRole verifies the summariser handles "leader" as
// a first-class role even though it isn't in the archetype set.
func TestSummarizerLeaderRole(t *testing.T) {
	s := newStore(t, "worker")
	now := time.Now().UTC()
	_ = s.AppendEntry(LeaderRole, Entry{
		Timestamp: now.Add(-1 * time.Hour),
		AgentID:   "leader",
		JobID:     "",
		Status:    "note",
		Summary:   "moved T1 to in_review",
	})
	stub := &stubLLM{out: "Leader has been triaging T1."}
	sm := &Summarizer{
		Store:           s,
		Complete:        stub.Complete,
		Roles:           func() []string { return []string{"worker", LeaderRole} },
		RetentionWindow: 7 * 24 * time.Hour,
	}
	if err := sm.summarizeRole(context.Background(), LeaderRole); err != nil {
		t.Fatalf("summarize leader: %v", err)
	}
	body, _ := s.Load(LeaderRole)
	if !strings.Contains(body, "Leader has been triaging T1.") {
		t.Errorf("digest missing from leader body:\n%s", body)
	}
}

func TestUnknownRoleRejected(t *testing.T) {
	s := newStore(t, "worker")
	err := s.AppendEntry("ghost", Entry{
		Timestamp: time.Now().UTC(),
		AgentID:   "ghost-1",
		JobID:     "j",
		Status:    "done",
		Summary:   "should fail",
	})
	if err == nil {
		t.Fatal("expected error for unknown role, got nil")
	}
	// File should NOT be created.
	if _, err := os.Stat(filepath.Join(s.Dir(), "ghost.md")); !os.IsNotExist(err) {
		t.Errorf("ghost.md should not exist: %v", err)
	}
}

func TestSweepTmp(t *testing.T) {
	s := newStore(t, "worker")
	_ = os.MkdirAll(s.Dir(), 0o700)
	tmp := filepath.Join(s.Dir(), "worker.md.tmp")
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}
	s.SweepTmp()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("tmp file should be gone: %v", err)
	}
}

// --- summarizer ----------------------------------------------------------

type stubLLM struct {
	out         string
	reply       string // alias for out; either field works in tests
	prompt      string
	beforeReply func() // fires just before returning a response — for race tests
}

func (c *stubLLM) Complete(_ context.Context, prompt string) (string, error) {
	c.prompt = prompt
	if c.beforeReply != nil {
		c.beforeReply()
	}
	content := c.out
	if content == "" {
		content = c.reply
	}
	return content, nil
}

func TestSummarizerStubLLM(t *testing.T) {
	s := newStore(t, "worker")
	// Two entries inside the window, one outside.
	now := time.Now().UTC()
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-1 * time.Hour), AgentID: "worker-1", JobID: "a", Status: "done", Summary: "recent A"})
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-2 * time.Hour), AgentID: "worker-2", JobID: "b", Status: "done", Summary: "recent B"})
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-30 * 24 * time.Hour), AgentID: "worker-3", JobID: "c", Status: "done", Summary: "ancient"})

	stub := &stubLLM{out: "Digest: the worker pattern is X."}
	sm := &Summarizer{
		Store:           s,
		Complete:        stub.Complete,
		Roles:           func() []string { return []string{"worker"} },
		RetentionWindow: 7 * 24 * time.Hour,
	}
	if err := sm.summarizeRole(context.Background(), "worker"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	body, _ := s.Load("worker")
	if !strings.Contains(body, "Digest: the worker pattern is X.") {
		t.Errorf("digest body missing:\n%s", body)
	}
	if strings.Contains(body, "ancient") {
		t.Errorf("retention pruning failed — old entry still present:\n%s", body)
	}
	if !strings.Contains(body, "recent A") || !strings.Contains(body, "recent B") {
		t.Errorf("kept entries missing from body:\n%s", body)
	}
	fm, _ := s.LoadFrontmatter("worker")
	if fm.DigestUpdated.IsZero() {
		t.Errorf("digest_updated should be set")
	}
	if fm.DigestWindowDays != 7 {
		t.Errorf("digest_window_days = %d, want 7", fm.DigestWindowDays)
	}
	if !strings.Contains(stub.prompt, "recent A") {
		t.Errorf("LLM prompt should include recent entries; got:\n%s", stub.prompt)
	}
}

// TestSummarizerPreservesConcurrentAppends verifies the read-modify-write
// race the prior implementation had: an AppendEntry that lands while the
// LLM call is in flight must survive the rewrite (not get clobbered).
func TestSummarizerPreservesConcurrentAppends(t *testing.T) {
	s := newStore(t, "worker")
	now := time.Now().UTC().Truncate(time.Second)
	// Seed an entry that will be in the snapshot the summarizer feeds the LLM.
	if err := s.AppendEntry("worker", Entry{
		Timestamp: now.Add(-time.Minute),
		AgentID:   "worker-1",
		JobID:     "old",
		Status:    "done",
		Summary:   "pre-summarize",
	}); err != nil {
		t.Fatal(err)
	}

	stub := &stubLLM{reply: "digest text"}
	stub.beforeReply = func() {
		// Simulate an append landing mid-LLM-call.
		if err := s.AppendEntry("worker", Entry{
			Timestamp: now,
			AgentID:   "worker-2",
			JobID:     "raced",
			Status:    "done",
			Summary:   "arrived during LLM call",
		}); err != nil {
			t.Fatalf("racing append: %v", err)
		}
	}
	sum := &Summarizer{
		Store:           s,
		Roles:           func() []string { return []string{"worker"} },
		Complete:        stub.Complete,
		RetentionWindow: 24 * time.Hour,
	}
	if err := sum.summarizeRole(context.Background(), "worker"); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	entries, err := s.LoadEntries("worker")
	if err != nil {
		t.Fatal(err)
	}
	jobs := map[string]bool{}
	for _, e := range entries {
		jobs[e.JobID] = true
	}
	if !jobs["old"] || !jobs["raced"] {
		t.Errorf("rewrite dropped a concurrent append; have job_ids=%v", jobs)
	}
}

// TestSummarizer_NilCompleterIsTolerated locks in the fix for a daemon
// crash: when no Completer is configured (e.g. claude CLI unavailable),
// summarizeRole must skip the LLM call and not panic.
func TestSummarizer_NilCompleterIsTolerated(t *testing.T) {
	s := newStore(t, "worker")
	now := time.Now().UTC()
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-1 * time.Hour), AgentID: "worker-1", JobID: "a", Status: "done", Summary: "recent"})
	sm := &Summarizer{
		Store:           s,
		Complete:        nil,
		Roles:           func() []string { return []string{"worker"} },
		RetentionWindow: 7 * 24 * time.Hour,
	}
	if err := sm.summarizeRole(context.Background(), "worker"); err != nil {
		t.Fatalf("summarize with nil completer should be a no-op, got: %v", err)
	}
}

// panicCompleter unconditionally panics — proves the per-role recover
// keeps the summarizer loop alive even if a completer misbehaves
// catastrophically.
func panicCompleter(context.Context, string) (string, error) {
	panic("LLM exploded")
}

func TestSummarizer_RecoversFromCompleterPanic(t *testing.T) {
	s := newStore(t, "worker")
	now := time.Now().UTC()
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-1 * time.Hour), AgentID: "worker-1", JobID: "a", Status: "done", Summary: "x"})
	sm := &Summarizer{
		Store:           s,
		Complete:        panicCompleter,
		Roles:           func() []string { return []string{"worker"} },
		RetentionWindow: 7 * 24 * time.Hour,
	}
	// runOnce must NOT propagate the panic. If it did, this test would
	// crash the test binary instead of failing cleanly.
	sm.runOnce(context.Background())
}

// errCompleter always errors — used to confirm errors propagate as
// log lines rather than crashing the loop.
func errCompleter(context.Context, string) (string, error) {
	return "", errors.New("simulated failure")
}

func TestSummarizer_CompleterErrorDoesNotPanic(t *testing.T) {
	s := newStore(t, "worker")
	now := time.Now().UTC()
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-1 * time.Hour), AgentID: "worker-1", JobID: "a", Status: "done", Summary: "x"})
	sm := &Summarizer{
		Store:           s,
		Complete:        errCompleter,
		Roles:           func() []string { return []string{"worker"} },
		RetentionWindow: 7 * 24 * time.Hour,
	}
	sm.runOnce(context.Background())
}

// TestAppendBlock_ReplacesNotAppends pins the file-size invariant: a
// second AppendBlock call with the same block name overwrites the first
// payload instead of stacking a new block alongside it. This is the
// core reason peeraware uses AppendBlock over AppendEntry — the hourly
// digest must not grow leader.md unboundedly.
func TestAppendBlock_ReplacesNotAppends(t *testing.T) {
	s := newStore(t)
	if err := s.AppendBlock(LeaderRole, "peer-projects", "# Peer projects\n- first tick body\n"); err != nil {
		t.Fatalf("first AppendBlock: %v", err)
	}
	if err := s.AppendBlock(LeaderRole, "peer-projects", "# Peer projects\n- second tick body\n"); err != nil {
		t.Fatalf("second AppendBlock: %v", err)
	}
	body, err := s.Load(LeaderRole)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	openMarker := "<!-- block: peer-projects -->"
	closeMarker := "<!-- /block: peer-projects -->"
	if got := strings.Count(body, openMarker); got != 1 {
		t.Errorf("want exactly 1 open marker, got %d\nbody:\n%s", got, body)
	}
	if got := strings.Count(body, closeMarker); got != 1 {
		t.Errorf("want exactly 1 close marker, got %d\nbody:\n%s", got, body)
	}
	if strings.Contains(body, "first tick body") {
		t.Errorf("old block content survived second AppendBlock\nbody:\n%s", body)
	}
	if !strings.Contains(body, "second tick body") {
		t.Errorf("new block content missing\nbody:\n%s", body)
	}
	// Block must land between digest and recent-entries.
	iDigest := strings.Index(body, digestHeader)
	iBlock := strings.Index(body, openMarker)
	iRecent := strings.Index(body, recentHeader)
	if !(iDigest >= 0 && iDigest < iBlock && iBlock < iRecent) {
		t.Errorf("block not positioned between digest and recent-entries: digest=%d block=%d recent=%d\nbody:\n%s",
			iDigest, iBlock, iRecent, body)
	}
}

// TestAppendBlock_PreservesNewlines confirms the documented contract:
// unlike AppendEntry, AppendBlock writes the markdown body verbatim.
// peeraware relies on this so the rendered "## Peer: ..." sub-headers
// survive the round-trip to disk as real markdown structure.
func TestAppendBlock_PreservesNewlines(t *testing.T) {
	s := newStore(t)
	md := "# Peer projects\n\n## Peer: beta\n- 2 tasks in flight: t-abcd (building)\n- Workers active: worker-ada\n"
	if err := s.AppendBlock(LeaderRole, "peer-projects", md); err != nil {
		t.Fatalf("AppendBlock: %v", err)
	}
	body, _ := s.Load(LeaderRole)
	for _, want := range []string{"# Peer projects", "## Peer: beta", "- 2 tasks in flight: t-abcd (building)", "- Workers active: worker-ada"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestAppendBlock_CoexistsWithAppendEntry(t *testing.T) {
	s := newStore(t)
	if err := s.AppendEntry(LeaderRole, Entry{
		Timestamp: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		AgentID:   "worker-1",
		JobID:     "j",
		Status:    "done",
		Summary:   "did a thing",
	}); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if err := s.AppendBlock(LeaderRole, "peer-projects", "# Peer projects\n- body\n"); err != nil {
		t.Fatalf("AppendBlock: %v", err)
	}
	if err := s.AppendEntry(LeaderRole, Entry{
		Timestamp: time.Date(2026, 5, 14, 13, 0, 0, 0, time.UTC),
		AgentID:   "worker-2",
		JobID:     "k",
		Status:    "done",
		Summary:   "later thing",
	}); err != nil {
		t.Fatalf("AppendEntry after AppendBlock: %v", err)
	}
	entries, err := s.LoadEntries(LeaderRole)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 recent entries, got %d", len(entries))
	}
	body, _ := s.Load(LeaderRole)
	if !strings.Contains(body, "<!-- block: peer-projects -->") {
		t.Errorf("block marker lost after subsequent AppendEntry\nbody:\n%s", body)
	}
}
