package leaderstatus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leader_status.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("leader", "Reviewing T1", []string{"t-1", "t-2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("worker-12", "Spawning reviewer", nil); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("leader")
	if !ok {
		t.Fatal("leader missing after reopen")
	}
	if got.Text != "Reviewing T1" {
		t.Errorf("text: %q", got.Text)
	}
	if len(got.CurrentTaskIDs) != 2 || got.CurrentTaskIDs[0] != "t-1" {
		t.Errorf("task ids: %v", got.CurrentTaskIDs)
	}
	all := s2.All()
	if len(all) != 2 {
		t.Errorf("All: %d want 2", len(all))
	}
}

func TestStore_AtomicWrite_NoTmpLeftOver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leader_status.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("leader", "x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file should not remain on disk: %v", err)
	}
	// And the persisted JSON must be parseable as map[string]Entry.
	body, _ := os.ReadFile(path)
	var loaded map[string]Entry
	if err := json.Unmarshal(body, &loaded); err != nil {
		t.Fatalf("persisted JSON not parseable: %v", err)
	}
	if _, ok := loaded["leader"]; !ok {
		t.Errorf("expected leader key in: %s", string(body))
	}
}

func TestStore_TruncatesLongText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leader_status.json")
	s, _ := Open(path)
	long := strings.Repeat("x", MaxTextBytes+50)
	if err := s.Set("leader", long, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("leader")
	if !strings.HasSuffix(got.Text, "…") {
		t.Errorf("expected truncation ellipsis, got %q", got.Text[len(got.Text)-3:])
	}
	if len(got.Text) > MaxTextBytes+5 {
		t.Errorf("text too long after truncation: %d bytes", len(got.Text))
	}
}

func TestStore_TolerantOfCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leader_status.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open should not fail on corrupt file: %v", err)
	}
	if len(s.All()) != 0 {
		t.Errorf("corrupt file should yield empty store")
	}
}
