package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/prompts"
	"github.com/frasergraham/teem/internal/team"
)

func newPromptServer(t *testing.T) (*Server, *prompts.Builder) {
	t.Helper()
	tm := &team.Team{
		Name:   "t",
		Leader: team.LeaderSpec{SystemPrompt: "Ship the MVP."},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Description: "Implements features.", Placement: "local", MaxConcurrent: 1},
		},
	}
	pb := prompts.New(tm, t.TempDir())
	srv, err := New(Config{
		Bus:      bus.NewMemBus(),
		Team:     tm,
		Registry: NewRegistry(),
		Spawner:  &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}},
		Prompts:  pb,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, pb
}

func TestReadPromptTool_Leader(t *testing.T) {
	srv, _ := newPromptServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read_prompt"
	req.Params.Arguments = map[string]any{"role": "leader"}
	res, err := srv.handleReadPrompt(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	var resp struct {
		Role      string `json:"role"`
		Assembled string `json:"assembled"`
		Override  string `json:"override"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Role != "leader" {
		t.Errorf("role = %q", resp.Role)
	}
	if !strings.Contains(resp.Assembled, "Ship the MVP.") {
		t.Errorf("assembled missing leader brief:\n%s", resp.Assembled)
	}
	if resp.Override != "" {
		t.Errorf("override should be empty:\n%s", resp.Override)
	}
}

func TestReadPromptTool_ArchetypeWithOverride(t *testing.T) {
	srv, pb := newPromptServer(t)
	if err := pb.AppendOverride("worker", "Always run go vet."); err != nil {
		t.Fatalf("append: %v", err)
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read_prompt"
	req.Params.Arguments = map[string]any{"role": "worker"}
	res, err := srv.handleReadPrompt(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("handler: err=%v isErr=%v body=%s", err, res.IsError, resultText(t, res))
	}
	var resp struct {
		Role      string `json:"role"`
		Assembled string `json:"assembled"`
		Override  string `json:"override"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(resp.Assembled, "Implements features.") {
		t.Errorf("assembled missing YAML description:\n%s", resp.Assembled)
	}
	if !strings.Contains(resp.Assembled, "Always run go vet.") {
		t.Errorf("assembled missing override:\n%s", resp.Assembled)
	}
	if !strings.Contains(resp.Override, "Always run go vet.") {
		t.Errorf("override field missing text:\n%s", resp.Override)
	}
}

func TestReadPromptTool_UnknownRole(t *testing.T) {
	srv, _ := newPromptServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read_prompt"
	req.Params.Arguments = map[string]any{"role": "ghost"}
	res, _ := srv.handleReadPrompt(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected error for unknown role")
	}
}

func TestReadPromptTool_RejectsBadRole(t *testing.T) {
	srv, _ := newPromptServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "read_prompt"
	req.Params.Arguments = map[string]any{"role": "../etc/passwd"}
	res, _ := srv.handleReadPrompt(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected error for traversal role")
	}
}

func TestAppendPromptTool_RoundTrip(t *testing.T) {
	srv, pb := newPromptServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "append_prompt"
	req.Params.Arguments = map[string]any{
		"role": "worker",
		"text": "prefer tests over assertions",
	}
	res, err := srv.handleAppendPrompt(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("append: err=%v isErr=%v body=%s", err, res.IsError, resultText(t, res))
	}
	body, ok, err := pb.Override("worker")
	if err != nil || !ok {
		t.Fatalf("override after append: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(body, "prefer tests over assertions") {
		t.Errorf("override missing appended text:\n%s", body)
	}
	if !strings.Contains(body, "## Appended ") {
		t.Errorf("expected timestamp header in:\n%s", body)
	}
}

func TestAppendPromptTool_UnknownRole(t *testing.T) {
	srv, _ := newPromptServer(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "append_prompt"
	req.Params.Arguments = map[string]any{"role": "ghost", "text": "x"}
	res, _ := srv.handleAppendPrompt(context.Background(), req)
	if !res.IsError {
		t.Errorf("expected error appending to unknown role")
	}
}
