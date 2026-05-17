package audit

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFileSink_WriteQuery(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFile(filepath.Join(dir, "a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	for i, ev := range []Event{
		{AgentID: "be-1", JobID: "j1", Kind: KindJobReceived, Timestamp: now},
		{AgentID: "be-1", JobID: "j1", Kind: KindJobComplete, Message: "done", Timestamp: now.Add(time.Second)},
		{AgentID: "fe-1", JobID: "j2", Kind: KindJobReceived, Timestamp: now.Add(2 * time.Second)},
	} {
		if err := s.Write(ev); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	all, err := s.Query("", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("all: got %d, want 3", len(all))
	}

	be, err := s.Query("be-1", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(be) != 2 {
		t.Errorf("be-1: got %d, want 2", len(be))
	}

	recent, err := s.Query("", now.Add(1500*time.Millisecond), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 {
		t.Errorf("recent: got %d, want 1", len(recent))
	}

	limited, err := s.Query("", time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Errorf("limit: got %d, want 2", len(limited))
	}
}

func TestFileSink_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFile(filepath.Join(dir, "a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Write(Event{AgentID: "x", Kind: KindNote, Message: "n", Meta: map[string]any{"i": i}})
		}(i)
	}
	wg.Wait()
	all, err := s.Query("", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 50 {
		t.Errorf("got %d events, want 50", len(all))
	}
}

func TestFileSink_MissingFileNoError(t *testing.T) {
	dir := t.TempDir()
	s := &FileSink{path: filepath.Join(dir, "does-not-exist.jsonl")}
	out, err := s.Query("", time.Time{}, 0)
	if err != nil {
		t.Fatalf("missing file should be ok: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events, want 0", len(out))
	}
}

func TestTailCache_AppendAndRead(t *testing.T) {
	c := NewTailCache(100, 24*time.Hour)
	now := time.Now().UTC()
	// Mirror real usage: a bootstrap step declares the floor before any
	// Appends. Here we declare "this cache covers everything from
	// now-1h onward" — without it, the cache would only claim coverage
	// from the timestamp of the first Append.
	c.SetBootstrapFloor(now.Add(-1 * time.Hour))
	c.Append(Event{AgentID: "a", Kind: KindNote, Timestamp: now.Add(-3 * time.Second)}, now)
	c.Append(Event{AgentID: "b", Kind: KindNote, Timestamp: now.Add(-2 * time.Second)}, now)
	c.Append(Event{AgentID: "a", Kind: KindNote, Timestamp: now.Add(-1 * time.Second)}, now)

	got, ok := c.QueryFromCache("", now.Add(-10*time.Second), 0)
	if !ok {
		t.Fatal("expected cache coverage for in-window since")
	}
	if len(got) != 3 {
		t.Errorf("all: got %d, want 3", len(got))
	}

	got, ok = c.QueryFromCache("a", now.Add(-10*time.Second), 0)
	if !ok || len(got) != 2 {
		t.Errorf("agent filter: ok=%v len=%d, want ok=true len=2", ok, len(got))
	}

	got, ok = c.QueryFromCache("", now.Add(-1500*time.Millisecond), 0)
	if !ok || len(got) != 1 {
		t.Errorf("since filter: ok=%v len=%d, want ok=true len=1", ok, len(got))
	}

	got, ok = c.QueryFromCache("", now.Add(-10*time.Second), 2)
	if !ok || len(got) != 2 {
		t.Errorf("limit: ok=%v len=%d, want ok=true len=2", ok, len(got))
	}

	if _, ok := c.QueryFromCache("", time.Time{}, 0); ok {
		t.Error("zero since must never be served from cache")
	}
}

func TestTailCache_BootstrapFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")

	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-1 * time.Hour)
	for i := 0; i < 3; i++ {
		if err := s.Write(Event{
			AgentID:   "a",
			Kind:      KindNote,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open: cache must rebuild from disk.
	s2, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if got := s2.cache.Size(); got != 3 {
		t.Fatalf("cache size after bootstrap: got %d, want 3", got)
	}

	events, err := s2.Query("", time.Now().UTC().Add(-2*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Errorf("post-bootstrap query: got %d, want 3", len(events))
	}
}

func TestTailCache_FallsThroughOnDeepSince(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	// One event older than the 24h bootstrap window, one inside it.
	if err := s.Write(Event{AgentID: "a", Kind: KindNote, Timestamp: now.Add(-36 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Event{AgentID: "a", Kind: KindNote, Timestamp: now.Add(-1 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// In-window since: served from cache, only the recent event.
	fromCache, ok := s.cache.QueryFromCache("", now.Add(-2*time.Hour), 0)
	if !ok {
		t.Fatal("expected cache coverage for since=now-2h")
	}
	if len(fromCache) != 1 {
		t.Errorf("cache: got %d, want 1", len(fromCache))
	}

	// Deep since: cache must report no coverage.
	if _, ok := s.cache.QueryFromCache("", now.Add(-48*time.Hour), 0); ok {
		t.Error("expected fall-through for since older than floor")
	}

	// Public Query falls through to disk and returns BOTH events.
	all, err := s.Query("", now.Add(-48*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("disk fallback: got %d, want 2", len(all))
	}
}

func TestTailCache_RingEviction(t *testing.T) {
	c := NewTailCache(3, 24*time.Hour)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		c.Append(Event{
			AgentID:   "a",
			Kind:      KindNote,
			Message:   fmt.Sprintf("%d", i),
			Timestamp: now.Add(time.Duration(i) * time.Second),
		}, now)
	}
	if got := c.Size(); got != 3 {
		t.Fatalf("size after overflow: got %d, want 3", got)
	}

	// After two capacity evictions the oldest kept event is i=2.
	wantFloor := now.Add(2 * time.Second)
	if got := c.Floor(); !got.Equal(wantFloor) {
		t.Errorf("floor: got %v, want %v", got, wantFloor)
	}

	got, ok := c.QueryFromCache("", wantFloor, 0)
	if !ok || len(got) != 3 {
		t.Fatalf("query at floor: ok=%v len=%d, want ok=true len=3", ok, len(got))
	}
	for i, e := range got {
		want := fmt.Sprintf("%d", i+2)
		if e.Message != want {
			t.Errorf("event %d message=%q, want %q", i, e.Message, want)
		}
	}

	// Since before floor → must fall through.
	if _, ok := c.QueryFromCache("", now.Add(1*time.Second), 0); ok {
		t.Error("expected fall-through for since before floor after eviction")
	}
}
