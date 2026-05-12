package audit

import (
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
