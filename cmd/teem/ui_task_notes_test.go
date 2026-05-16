package main

import (
	"testing"

	"github.com/frasergraham/teem/internal/plan"
)

// TestTeamSnapshot_TaskNotesInDashboard locks in the wiring that lets
// the SPA's TaskDetailModal render plan.Task.Notes on click. The
// dashboardTask emitted by teamSnapshot must carry the notes verbatim
// so the modal can run them through marked without a server roundtrip.
func TestTeamSnapshot_TaskNotesInDashboard(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	const notes = "## Spec\n\nFix the bug in **handler.go**."
	task, err := rt.plan.AddTask(plan.NewTaskInput{Title: "Triage", Notes: notes})
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	snap := teamSnapshot(d.snapshotTeam(rt))
	got, ok := findTaskByID(snap.OpenTasks, task.ID)
	if !ok {
		t.Fatalf("task %s missing from OpenTasks", task.ID)
	}
	if got.Notes != notes {
		t.Errorf("dashboardTask.Notes = %q, want %q", got.Notes, notes)
	}
}
