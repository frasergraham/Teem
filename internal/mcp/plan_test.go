package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

func newTestServerWithPlan(t *testing.T) (*Server, *plan.Plan) {
	t.Helper()
	dir := t.TempDir()
	p, err := plan.Open(filepath.Join(dir, "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1}},
	}
	srv, err := New(Config{
		Bus:      bus.NewMemBus(),
		Team:     tm,
		Registry: NewRegistry(),
		Spawner:  &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}},
		Plan:     p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, p
}

func TestAddTask(t *testing.T) {
	srv, _ := newTestServerWithPlan(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "add_task"
	req.Params.Arguments = map[string]any{
		"title":      "Implement migrations",
		"notes":      "see spec §3.2",
		"depends_on": "t-aa,t-bb",
	}
	res, err := srv.handleAddTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("add_task error: %s", textOf(t, res))
	}
	var task plan.Task
	if err := json.Unmarshal([]byte(textOf(t, res)), &task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if task.Title != "Implement migrations" {
		t.Errorf("title: %q", task.Title)
	}
	if len(task.DependsOn) != 2 || task.DependsOn[0] != "t-aa" || task.DependsOn[1] != "t-bb" {
		t.Errorf("depends_on: %v", task.DependsOn)
	}
}

func TestUpdateTask_FullCycle(t *testing.T) {
	srv, p := newTestServerWithPlan(t)
	created, _ := p.AddTask(plan.NewTaskInput{Title: "Run tests"})

	// Mark in_progress with an assignee.
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "update_task"
	req.Params.Arguments = map[string]any{
		"id":           created.ID,
		"status":       "in_progress",
		"assigned_to":  "worker-3",
		"add_evidence": "j7,j8",
	}
	res, err := srv.handleUpdateTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("update_task error: %s", textOf(t, res))
	}
	var task plan.Task
	if err := json.Unmarshal([]byte(textOf(t, res)), &task); err != nil {
		t.Fatal(err)
	}
	if task.Status != plan.StatusInProgress {
		t.Errorf("status: %q", task.Status)
	}
	if task.AssignedTo != "worker-3" {
		t.Errorf("assigned_to: %q", task.AssignedTo)
	}
	if len(task.Evidence) != 2 {
		t.Errorf("evidence: %v", task.Evidence)
	}
}

func TestUpdateTask_MissingErrors(t *testing.T) {
	srv, _ := newTestServerWithPlan(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "update_task"
	req.Params.Arguments = map[string]any{
		"id":     "t-nope",
		"status": "done",
	}
	res, _ := srv.handleUpdateTask(context.Background(), req)
	if !res.IsError {
		t.Error("expected not-found error")
	}
}

func TestListTasks_OpenOnly(t *testing.T) {
	srv, p := newTestServerWithPlan(t)
	a, _ := p.AddTask(plan.NewTaskInput{Title: "A"})
	b, _ := p.AddTask(plan.NewTaskInput{Title: "B"})
	_, _ = p.UpdateTask(b.ID, plan.UpdateInput{Status: plan.StatusDone})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "list_tasks"
	req.Params.Arguments = map[string]any{"open_only": "true"}
	res, _ := srv.handleListTasks(context.Background(), req)
	var tasks []plan.Task
	if err := json.Unmarshal([]byte(textOf(t, res)), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != a.ID {
		t.Errorf("open-only list: %+v", tasks)
	}
}

func TestLinkTaskToJob(t *testing.T) {
	srv, p := newTestServerWithPlan(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "Build"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "link_task_to_job"
	req.Params.Arguments = map[string]any{"task_id": task.ID, "job_id": "j7"}
	res, _ := srv.handleLinkTaskToJob(context.Background(), req)
	if res.IsError {
		t.Fatalf("link error: %s", textOf(t, res))
	}
	got, _ := p.Get(task.ID)
	if len(got.Evidence) != 1 || got.Evidence[0] != "j7" {
		t.Errorf("evidence after link: %v", got.Evidence)
	}
}

func TestDeleteTask(t *testing.T) {
	srv, p := newTestServerWithPlan(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "doomed"})
	keep, _ := p.AddTask(plan.NewTaskInput{Title: "stays"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "delete_task"
	req.Params.Arguments = map[string]any{"id": task.ID}
	res, err := srv.handleDeleteTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("delete_task error: %s", textOf(t, res))
	}
	if _, ok := p.Get(task.ID); ok {
		t.Error("task still present after delete_task")
	}
	if _, ok := p.Get(keep.ID); !ok {
		t.Error("delete_task removed an unrelated task")
	}

	// Re-deleting the same id should surface a clear not-found error.
	res2, err := srv.handleDeleteTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.IsError {
		t.Error("second delete should return an error result")
	}
}

func textOf(t *testing.T, r *mcpgo.CallToolResult) string {
	t.Helper()
	if r == nil || len(r.Content) == 0 {
		t.Fatal("nil/empty result")
	}
	tc, ok := r.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("not text: %T", r.Content[0])
	}
	return tc.Text
}
