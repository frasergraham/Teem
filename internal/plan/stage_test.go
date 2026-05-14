package plan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanTransition_InitialStageAlwaysAllowed(t *testing.T) {
	for _, s := range AllStages {
		if !CanTransition("", s) {
			t.Errorf("empty → %q should be allowed (initial assignment)", s)
		}
	}
}

func TestCanTransition_ForbidsArbitraryJumps(t *testing.T) {
	// Verified should not skip back to proposed; that's a regression
	// — callers should go through building first.
	if CanTransition(StageVerified, StageProposed) {
		t.Errorf("verified → proposed should be forbidden")
	}
	if CanTransition(StageMerging, StageProposed) {
		t.Errorf("merging → proposed should be forbidden")
	}
}

func TestCanTransition_RejectsUnknownStage(t *testing.T) {
	if CanTransition(StageBuilding, Stage("not-a-stage")) {
		t.Errorf("unknown destination should be rejected")
	}
}

func TestUpdateTask_RejectsInvalidStageTransition(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "T"})
	// Force it to verified.
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageBuilding}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageInReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageMerging}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageVerified}); err != nil {
		t.Fatal(err)
	}
	// Now an illegal hop:
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageProposed}); err != ErrInvalidStage {
		t.Errorf("want ErrInvalidStage, got %v", err)
	}
}

func TestPlan_StageEnteredAtTracksMoves(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "T"})
	first := task.StageEnteredAt
	if first.IsZero() {
		t.Fatal("freshly-created task should have StageEnteredAt set")
	}
	updated, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageBuilding})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.StageEnteredAt.After(first) {
		t.Errorf("StageEnteredAt should advance on stage change (was %v → %v)", first, updated.StageEnteredAt)
	}
	// Re-applying the same stage should NOT bump StageEnteredAt.
	again, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageBuilding})
	if err != nil {
		t.Fatal(err)
	}
	if !again.StageEnteredAt.Equal(updated.StageEnteredAt) {
		t.Errorf("re-applying same stage should leave StageEnteredAt: was %v, got %v", updated.StageEnteredAt, again.StageEnteredAt)
	}
}

func TestReplay_BackfillsStageOnLegacyTasks(t *testing.T) {
	// Pre-Stage JSONL: only Status, no Stage. Replay should fill in
	// based on Status so the dashboard buckets correctly.
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	if err := os.WriteFile(path, []byte(
		`{"op":"create","id":"t-old1","title":"legacy pending","ts":"2026-01-01T00:00:00Z"}`+"\n"+
			`{"op":"create","id":"t-old2","title":"legacy progress","ts":"2026-01-01T00:00:01Z"}`+"\n"+
			`{"op":"update","id":"t-old2","status":"in_progress","ts":"2026-01-01T00:00:02Z"}`+"\n"+
			`{"op":"create","id":"t-old3","title":"legacy done","ts":"2026-01-01T00:00:03Z"}`+"\n"+
			`{"op":"update","id":"t-old3","status":"done","ts":"2026-01-01T00:00:04Z"}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	gotPending, _ := p.Get("t-old1")
	if gotPending.Stage != StageProposed {
		t.Errorf("legacy pending should backfill to proposed, got %q", gotPending.Stage)
	}
	gotProg, _ := p.Get("t-old2")
	if gotProg.Stage != StageBuilding {
		t.Errorf("legacy in_progress should backfill to building, got %q", gotProg.Stage)
	}
	gotDone, _ := p.Get("t-old3")
	if gotDone.Stage != StageVerified {
		t.Errorf("legacy done should backfill to verified, got %q", gotDone.Stage)
	}
}

func TestPlan_StageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := p.AddTask(NewTaskInput{Title: "X"})
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageBuilding}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageInReview}); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	got, _ := p2.Get(task.ID)
	if got.Stage != StageInReview {
		t.Errorf("stage after replay: %q want in_review", got.Stage)
	}
}

func TestListFilter_ByStage(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	a, _ := p.AddTask(NewTaskInput{Title: "A"})
	b, _ := p.AddTask(NewTaskInput{Title: "B"})
	_, _ = p.UpdateTask(a.ID, UpdateInput{Stage: StageBuilding})
	_, _ = p.UpdateTask(b.ID, UpdateInput{Stage: StageBuilding})
	_, _ = p.UpdateTask(b.ID, UpdateInput{Stage: StageInReview})

	got := p.List(Filter{Stage: StageBuilding})
	if len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("filter stage=building: %+v", got)
	}
}
