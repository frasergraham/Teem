package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

func (s *Server) handleSpawnAgent(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if s.team.FindArchetypeByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no archetype with role %q in team roster", role), nil
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

func (s *Server) handleAddArchetype(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	placement, err := req.RequireString("placement")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	maxStr, err := req.RequireString("max_concurrent")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	maxN, err := strconv.Atoi(maxStr)
	if err != nil || maxN <= 0 {
		return mcpgo.NewToolResultErrorf("max_concurrent must be a positive integer (got %q)", maxStr), nil
	}
	spec := team.ArchetypeSpec{
		Role:          role,
		Description:   req.GetString("description", ""),
		Placement:     placement,
		WorkingDir:    req.GetString("working_dir", ""),
		MaxConcurrent: maxN,
		Lifecycle:     req.GetString("lifecycle", ""),
	}
	if err := s.team.AddArchetype(spec); err != nil {
		if errors.Is(err, team.ErrArchetypeExists) {
			return mcpgo.NewToolResultErrorf("archetype %q already in roster", role), nil
		}
		return mcpgo.NewToolResultErrorFromErr("add_archetype", err), nil
	}
	out, _ := json.Marshal(map[string]any{"role": role, "max_concurrent": maxN})
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleRemoveArchetype(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if s.spawner.AnyRunningWithRole(role) {
		return mcpgo.NewToolResultErrorf("archetype %q has running instances — stop_agent them first", role), nil
	}
	if err := s.team.RemoveArchetype(role); err != nil {
		if errors.Is(err, team.ErrArchetypeNotFound) {
			return mcpgo.NewToolResultErrorf("archetype %q not in roster", role), nil
		}
		return mcpgo.NewToolResultErrorFromErr("remove_archetype", err), nil
	}
	out, _ := json.Marshal(map[string]string{"removed": role})
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleStopAgent(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	agentID, err := req.RequireString("agent_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.spawner.StopAgent(ctx, agentID); err != nil {
		return mcpgo.NewToolResultErrorFromErr("stop_agent", err), nil
	}
	out, _ := json.Marshal(map[string]string{"stopped": agentID})
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleUpdateArchetype(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if desc := req.GetString("description", ""); desc != "" {
		if err := s.team.UpdateArchetypeDescription(role, desc); err != nil {
			if errors.Is(err, team.ErrArchetypeNotFound) {
				return mcpgo.NewToolResultErrorf("archetype %q not in roster", role), nil
			}
			return mcpgo.NewToolResultErrorFromErr("update_archetype: description", err), nil
		}
	}
	if maxStr := req.GetString("max_concurrent", ""); maxStr != "" {
		n, err := strconv.Atoi(maxStr)
		if err != nil || n <= 0 {
			return mcpgo.NewToolResultErrorf("max_concurrent must be a positive integer (got %q)", maxStr), nil
		}
		if err := s.team.SetArchetypeMaxConcurrent(role, n); err != nil {
			if errors.Is(err, team.ErrArchetypeNotFound) {
				return mcpgo.NewToolResultErrorf("archetype %q not in roster", role), nil
			}
			return mcpgo.NewToolResultErrorFromErr("update_archetype: max", err), nil
		}
	}
	out, _ := json.Marshal(map[string]string{"updated": role})
	return mcpgo.NewToolResultText(string(out)), nil
}

// --- archetype memory handlers --------------------------------------------

func (s *Server) handleReadArchetypeMemory(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.archMem == nil {
		return mcpgo.NewToolResultError("archetype memory is not configured"), nil
	}
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if s.team != nil && s.team.FindArchetypeByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no archetype with role %q in team roster", role), nil
	}
	body, err := s.archMem.Load(role)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("read_archetype_memory", err), nil
	}
	out, _ := json.Marshal(map[string]string{"role": role, "body": body})
	return mcpgo.NewToolResultText(string(out)), nil
}

func (s *Server) handleAppendArchetypeMemory(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.archMem == nil {
		return mcpgo.NewToolResultError("archetype memory is not configured"), nil
	}
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	note, err := req.RequireString("note")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if s.team != nil && s.team.FindArchetypeByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no archetype with role %q in team roster", role), nil
	}
	entry := archmem.Entry{
		Timestamp: time.Now().UTC(),
		AgentID:   "leader",
		JobID:     "",
		Status:    "note",
		Summary:   note,
	}
	if err := s.archMem.AppendEntry(role, entry); err != nil {
		return mcpgo.NewToolResultErrorFromErr("append_archetype_memory", err), nil
	}
	out, _ := json.Marshal(map[string]string{"appended": role})
	return mcpgo.NewToolResultText(string(out)), nil
}

// --- notes handler --------------------------------------------------------

func (s *Server) handleWriteUserNote(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.notes == nil {
		return mcpgo.NewToolResultError("notes inbox is not configured"), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.notes.Write(notes.Note{Text: text, Timestamp: time.Now().UTC()}); err != nil {
		return mcpgo.NewToolResultErrorFromErr("write_user_note", err), nil
	}
	body, _ := json.Marshal(map[string]string{"queued": "ok"})
	return mcpgo.NewToolResultText(string(body)), nil
}

// --- plan / task handlers -------------------------------------------------

func (s *Server) handleAddTask(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured"), nil
	}
	title, err := req.RequireString("title")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	in := plan.NewTaskInput{
		Title:    title,
		ParentID: req.GetString("parent_id", ""),
		Notes:    req.GetString("notes", ""),
	}
	if v := req.GetString("depends_on", ""); v != "" {
		in.DependsOn = splitCSV(v)
	}
	task, err := s.plan.AddTask(in)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("add_task", err), nil
	}
	body, _ := json.Marshal(task)
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleUpdateTask(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured"), nil
	}
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	in := plan.UpdateInput{}
	if v := req.GetString("status", ""); v != "" {
		in.Status = plan.Status(v)
	}
	if v := req.GetString("assigned_to", ""); v != "" {
		s := v
		in.AssignedTo = &s
	}
	// "notes" present-but-empty = "clear notes". We treat the missing
	// arg as "leave alone", which is the common case for the leader.
	if _, ok := req.Params.Arguments.(map[string]any)["notes"]; ok {
		v := req.GetString("notes", "")
		in.Notes = &v
	}
	if v := req.GetString("depends_on", ""); v != "" {
		deps := splitCSV(v)
		in.DependsOn = &deps
	}
	if v := req.GetString("add_evidence", ""); v != "" {
		in.AddEvidence = splitCSV(v)
	}
	task, err := s.plan.UpdateTask(id, in)
	if err != nil {
		if errors.Is(err, plan.ErrTaskNotFound) {
			return mcpgo.NewToolResultErrorf("task %q not found", id), nil
		}
		return mcpgo.NewToolResultErrorFromErr("update_task", err), nil
	}
	body, _ := json.Marshal(task)
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleListTasks(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured"), nil
	}
	f := plan.Filter{
		ParentID: req.GetString("parent_id", ""),
	}
	if v := req.GetString("status", ""); v != "" {
		f.Status = plan.Status(v)
	}
	if req.GetString("open_only", "") == "true" {
		f.OpenOnly = true
	}
	tasks := s.plan.List(f)
	body, _ := json.Marshal(tasks)
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleLinkTaskToJob(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	task, err := s.plan.LinkJob(taskID, jobID)
	if err != nil {
		if errors.Is(err, plan.ErrTaskNotFound) {
			return mcpgo.NewToolResultErrorf("task %q not found", taskID), nil
		}
		return mcpgo.NewToolResultErrorFromErr("link_task_to_job", err), nil
	}
	body, _ := json.Marshal(task)
	return mcpgo.NewToolResultText(string(body)), nil
}

// splitCSV trims whitespace and drops empty entries from a
// comma-separated MCP-string-arg.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleRecallJobs(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.audit == nil {
		return mcpgo.NewToolResultError("audit log is not configured"), nil
	}
	agentID := req.GetString("agent_id", "")
	var since time.Time
	if v := req.GetString("since", ""); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return mcpgo.NewToolResultErrorf("bad since %q: %v", v, err), nil
		}
		since = t
	}
	limit := 25
	if v := req.GetString("limit", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return mcpgo.NewToolResultErrorf("bad limit %q", v), nil
		}
		limit = n
	}
	// Pull a generous slice of events and let MaterializeJobs do the
	// joining. Most teams won't exceed a few thousand events in a
	// recent window; bigger logs can use --since to bound.
	events, err := s.audit.Query(agentID, since, 4096)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("audit query", err), nil
	}
	jobs := audit.MaterializeJobs(events)
	// Newest first, then trim to limit.
	if len(jobs) > 1 {
		for i, j := 0, len(jobs)-1; i < j; i, j = i+1, j-1 {
			jobs[i], jobs[j] = jobs[j], jobs[i]
		}
	}
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	body, err := json.Marshal(jobs)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal jobs", err), nil
	}
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleQueryAudit(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.audit == nil {
		return mcpgo.NewToolResultError("audit log is not configured"), nil
	}
	agentID := req.GetString("agent_id", "")
	var since time.Time
	if v := req.GetString("since", ""); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return mcpgo.NewToolResultErrorf("bad since %q: %v", v, err), nil
		}
		since = t
	}
	limit := 50
	if v := req.GetString("limit", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return mcpgo.NewToolResultErrorf("bad limit %q", v), nil
		}
		limit = n
	}
	events, err := s.audit.Query(agentID, since, limit)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("audit query", err), nil
	}
	body, err := json.Marshal(events)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal audit", err), nil
	}
	return mcpgo.NewToolResultText(string(body)), nil
}
