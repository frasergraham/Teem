package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

// Spawner is the abstraction the spawn_agent MCP tool calls into.
// Implemented in internal/agent to avoid an import cycle.
type Spawner interface {
	SpawnByRole(ctx context.Context, role string) (string, error)
	AssignJob(ctx context.Context, agentID, prompt, contextNote string) (string, error)
	JobStatus(jobID string) (status string, output string, found bool)
	StopAgent(ctx context.Context, agentID string) error
	IsRunning(agentID string) bool
	AnyRunningWithRole(role string) bool
}

// Server bundles the MCP server, its handler, and the dependencies its
// tools close over.
type Server struct {
	core    *mcpsrv.MCPServer
	handler http.Handler
	http    *http.Server

	bus            bus.Bus
	team           *team.Team
	registry       *Registry
	spawner        Spawner
	audit          audit.Sink
	plan           *plan.Plan
	notes          *notes.Inbox
	archMem        *archmem.Store
	transcriptsDir string
}

// Config holds the deps the orchestrator server needs.
type Config struct {
	Bus      bus.Bus
	Team     *team.Team
	Registry *Registry
	Spawner  Spawner
	// Audit is the audit-log Sink the query_audit tool reads from. Optional
	// — if nil the tool returns an error explaining audit isn't configured.
	Audit audit.Sink
	// Plan is the task store the add_task/update_task/list_tasks tools
	// operate on. Optional — if nil those tools return a clear error.
	Plan *plan.Plan
	// Notes is the user-notes inbox the write_user_note tool appends
	// to. Optional — if nil the tool returns a clear error.
	Notes *notes.Inbox
	// TranscriptsDir is the leader-side root directory mirroring
	// worker transcripts (<dir>/<agent_id>/<job_id>.jsonl). When
	// empty, the get_job_transcript tool returns an error explaining
	// transcripts aren't configured.
	TranscriptsDir string
	// ArchMem is the per-archetype memory store. When nil the
	// read_archetype_memory / append_archetype_memory tools return
	// an error explaining the feature is unconfigured.
	ArchMem *archmem.Store
}

// New builds an orchestrator MCP server. Call Serve to start serving on a
// listener.
func New(cfg Config) (*Server, error) {
	if cfg.Bus == nil || cfg.Team == nil || cfg.Registry == nil || cfg.Spawner == nil {
		return nil, errors.New("mcp: Config requires Bus, Team, Registry, Spawner")
	}
	core := mcpsrv.NewMCPServer(
		"teem-orchestrator",
		"0.1.0",
		mcpsrv.WithToolCapabilities(true),
	)
	s := &Server{
		core:           core,
		bus:            cfg.Bus,
		team:           cfg.Team,
		registry:       cfg.Registry,
		spawner:        cfg.Spawner,
		audit:          cfg.Audit,
		plan:           cfg.Plan,
		notes:          cfg.Notes,
		archMem:        cfg.ArchMem,
		transcriptsDir: cfg.TranscriptsDir,
	}
	s.registerTools()
	s.handler = mcpsrv.NewStreamableHTTPServer(core)
	return s, nil
}

// Handler returns the HTTP handler mounted at /mcp by the streamable
// transport. Exposed for callers that want to embed it in their own mux.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Serve starts the HTTP server on the supplied listener and blocks until
// it stops. Call Shutdown to stop gracefully.
func (s *Server) Serve(l net.Listener) error {
	s.http = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := s.http.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("mcp: serve: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

func (s *Server) registerTools() {
	s.core.AddTool(
		mcpgo.NewTool("spawn_agent",
			mcpgo.WithDescription("Spawn a worker agent of the given role from the team roster."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Role name as declared in the team YAML.")),
		),
		s.handleSpawnAgent,
	)
	s.core.AddTool(
		mcpgo.NewTool("assign_job",
			mcpgo.WithDescription("Assign a job to an agent. Returns a job id; poll get_results for status."),
			mcpgo.WithString("agent_id", mcpgo.Required(), mcpgo.Description("Agent id from list_agents.")),
			mcpgo.WithString("prompt", mcpgo.Required(), mcpgo.Description("The prompt for the worker.")),
			mcpgo.WithString("context", mcpgo.Description("Optional extra context for the job.")),
		),
		s.handleAssignJob,
	)
	s.core.AddTool(
		mcpgo.NewTool("get_results",
			mcpgo.WithDescription("Poll for the result of a previously-assigned job."),
			mcpgo.WithString("job_id", mcpgo.Required(), mcpgo.Description("Id returned by assign_job.")),
		),
		s.handleGetResults,
	)
	s.core.AddTool(
		mcpgo.NewTool("list_agents",
			mcpgo.WithDescription("List all active agents, their role and lifecycle state."),
		),
		s.handleListAgents,
	)
	s.core.AddTool(
		mcpgo.NewTool("query_bus",
			mcpgo.WithDescription("Return recent messages on a bus topic. Useful for tailing worker logs/results."),
			mcpgo.WithString("topic", mcpgo.Required(), mcpgo.Description("Bus topic name (e.g. agent.be-1.results).")),
		),
		s.handleQueryBus,
	)
	s.core.AddTool(
		mcpgo.NewTool("read_team",
			mcpgo.WithDescription("Return the team roster and Leader configuration."),
		),
		s.handleReadTeam,
	)
	s.core.AddTool(
		mcpgo.NewTool("query_audit",
			mcpgo.WithDescription("Read the audit log: structured events workers emit about their work (job lifecycle, decisions, errors). Use this to summarize what an agent did or to diagnose a job."),
			mcpgo.WithString("agent_id", mcpgo.Description("Optional. Restrict to events from this agent.")),
			mcpgo.WithString("since", mcpgo.Description("Optional. RFC3339 timestamp; only events at or after.")),
			mcpgo.WithString("limit", mcpgo.Description("Optional. Max events to return (default 50).")),
		),
		s.handleQueryAudit,
	)
	s.core.AddTool(
		mcpgo.NewTool("add_archetype",
			mcpgo.WithDescription("Add a new role template (archetype) to the team. Use when the user wants a new specialty available — instances are spawned later via spawn_agent up to max_concurrent. Changes are in-memory only; they revert when the daemon restarts."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Role name (worker, reviewer, integrator, custom).")),
			mcpgo.WithString("placement", mcpgo.Required(), mcpgo.Description("Where instances run: 'local', 'ssh:user@host', or 'fargate'.")),
			mcpgo.WithString("max_concurrent", mcpgo.Required(), mcpgo.Description("Cap on simultaneously-running instances. Positive integer.")),
			mcpgo.WithString("description", mcpgo.Description("One-line description shown to the leader.")),
			mcpgo.WithString("working_dir", mcpgo.Description("Required for ssh placement; optional otherwise.")),
			mcpgo.WithString("lifecycle", mcpgo.Description("'ephemeral' (default) or 'persistent'.")),
		),
		s.handleAddArchetype,
	)
	s.core.AddTool(
		mcpgo.NewTool("remove_archetype",
			mcpgo.WithDescription("Drop a role template from the roster. Refuses if any instance of that role is currently running — call stop_agent first."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Role to remove.")),
		),
		s.handleRemoveArchetype,
	)
	s.core.AddTool(
		mcpgo.NewTool("stop_agent",
			mcpgo.WithDescription("Tear down a running worker instance. Cancels its result subscriber and calls Teardown on the provisioner (unless the archetype is persistent). The archetype stays in the roster."),
			mcpgo.WithString("agent_id", mcpgo.Required(), mcpgo.Description("Id of the running instance, e.g. worker-3.")),
		),
		s.handleStopAgent,
	)
	s.core.AddTool(
		mcpgo.NewTool("recall_jobs",
			mcpgo.WithDescription("Reconstruct past job assignments from the audit log. Use this when you want to remember what an agent was asked to do, especially across daemon restarts or fresh chat sessions. Returns materialized job records joining job_received with job_complete/error: {job_id, agent_id, status, prompt, output, error, started_at, completed_at}. Bodies are capped at TEEM_JOB_BODY_CAP_BYTES on the worker side."),
			mcpgo.WithString("agent_id", mcpgo.Description("Optional. Restrict to one agent.")),
			mcpgo.WithString("since", mcpgo.Description("Optional. RFC3339 timestamp; only jobs whose audit events are at or after.")),
			mcpgo.WithString("limit", mcpgo.Description("Optional. Max jobs to return (default 25). Most recent first.")),
		),
		s.handleRecallJobs,
	)
	s.core.AddTool(
		mcpgo.NewTool("update_archetype",
			mcpgo.WithDescription("Refine an archetype's description and/or change its max_concurrent. At least one of those fields must be supplied."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Archetype role.")),
			mcpgo.WithString("description", mcpgo.Description("New description text (optional).")),
			mcpgo.WithString("max_concurrent", mcpgo.Description("New cap (optional). Positive integer.")),
		),
		s.handleUpdateArchetype,
	)
	s.core.AddTool(
		mcpgo.NewTool("add_task",
			mcpgo.WithDescription("Add a task to the team's plan — the canonical work queue. Use this at the start of complex work to break it into pieces, then update_task as instances make progress. Tasks persist across daemon restarts, so the leader can pick up where it left off in a new session."),
			mcpgo.WithString("title", mcpgo.Required(), mcpgo.Description("Short title for the task.")),
			mcpgo.WithString("parent_id", mcpgo.Description("Optional parent task id for hierarchies.")),
			mcpgo.WithString("depends_on", mcpgo.Description("Optional comma-separated task ids this task is blocked on.")),
			mcpgo.WithString("notes", mcpgo.Description("Optional markdown notes the leader keeps as working memory.")),
		),
		s.handleAddTask,
	)
	s.core.AddTool(
		mcpgo.NewTool("update_task",
			mcpgo.WithDescription("Mutate a task. Any subset of fields can be supplied; omitted fields are left as-is. Add evidence (job_ids that worked on this task) via add_evidence."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Task id.")),
			mcpgo.WithString("status", mcpgo.Description("New status: pending, in_progress, blocked, done, abandoned.")),
			mcpgo.WithString("assigned_to", mcpgo.Description("Agent id currently working on this task.")),
			mcpgo.WithString("notes", mcpgo.Description("Replace the notes field.")),
			mcpgo.WithString("depends_on", mcpgo.Description("Comma-separated task ids; replaces existing list.")),
			mcpgo.WithString("add_evidence", mcpgo.Description("Comma-separated job_ids to append to evidence.")),
		),
		s.handleUpdateTask,
	)
	s.core.AddTool(
		mcpgo.NewTool("list_tasks",
			mcpgo.WithDescription("List tasks in the plan, optionally filtered. Returns the materialised view (title, status, assigned_to, depends_on, evidence, timestamps)."),
			mcpgo.WithString("status", mcpgo.Description("Restrict to one status.")),
			mcpgo.WithString("parent_id", mcpgo.Description("Only direct children of this task.")),
			mcpgo.WithString("open_only", mcpgo.Description("If 'true', skip done/abandoned tasks.")),
		),
		s.handleListTasks,
	)
	s.core.AddTool(
		mcpgo.NewTool("link_task_to_job",
			mcpgo.WithDescription("Record that a particular job worked on a task. Shortcut for update_task with add_evidence."),
			mcpgo.WithString("task_id", mcpgo.Required(), mcpgo.Description("Task id.")),
			mcpgo.WithString("job_id", mcpgo.Required(), mcpgo.Description("Job id from assign_job.")),
		),
		s.handleLinkTaskToJob,
	)
	s.core.AddTool(
		mcpgo.NewTool("get_job_transcript",
			mcpgo.WithDescription("Fetch the full stream-json transcript a worker produced for a job. Use this to inspect what a sub-agent actually did beyond the truncated final assistant text in recall_jobs / query_audit. Returns either the raw NDJSON events ('raw') or a flat 'role: text' rendering ('text', default). Body is capped at 200 KiB; use head=N to fetch only the first N events."),
			mcpgo.WithString("agent_id", mcpgo.Required(), mcpgo.Description("Agent that ran the job.")),
			mcpgo.WithString("job_id", mcpgo.Required(), mcpgo.Description("Job id from assign_job.")),
			mcpgo.WithString("head", mcpgo.Description("Optional. Return only the first N stream-json events.")),
			mcpgo.WithString("format", mcpgo.Description("'raw' (NDJSON verbatim) or 'text' (flat role/text rendering). Default 'text'.")),
		),
		s.handleGetJobTranscript,
	)
	s.core.AddTool(
		mcpgo.NewTool("read_archetype_memory",
			mcpgo.WithDescription("Return the persisted long-term memory for an archetype role: the rolling LLM digest plus the recent-entries list every freshly-spawned worker of that role inherits as baseline context. Use when triaging an agent's behavior or before adjusting how a role should work."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Archetype role (e.g. worker, reviewer).")),
		),
		s.handleReadArchetypeMemory,
	)
	s.core.AddTool(
		mcpgo.NewTool("append_archetype_memory",
			mcpgo.WithDescription("Append an operator-authored note to an archetype's memory file. Use sparingly — every line shows up as baseline context in future worker spawns. Good for one-off corrections (\"this role should always X\") that the LLM-generated digest hasn't picked up yet."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Archetype role to write under.")),
			mcpgo.WithString("note", mcpgo.Required(), mcpgo.Description("The note text — one line, no markdown headers.")),
		),
		s.handleAppendArchetypeMemory,
	)
	s.core.AddTool(
		mcpgo.NewTool("write_user_note",
			mcpgo.WithDescription("Leave a short message for the user to read when they next open `teem chat`. Use during autonomous ticks for anything that needs the human's attention — completed milestones, decisions made, questions you want answered, blockers. The user sees a banner with unread notes before the chat opens."),
			mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("Note body. One or more lines of markdown.")),
		),
		s.handleWriteUserNote,
	)
}
