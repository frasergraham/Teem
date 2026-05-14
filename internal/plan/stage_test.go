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
	// — callers should go through coding first.
	if CanTransition(StageVerified, StageProposed) {
		t.Errorf("verified → proposed should be forbidden")
	}
	if CanTransition(StageIntegrating, StageProposed) {
		t.Errorf("integrating → proposed should be forbidden")
	}
}

func TestCanTransition_RejectsUnknownStage(t *testing.T) {
	if CanTransition(StageCoding, Stage("not-a-stage")) {
		t.Errorf("unknown destination should be rejected")
	}
}

func TestStatusShelved_IsNeitherOpenNorDone(t *testing.T) {
	if StatusShelved.IsOpen() {
		t.Error("shelved should not be open — it's intentionally paused")
	}
	if !StatusShelved.IsShelved() {
		t.Error("StatusShelved.IsShelved() must return true")
	}
}

func TestCanTransition_ShelvedRoundTrip(t *testing.T) {
	// Any active stage should be able to shelve.
	for _, from := range []Stage{StageProposed, StageSpecced, StagePlanning, StageCoding, StageReviewing, StageIntegrating} {
		if !CanTransition(from, StageShelved) {
			t.Errorf("%q → shelved should be allowed (operator pause)", from)
		}
	}
	// And come back to any active stage from shelved.
	for _, to := range []Stage{StageProposed, StageSpecced, StagePlanning, StageCoding, StageReviewing} {
		if !CanTransition(StageShelved, to) {
			t.Errorf("shelved → %q should be allowed (operator resumes)", to)
		}
	}
}

func TestUpdateTask_RejectsInvalidStageTransition(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "T"})
	// Force it to verified.
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageCoding}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageReviewing}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageIntegrating}); err != nil {
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
	updated, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageCoding})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.StageEnteredAt.After(first) {
		t.Errorf("StageEnteredAt should advance on stage change (was %v → %v)", first, updated.StageEnteredAt)
	}
	// Re-applying the same stage should NOT bump StageEnteredAt.
	again, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageCoding})
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
	if gotProg.Stage != StageCoding {
		t.Errorf("legacy in_progress should backfill to coding, got %q", gotProg.Stage)
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
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageCoding}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateTask(task.ID, UpdateInput{Stage: StageReviewing}); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	p2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	got, _ := p2.Get(task.ID)
	if got.Stage != StageReviewing {
		t.Errorf("stage after replay: %q want reviewing", got.Stage)
	}
}

func TestListFilter_ByStage(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	a, _ := p.AddTask(NewTaskInput{Title: "A"})
	b, _ := p.AddTask(NewTaskInput{Title: "B"})
	_, _ = p.UpdateTask(a.ID, UpdateInput{Stage: StageCoding})
	_, _ = p.UpdateTask(b.ID, UpdateInput{Stage: StageCoding})
	_, _ = p.UpdateTask(b.ID, UpdateInput{Stage: StageReviewing})

	got := p.List(Filter{Stage: StageCoding})
	if len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("filter stage=coding: %+v", got)
	}
}

// TestStageAliases locks in the rename-compatibility path: old stage
// strings on input ("building" / "in_review" / "merging") are accepted
// and normalised to their new canonical values.
func TestStageAliases(t *testing.T) {
	cases := []struct {
		in   Stage
		want Stage
	}{
		{Stage("building"), StageCoding},
		{Stage("in_review"), StageReviewing},
		{Stage("merging"), StageIntegrating},
		// Already-canonical values pass through.
		{StageCoding, StageCoding},
		{StageReviewing, StageReviewing},
		{StageIntegrating, StageIntegrating},
		// Unknown stages pass through untouched so IsValidStage can
		// reject them downstream.
		{Stage("bogus"), Stage("bogus")},
	}
	for _, tc := range cases {
		if got := NormalizeStage(tc.in); got != tc.want {
			t.Errorf("NormalizeStage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUpdateTask_AcceptsLegacyStageName covers the end-to-end alias
// path: a caller that still passes Stage="building" through
// UpdateTask gets the new canonical value stored.
func TestUpdateTask_AcceptsLegacyStageName(t *testing.T) {
	p := openTest(t)
	defer p.Close()
	task, _ := p.AddTask(NewTaskInput{Title: "alias-input"})
	got, err := p.UpdateTask(task.ID, UpdateInput{Stage: Stage("building")})
	if err != nil {
		t.Fatalf("UpdateTask building: %v", err)
	}
	if got.Stage != StageCoding {
		t.Errorf("legacy building input not aliased: stage=%q want coding", got.Stage)
	}
}

// TestReplay_NormalisesLegacyStageStrings exercises the on-disk path:
// a JSONL written by an older daemon with stage="building" should
// replay into the new canonical stage.
func TestReplay_NormalisesLegacyStageStrings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.jsonl")
	body := `{"op":"create","id":"t-r1","title":"old-building","stage":"building","status":"in_progress","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"op":"create","id":"t-r2","title":"old-in-review","stage":"in_review","status":"in_progress","ts":"2026-01-01T00:00:01Z"}` + "\n" +
		`{"op":"create","id":"t-r3","title":"old-merging","stage":"merging","status":"in_progress","ts":"2026-01-01T00:00:02Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range []struct {
		id   string
		want Stage
	}{
		{"t-r1", StageCoding},
		{"t-r2", StageReviewing},
		{"t-r3", StageIntegrating},
	} {
		got, ok := p.Get(c.id)
		if !ok {
			t.Fatalf("%s missing", c.id)
		}
		if got.Stage != c.want {
			t.Errorf("%s stage=%q want %q", c.id, got.Stage, c.want)
		}
	}
}
