package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/state"
	"github.com/frasergraham/teem/internal/team"
)

// jobRecord tracks an outstanding job for the get_results MCP tool.
type jobRecord struct {
	agentID string
	status  string
	output  string
}

// CloudProvisionerFactory builds a Provisioner for a cloud backend. The
// spawner injects runtime deps (tailnet HTTP client, worker token) here
// rather than burning them into provisioner.Select. Returns nil to signal
// "this backend isn't configured" — the spawner surfaces a clear error.
type CloudProvisionerFactory func(backend provisioner.Backend) (provisioner.Provisioner, error)

// Config bundles the runtime deps the Spawner needs to wire cloud agents.
type Config struct {
	// HTTPClient resolves tailnet hostnames. Required when any agent uses a
	// cloud backend; pass tnetNode.HTTPClient() from main.
	HTTPClient *http.Client
	// WorkerToken is the shared bearer the leader hands to every cloud
	// worker via its container env. Generated per leader session.
	WorkerToken string
	// CloudProvisioner builds the provisioner for cloud backends. Optional;
	// if nil and a cloud agent is requested, spawn fails.
	CloudProvisioner CloudProvisionerFactory
	// RepoRoot is the leader's git repo root, used to create per-agent
	// worktrees for local agents that don't declare an explicit
	// working_dir. Empty disables auto-worktree (agents fall back to the
	// YAML's working_dir or error if neither is set).
	RepoRoot string
	// WorktreeBase is the directory under which local agent worktrees are
	// placed. Defaults to ~/.teem/worktrees/<team> when empty.
	WorktreeBase string
	// LeaderURL is the base URL workers POST audit events to (the
	// leader's tailnet hostname + listen port, e.g.
	// http://teem-leader:7777). Cloud provisioners pass this to workers
	// via container env. Empty disables the worker→leader event channel.
	LeaderURL string
	// Git is the source-control configuration cloud workers should use
	// to clone, configure credentials, and push. Empty fields are
	// passed through unset so the worker daemon's defaults apply.
	Git GitConfig
	// StateStore persists records for agents whose lifecycle is
	// "persistent". When set, the spawner reconciles persistent agents
	// at startup and skips teardown for them on Stop.
	StateStore *state.Store
	// AuditSink is the team's audit log. When set, Spawner.JobStatus
	// falls back to the audit log on cache misses so leaders can
	// recall results across daemon restarts.
	AuditSink audit.Sink
}

// GitConfig is the source-control configuration the leader hands to
// cloud workers via container env. Fields map 1:1 to TEEM_GIT_* env vars
// the worker reads.
type GitConfig struct {
	RepoURL      string
	Token        string
	Username     string
	AuthorName   string
	AuthorEmail  string
	BranchPrefix string
	AutoPush     string // "true" / "false" / "" (worker default applies)
}

// Spawner satisfies mcp.Spawner. It owns the workers it spawns and the
// outstanding job table.
//
// baseCtx is the long-lived context all worker goroutines are tied to.
// Without this, workers would die the instant an MCP request returns
// (request contexts are cancelled by the framework on response). Set at
// construction; cancelled on Stop.
type Spawner struct {
	team     *team.Team
	bus      bus.Bus
	registry *mcpsrv.Registry
	cfg      Config

	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu          sync.Mutex
	workers     map[string]*Worker
	jobs        map[string]*jobRecord
	subs        map[string]context.CancelFunc
	provisioned map[string]provisionedAgent
	// archetypeSeq is a monotonic counter per archetype role; the next
	// auto-generated id is `<role>-<seq+1>`. IDs are never reused so
	// audit history stays coherent — `worker-3` always refers to one
	// historical worker even after it stopped.
	archetypeSeq map[string]int
}

type provisionedAgent struct {
	provisioner provisioner.Provisioner
	agent       *provisioner.Agent
}

// NewSpawner constructs a Spawner. baseCtx scopes the lifetime of every
// worker goroutine this spawner manages — pass the daemon's lifetime
// ctx so workers survive past the MCP request that triggered them.
// Call Stop to tear it down.
func NewSpawner(baseCtx context.Context, t *team.Team, b bus.Bus, r *mcpsrv.Registry, cfg Config) *Spawner {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)
	return &Spawner{
		team:        t,
		bus:         b,
		registry:    r,
		cfg:         cfg,
		baseCtx:     ctx,
		baseCancel:  cancel,
		workers:      map[string]*Worker{},
		jobs:         map[string]*jobRecord{},
		subs:         map[string]context.CancelFunc{},
		provisioned:  map[string]provisionedAgent{},
		archetypeSeq: map[string]int{},
	}
}

// Reconcile attempts to reconnect every persistent agent in the team
// without re-provisioning. For each one it checks the worker's /healthz
// (over the tailnet HTTP client) and, if reachable, registers the agent
// as running so the leader's `list_agents` sees it immediately. Agents
// that don't answer are left for the regular spawn flow to handle.
//
// Errors from a single agent never abort the loop — reconcile is
// best-effort. Returns the number of agents successfully reconnected.
//
// For each persistent archetype, walk instance slots teem-<role>-1
// through teem-<role>-N (where N = MaxConcurrent) and probe /healthz.
// Each that answers gets registered as a running worker.
func (s *Spawner) Reconcile(ctx context.Context) int {
	if s.cfg.HTTPClient == nil || s.cfg.WorkerToken == "" {
		return 0
	}
	connected := 0
	for _, arch := range s.team.SnapshotArchetypes() {
		if arch.LifecycleOrDefault() != "persistent" {
			continue
		}
		backend := provisioner.Backend("local")
		switch {
		case arch.Placement == "fargate":
			backend = provisioner.BackendFargate
		case len(arch.Placement) > 4 && arch.Placement[:4] == "ssh:":
			backend = provisioner.BackendSSH
		}
		for n := 1; n <= arch.MaxConcurrent; n++ {
			id := fmt.Sprintf("%s-%d", arch.Role, n)
			host := "teem-" + id
			exec := executor.NewHTTP(s.cfg.HTTPClient, "http://"+host+":7780", s.cfg.WorkerToken)
			hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := exec.CheckHealth(hctx)
			cancel()
			if err != nil {
				continue
			}
			a := &provisioner.Agent{
				ID:          id,
				Role:        arch.Role,
				Backend:     backend,
				Lifecycle:   "persistent",
				TailnetHost: host,
				MCPs:        arch.MCPs,
			}
			if err := s.startWorker(noopTeardownProvisioner{}, a); err != nil {
				s.publishLog(id, fmt.Sprintf("reconcile start failed: %v", err))
				continue
			}
			// Bump the per-role counter so future ad-hoc spawns don't
			// collide with the reconciled id.
			s.mu.Lock()
			if s.archetypeSeq[arch.Role] < n {
				s.archetypeSeq[arch.Role] = n
			}
			s.mu.Unlock()
			connected++
			s.publishLog(id, "reconciled — persistent worker reused")
		}
	}
	return connected
}

// specForRole resolves a role to a concrete team.AgentSpec by spawning
// a fresh instance from the matching archetype. Returns an error if
// the role has no archetype or the archetype is at its concurrency
// cap.
func (s *Spawner) specForRole(role string) (*team.AgentSpec, bool, error) {
	arch := s.team.FindArchetypeByRole(role)
	if arch == nil {
		return nil, false, fmt.Errorf("no archetype with role %q", role)
	}
	active := s.countActiveByRole(role)
	if active >= arch.MaxConcurrent {
		return nil, false, fmt.Errorf("archetype %q is at capacity (%d/%d running)", role, active, arch.MaxConcurrent)
	}
	id := s.nextArchetypeID(role)
	spec := s.specFromArchetype(*arch, id)
	return &spec, true, nil
}

// countActiveByRole walks current workers and counts those whose role
// matches. Used to enforce archetype MaxConcurrent.
func (s *Spawner) countActiveByRole(role string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, pa := range s.provisioned {
		if pa.agent != nil && pa.agent.Role == role {
			n++
		}
	}
	return n
}

// nextArchetypeID returns the next monotonic id for an archetype role.
// Counter never decrements: even after a stopped worker frees a slot,
// the next spawn gets a fresh number so audit history isn't ambiguous.
func (s *Spawner) nextArchetypeID(role string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archetypeSeq[role]++
	return fmt.Sprintf("%s-%d", role, s.archetypeSeq[role])
}

// specFromArchetype materialises a concrete team.AgentSpec from an
// archetype template plus a freshly-minted id. Placement strings are
// expanded back into Local/SSHTarget/Backend fields. The instance
// inherits the archetype's lifecycle.
func (s *Spawner) specFromArchetype(arch team.ArchetypeSpec, id string) team.AgentSpec {
	spec := team.AgentSpec{
		ID:          id,
		Role:        arch.Role,
		Description: arch.Description,
		WorkingDir:  arch.WorkingDir,
		Lifecycle:   arch.Lifecycle,
		MCPs:        arch.MCPs,
	}
	switch {
	case arch.Placement == "local":
		spec.Local = true
	case arch.Placement == "fargate":
		spec.Backend = "fargate"
	case len(arch.Placement) > 4 && arch.Placement[:4] == "ssh:":
		spec.SSHTarget = arch.Placement[4:]
	}
	return spec
}

// noopTeardownProvisioner is the placeholder provisioner attached to
// reconciled agents. startWorker stores it on the provisioned map so
// Stop() walks it, but its Teardown does nothing (persistent agents
// outlive the leader by design, and we'd also skip in Stop based on
// Lifecycle anyway).
type noopTeardownProvisioner struct{}

func (noopTeardownProvisioner) Provision(context.Context, provisioner.AgentSpec) (*provisioner.Agent, error) {
	return nil, fmt.Errorf("noopTeardownProvisioner: Provision should not be called")
}
func (noopTeardownProvisioner) Teardown(context.Context, *provisioner.Agent) error { return nil }

// SpawnByRole provisions a worker for the role and starts its loop.
//
// Lookup precedence:
//  1. team.Agents with this role and not currently running (specific
//     IDs declared by the operator, typically persistent or otherwise
//     identity-bound).
//  2. team.Archetypes with this role: if the current count of running
//     instances < MaxConcurrent, generate the next sequential id
//     (<role>-<N>) and provision from the archetype template.
//
// Returns an error if neither matches or the archetype is at its cap.
func (s *Spawner) SpawnByRole(ctx context.Context, role string) (string, error) {
	spec, fromArchetype, err := s.specForRole(role)
	if err != nil {
		return "", err
	}
	if err := EnsureDir(spec.WorkingDir); err != nil {
		return "", err
	}
	pSpec := provisioner.FromTeamSpec(*spec)
	p, err := s.selectProvisioner(pSpec.Backend)
	if err != nil {
		return "", err
	}
	_ = fromArchetype // currently no per-source behavior; kept for future logging

	// For cloud backends, register the agent immediately with state
	// "provisioning" and finish provisioning + worker startup in the
	// background. The MCP spawn_agent tool returns the agent id without
	// blocking the leader's chat for 30–90s.
	if pSpec.Backend == provisioner.BackendFargate {
		entry := mcpsrv.AgentEntry{
			ID:        spec.ID,
			Role:      spec.Role,
			State:     mcpsrv.StateProvisioning,
			Backend:   string(pSpec.Backend),
			StartedAt: time.Now(),
		}
		s.registry.Add(entry)
		go s.provisionAndStart(p, pSpec)
		return spec.ID, nil
	}

	a, err := p.Provision(ctx, pSpec)
	if err != nil {
		return "", err
	}
	if err := s.startWorker(p, a); err != nil {
		return "", err
	}
	return a.ID, nil
}

// provisionAndStart runs the slow cloud provisioner in the background
// and flips the registry entry to running once the worker is healthy.
// The provisioning step uses s.baseCtx so a long Fargate cold start
// isn't cancelled by the MCP request returning early.
func (s *Spawner) provisionAndStart(p provisioner.Provisioner, spec provisioner.AgentSpec) {
	a, err := p.Provision(s.baseCtx, spec)
	if err != nil {
		_ = s.registry.SetState(spec.ID, mcpsrv.StateError)
		s.publishLog(spec.ID, fmt.Sprintf("provision failed: %v", err))
		return
	}
	if err := s.startWorker(p, a); err != nil {
		_ = s.registry.SetState(spec.ID, mcpsrv.StateError)
		s.publishLog(spec.ID, fmt.Sprintf("worker start failed: %v", err))
		_ = p.Teardown(context.Background(), a)
		return
	}
	s.publishLog(a.ID, "agent ready")
}

// startWorker is the shared half of SpawnByRole and provisionAndStart: it
// builds the executor, kicks off the worker loop, subscribes results, and
// updates the registry entry.
//
// All long-lived goroutines (worker job loop, result subscriber,
// liveness watcher) are scoped to s.baseCtx — NOT the caller's ctx,
// which for MCP-triggered spawns is the request context that the MCP
// framework cancels the moment the tool returns. Tying goroutines to
// the request ctx made jobs sit forever in early multi-tenant builds;
// don't reintroduce.
func (s *Spawner) startWorker(p provisioner.Provisioner, a *provisioner.Agent) error {
	exec, err := s.executorFor(a)
	if err != nil {
		return err
	}
	w := &Worker{Agent: a, Bus: s.bus, Executor: exec}
	if err := w.Start(s.baseCtx); err != nil {
		return err
	}

	// For local/ssh agents the registry entry is added here. For cloud
	// agents it already exists (added in StateProvisioning) — overwrite to
	// pick up the tailnet host and flip to running.
	s.registry.Add(mcpsrv.AgentEntry{
		ID:          a.ID,
		Role:        a.Role,
		State:       mcpsrv.StateRunning,
		Backend:     string(a.Backend),
		TailnetHost: a.TailnetHost,
		StartedAt:   time.Now(),
	})

	s.subscribeResults(a.ID)

	s.mu.Lock()
	s.workers[a.ID] = w
	s.provisioned[a.ID] = provisionedAgent{provisioner: p, agent: a}
	s.mu.Unlock()

	if watcher, ok := p.(provisioner.Watcher); ok {
		s.startLivenessWatch(watcher, a)
	}
	return nil
}

// startLivenessWatch polls the backend every 15s. On ErrAgentStopped it
// flips the registry to StateStopped and fails any outstanding jobs for
// this agent via the bus. Lives for s.baseCtx (daemon lifetime).
func (s *Spawner) startLivenessWatch(w provisioner.Watcher, a *provisioner.Agent) {
	subCtx, cancel := context.WithCancel(s.baseCtx)
	s.mu.Lock()
	prev := s.subs["liveness:"+a.ID]
	s.subs["liveness:"+a.ID] = cancel
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-subCtx.Done():
				return
			case <-t.C:
			}
			cctx, cc := context.WithTimeout(subCtx, 10*time.Second)
			err := w.CheckLiveness(cctx, a)
			cc()
			if err == nil {
				continue
			}
			if !errors.Is(err, provisioner.ErrAgentStopped) {
				// Transient — log and keep polling. We don't have a logger
				// plumbed; surface on the agent's log topic.
				s.publishLog(a.ID, fmt.Sprintf("liveness check error: %v", err))
				continue
			}
			// Agent has stopped on the backend.
			_ = s.registry.SetState(a.ID, mcpsrv.StateStopped)
			s.failOutstandingJobs(subCtx, a.ID, "agent stopped")
			s.publishLog(a.ID, "agent stopped on backend; jobs failed")
			return
		}
	}()
}

// failOutstandingJobs publishes a synthetic resultMessage with an error for
// every pending job assigned to agentID. The normal subscribeResults path
// picks them up and updates the get_results table.
func (s *Spawner) failOutstandingJobs(ctx context.Context, agentID, reason string) {
	s.mu.Lock()
	pending := make([]string, 0)
	for id, rec := range s.jobs {
		if rec.agentID == agentID && rec.status == "pending" {
			pending = append(pending, id)
		}
	}
	s.mu.Unlock()
	for _, id := range pending {
		body, _ := json.Marshal(resultMessage{JobID: id, Error: reason})
		_ = s.bus.Publish(ctx, bus.Message{
			Topic:   ResultsTopic(agentID),
			Kind:    bus.KindResult,
			From:    agentID,
			Payload: body,
		})
	}
}

// selectProvisioner picks a Provisioner for the backend, deferring to the
// CloudProvisionerFactory for cloud backends so runtime deps (HTTP client,
// worker token, AWS client) flow in cleanly.
func (s *Spawner) selectProvisioner(b provisioner.Backend) (provisioner.Provisioner, error) {
	switch b {
	case provisioner.BackendLocal:
		return provisioner.NewLocalProvisioner(s.cfg.RepoRoot, s.cfg.WorktreeBase), nil
	case provisioner.BackendSSH:
		return provisioner.Select(provisioner.AgentSpec{Backend: b})
	case provisioner.BackendFargate:
		if s.cfg.CloudProvisioner == nil {
			return nil, fmt.Errorf("fargate backend requested but no cloud provisioner is configured (set the relevant TEEM_ECS_* env vars)")
		}
		return s.cfg.CloudProvisioner(b)
	default:
		return nil, fmt.Errorf("unknown backend %q", b)
	}
}

func (s *Spawner) publishLog(agentID, line string) {
	_ = s.bus.Publish(s.baseCtx, bus.Message{
		Topic:   LogsTopic(agentID),
		Kind:    bus.KindLog,
		From:    "spawner",
		To:      agentID,
		Payload: []byte(line),
	})
}

// AssignJob publishes a job to the worker's bus topic.
func (s *Spawner) AssignJob(ctx context.Context, agentID, prompt, contextNote string) (string, error) {
	if _, ok := s.registry.Get(agentID); !ok {
		return "", fmt.Errorf("agent %q not in registry", agentID)
	}
	jobID := bus.NewID()
	payload, _ := json.Marshal(jobMessage{
		JobID:   jobID,
		Prompt:  prompt,
		Context: contextNote,
	})
	s.mu.Lock()
	s.jobs[jobID] = &jobRecord{agentID: agentID, status: "pending"}
	s.mu.Unlock()
	if err := s.bus.Publish(ctx, bus.Message{
		Topic:   JobsTopic(agentID),
		Kind:    bus.KindJob,
		From:    "leader",
		To:      agentID,
		Payload: payload,
	}); err != nil {
		return "", err
	}
	_ = s.registry.SetState(agentID, mcpsrv.StateBusy)
	return jobID, nil
}

// JobStatus implements mcp.Spawner. Reads the in-memory jobs table
// first; on a miss falls back to the audit log so jobs from earlier
// daemon sessions are still recallable.
func (s *Spawner) JobStatus(jobID string) (string, string, bool) {
	s.mu.Lock()
	rec, ok := s.jobs[jobID]
	s.mu.Unlock()
	if ok {
		return rec.status, rec.output, true
	}
	if s.cfg.AuditSink == nil {
		return "", "", false
	}
	// The audit log isn't indexed by job_id; scan recent-ish events.
	// 1000 is plenty for most teams and bounds the work.
	events, err := s.cfg.AuditSink.Query("", time.Time{}, 1000)
	if err != nil {
		return "", "", false
	}
	job, ok := audit.MaterializeJob(events, jobID)
	if !ok {
		return "", "", false
	}
	out := job.Output
	if job.Status == "error" && out == "" {
		out = job.Error
	}
	return job.Status, out, true
}

// subscribeResults wires a single goroutine per agent that translates
// KindResult bus messages into the in-process job table the
// get_results MCP tool reads from. Subscription lifetime is s.baseCtx.
func (s *Spawner) subscribeResults(agentID string) {
	subCtx, cancel := context.WithCancel(s.baseCtx)
	ch, err := s.bus.Subscribe(subCtx, ResultsTopic(agentID))
	if err != nil {
		cancel()
		return
	}
	s.mu.Lock()
	s.subs[agentID] = cancel
	s.mu.Unlock()
	go func() {
		for msg := range ch {
			var r resultMessage
			if err := json.Unmarshal(msg.Payload, &r); err != nil {
				continue
			}
			s.mu.Lock()
			rec := s.jobs[r.JobID]
			if rec != nil {
				rec.output = r.Output
				if r.Error != "" {
					rec.status = "error"
					if rec.output == "" {
						rec.output = r.Error
					}
				} else {
					rec.status = "done"
				}
			}
			s.mu.Unlock()
			_ = s.registry.SetState(agentID, mcpsrv.StateRunning)
		}
	}()
}

// executorFor builds the Executor for a provisioned agent. It picks by
// capability rather than backend: if the provisioner gave us a Transport
// we use ProcessExecutor (local exec / SSH); otherwise we look for a
// tailnet host and talk to a remote teem-worker daemon over HTTP. This
// lets persistent local agents (no spawn, just connect to a daemon the
// operator started) fall out without a special case.
func (s *Spawner) executorFor(a *provisioner.Agent) (executor.Executor, error) {
	if a.Transport != nil {
		return executor.NewProcess(a.Transport, a.WorkingDir, a.MCPs), nil
	}
	if a.TailnetHost != "" {
		if s.cfg.HTTPClient == nil || s.cfg.WorkerToken == "" {
			return nil, fmt.Errorf("agent %s: remote executor needs HTTPClient + WorkerToken (tailnet must be enabled)", a.ID)
		}
		return executor.NewHTTP(s.cfg.HTTPClient, "http://"+a.TailnetHost+":7780", s.cfg.WorkerToken), nil
	}
	return nil, fmt.Errorf("agent %s: provisioner returned neither transport nor tailnet host", a.ID)
}

// StopAgent tears down a single agent: cancels its result subscriber,
// calls provisioner.Teardown (unless the agent is persistent), flips
// the registry to Stopped, and removes it from internal maps. Returns
// nil if the agent isn't currently running.
func (s *Spawner) StopAgent(ctx context.Context, agentID string) error {
	s.mu.Lock()
	w, hasWorker := s.workers[agentID]
	pa, hasProv := s.provisioned[agentID]
	cancelLiveness := s.subs["liveness:"+agentID]
	cancelResults := s.subs[agentID]
	delete(s.workers, agentID)
	delete(s.provisioned, agentID)
	delete(s.subs, "liveness:"+agentID)
	delete(s.subs, agentID)
	s.mu.Unlock()

	_ = w // worker has no Stop() — its goroutine exits when the bus subscription cancels

	if cancelLiveness != nil {
		cancelLiveness()
	}
	if cancelResults != nil {
		cancelResults()
	}

	_ = s.registry.SetState(agentID, mcpsrv.StateStopped)

	if !hasWorker && !hasProv {
		return nil // agent wasn't running
	}
	if hasProv && !pa.agent.IsPersistent() {
		tdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := pa.provisioner.Teardown(tdCtx, pa.agent); err != nil {
			return fmt.Errorf("teardown %s: %w", agentID, err)
		}
	}
	return nil
}

// IsRunning reports whether the spawner currently owns a worker for
// agentID. Used by MCP tools that need to decide whether removing the
// agent from the roster is safe.
func (s *Spawner) IsRunning(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.workers[agentID]
	return ok
}

// AnyRunningWithRole reports whether at least one instance of an
// archetype role is currently running. The MCP remove_archetype tool
// uses this to refuse drops that would orphan workers.
func (s *Spawner) AnyRunningWithRole(role string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pa := range s.provisioned {
		if pa.agent != nil && pa.agent.Role == role {
			return true
		}
	}
	return false
}

// Stop tears down all workers and result subscribers. For cloud-backed
// agents this also calls Teardown so we don't leak running tasks.
// Cancels the spawner's base context, which stops every long-lived
// goroutine the spawner owns.
func (s *Spawner) Stop() {
	if s.baseCancel != nil {
		s.baseCancel()
	}
	s.mu.Lock()
	subs := s.subs
	provisioned := s.provisioned
	workerIDs := make([]string, 0, len(s.workers))
	for id := range s.workers {
		workerIDs = append(workerIDs, id)
	}
	s.mu.Unlock()

	for _, cancel := range subs {
		cancel()
	}
	for _, id := range workerIDs {
		_ = s.registry.SetState(id, mcpsrv.StateStopped)
	}
	// Best-effort teardown. Bounded so a hung AWS call can't block shutdown.
	tdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for id, p := range provisioned {
		if p.agent.IsPersistent() {
			// Persistent agents outlive the leader by design — that's
			// the whole point. Don't tear them down.
			continue
		}
		if err := p.provisioner.Teardown(tdCtx, p.agent); err != nil {
			fmt.Printf("spawner: teardown %s: %v\n", id, err)
		}
	}
}
