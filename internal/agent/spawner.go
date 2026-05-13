package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

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
type Spawner struct {
	team     *team.Team
	bus      bus.Bus
	registry *mcpsrv.Registry
	cfg      Config

	mu          sync.Mutex
	workers     map[string]*Worker
	jobs        map[string]*jobRecord
	subs        map[string]context.CancelFunc
	provisioned map[string]provisionedAgent
}

type provisionedAgent struct {
	provisioner provisioner.Provisioner
	agent       *provisioner.Agent
}

// NewSpawner constructs a Spawner. Call Stop to tear it down.
func NewSpawner(t *team.Team, b bus.Bus, r *mcpsrv.Registry, cfg Config) *Spawner {
	return &Spawner{
		team:        t,
		bus:         b,
		registry:    r,
		cfg:         cfg,
		workers:     map[string]*Worker{},
		jobs:        map[string]*jobRecord{},
		subs:        map[string]context.CancelFunc{},
		provisioned: map[string]provisionedAgent{},
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
func (s *Spawner) Reconcile(ctx context.Context) int {
	if s.cfg.HTTPClient == nil || s.cfg.WorkerToken == "" {
		return 0
	}
	connected := 0
	for _, spec := range s.team.Agents {
		if spec.LifecycleOrDefault() != "persistent" {
			continue
		}
		host, ok := s.reconcileHostFor(spec)
		if !ok {
			continue
		}
		exec := executor.NewHTTP(s.cfg.HTTPClient, "http://"+host+":7780", s.cfg.WorkerToken)
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := exec.CheckHealth(hctx)
		cancel()
		if err != nil {
			continue
		}
		a := &provisioner.Agent{
			ID:          spec.ID,
			Role:        spec.Role,
			Backend:     provisioner.Backend(s.backendOf(spec)),
			Lifecycle:   "persistent",
			TailnetHost: host,
			MCPs:        spec.MCPs,
		}
		if err := s.startWorker(ctx, noopTeardownProvisioner{}, a); err != nil {
			s.publishLog(ctx, spec.ID, fmt.Sprintf("reconcile start failed: %v", err))
			continue
		}
		connected++
		s.publishLog(ctx, spec.ID, "reconciled — persistent worker reused")
	}
	return connected
}

// reconcileHostFor returns the tailnet hostname Reconcile should probe
// for an agent. For local persistent agents the hostname is just
// teem-<id>. For fargate persistent agents we also consult the state
// store; if the prior task ARN is recorded but no longer alive we drop
// the record so the next spawn launches a fresh task. The bool is false
// when we shouldn't attempt reconcile (e.g. ssh placement).
func (s *Spawner) reconcileHostFor(spec team.AgentSpec) (string, bool) {
	switch {
	case spec.Local:
		return "teem-" + spec.ID, true
	case spec.Backend == "fargate":
		// Could optionally validate against state.Load + DescribeTasks
		// here. The FargateProvisioner already does that lazily inside
		// Provision; reusing it during reconcile means we trust the
		// tailnet host to remain teem-<id> even if the task ARN
		// changed. For v1 the assumption is correct because we always
		// set hostname=teem-<id> at RunTask time.
		return "teem-" + spec.ID, true
	default:
		return "", false
	}
}

func (s *Spawner) backendOf(spec team.AgentSpec) string {
	switch {
	case spec.Backend != "":
		return spec.Backend
	case spec.Local:
		return "local"
	case spec.SSHTarget != "":
		return "ssh"
	}
	return "local"
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
func (s *Spawner) SpawnByRole(ctx context.Context, role string) (string, error) {
	spec := s.team.FindAgentByRole(role)
	if spec == nil {
		return "", fmt.Errorf("no agent with role %q", role)
	}
	if err := EnsureDir(spec.WorkingDir); err != nil {
		return "", err
	}
	pSpec := provisioner.FromTeamSpec(*spec)
	p, err := s.selectProvisioner(pSpec.Backend)
	if err != nil {
		return "", err
	}

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
		go s.provisionAndStart(ctx, p, pSpec)
		return spec.ID, nil
	}

	a, err := p.Provision(ctx, pSpec)
	if err != nil {
		return "", err
	}
	if err := s.startWorker(ctx, p, a); err != nil {
		return "", err
	}
	return a.ID, nil
}

// provisionAndStart runs the slow cloud provisioner in the background and
// flips the registry entry to running once the worker is healthy.
func (s *Spawner) provisionAndStart(ctx context.Context, p provisioner.Provisioner, spec provisioner.AgentSpec) {
	a, err := p.Provision(ctx, spec)
	if err != nil {
		_ = s.registry.SetState(spec.ID, mcpsrv.StateError)
		s.publishLog(ctx, spec.ID, fmt.Sprintf("provision failed: %v", err))
		return
	}
	if err := s.startWorker(ctx, p, a); err != nil {
		_ = s.registry.SetState(spec.ID, mcpsrv.StateError)
		s.publishLog(ctx, spec.ID, fmt.Sprintf("worker start failed: %v", err))
		_ = p.Teardown(context.Background(), a)
		return
	}
	s.publishLog(ctx, a.ID, "agent ready")
}

// startWorker is the shared half of SpawnByRole and provisionAndStart: it
// builds the executor, kicks off the worker loop, subscribes results, and
// updates the registry entry.
func (s *Spawner) startWorker(ctx context.Context, p provisioner.Provisioner, a *provisioner.Agent) error {
	exec, err := s.executorFor(a)
	if err != nil {
		return err
	}
	w := &Worker{Agent: a, Bus: s.bus, Executor: exec}
	if err := w.Start(ctx); err != nil {
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

	s.subscribeResults(ctx, a.ID)

	s.mu.Lock()
	s.workers[a.ID] = w
	s.provisioned[a.ID] = provisionedAgent{provisioner: p, agent: a}
	s.mu.Unlock()

	if watcher, ok := p.(provisioner.Watcher); ok {
		s.startLivenessWatch(ctx, watcher, a)
	}
	return nil
}

// startLivenessWatch polls the backend every 15s. On ErrAgentStopped it
// flips the registry to StateStopped and fails any outstanding jobs for
// this agent via the bus (so existing leader-side wiring picks them up).
func (s *Spawner) startLivenessWatch(ctx context.Context, w provisioner.Watcher, a *provisioner.Agent) {
	subCtx, cancel := context.WithCancel(ctx)
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
				s.publishLog(subCtx, a.ID, fmt.Sprintf("liveness check error: %v", err))
				continue
			}
			// Agent has stopped on the backend.
			_ = s.registry.SetState(a.ID, mcpsrv.StateStopped)
			s.failOutstandingJobs(subCtx, a.ID, "agent stopped")
			s.publishLog(subCtx, a.ID, "agent stopped on backend; jobs failed")
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

func (s *Spawner) publishLog(ctx context.Context, agentID, line string) {
	_ = s.bus.Publish(ctx, bus.Message{
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

// JobStatus implements mcp.Spawner.
func (s *Spawner) JobStatus(jobID string) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.jobs[jobID]
	if !ok {
		return "", "", false
	}
	return rec.status, rec.output, true
}

// subscribeResults wires a single goroutine per agent that translates
// KindResult bus messages into the in-process job table the
// get_results MCP tool reads from.
func (s *Spawner) subscribeResults(ctx context.Context, agentID string) {
	subCtx, cancel := context.WithCancel(ctx)
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

// Stop tears down all workers and result subscribers. For cloud-backed
// agents this also calls Teardown so we don't leak running tasks.
func (s *Spawner) Stop() {
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
