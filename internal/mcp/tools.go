package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) handleSpawnAgent(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if s.team.FindAgentByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no agent with role %q in team roster", role), nil
	}
	id, err := s.spawner.SpawnByRole(ctx, role)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("spawn failed", err), nil
	}
	out, _ := json.Marshal(map[string]string{"agent_id": id})
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleAssignJob(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	agentID, err := req.RequireString("agent_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	prompt, err := req.RequireString("prompt")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	contextNote := req.GetString("context", "")
	if _, ok := s.registry.Get(agentID); !ok {
		return mcpgo.NewToolResultErrorf("agent %q not found; spawn it first", agentID), nil
	}
	jobID, err := s.spawner.AssignJob(ctx, agentID, prompt, contextNote)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("assign_job failed", err), nil
	}
	out, _ := json.Marshal(map[string]string{"job_id": jobID})
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleGetResults(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	status, output, found := s.spawner.JobStatus(jobID)
	if !found {
		return mcpgo.NewToolResultErrorf("job %q not found", jobID), nil
	}
	out, _ := json.Marshal(map[string]string{
		"status": status,
		"output": output,
	})
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleListAgents(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	agents := s.registry.List()
	out, err := json.Marshal(agents)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal agents", err), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleQueryBus(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	topic, err := req.RequireString("topic")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	msgs := s.bus.Recent(topic, 32)
	type wireMsg struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		From      string `json:"from,omitempty"`
		To        string `json:"to,omitempty"`
		Payload   string `json:"payload,omitempty"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]wireMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, wireMsg{
			ID:        m.ID,
			Kind:      string(m.Kind),
			From:      m.From,
			To:        m.To,
			Payload:   string(m.Payload),
			CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	body, err := json.Marshal(out)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal bus messages", err), nil
	}
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleReadTeam(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	body, err := json.Marshal(s.team)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal team", err), nil
	}
	return mcpgo.NewToolResultText(string(body)), nil
}

