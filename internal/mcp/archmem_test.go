package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/team"
)

func newArchMemServer(t *testing.T) (*Server, *archmem.Store, *team.Team) {
	t.Helper()
	tm := &team.Team{
		Name:   "t",
		Leader: team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Placement: "local", MaxConcurrent: 1},
		},
	}
	dir := t.TempDir()
	store := archmem.New(dir, func() []string { return []string{"worker"} })
	srv, err := New(Config{
		Bus:      bus.NewMemBus(),
		Team:     tm,
		Registry: NewRegistry(),
		Spawner:  &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}},
		ArchMem:  store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, store, tm
}

func TestReadArchetypeMemoryTool(t *testing.T) {
	srv, store, _ := newArchMemServer(t)
	// Seed an entry so there's something to read.
	if err := store.AppendEntry("worker", archmem.Entry{
		AgentID: "worker-1",
		JobID:   "abc",
		Status:  "done",
		Summary: "edited spawner.go",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read_archetype_memory"
	req.Params.Arguments = map[string]any{"role": "worker"}
	res, err := srv.handleReadArchetypeMemory(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	var resp struct {
		Role string `json:"role"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Role != "worker" {
		t.Errorf("role = %q, want worker", resp.Role)
	}
	if !strings.Contains(resp.Body, "edited spawner.go") {
		t.Errorf("body missing entry text:\n%s", resp.Body)
	}
}

func TestReadArchetypeMemoryTool_UnknownRole(t *testing.T) {
	srv, _, _ := newArchMemServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read_archetype_memory"
	req.Params.Arguments = map[string]any{"role": "ghost"}
	res, _ := srv.handleReadArchetypeMemory(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected error for unknown role, got: %s", resultText(t, res))
	}
}

func TestAppendArchetypeMemoryTool(t *testing.T) {
	srv, store, _ := newArchMemServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "append_archetype_memory"
	req.Params.Arguments = map[string]any{
		"role": "worker",
		"note": "always run gofmt before claiming done",
	}
	res, err := srv.handleAppendArchetypeMemory(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	body, _ := store.Load("worker")
	if !strings.Contains(body, "always run gofmt before claiming done") {
		t.Errorf("note not appended:\n%s", body)
	}
	if !strings.Contains(body, "leader") {
		t.Errorf("appended note should mention leader agent_id:\n%s", body)
	}
}

func TestAppendArchetypeMemoryTool_RejectsUnknownRole(t *testing.T) {
	srv, _, _ := newArchMemServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "append_archetype_memory"
	req.Params.Arguments = map[string]any{
		"role": "ghost",
		"note": "x",
	}
	res, _ := srv.handleAppendArchetypeMemory(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected error appending to unknown role")
	}
}
