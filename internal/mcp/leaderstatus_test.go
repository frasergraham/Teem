package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/leaderstatus"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

func newTestServerFull(t *testing.T) (*Server, *plan.Plan, *leaderstatus.Store, *audit.FileSink) {
	t.Helper()
	dir := t.TempDir()
	p, err := plan.Open(filepath.Join(dir, "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ls, err := leaderstatus.Open(filepath.Join(dir, "leader_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1}},
	}
	srv, err := New(Config{
		Bus:          bus.NewMemBus(),
		Team:         tm,
		Registry:     NewRegistry(),
		Spawner:      &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}},
		Plan:         p,
		Audit:        a,
		LeaderStatus: ls,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, p, ls, a
}

func TestUpdateLeaderStatus_DefaultsAgentToLeader(t *testing.T) {
	srv, _, ls, _ := newTestServerFull(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "update_leader_status"
	req.Params.Arguments = map[string]any{
		"text":             "Reviewing T1",
		"current_task_ids": "t-1,t-2",
	}
	res, err := srv.handleUpdateLeaderStatus(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("update_leader_status error: %s", textOf(t, res))
	}
	got, ok := ls.Get("leader")
	if !ok {
		t.Fatal("leader entry missing")
	}
	if got.Text != "Reviewing T1" {
		t.Errorf("text: %q", got.Text)
	}
	if len(got.CurrentTaskIDs) != 2 {
		t.Errorf("task ids: %v", got.CurrentTaskIDs)
	}
}

func TestUpdateLeaderStatus_RespectsExplicitAgentID(t *testing.T) {
	srv, _, ls, _ := newTestServerFull(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "update_leader_status"
	req.Params.Arguments = map[string]any{
		"text":     "Spawning reviewer-7",
		"agent_id": "worker-12",
	}
	_, _ = srv.handleUpdateLeaderStatus(context.Background(), req)
	if _, ok := ls.Get("worker-12"); !ok {
		t.Errorf("worker-12 entry missing")
	}
	if _, ok := ls.Get("leader"); ok {
		t.Errorf("leader should not be set when agent_id is explicit")
	}
}

func TestGetLeaderStatus_ReturnsMap(t *testing.T) {
	srv, _, ls, _ := newTestServerFull(t)
	_ = ls.Set("leader", "A", nil)
	_ = ls.Set("worker-2", "B", nil)

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "get_leader_status"
	res, _ := srv.handleGetLeaderStatus(context.Background(), req)
	if res.IsError {
		t.Fatalf("get_leader_status error: %s", textOf(t, res))
	}
	var out map[string]leaderstatus.Entry
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["leader"]; !ok {
		t.Errorf("missing leader: %v", out)
	}
	if _, ok := out["worker-2"]; !ok {
		t.Errorf("missing worker-2: %v", out)
	}
}

func TestSetTaskStage_HappyPath(t *testing.T) {
	srv, p, _, _ := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "set_task_stage"
	req.Params.Arguments = map[string]any{"task_id": task.ID, "stage": "building"}
	res, err := srv.handleSetTaskStage(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("set_task_stage failed: %v / %s", err, textOf(t, res))
	}
	got, _ := p.Get(task.ID)
	if got.Stage != plan.StageBuilding {
		t.Errorf("stage: %q", got.Stage)
	}
}

func TestSetTaskStage_InvalidTransition(t *testing.T) {
	srv, p, _, _ := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})
	_, _ = p.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageBuilding})
	_, _ = p.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageInReview})
	_, _ = p.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageMerging})
	_, _ = p.UpdateTask(task.ID, plan.UpdateInput{Stage: plan.StageVerified})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "set_task_stage"
	req.Params.Arguments = map[string]any{"task_id": task.ID, "stage": "proposed"}
	res, _ := srv.handleSetTaskStage(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected illegal transition error")
	}
}

func TestSetTaskStage_UnknownStage(t *testing.T) {
	srv, p, _, _ := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "set_task_stage"
	req.Params.Arguments = map[string]any{"task_id": task.ID, "stage": "bogus"}
	res, _ := srv.handleSetTaskStage(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected unknown-stage error")
	}
}

func TestSetTaskStage_EmitsAudit(t *testing.T) {
	srv, p, _, a := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "set_task_stage"
	req.Params.Arguments = map[string]any{"task_id": task.ID, "stage": "building"}
	res, err := srv.handleSetTaskStage(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("set_task_stage: %v / %s", err, textOf(t, res))
	}

	events, _ := a.Query("", parseZero(), 100)
	var found *audit.Event
	for i := range events {
		if events[i].Kind == audit.KindTaskStageChanged {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatal("task_stage_changed not written to audit")
	}
	if id, _ := found.Meta["task_id"].(string); id != task.ID {
		t.Errorf("task_id meta: %v", found.Meta)
	}
	if stage, _ := found.Meta["stage"].(string); stage != "building" {
		t.Errorf("stage meta: %v", found.Meta)
	}
	if from, _ := found.Meta["from"].(string); from != "proposed" {
		t.Errorf("from meta: %v", found.Meta)
	}
}

func TestSetTaskStage_SkipsAuditWhenStageUnchanged(t *testing.T) {
	srv, p, _, a := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "set_task_stage"
	req.Params.Arguments = map[string]any{"task_id": task.ID, "stage": "building"}
	res, err := srv.handleSetTaskStage(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("first set_task_stage: %v / %s", err, textOf(t, res))
	}

	// Re-issuing the same stage must be a no-op for audit emission.
	res, err = srv.handleSetTaskStage(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("second set_task_stage: %v / %s", err, textOf(t, res))
	}

	events, _ := a.Query("", parseZero(), 100)
	count := 0
	for _, e := range events {
		if e.Kind == audit.KindTaskStageChanged {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 task_stage_changed event, got %d", count)
	}
}

func TestRecordDecision_EmitsAudit(t *testing.T) {
	srv, p, _, a := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "record_decision"
	req.Params.Arguments = map[string]any{
		"task_id": task.ID,
		"text":    "Vendoring foo because upstream is unmaintained",
	}
	res, err := srv.handleRecordDecision(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("record_decision: %v / %s", err, textOf(t, res))
	}
	events, _ := a.Query("", parseZero(), 100)
	var found *audit.Event
	for i := range events {
		if events[i].Kind == audit.KindDecisionNote {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatal("decision_note not written to audit")
	}
	if id, _ := found.Meta["task_id"].(string); id != task.ID {
		t.Errorf("decision_note task_id: %v", found.Meta)
	}
}

func TestRecordDecision_RejectsUnknownTask(t *testing.T) {
	srv, _, _, _ := newTestServerFull(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "record_decision"
	req.Params.Arguments = map[string]any{"task_id": "t-nope", "text": "x"}
	res, _ := srv.handleRecordDecision(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected not-found error")
	}
}

func TestRecordBlocker_MovesTaskAndAudits(t *testing.T) {
	srv, p, _, a := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "record_blocker"
	req.Params.Arguments = map[string]any{
		"task_id": task.ID,
		"text":    "needs API key from ops",
	}
	res, err := srv.handleRecordBlocker(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("record_blocker: %v / %s", err, textOf(t, res))
	}
	got, _ := p.Get(task.ID)
	if got.Status != plan.StatusBlocked {
		t.Errorf("status: %q", got.Status)
	}
	if got.Stage != plan.StageBlocked {
		t.Errorf("stage: %q", got.Stage)
	}
	events, _ := a.Query("", parseZero(), 100)
	found := false
	for _, e := range events {
		if e.Kind == audit.KindBlockerNote {
			found = true
			break
		}
	}
	if !found {
		t.Error("blocker_note not in audit log")
	}
}

func TestListTasks_ReturnsStage(t *testing.T) {
	srv, p, _, _ := newTestServerFull(t)
	a, _ := p.AddTask(plan.NewTaskInput{Title: "A"})
	_, _ = p.UpdateTask(a.ID, plan.UpdateInput{Stage: plan.StageBuilding})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "list_tasks"
	res, _ := srv.handleListTasks(context.Background(), req)
	var tasks []plan.Task
	if err := json.Unmarshal([]byte(textOf(t, res)), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks", len(tasks))
	}
	if tasks[0].Stage != plan.StageBuilding {
		t.Errorf("stage not returned in list_tasks payload: %q", tasks[0].Stage)
	}
	if tasks[0].StageEnteredAt.IsZero() {
		t.Errorf("stage_entered_at should be set")
	}
}

func TestListTasks_FilterByStage(t *testing.T) {
	srv, p, _, _ := newTestServerFull(t)
	a, _ := p.AddTask(plan.NewTaskInput{Title: "A"})
	b, _ := p.AddTask(plan.NewTaskInput{Title: "B"})
	_, _ = p.UpdateTask(a.ID, plan.UpdateInput{Stage: plan.StageBuilding})
	_, _ = p.UpdateTask(b.ID, plan.UpdateInput{Stage: plan.StageBuilding})
	_, _ = p.UpdateTask(b.ID, plan.UpdateInput{Stage: plan.StageInReview})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "list_tasks"
	req.Params.Arguments = map[string]any{"stage": "building"}
	res, _ := srv.handleListTasks(context.Background(), req)
	var tasks []plan.Task
	_ = json.Unmarshal([]byte(textOf(t, res)), &tasks)
	if len(tasks) != 1 || tasks[0].ID != a.ID {
		t.Errorf("stage filter: %+v", tasks)
	}
}

func parseZero() time.Time { return time.Time{} }
