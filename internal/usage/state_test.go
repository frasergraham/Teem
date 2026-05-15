package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestStore_RecordAndSnapshot(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := Config{ResetAnchor: "00:00 UTC"}
	when := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	if err := s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 100, Output: 50, CacheCreate: 25, CacheRead: 500}, when); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	got := snap.ByModel["claude-opus-4-7"]
	if got.Input != 100 || got.Output != 50 || got.CacheCreate != 25 || got.CacheRead != 500 {
		t.Errorf("totals: %+v", got)
	}
	if s.TotalBillable() != 175 { // 100 + 50 + 25 (cache_read excluded)
		t.Errorf("billable: %d", s.TotalBillable())
	}
}

func TestStore_AccumulatesAcrossCalls(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := Config{ResetAnchor: "00:00 UTC"}
	when := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 10, Output: 5}, when); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Snapshot().ByModel["claude-opus-4-7"]
	if got.Input != 30 || got.Output != 15 {
		t.Errorf("accumulated: %+v", got)
	}
}

func TestStore_PerModelBreakdown(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := Config{ResetAnchor: "00:00 UTC"}
	when := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	_ = s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 100}, when)
	_ = s.Record(cfg, "claude-sonnet-4-6", ModelTotals{Input: 200}, when)
	snap := s.Snapshot()
	if snap.ByModel["claude-opus-4-7"].Input != 100 {
		t.Errorf("opus: %+v", snap.ByModel["claude-opus-4-7"])
	}
	if snap.ByModel["claude-sonnet-4-6"].Input != 200 {
		t.Errorf("sonnet: %+v", snap.ByModel["claude-sonnet-4-6"])
	}
}

func TestStore_ResetAtAnchorBoundary(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := Config{ResetAnchor: "00:00 UTC"}
	// Day 1: record some tokens.
	day1 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	if err := s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 100}, day1); err != nil {
		t.Fatal(err)
	}
	// Day 2: should reset before recording the next batch.
	day2 := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC) // past 00:00 anchor
	if err := s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 5}, day2); err != nil {
		t.Fatal(err)
	}
	got := s.Snapshot().ByModel["claude-opus-4-7"]
	if got.Input != 5 {
		t.Errorf("after reset, want 5, got %d (totals not cleared)", got.Input)
	}
}

func TestStore_NoResetWithinSameWindow(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := Config{ResetAnchor: "00:00 UTC"}
	now1 := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	now2 := time.Date(2026, 5, 15, 22, 0, 0, 0, time.UTC)
	_ = s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 10}, now1)
	_ = s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 20}, now2)
	if got := s.Snapshot().ByModel["claude-opus-4-7"].Input; got != 30 {
		t.Errorf("within window: got %d want 30", got)
	}
}

func TestStore_PersistsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	cfg := Config{ResetAnchor: "00:00 UTC"}
	when := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	s1, _ := OpenStore(path)
	_ = s1.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 42}, when)
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Snapshot().ByModel["claude-opus-4-7"].Input; got != 42 {
		t.Errorf("reload: got %d want 42", got)
	}
}

func TestStore_AtomicWrite_NoTempFilesLeftBehind(t *testing.T) {
	s, path := newTestStore(t)
	cfg := Config{ResetAnchor: "00:00 UTC"}
	when := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	_ = s.Record(cfg, "claude-opus-4-7", ModelTotals{Input: 1}, when)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "usage.json" {
			t.Errorf("unexpected file in state dir: %s", e.Name())
		}
	}
	// Sanity: file is valid JSON.
	body, _ := os.ReadFile(path)
	var sf StateFile
	if err := json.Unmarshal(body, &sf); err != nil {
		t.Errorf("disk JSON malformed: %v", err)
	}
}

func TestOpenStore_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(path, []byte("{bogus"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Errorf("expected parse error for malformed state file")
	}
}

func TestStore_EmptyModelRecordedAsUnknown(t *testing.T) {
	s, _ := newTestStore(t)
	cfg := Config{ResetAnchor: "00:00 UTC"}
	_ = s.Record(cfg, "", ModelTotals{Input: 1}, time.Now())
	if _, ok := s.Snapshot().ByModel["unknown"]; !ok {
		t.Errorf("empty model should bucket under \"unknown\"")
	}
}
