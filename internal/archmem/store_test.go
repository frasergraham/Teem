package archmem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/llm"
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
	req         llm.CompletionRequest
	beforeReply func() // fires just before returning a response — for race tests
}

func (c *stubLLM) Complete(_ context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	c.req = req
	if c.beforeReply != nil {
		c.beforeReply()
	}
	content := c.out
	if content == "" {
		content = c.reply
	}
	return llm.CompletionResponse{Model: "stub", Content: content}, nil
}

func (c *stubLLM) Stream(context.Context, llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	return nil, nil
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
		Client:          stub,
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
	if !strings.Contains(stub.req.Messages[0].Content, "recent A") {
		t.Errorf("LLM prompt should include recent entries; got:\n%s", stub.req.Messages[0].Content)
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
		Client:          stub,
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

// TestSummarizer_NilClientIsTolerated locks in the fix for a daemon
// crash: when the LLM client is genuinely nil (key unset), summarizeRole
// must skip the LLM call and not panic. Earlier we passed a typed-nil
// *AnthropicClient via interface, which made `s.Client != nil` true and
// triggered a nil-deref on the next call.
func TestSummarizer_NilClientIsTolerated(t *testing.T) {
	s := newStore(t, "worker")
	now := time.Now().UTC()
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-1 * time.Hour), AgentID: "worker-1", JobID: "a", Status: "done", Summary: "recent"})
	sm := &Summarizer{
		Store:           s,
		Client:          nil,
		Roles:           func() []string { return []string{"worker"} },
		RetentionWindow: 7 * 24 * time.Hour,
	}
	if err := sm.summarizeRole(context.Background(), "worker"); err != nil {
		t.Fatalf("summarize with nil client should be a no-op, got: %v", err)
	}
}

// panicLLM unconditionally panics — proves the per-role recover keeps
// the summarizer loop alive even if a client misbehaves catastrophically.
type panicLLM struct{}

func (panicLLM) Complete(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
	panic("LLM exploded")
}
func (panicLLM) Stream(context.Context, llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	panic("LLM exploded")
}

func TestSummarizer_RecoversFromClientPanic(t *testing.T) {
	s := newStore(t, "worker")
	now := time.Now().UTC()
	_ = s.AppendEntry("worker", Entry{Timestamp: now.Add(-1 * time.Hour), AgentID: "worker-1", JobID: "a", Status: "done", Summary: "x"})
	sm := &Summarizer{
		Store:           s,
		Client:          panicLLM{},
		Roles:           func() []string { return []string{"worker"} },
		RetentionWindow: 7 * 24 * time.Hour,
	}
	// runOnce must NOT propagate the panic. If it did, this test would
	// crash the test binary instead of failing cleanly.
	sm.runOnce(context.Background())
}
