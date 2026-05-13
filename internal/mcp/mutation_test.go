package mcp

import (
	"context"
	"errors"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/team"
)

// fakeSpawner implements the Spawner interface for tests. It tracks
// which agent ids and roles are "running" so the IsRunning /
// AnyRunningWithRole / StopAgent flows can be exercised.
type fakeSpawner struct {
	running map[string]bool
	roles   map[string]string // agent_id → role
}

func (f *fakeSpawner) SpawnByRole(ctx context.Context, role string) (string, error) {
	return "", nil
}
func (f *fakeSpawner) AssignJob(ctx context.Context, agentID, prompt, contextNote string) (string, error) {
	return "", nil
}
func (f *fakeSpawner) JobStatus(jobID string) (string, string, bool) { return "", "", false }
func (f *fakeSpawner) IsRunning(id string) bool                      { return f.running[id] }
func (f *fakeSpawner) AnyRunningWithRole(role string) bool {
	for id, r := range f.roles {
		if r == role && f.running[id] {
			return true
		}
	}
	return false
}
func (f *fakeSpawner) StopAgent(ctx context.Context, id string) error {
	delete(f.running, id)
	return nil
}

func newTestServer(t *testing.T, tm *team.Team, sp Spawner) *Server {
	t.Helper()
	srv, err := New(Config{
		Bus:      bus.NewMemBus(),
		Team:     tm,
		Registry: NewRegistry(),
		Spawner:  sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func callTool(t *testing.T, srv *Server, name string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	var res *mcpgo.CallToolResult
	var err error
	switch name {
	case "add_archetype":
		res, err = srv.handleAddArchetype(context.Background(), req)
	case "remove_archetype":
		res, err = srv.handleRemoveArchetype(context.Background(), req)
	case "update_archetype":
		res, err = srv.handleUpdateArchetype(context.Background(), req)
	case "stop_agent":
		res, err = srv.handleStopAgent(context.Background(), req)
	case "read_team":
		res, err = srv.handleReadTeam(context.Background(), req)
	default:
		t.Fatalf("no test dispatch for tool %q", name)
	}
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func resultText(t *testing.T, r *mcpgo.CallToolResult) string {
	t.Helper()
	if r == nil {
		t.Fatal("nil result")
	}
	if len(r.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := r.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("not text content: %T", r.Content[0])
	}
	return tc.Text
}

func resultIsError(r *mcpgo.CallToolResult) bool {
	if r == nil {
		return false
	}
	return r.IsError
}

func TestAddArchetype_HappyPath(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	sp := &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}}
	srv := newTestServer(t, tm, sp)

	res := callTool(t, srv, "add_archetype", map[string]any{
		"role":           "reviewer",
		"placement":      "local",
		"max_concurrent": "3",
		"description":    "Reads diffs",
	})
	if resultIsError(res) {
		t.Fatalf("add_archetype should succeed, got: %s", resultText(t, res))
	}
	a := tm.FindArchetypeByRole("reviewer")
	if a == nil {
		t.Fatal("reviewer not in roster after add")
	}
	if a.Placement != "local" || a.MaxConcurrent != 3 || a.Description != "Reads diffs" {
		t.Errorf("unexpected archetype: %+v", a)
	}
}

func TestAddArchetype_DuplicateRejected(t *testing.T) {
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "reviewer", Placement: "local", MaxConcurrent: 1}},
	}
	sp := &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}}
	srv := newTestServer(t, tm, sp)

	res := callTool(t, srv, "add_archetype", map[string]any{
		"role":           "reviewer",
		"placement":      "local",
		"max_concurrent": "1",
	})
	if !resultIsError(res) {
		t.Errorf("duplicate add should error, got: %s", resultText(t, res))
	}
}

func TestRemoveArchetype_RefusesWhileRunning(t *testing.T) {
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 2}},
	}
	sp := &fakeSpawner{
		running: map[string]bool{"worker-1": true},
		roles:   map[string]string{"worker-1": "worker"},
	}
	srv := newTestServer(t, tm, sp)
	res := callTool(t, srv, "remove_archetype", map[string]any{"role": "worker"})
	if !resultIsError(res) {
		t.Errorf("remove with running instance should error")
	}
	if tm.FindArchetypeByRole("worker") == nil {
		t.Errorf("archetype removed despite running instance")
	}
}

func TestRemoveArchetype_HappyPath(t *testing.T) {
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1}},
	}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}})
	res := callTool(t, srv, "remove_archetype", map[string]any{"role": "worker"})
	if resultIsError(res) {
		t.Fatalf("remove failed: %s", resultText(t, res))
	}
	if tm.FindArchetypeByRole("worker") != nil {
		t.Errorf("archetype still in roster after remove")
	}
}

func TestStopAgent_CallsSpawner(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	sp := &fakeSpawner{
		running: map[string]bool{"worker-1": true},
		roles:   map[string]string{"worker-1": "worker"},
	}
	srv := newTestServer(t, tm, sp)
	res := callTool(t, srv, "stop_agent", map[string]any{"agent_id": "worker-1"})
	if resultIsError(res) {
		t.Fatalf("stop_agent failed: %s", resultText(t, res))
	}
	if sp.IsRunning("worker-1") {
		t.Errorf("agent still marked running after stop_agent")
	}
}

func TestUpdateArchetype_Description(t *testing.T) {
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1, Description: "old"}},
	}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}})
	res := callTool(t, srv, "update_archetype", map[string]any{
		"role":        "worker",
		"description": "new",
	})
	if resultIsError(res) {
		t.Fatalf("update failed: %s", resultText(t, res))
	}
	a := tm.FindArchetypeByRole("worker")
	if a == nil || a.Description != "new" {
		t.Errorf("description not updated: %+v", a)
	}
}

func TestUpdateArchetype_MaxConcurrent(t *testing.T) {
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1}},
	}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}})
	res := callTool(t, srv, "update_archetype", map[string]any{
		"role":           "worker",
		"max_concurrent": "5",
	})
	if resultIsError(res) {
		t.Fatalf("update failed: %s", resultText(t, res))
	}
	a := tm.FindArchetypeByRole("worker")
	if a == nil || a.MaxConcurrent != 5 {
		t.Errorf("max not updated: %+v", a)
	}
}

func TestUpdateArchetype_MissingRole(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}})
	res := callTool(t, srv, "update_archetype", map[string]any{
		"role":        "nope",
		"description": "anything",
	})
	if !resultIsError(res) {
		t.Errorf("missing archetype should error")
	}
}

func TestArchetype_ErrorTypes(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	if err := tm.RemoveArchetype("nope"); !errors.Is(err, team.ErrArchetypeNotFound) {
		t.Errorf("RemoveArchetype missing should return ErrArchetypeNotFound, got %v", err)
	}
	if err := tm.UpdateArchetypeDescription("nope", "x"); !errors.Is(err, team.ErrArchetypeNotFound) {
		t.Errorf("UpdateArchetypeDescription missing should return ErrArchetypeNotFound, got %v", err)
	}
}
