package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlan_AddAndGet(t *testing.T) {
	p := openTest(t)
	defer p.Close()

	task, err := p.AddTask(NewTaskInput{Title: "Implement migration", Notes: "see spec §3.2"})
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if !strings.HasPrefix(task.ID, "t-") {
		t.Errorf("id: %q", task.ID)
	}
	if task.Status != StatusPending {
		t.Errorf("status = %q want pending", task.Status)
	}
	if task.Notes != "see spec §3.2" {
		t.Errorf("notes: %q", task.Notes)
	}

	got, ok := p.Get(task.ID)
	if !ok {
		t.Fatal("Get returned false")
	}
	if got.Title != task.Title {
		t.Errorf("title round-trip: %q vs %q", got.Title, task.Title)
	}
}

func TestPlan_UpdateAndEvidence(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "Write tests"})

	assignTo := "worker-2"
	updated, err := p.UpdateTask(task.ID, UpdateInput{
		Status:      StatusInProgress,
		AssignedTo:  &assignTo,
		AddEvidence: []string{"j7"},
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Status != StatusInProgress {
		t.Errorf("status: %q", updated.Status)
	}
	if updated.AssignedTo != "worker-2" {
		t.Errorf("assigned_to: %q", updated.AssignedTo)
	}
	if len(updated.Evidence) != 1 || updated.Evidence[0] != "j7" {
		t.Errorf("evidence: %v", updated.Evidence)
	}

	// LinkJob is the shortcut path.
	linked, err := p.LinkJob(task.ID, "j8")
	if err != nil {
		t.Fatalf("LinkJob: %v", err)
	}
	if len(linked.Evidence) != 2 {
		t.Errorf("evidence after link: %v", linked.Evidence)
	}
}

func TestPlan_UpdateMissingIsErrTaskNotFound(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	_, err := p.UpdateTask("t-nope", UpdateInput{Status: StatusDone})
	if err == nil || err != ErrTaskNotFound {
		t.Errorf("want ErrTaskNotFound, got %v", err)
	}
}

func TestPlan_ListFilters(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	a, _ := p.AddTask(NewTaskInput{Title: "A"})
	b, _ := p.AddTask(NewTaskInput{Title: "B"})
	c, _ := p.AddTask(NewTaskInput{Title: "C", ParentID: a.ID})
	_, _ = p.UpdateTask(b.ID, UpdateInput{Status: StatusDone})

	all := p.List(Filter{})
	if len(all) != 3 {
		t.Fatalf("all: %d want 3", len(all))
	}

	open := p.List(Filter{OpenOnly: true})
	if len(open) != 2 {
		t.Errorf("open-only: %d want 2", len(open))
	}

	children := p.List(Filter{ParentID: a.ID})
	if len(children) != 1 || children[0].ID != c.ID {
		t.Errorf("parent filter: %+v", children)
	}

	doneOnly := p.List(Filter{Status: StatusDone})
	if len(doneOnly) != 1 || doneOnly[0].ID != b.ID {
		t.Errorf("status filter: %+v", doneOnly)
	}
}

func TestPlan_RoundTripAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := p.AddTask(NewTaskInput{Title: "First"})
	notes := "in progress notes"
	_, _ = p.UpdateTask(a.ID, UpdateInput{
		Status: StatusInProgress,
		Notes:  &notes,
		AddEvidence: []string{"j1", "j2"},
	})
	b, _ := p.AddTask(NewTaskInput{Title: "Second", DependsOn: []string{a.ID}})
	_ = p.Close()

	// Re-open and verify state is identical.
	p2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer p2.Close()
	gotA, _ := p2.Get(a.ID)
	if gotA.Status != StatusInProgress {
		t.Errorf("A status after replay: %q", gotA.Status)
	}
	if gotA.Notes != notes {
		t.Errorf("A notes after replay: %q", gotA.Notes)
	}
	if len(gotA.Evidence) != 2 {
		t.Errorf("A evidence after replay: %v", gotA.Evidence)
	}
	gotB, _ := p2.Get(b.ID)
	if len(gotB.DependsOn) != 1 || gotB.DependsOn[0] != a.ID {
		t.Errorf("B depends_on after replay: %v", gotB.DependsOn)
	}
}

func TestPlan_NotesAreReplaced(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "T", Notes: "first"})
	second := "second"
	got, _ := p.UpdateTask(task.ID, UpdateInput{Notes: &second})
	if got.Notes != "second" {
		t.Errorf("notes should be replaced; got %q", got.Notes)
	}
}

func openTest(t *testing.T) *Plan {
	t.Helper()
	dir := t.TempDir()
	p, err := Open(filepath.Join(dir, "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Sanity: a malformed line shouldn't kill Open. Forward-compat check.
func TestPlan_TolerantOfGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	if err := os.WriteFile(path, []byte(`{"op":"create","id":"t-aa","title":"ok","ts":"2026-01-01T00:00:00Z"}`+"\n"+
		`not json`+"\n"+
		`{"op":"update","id":"t-aa","status":"done","ts":"2026-01-01T00:00:01Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	got, ok := p.Get("t-aa")
	if !ok {
		t.Fatal("t-aa missing")
	}
	if got.Status != StatusDone {
		t.Errorf("status = %q want done (replay should skip garbage line, not abort)", got.Status)
	}
}
