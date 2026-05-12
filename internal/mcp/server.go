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

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/team"
)

// Spawner is the abstraction the spawn_agent MCP tool calls into.
// Implemented in internal/agent to avoid an import cycle.
type Spawner interface {
	SpawnByRole(ctx context.Context, role string) (string, error)
	AssignJob(ctx context.Context, agentID, prompt, contextNote string) (string, error)
	JobStatus(jobID string) (status string, output string, found bool)
}

// Server bundles the MCP server, its handler, and the dependencies its
// tools close over.
type Server struct {
	core    *mcpsrv.MCPServer
	handler http.Handler
	http    *http.Server

	bus      bus.Bus
	team     *team.Team
	registry *Registry
	spawner  Spawner
	audit    audit.Sink
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
		core:     core,
		bus:      cfg.Bus,
		team:     cfg.Team,
		registry: cfg.Registry,
		spawner:  cfg.Spawner,
		audit:    cfg.Audit,
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
}
