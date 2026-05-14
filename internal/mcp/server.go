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
	"github.com/frasergraham/teem/internal/leaderstatus"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/prompts"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/team"
)

// Spawner is the abstraction the spawn_agent MCP tool calls into.
// Implemented in internal/agent to avoid an import cycle.
//
// Spawn takes an optional operator-supplied name. When name == ""
// the allocator picks an id from the role's wordlist (current
// behavior). When name is set the worker is spawned under that
// exact id — reincarnating a prior worker if the name was already
// retired, idempotently returning the existing id if it's already
// live, or rejecting if the name belongs to a different role.
type Spawner interface {
	Spawn(ctx context.Context, role, name string) (string, error)
	AssignJob(ctx context.Context, agentID, prompt, contextNote string) (string, error)
	JobStatus(jobID string) (status string, output string, found bool)
	StopAgent(ctx context.Context, agentID string) error
	IsRunning(agentID string) bool
	AnyRunningWithRole(role string) bool
	RosterSnapshot(role string) []roster.Entry
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
	leaderStatus   *leaderstatus.Store
	prompts        *prompts.Builder
	transcriptsDir string
	channelSink    func(content string, meta map[string]string)
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
	// LeaderStatus persists the "what is each leader-tier agent
	// doing right now" entries shown at the top of the dashboard.
	// When nil the update_leader_status / get_leader_status tools
	// return a clear error.
	LeaderStatus *leaderstatus.Store
	// Prompts is the layered system-prompt builder used by the
	// read_prompt / append_prompt tools. When nil those tools
	// return an error explaining the feature is unconfigured.
	Prompts *prompts.Builder
	// ChannelSink, when non-nil, receives every PushChannel call in
	// addition to the in-process MCP notification. The daemon plugs
	// a channelbus.Bus.Publish here so the teem-channel stdio shim
	// (which is the transport Claude Code actually listens on for
	// channel notifications) can forward the event. The legacy
	// SendNotificationToAllClients path is preserved for unit tests
	// and so a future native HTTP-channels client would still work.
	ChannelSink func(content string, meta map[string]string)
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
		// Advertise the Claude Code channel capability so a leader
		// launched with `--channels server:teem` can subscribe to the
		// notifications/claude/channel stream PushChannel emits.
		mcpsrv.WithExperimental(map[string]any{
			"claude/channel": map[string]any{},
		}),
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
		leaderStatus:   cfg.LeaderStatus,
		prompts:        cfg.Prompts,
		transcriptsDir: cfg.TranscriptsDir,
		channelSink:    cfg.ChannelSink,
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

// PushChannel emits a notifications/claude/channel notification on every
// currently-connected MCP session. The params shape matches what Claude
// Code expects when subscribed via `--channels server:teem`:
//
//	{ "content": "<body>", "meta": { ...flat string map... } }
//
// Fire-and-forget: there is no ack from the client. Safe to call with no
// active sessions (the underlying SendNotificationToAllClients is a
// no-op on an empty session set).
func (s *Server) PushChannel(content string, meta map[string]string) {
	params := map[string]any{"content": content}
	if len(meta) > 0 {
		m := make(map[string]any, len(meta))
		for k, v := range meta {
			m[k] = v
		}
		params["meta"] = m
	}
	s.core.SendNotificationToAllClients("notifications/claude/channel", params)
	if s.channelSink != nil {
		// Copy so a downstream subscriber that mutates the map can't
		// corrupt the caller's reference (callers commonly literal-init
		// the meta map).
		var metaCopy map[string]string
		if len(meta) > 0 {
			metaCopy = make(map[string]string, len(meta))
			for k, v := range meta {
				metaCopy[k] = v
			}
		}
		s.channelSink(content, metaCopy)
	}
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
			mcpgo.WithDescription("Spawn a worker agent of the given role from the team roster. The resulting agent_id is always <role>-<name>. Pass `name` to choose the worker's identity: a previously-retired name reincarnates the same worker (its branch teem/<role>-<name> and roster history come back); a name already in use returns idempotently; an unknown name is registered. Omit `name` to let the daemon pick from the wordlist."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Role name as declared in the team YAML.")),
			mcpgo.WithString("name", mcpgo.Description("Optional. Bare wordlist name (e.g. `ada`). Must match ^[a-z][a-z0-9]{0,30}$. The full agent_id form (`worker-ada`) is also accepted — the `<role>-` prefix is stripped before validation, so pasting an id straight back from list_agents works. Reincarnates a prior worker with the same (role, name), idempotently returns an already-live id, or registers a fresh entry.")),
		),
		s.handleSpawnAgent,
	)
	s.core.AddTool(
		mcpgo.NewTool("list_roster",
			mcpgo.WithDescription("Return the persistent roster of named workers for this team. Use this before spawn_agent to choose a previously-used name (reincarnation) or see what names are taken. Each entry includes {name, role, first_seen, last_seen, in_use, source} where source is one of 'wordlist' (allocator-picked), 'named' (operator-supplied), 'legacy' (migrated pre-T9 id)."),
			mcpgo.WithString("role", mcpgo.Description("Optional. Restrict the result to one role.")),
		),
		s.handleListRoster,
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
			mcpgo.WithString("agent_id", mcpgo.Required(), mcpgo.Description("Id of the running instance, e.g. worker-ada.")),
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
			mcpgo.WithDescription("Mutate a task's status, assignee, notes, depends_on, or evidence. Any subset of fields can be supplied; omitted fields are left as-is. To change the pipeline stage (proposed/specced/planning/coding/reviewing/integrating/verified/blocked/shelved/abandoned), use set_task_stage instead — it enforces the transitions matrix."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Task id.")),
			mcpgo.WithString("status", mcpgo.Description("New status: pending, in_progress, blocked, shelved (paused, will pick up later), done, abandoned.")),
			mcpgo.WithString("assigned_to", mcpgo.Description("Agent id currently working on this task.")),
			mcpgo.WithString("notes", mcpgo.Description("Replace the notes field.")),
			mcpgo.WithString("depends_on", mcpgo.Description("Comma-separated task ids; replaces existing list.")),
			mcpgo.WithString("add_evidence", mcpgo.Description("Comma-separated job_ids to append to evidence.")),
		),
		s.handleUpdateTask,
	)
	s.core.AddTool(
		mcpgo.NewTool("delete_task",
			mcpgo.WithDescription("Permanently remove a task from the plan. Use for typos, duplicates, or stub tasks that should never have been recorded — anything you'd otherwise have to look at and ignore. For work the team explicitly decided not to do, use status='abandoned' (kept visible in the recently-completed rail); for work paused mid-flight, use status='shelved'. The tombstone is appended to the log; replays reproduce the deletion."),
			mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Task id to delete.")),
		),
		s.handleDeleteTask,
	)
	s.core.AddTool(
		mcpgo.NewTool("list_tasks",
			mcpgo.WithDescription("List tasks in the plan, optionally filtered. Returns the materialised view (title, status, stage, stage_entered_at, assigned_to, depends_on, evidence, timestamps)."),
			mcpgo.WithString("status", mcpgo.Description("Restrict to one status.")),
			mcpgo.WithString("stage", mcpgo.Description("Restrict to one stage (proposed/specced/planning/coding/reviewing/integrating/verified/blocked/shelved/abandoned).")),
			mcpgo.WithString("parent_id", mcpgo.Description("Only direct children of this task.")),
			mcpgo.WithString("open_only", mcpgo.Description("If 'true', skip non-open tasks (only returns pending/in_progress/blocked).")),
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
			mcpgo.WithDescription("Return the persisted long-term memory for a role: the rolling LLM digest plus the recent-entries list every freshly-spawned worker of that role inherits as baseline context. Pass role=\"leader\" to read the per-team leader memory (folded into the leader's brief on every `teem chat`). Use when triaging an agent's behavior or before adjusting how a role should work."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Archetype role (e.g. worker, reviewer) or \"leader\" for the per-team leader memory.")),
		),
		s.handleReadArchetypeMemory,
	)
	s.core.AddTool(
		mcpgo.NewTool("append_archetype_memory",
			mcpgo.WithDescription("Append an operator-authored note to a role's memory file. Use sparingly — every line shows up as baseline context in future worker spawns (or, for role=\"leader\", in the leader's next `teem chat` brief). Good for one-off corrections (\"this role should always X\") that the LLM-generated digest hasn't picked up yet."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("Archetype role to write under, or \"leader\" for the per-team leader memory.")),
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
	s.core.AddTool(
		mcpgo.NewTool("update_leader_status",
			mcpgo.WithDescription("Set the short, human-readable \"what am I doing right now\" line shown at the top of the dashboard. Keep ≤120 chars; answer the right-now question (\"Reviewing T1+T6 diff\", \"Spawning reviewer-blake for T4\"). Planning detail belongs in record_decision. agent_id defaults to 'leader' for the Leader; PM-style workers should pass their own id."),
			mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("Status line text. One sentence.")),
			mcpgo.WithString("current_task_ids", mcpgo.Description("Optional comma-separated task ids being actively worked.")),
			mcpgo.WithString("agent_id", mcpgo.Description("Optional. Defaults to 'leader'.")),
		),
		s.handleUpdateLeaderStatus,
	)
	s.core.AddTool(
		mcpgo.NewTool("get_leader_status",
			mcpgo.WithDescription("Read back the per-agent status board. Returns the map of agent_id → {text, updated_at, current_task_ids}."),
		),
		s.handleGetLeaderStatus,
	)
	s.core.AddTool(
		mcpgo.NewTool("set_task_stage",
			mcpgo.WithDescription("Move a task to a new pipeline stage: proposed, specced, planning, coding, reviewing, integrating, verified, blocked, shelved, abandoned. Stage is the lifecycle marker the dashboard pipeline view uses; Status (open/done) is separate. Shelved is for tasks intentionally paused — they leave the open pipeline but stay visible in their own dashboard section. Invalid transitions (e.g. verified → proposed) return an error."),
			mcpgo.WithString("task_id", mcpgo.Required(), mcpgo.Description("Task id.")),
			mcpgo.WithString("stage", mcpgo.Required(), mcpgo.Description("Target stage.")),
		),
		s.handleSetTaskStage,
	)
	s.core.AddTool(
		mcpgo.NewTool("record_decision",
			mcpgo.WithDescription("Record a non-trivial decision against a task: the \"why\" behind a choice that wouldn't be obvious from the diff alone. Persisted to the audit log under decision_note and rendered in the task's flow view. Use this for design choices, trade-offs picked, vendored deps, etc."),
			mcpgo.WithString("task_id", mcpgo.Required(), mcpgo.Description("Task id this decision attaches to.")),
			mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("Decision text — markdown allowed.")),
		),
		s.handleRecordDecision,
	)
	s.core.AddTool(
		mcpgo.NewTool("read_prompt",
			mcpgo.WithDescription("Return the assembled system prompt for a role: the YAML-derived base plus any operator override layer on disk. Use \"leader\" for the leader's prompt or any archetype role name. The response includes both the fully assembled prompt (`assembled`) and the raw override text (`override`, empty when no override exists) so a leader can see what's being injected on its own behalf and what archetypes will see when spawned."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("\"leader\" or an archetype role (worker, reviewer, …).")),
		),
		s.handleReadPrompt,
	)
	s.core.AddTool(
		mcpgo.NewTool("append_prompt",
			mcpgo.WithDescription("Append an operator-authored block to a role's prompt override file. Adds a timestamped `## Appended <UTC>` section preserving prior content; future leader chats / worker spawns of that role will see the new text after the standing system prompt. Use for durable behaviour tweaks (\"always run go vet before commit\"); use update_archetype instead for one-line description edits."),
			mcpgo.WithString("role", mcpgo.Required(), mcpgo.Description("\"leader\" or an archetype role.")),
			mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("Markdown body to append.")),
		),
		s.handleAppendPrompt,
	)
	s.core.AddTool(
		mcpgo.NewTool("record_blocker",
			mcpgo.WithDescription("Record a blocker against a task. Atomic effect: the task moves to Stage='blocked' AND Status='blocked', and a blocker_note event lands in audit. Use when a worker reports unblockable issues that need leader or human action."),
			mcpgo.WithString("task_id", mcpgo.Required(), mcpgo.Description("Task id being blocked.")),
			mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("What's blocking and what's needed to unblock.")),
		),
		s.handleRecordBlocker,
	)
}
