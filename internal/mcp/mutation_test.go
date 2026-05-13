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
// which agent ids are "running" so IsRunning / StopAgent flows can be
// exercised.
type fakeSpawner struct {
	running map[string]bool
}

func (f *fakeSpawner) SpawnByRole(ctx context.Context, role string) (string, error) {
	return "", nil
}
func (f *fakeSpawner) AssignJob(ctx context.Context, agentID, prompt, contextNote string) (string, error) {
	return "", nil
}
func (f *fakeSpawner) JobStatus(jobID string) (string, string, bool) { return "", "", false }
func (f *fakeSpawner) IsRunning(id string) bool                       { return f.running[id] }
func (f *fakeSpawner) StopAgent(ctx context.Context, id string) error {
	if !f.running[id] {
		return nil
	}
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

	// Dispatch by name to the matching handler — tool dispatch in the
	// real server goes through the streamable HTTP layer, which is more
	// machinery than a unit test needs.
	var res *mcpgo.CallToolResult
	var err error
	switch name {
	case "add_agent":
		res, err = srv.handleAddAgent(context.Background(), req)
	case "remove_agent":
		res, err = srv.handleRemoveAgent(context.Background(), req)
	case "stop_agent":
		res, err = srv.handleStopAgent(context.Background(), req)
	case "update_agent_description":
		res, err = srv.handleUpdateAgentDescription(context.Background(), req)
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

func TestAddAgent_HappyPath(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	sp := &fakeSpawner{running: map[string]bool{}}
	srv := newTestServer(t, tm, sp)

	res := callTool(t, srv, "add_agent", map[string]any{
		"id":          "rv-1",
		"role":        "reviewer",
		"placement":   "local",
		"description": "Reviews diffs",
	})
	if resultIsError(res) {
		t.Fatalf("add_agent should succeed, got: %s", resultText(t, res))
	}
	a := tm.FindAgentByID("rv-1")
	if a == nil {
		t.Fatal("rv-1 not in roster after add")
	}
	if !a.Local || a.Description != "Reviews diffs" {
		t.Errorf("unexpected agent: %+v", a)
	}
}

func TestAddAgent_DuplicateRejected(t *testing.T) {
	tm := &team.Team{
		Name:   "t",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Agents: []team.AgentSpec{{ID: "rv-1", Role: "reviewer", Local: true}},
	}
	sp := &fakeSpawner{running: map[string]bool{}}
	srv := newTestServer(t, tm, sp)

	res := callTool(t, srv, "add_agent", map[string]any{
		"id":        "rv-1",
		"role":      "reviewer",
		"placement": "local",
	})
	if !resultIsError(res) {
		t.Errorf("duplicate add should error, got: %s", resultText(t, res))
	}
}

func TestAddAgent_SSHRequiresWorkingDir(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}})
	res := callTool(t, srv, "add_agent", map[string]any{
		"id":        "rmt-1",
		"role":      "remote",
		"placement": "ssh:user@box",
	})
	if !resultIsError(res) {
		t.Errorf("ssh without working_dir should error")
	}
}

func TestRemoveAgent_RefusesWhileRunning(t *testing.T) {
	tm := &team.Team{
		Name:   "t",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Agents: []team.AgentSpec{{ID: "wk-1", Role: "worker", Local: true}},
	}
	sp := &fakeSpawner{running: map[string]bool{"wk-1": true}}
	srv := newTestServer(t, tm, sp)
	res := callTool(t, srv, "remove_agent", map[string]any{"agent_id": "wk-1"})
	if !resultIsError(res) {
		t.Errorf("remove of running agent should error")
	}
	if tm.FindAgentByID("wk-1") == nil {
		t.Errorf("agent removed despite error")
	}
}

func TestRemoveAgent_HappyPath(t *testing.T) {
	tm := &team.Team{
		Name:   "t",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Agents: []team.AgentSpec{{ID: "wk-1", Role: "worker", Local: true}},
	}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}})
	res := callTool(t, srv, "remove_agent", map[string]any{"agent_id": "wk-1"})
	if resultIsError(res) {
		t.Fatalf("remove failed: %s", resultText(t, res))
	}
	if tm.FindAgentByID("wk-1") != nil {
		t.Errorf("agent still in roster after remove")
	}
}

func TestStopAgent_CallsSpawner(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	sp := &fakeSpawner{running: map[string]bool{"wk-1": true}}
	srv := newTestServer(t, tm, sp)
	res := callTool(t, srv, "stop_agent", map[string]any{"agent_id": "wk-1"})
	if resultIsError(res) {
		t.Fatalf("stop_agent failed: %s", resultText(t, res))
	}
	if sp.IsRunning("wk-1") {
		t.Errorf("agent still marked running after stop_agent")
	}
}

func TestUpdateAgentDescription(t *testing.T) {
	tm := &team.Team{
		Name:   "t",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Agents: []team.AgentSpec{{ID: "wk-1", Role: "worker", Local: true, Description: "old"}},
	}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}})
	res := callTool(t, srv, "update_agent_description", map[string]any{
		"agent_id":    "wk-1",
		"description": "new",
	})
	if resultIsError(res) {
		t.Fatalf("update failed: %s", resultText(t, res))
	}
	a := tm.FindAgentByID("wk-1")
	if a == nil || a.Description != "new" {
		t.Errorf("description not updated: %+v", a)
	}
}

func TestUpdateAgentDescription_MissingAgent(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	srv := newTestServer(t, tm, &fakeSpawner{running: map[string]bool{}})
	res := callTool(t, srv, "update_agent_description", map[string]any{
		"agent_id":    "nope",
		"description": "anything",
	})
	if !resultIsError(res) {
		t.Errorf("missing agent should error")
	}
}

// Belt-and-suspenders: make sure ErrAgentNotFound is wired through.
func TestAddRemoveUpdate_ErrorTypes(t *testing.T) {
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	if err := tm.RemoveAgent("nope"); !errors.Is(err, team.ErrAgentNotFound) {
		t.Errorf("RemoveAgent missing should return ErrAgentNotFound, got %v", err)
	}
	if err := tm.UpdateAgentDescription("nope", "x"); !errors.Is(err, team.ErrAgentNotFound) {
		t.Errorf("UpdateAgentDescription missing should return ErrAgentNotFound, got %v", err)
	}
}
