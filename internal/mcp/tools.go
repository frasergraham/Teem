package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/prompts"
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
	name := req.GetString("name", "")
	id, err := s.spawner.Spawn(ctx, role, name)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("spawn failed", err), nil
	}
	out, _ := json.Marshal(map[string]string{"agent_id": id})
	return mcpgo.NewToolResultText(string(out)), nil
}

// handleListRoster returns the persistent roster, optionally
// filtered by role. The wire shape uses `name` / `last_seen` to
// match MCP-facing terminology; `name` is the bare suffix when the
// id has the role prefix and the full id for operator-supplied
// (named) entries. in_use ORs the roster bit with a live
// registry check so a never-cleared roster entry can't masquerade
// as a still-running worker.
func (s *Server) handleListRoster(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	role := req.GetString("role", "")
	entries := s.spawner.RosterSnapshot(role)
	type wire struct {
		Name      string    `json:"name"`
		Role      string    `json:"role"`
		FirstSeen time.Time `json:"first_seen,omitempty"`
		LastSeen  time.Time `json:"last_seen"`
		InUse     bool      `json:"in_use"`
		Source    string    `json:"source,omitempty"`
	}
	out := make([]wire, 0, len(entries))
	for _, e := range entries {
		name := e.ID
		if prefix := e.Role + "-"; strings.HasPrefix(e.ID, prefix) {
			name = e.ID[len(prefix):]
		}
		// in_use derives from the live registry — provisioning,
		// running, and busy all count; stopped, error, and unknown
		// do not. The roster's own in_use bit can lag behind a
		// crashed worker, so we don't trust it as the source of
		// truth here.
		inUse := false
		if entry, ok := s.registry.Get(e.ID); ok {
			switch entry.State {
			case StateProvisioning, StateRunning, StateBusy:
				inUse = true
			}
		}
		out = append(out, wire{
			Name:      name,
			Role:      e.Role,
			FirstSeen: e.FirstSeen,
			LastSeen:  e.LastUsedAt,
			InUse:     inUse,
			Source:    e.Source,
		})
	}
	body, err := json.Marshal(out)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal roster", err), nil
	}
	return mcpgo.NewToolResultText(string(body)), nil
}

// taskIDPattern is the canonical regex for plan task ids: "t-" followed
// by 8 lowercase hex chars. Used as a fast-fail input check so a
// typo'd task_id surfaces a clearer error than the plan-side
// "not found" message.
var taskIDPattern = regexp.MustCompile(`^t-[a-f0-9]{8}$`)

func (s *Server) handleAssignJob(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	agentID, err := req.RequireString("agent_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError("task_id is required (every job is task-scoped; create a task with add_task first)"), nil
	}
	if !taskIDPattern.MatchString(taskID) {
		return mcpgo.NewToolResultErrorf("task_id %q does not match the canonical format ^t-[a-f0-9]{8}$", taskID), nil
	}
	prompt, err := req.RequireString("prompt")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	contextNote := req.GetString("context", "")
	if _, ok := s.registry.Get(agentID); !ok {
		return mcpgo.NewToolResultErrorf("agent %q not found; spawn it first", agentID), nil
	}
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured — assign_job needs a task_id to attribute work"), nil
	}
	if _, ok := s.plan.Get(taskID); !ok {
		return mcpgo.NewToolResultErrorf("task %q not found in the plan", taskID), nil
	}
	jobID, err := s.spawner.AssignJob(ctx, agentID, prompt, contextNote)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("assign_job failed", err), nil
	}
	// Link the new job to the task synchronously: plan.Evidence is the
	// durable record (survives daemon restart, rehydrates the index)
	// and JobTaskIndex is the fast in-memory lookup that drives
	// audit-event task_id injection. Both updates use the same
	// jobID so a restart that replays plan-from-disk reconstructs
	// the index exactly.
	if _, err := s.plan.LinkJob(taskID, jobID); err != nil {
		// Race: delete_task fired between the task-exists check
		// at line 122 and LinkJob's evidence append. The worker
		// is already running with a job that has no task
		// evidence — best-effort cancel so the spawner doesn't
		// leak a "pending" row and so the worker's incoming bus
		// message is at least dropped on the floor when we beat
		// the subscriber. The race window is narrow but the
		// failure mode (orphan worker, no attribution) is worse
		// than an over-eager cancel.
		s.spawner.CancelJob(jobID)
		return mcpgo.NewToolResultErrorFromErr("assign_job: link evidence", err), nil
	}
	s.jobTaskIdx.Set(jobID, taskID, agentID)
	out, _ := json.Marshal(map[string]string{"job_id": jobID, "task_id": taskID})
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
	if role != archmem.LeaderRole && s.team != nil && s.team.FindArchetypeByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no archetype with role %q in team roster (use %q for the per-team leader memory)", role, archmem.LeaderRole), nil
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
	if role != archmem.LeaderRole && s.team != nil && s.team.FindArchetypeByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no archetype with role %q in team roster (use %q for the per-team leader memory)", role, archmem.LeaderRole), nil
	}
	// Bound operator notes the same way job-complete summaries are
	// bounded — keeps the file from being grown unboundedly by repeated
	// calls before the next summariser run prunes by age. Also flattens
	// newlines so the bullet line stays parseable.
	const maxNoteBytes = 1024
	clean := strings.ReplaceAll(strings.ReplaceAll(note, "\r", " "), "\n", " ")
	if len(clean) > maxNoteBytes {
		end := maxNoteBytes
		for end > 0 && !utf8.RuneStart(clean[end]) {
			end--
		}
		clean = clean[:end] + "…"
	}
	entry := archmem.Entry{
		Timestamp: time.Now().UTC(),
		AgentID:   "leader",
		JobID:     "",
		Status:    "note",
		Summary:   clean,
	}
	if err := s.archMem.AppendEntry(role, entry); err != nil {
		return mcpgo.NewToolResultErrorFromErr("append_archetype_memory", err), nil
	}
	out, _ := json.Marshal(map[string]string{"appended": role})
	return mcpgo.NewToolResultText(string(out)), nil
}

// --- prompt handlers ------------------------------------------------------

// handleReadPrompt returns the assembled prompt for the role plus the
// raw override text. role is either "leader" or an archetype role.
// Unknown archetype roles error out (other than the synthetic leader).
func (s *Server) handleReadPrompt(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.prompts == nil {
		return mcpgo.NewToolResultError("prompt builder is not configured"), nil
	}
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := prompts.ValidateRole(role); err != nil {
		return mcpgo.NewToolResultErrorf("invalid role %q", role), nil
	}
	if role != prompts.LeaderRole && s.team != nil && s.team.FindArchetypeByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no archetype with role %q in team roster", role), nil
	}
	var assembled string
	if role == prompts.LeaderRole {
		assembled = s.prompts.Leader()
	} else {
		got, ok := s.prompts.Archetype(role)
		if !ok {
			return mcpgo.NewToolResultErrorf("role %q is not declared in the team's archetypes", role), nil
		}
		assembled = got
	}
	override, _, oerr := s.prompts.Override(role)
	if oerr != nil {
		return mcpgo.NewToolResultErrorFromErr("read_prompt: override", oerr), nil
	}
	out, _ := json.Marshal(map[string]string{
		"role":      role,
		"assembled": assembled,
		"override":  override,
	})
	return mcpgo.NewToolResultText(string(out)), nil
}

// handleAppendPrompt appends a timestamped block to the role's
// override file via Builder.AppendOverride.
func (s *Server) handleAppendPrompt(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.prompts == nil {
		return mcpgo.NewToolResultError("prompt builder is not configured"), nil
	}
	role, err := req.RequireString("role")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := prompts.ValidateRole(role); err != nil {
		return mcpgo.NewToolResultErrorf("invalid role %q", role), nil
	}
	if role != prompts.LeaderRole && s.team != nil && s.team.FindArchetypeByRole(role) == nil {
		return mcpgo.NewToolResultErrorf("no archetype with role %q in team roster", role), nil
	}
	if err := s.prompts.AppendOverride(role, text); err != nil {
		return mcpgo.NewToolResultErrorFromErr("append_prompt", err), nil
	}
	body, _ := json.Marshal(map[string]string{
		"role": role,
		"path": s.prompts.OverridePath(role),
	})
	return mcpgo.NewToolResultText(string(body)), nil
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

func (s *Server) handleDeleteTask(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured"), nil
	}
	id, err := req.RequireString("id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if err := s.plan.DeleteTask(id); err != nil {
		if errors.Is(err, plan.ErrTaskNotFound) {
			return mcpgo.NewToolResultErrorf("task %q not found", id), nil
		}
		return mcpgo.NewToolResultErrorFromErr("delete_task", err), nil
	}
	return mcpgo.NewToolResultText("deleted " + id), nil
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
	if v := req.GetString("stage", ""); v != "" {
		// Accept legacy stage names (building/in_review/merging) as
		// filter input — they map onto the post-rename canonical values
		// already stored on disk.
		f.Stage = plan.NormalizeStage(plan.Stage(v))
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

// --- leader-status & stage / decision / blocker tools --------------------

func (s *Server) handleUpdateLeaderStatus(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.leaderStatus == nil {
		return mcpgo.NewToolResultError("leader_status store is not configured"), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	agentID := req.GetString("agent_id", "leader")
	var taskIDs []string
	if v := req.GetString("current_task_ids", ""); v != "" {
		taskIDs = splitCSV(v)
	}
	if err := s.leaderStatus.Set(agentID, text, taskIDs); err != nil {
		return mcpgo.NewToolResultErrorFromErr("update_leader_status", err), nil
	}
	entry, _ := s.leaderStatus.Get(agentID)
	body, _ := json.Marshal(entry)
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleGetLeaderStatus(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.leaderStatus == nil {
		return mcpgo.NewToolResultError("leader_status store is not configured"), nil
	}
	entries := s.leaderStatus.All()
	out := map[string]any{}
	for _, e := range entries {
		out[e.AgentID] = e
	}
	body, _ := json.Marshal(out)
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleSetTaskStage(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	stage, err := req.RequireString("stage")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	// Accept old stage names (building/in_review/merging) alongside
	// the new ones so callers carrying over after the rename keep
	// working. NormalizeStage maps aliases to their canonical post-
	// rename value; everything else passes through to IsValidStage.
	st := plan.NormalizeStage(plan.Stage(stage))
	if !plan.IsValidStage(st) {
		return mcpgo.NewToolResultErrorf("unknown stage %q (valid: proposed, specced, planning, coding, reviewing, integrating, verified, blocked, shelved, abandoned)", stage), nil
	}
	// Capture the previous stage before mutating so the audit event
	// can record from→to. A missing pre-image is harmless (we still
	// emit the event with from="").
	var fromStage plan.Stage
	if prev, ok := s.plan.Get(taskID); ok {
		fromStage = prev.Stage
	}
	task, err := s.plan.UpdateTask(taskID, plan.UpdateInput{Stage: st})
	if err != nil {
		switch {
		case errors.Is(err, plan.ErrTaskNotFound):
			return mcpgo.NewToolResultErrorf("task %q not found", taskID), nil
		case errors.Is(err, plan.ErrInvalidStage):
			return mcpgo.NewToolResultErrorf("illegal stage transition to %q", stage), nil
		default:
			return mcpgo.NewToolResultErrorFromErr("set_task_stage", err), nil
		}
	}
	if s.audit != nil && fromStage != st {
		_ = s.audit.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "leader",
			Kind:      audit.KindTaskStageChanged,
			Message:   fmt.Sprintf("task %s: %s → %s", taskID, fromStage, st),
			Meta: map[string]any{
				"task_id": taskID,
				"from":    string(fromStage),
				"stage":   string(st),
			},
		})
	}
	body, _ := json.Marshal(task)
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleRecordDecision(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.audit == nil {
		return mcpgo.NewToolResultError("audit sink is not configured"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	// Sanity check: the task should exist when plan is wired. Lets
	// the caller catch typos before scattering notes against
	// nonexistent ids.
	if s.plan != nil {
		if _, ok := s.plan.Get(taskID); !ok {
			return mcpgo.NewToolResultErrorf("task %q not found", taskID), nil
		}
	}
	agentID := req.GetString("agent_id", "leader")
	if agentID == "" {
		agentID = "leader"
	}
	severity := strings.ToLower(strings.TrimSpace(req.GetString("severity", "info")))
	switch severity {
	case "", "info":
		severity = "info"
	case "question":
		// allowed
	default:
		return mcpgo.NewToolResultErrorf("record_decision: severity must be 'info' or 'question', got %q", severity), nil
	}
	meta := map[string]any{"task_id": taskID, "severity": severity}
	ev := audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   agentID,
		Kind:      audit.KindDecisionNote,
		Message:   text,
		Meta:      meta,
	}
	if err := s.audit.Write(ev); err != nil {
		return mcpgo.NewToolResultErrorFromErr("record_decision", err), nil
	}
	body, _ := json.Marshal(map[string]string{"recorded": taskID, "severity": severity})
	return mcpgo.NewToolResultText(string(body)), nil
}

func (s *Server) handleRecordBlocker(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.audit == nil {
		return mcpgo.NewToolResultError("audit sink is not configured"), nil
	}
	if s.plan == nil {
		return mcpgo.NewToolResultError("plan store is not configured"), nil
	}
	taskID, err := req.RequireString("task_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	cur, ok := s.plan.Get(taskID)
	if !ok {
		return mcpgo.NewToolResultErrorf("task %q not found", taskID), nil
	}
	// Atomic-effect intent: move the task to blocked stage AND
	// status, then write a single audit event. UpdateTask is a single
	// write under the plan mutex; the audit write is a separate file.
	// We accept that interleaving order — the audit_kind=blocker_note
	// event with task_id meta is what the dashboard joins on, not the
	// plan store.
	//
	// Skip the UpdateTask entirely when the task is already at the
	// blocked stage (and status) so an idempotent re-block doesn't
	// fail the transition check. Any other rejection is a real error
	// — surface it to the caller WITH current stage so they can decide,
	// and do NOT emit the audit note (otherwise the audit and plan
	// stores disagree).
	if !(cur.Stage == plan.StageBlocked && cur.Status == plan.StatusBlocked) {
		if _, err := s.plan.UpdateTask(taskID, plan.UpdateInput{
			Status: plan.StatusBlocked,
			Stage:  plan.StageBlocked,
		}); err != nil {
			if errors.Is(err, plan.ErrInvalidStage) {
				return mcpgo.NewToolResultErrorf(
					"record_blocker: stage %q cannot transition to blocked (current status %q); fix the stage first or call set_task_stage",
					cur.Stage, cur.Status), nil
			}
			return mcpgo.NewToolResultErrorFromErr("record_blocker", err), nil
		}
	}
	agentID := req.GetString("agent_id", "leader")
	if agentID == "" {
		agentID = "leader"
	}
	ev := audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   agentID,
		Kind:      audit.KindBlockerNote,
		Message:   text,
		Meta:      map[string]any{"task_id": taskID},
	}
	if err := s.audit.Write(ev); err != nil {
		return mcpgo.NewToolResultErrorFromErr("record_blocker: audit", err), nil
	}
	body, _ := json.Marshal(map[string]string{"blocked": taskID})
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
