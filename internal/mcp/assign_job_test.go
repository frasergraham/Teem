package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/team"
)

// stubSpawner is a fakeSpawner-style stub specialised for the
// assign_job tests: AssignJob returns a fixed job_id so the test can
// assert that the daemon linked the right id into evidence + index.
type stubSpawner struct {
	jobID string
}

func (s *stubSpawner) Spawn(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (s *stubSpawner) AssignJob(_ context.Context, _, _, _ string) (string, error) {
	return s.jobID, nil
}
func (s *stubSpawner) JobStatus(_ string) (string, string, bool) { return "", "", false }
func (s *stubSpawner) StopAgent(_ context.Context, _ string) error {
	return nil
}
func (s *stubSpawner) IsRunning(_ string) bool                { return true }
func (s *stubSpawner) AnyRunningWithRole(_ string) bool       { return false }
func (s *stubSpawner) RosterSnapshot(_ string) []roster.Entry { return nil }

// newAssignJobServer wires the minimal Config needed for assign_job:
// plan store, registry pre-populated with one worker, audit sink, and
// the JobTaskIndex injection target.
func newAssignJobServer(t *testing.T, jobID string) (*Server, *plan.Plan, *audit.JobTaskIndex) {
	t.Helper()
	dir := t.TempDir()
	p, err := plan.Open(filepath.Join(dir, "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	a, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	idx := audit.NewJobTaskIndex()
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1}},
	}
	reg := NewRegistry()
	reg.Add(AgentEntry{ID: "worker-ada", Role: "worker", State: StateRunning})
	srv, err := New(Config{
		Bus:          bus.NewMemBus(),
		Team:         tm,
		Registry:     reg,
		Spawner:      &stubSpawner{jobID: jobID},
		Plan:         p,
		Audit:        a,
		JobTaskIndex: idx,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, p, idx
}

func callAssignJob(t *testing.T, srv *Server, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "assign_job"
	req.Params.Arguments = args
	res, err := srv.handleAssignJob(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAssignJob: %v", err)
	}
	return res
}

// TestAssignJob_RequiresTaskID asserts that a call without a task_id
// argument is rejected with an error tool result. RequireString
// surfaces the missing-arg through err; the handler maps it to a
// dedicated message naming the required field.
func TestAssignJob_RequiresTaskID(t *testing.T) {
	srv, _, _ := newAssignJobServer(t, "job-abc")
	res := callAssignJob(t, srv, map[string]any{
		"agent_id": "worker-ada",
		"prompt":   "do the thing",
	})
	if !res.IsError {
		t.Fatalf("expected error result, got %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "task_id") {
		t.Errorf("error should mention task_id, got %q", textOf(t, res))
	}
}

// TestAssignJob_RejectsUnknownTaskID asserts that a well-formed but
// non-existent task_id is rejected before the spawner is touched.
func TestAssignJob_RejectsUnknownTaskID(t *testing.T) {
	srv, _, idx := newAssignJobServer(t, "job-abc")
	res := callAssignJob(t, srv, map[string]any{
		"agent_id": "worker-ada",
		"task_id":  "t-deadbeef",
		"prompt":   "do the thing",
	})
	if !res.IsError {
		t.Fatalf("expected error result, got %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "not found") {
		t.Errorf("error should mention 'not found', got %q", textOf(t, res))
	}
	if idx.Size() != 0 {
		t.Errorf("index must stay empty when assign_job rejects; got %d", idx.Size())
	}
}

// TestAssignJob_AutoLinks asserts that a successful assign_job appends
// the returned job_id to task.Evidence in the plan store — exactly
// what link_task_to_job used to require as a follow-up call — and
// also registers the (job→task) mapping in the JobTaskIndex so the
// audit injection path can stamp meta.task_id on subsequent events.
func TestAssignJob_AutoLinks(t *testing.T) {
	srv, p, idx := newAssignJobServer(t, "job-abc")
	task, err := p.AddTask(plan.NewTaskInput{Title: "the task"})
	if err != nil {
		t.Fatal(err)
	}
	res := callAssignJob(t, srv, map[string]any{
		"agent_id": "worker-ada",
		"task_id":  task.ID,
		"prompt":   "do the thing",
	})
	if res.IsError {
		t.Fatalf("assign_job errored: %s", textOf(t, res))
	}
	got, ok := p.Get(task.ID)
	if !ok {
		t.Fatal("task vanished")
	}
	if len(got.Evidence) != 1 || got.Evidence[0] != "job-abc" {
		t.Errorf("evidence should be [job-abc], got %v", got.Evidence)
	}
	if v, ok := idx.Get("job-abc"); !ok || v != task.ID {
		t.Errorf("JobTaskIndex must hold job-abc → %s, got (%q, %v)", task.ID, v, ok)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(textOf(t, res)), &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["task_id"] != task.ID || payload["job_id"] != "job-abc" {
		t.Errorf("payload: %v", payload)
	}
}
