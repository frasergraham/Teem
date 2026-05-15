package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func strPtr(s string) *string { return &s }

func itoa(i int) string { return strconv.Itoa(i) }

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
		Status:      StatusInProgress,
		Notes:       &notes,
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

// TestPlan_NormalizesStatusStagePair locks in the rule that we never
// persist a contradictory (Stage, Status) — the bug a user hit where
// a task showed stage=coding / status=shelved.
func TestPlan_NormalizesStatusStagePair(t *testing.T) {
	cases := []struct {
		name      string
		fromStage Stage
		input     UpdateInput
		wantStage Stage
		wantStat  Status
	}{
		{
			name:      "status=shelved on a coding task snaps stage to shelved",
			fromStage: StageCoding,
			input:     UpdateInput{Status: StatusShelved},
			wantStage: StageShelved,
			wantStat:  StatusShelved,
		},
		{
			name:      "status=done on a proposed task jumps directly to verified",
			fromStage: StageProposed,
			input:     UpdateInput{Status: StatusDone},
			wantStage: StageVerified,
			wantStat:  StatusDone,
		},
		{
			name:      "status=in_progress on a proposed task advances stage to coding",
			fromStage: StageProposed,
			input:     UpdateInput{Status: StatusInProgress},
			wantStage: StageCoding,
			wantStat:  StatusInProgress,
		},
		{
			name:      "stage move alone derives canonical status",
			fromStage: StageCoding,
			input:     UpdateInput{Stage: StageReviewing},
			wantStage: StageReviewing,
			wantStat:  StatusInProgress,
		},
		{
			name:      "contradictory pair: terminal status wins",
			fromStage: StageCoding,
			input:     UpdateInput{Stage: StageCoding, Status: StatusShelved},
			wantStage: StageShelved,
			wantStat:  StatusShelved,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := openTest(t)
			defer p.Close()
			task, _ := p.AddTask(NewTaskInput{Title: tc.name})
			if tc.fromStage != StageProposed {
				if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: tc.fromStage}); err != nil {
					t.Fatalf("seed stage %q: %v", tc.fromStage, err)
				}
			}
			got, err := p.UpdateTask(task.ID, tc.input)
			if err != nil {
				t.Fatalf("UpdateTask: %v", err)
			}
			if got.Stage != tc.wantStage {
				t.Errorf("stage = %q want %q", got.Stage, tc.wantStage)
			}
			if got.Status != tc.wantStat {
				t.Errorf("status = %q want %q", got.Status, tc.wantStat)
			}
		})
	}
}

// TestPlan_UpdateTaskIfStage_RaceLosesNoUpdate hammers the
// awaiting_approval → building transition from many goroutines at
// once. Exactly one call must succeed; every other must observe
// ErrStageChanged and bail. Without the locked check the second writer
// would silently overwrite the first.
func TestPlan_UpdateTaskIfStage_RaceLosesNoUpdate(t *testing.T) {
	p := openTest(t)
	defer p.Close()

	task, _ := p.AddTask(NewTaskInput{Title: "race me"})
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageSpecced}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageAwaitingApproval}); err != nil {
		t.Fatal(err)
	}

	const N = 8
	start := make(chan struct{})
	results := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := p.UpdateTaskIfStage(task.ID, StageAwaitingApproval, UpdateInput{Stage: StageCoding})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes, raced, other := 0, 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrStageChanged):
			raced++
		default:
			other++
			t.Errorf("unexpected error from racing call: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("want exactly one successful approval, got %d (raced=%d other=%d)", successes, raced, other)
	}
	if raced != N-1 {
		t.Errorf("want %d racers to see ErrStageChanged, got %d", N-1, raced)
	}
	got, _ := p.Get(task.ID)
	if got.Stage != StageCoding {
		t.Errorf("final stage = %q want coding", got.Stage)
	}
}

// TestPlan_UpdateTaskIfStage_StageMismatch returns ErrStageChanged
// when the caller's expected stage is wrong from the start.
func TestPlan_UpdateTaskIfStage_StageMismatch(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "stage check"}) // proposed
	_, err := p.UpdateTaskIfStage(task.ID, StageAwaitingApproval, UpdateInput{Stage: StageCoding})
	if !errors.Is(err, ErrStageChanged) {
		t.Errorf("want ErrStageChanged, got %v", err)
	}
}

// TestPlan_UpdateTaskIfStage_TaskNotFound returns ErrTaskNotFound
// when the id is missing — distinct from ErrStageChanged so callers
// can return the correct HTTP status.
func TestPlan_UpdateTaskIfStage_TaskNotFound(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	_, err := p.UpdateTaskIfStage("t-nope", StageAwaitingApproval, UpdateInput{Stage: StageCoding})
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("want ErrTaskNotFound, got %v", err)
	}
}

// TestPlan_MutateTaskIfStage_ConcurrentAppendsAllLand fires N goroutines
// that each append a unique line to a task's Notes via the callback
// form. With the read-modify-write under the plan lock, every appended
// line must survive in the final notes. Without it the second writer
// would clobber the first's append (the COMMENT+COMMENT race).
func TestPlan_MutateTaskIfStage_ConcurrentAppendsAllLand(t *testing.T) {
	p := openTest(t)
	defer p.Close()

	task, _ := p.AddTask(NewTaskInput{Title: "append race"})
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageSpecced}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Notes: strPtr("initial"), Stage: StageAwaitingApproval}); err != nil {
		t.Fatal(err)
	}

	const N = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := p.MutateTaskIfStage(task.ID, StageAwaitingApproval, func(cur Task) UpdateInput {
				newNotes := cur.Notes + "\nline-" + itoa(i)
				return UpdateInput{Stage: StageAwaitingApproval, Notes: &newNotes}
			})
			if err != nil {
				t.Errorf("appender %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	got, _ := p.Get(task.ID)
	for i := 0; i < N; i++ {
		want := "line-" + itoa(i)
		if !strings.Contains(got.Notes, want) {
			t.Errorf("notes missing %q (concurrent append clobbered)", want)
		}
	}
	// Newline-delimited: one "initial" line + N appended lines.
	if lines := strings.Count(got.Notes, "\n"); lines != N {
		t.Errorf("expected %d newlines (one per appended line), got %d (notes=%q)", N, lines, got.Notes)
	}
}

// TestPlan_MutateTaskIfStage_StageMismatch and _TaskNotFound mirror the
// error-surface tests for UpdateTaskIfStage so callers can rely on the
// same shape from both APIs.
func TestPlan_MutateTaskIfStage_StageMismatch(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "mismatch"}) // proposed
	_, err := p.MutateTaskIfStage(task.ID, StageAwaitingApproval, func(Task) UpdateInput {
		return UpdateInput{Notes: strPtr("x")}
	})
	if !errors.Is(err, ErrStageChanged) {
		t.Errorf("want ErrStageChanged, got %v", err)
	}
}

func TestPlan_MutateTaskIfStage_TaskNotFound(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	_, err := p.MutateTaskIfStage("t-nope", StageAwaitingApproval, func(Task) UpdateInput {
		return UpdateInput{Notes: strPtr("x")}
	})
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("want ErrTaskNotFound, got %v", err)
	}
}

func TestPlan_DeleteRemovesFromSnapshotAndReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	keep, _ := p.AddTask(NewTaskInput{Title: "stays"})
	doomed, _ := p.AddTask(NewTaskInput{Title: "delete me"})

	if err := p.DeleteTask(doomed.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, ok := p.Get(doomed.ID); ok {
		t.Error("deleted task still present in snapshot")
	}
	if _, ok := p.Get(keep.ID); !ok {
		t.Error("DeleteTask must not affect other tasks")
	}
	// List should also drop the deleted task.
	all := p.List(Filter{})
	if len(all) != 1 || all[0].ID != keep.ID {
		t.Errorf("List after delete: %+v", all)
	}
	// Deleting again returns ErrTaskNotFound, not a silent success.
	if err := p.DeleteTask(doomed.ID); err != ErrTaskNotFound {
		t.Errorf("double-delete: want ErrTaskNotFound, got %v", err)
	}
	_ = p.Close()

	// Replay must reproduce the deletion.
	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if _, ok := p2.Get(doomed.ID); ok {
		t.Error("deleted task came back after replay — tombstone not honored")
	}
	if _, ok := p2.Get(keep.ID); !ok {
		t.Error("non-deleted task missing after replay")
	}
}

// TestPlan_LegacyContradictionHealsOnReplay covers the exact scenario
// the operator hit: a JSONL on disk with stage=coding, status=shelved
// (produced by an older pre-normalize daemon). Open() should heal it.
func TestPlan_LegacyContradictionHealsOnReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	body := `{"op":"create","id":"t-old","title":"orphan","stage":"building","status":"in_progress","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"op":"update","id":"t-old","status":"shelved","ts":"2026-01-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got, ok := p.Get("t-old")
	if !ok {
		t.Fatal("task missing after replay")
	}
	if got.Stage != StageShelved {
		t.Errorf("legacy contradiction not healed: stage=%q want shelved", got.Stage)
	}
	if got.Status != StatusShelved {
		t.Errorf("status=%q want shelved", got.Status)
	}
}
