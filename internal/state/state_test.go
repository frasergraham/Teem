package state

import (
	"path/filepath"
	"sort"
	"testing"
)

func TestStore_SaveLoadDelete(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state"))
	r := Record{AgentID: "be-1", Role: "backend", Backend: "fargate", Lifecycle: "persistent", TaskARN: "arn:aws:ecs:..."}
	if err := s.Save(r); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := s.Load("be-1")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.TaskARN != r.TaskARN || got.Role != "backend" {
		t.Errorf("Load: got %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt not filled in")
	}
	if err := s.Delete("be-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err = s.Load("be-1")
	if err != nil || ok {
		t.Fatalf("post-Delete Load: ok=%v err=%v", ok, err)
	}
}

func TestStore_MissingIsNotError(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state"))
	if err := s.Delete("nope"); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
	_, ok, err := s.Load("nope")
	if err != nil || ok {
		t.Errorf("Load missing: ok=%v err=%v", ok, err)
	}
	rs, err := s.List()
	if err != nil {
		t.Errorf("List empty dir: %v", err)
	}
	if len(rs) != 0 {
		t.Errorf("List: got %d, want 0", len(rs))
	}
}

func TestStore_List(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state"))
	for _, r := range []Record{
		{AgentID: "be-1", Role: "backend"},
		{AgentID: "fe-1", Role: "frontend"},
	} {
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("List: got %d, want 2", len(rs))
	}
	ids := []string{rs[0].AgentID, rs[1].AgentID}
	sort.Strings(ids)
	if ids[0] != "be-1" || ids[1] != "fe-1" {
		t.Errorf("List ids: %v", ids)
	}
}

func TestStore_SaveOverwrites(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state"))
	if err := s.Save(Record{AgentID: "x", TaskARN: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Record{AgentID: "x", TaskARN: "new"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Load("x")
	if got.TaskARN != "new" {
		t.Errorf("TaskARN = %q want new", got.TaskARN)
	}
}
