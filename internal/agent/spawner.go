package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/executor"
	"github.com/frasergraham/teem/internal/inflight"
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
	// SocketDir is the directory under which per-agent unix sockets
	// live for the subprocess local-worker model. When set, local
	// agents are spawned as teem-worker subprocesses that survive
	// daemon restart. Empty (e.g. in tests) falls back to the
	// in-process LocalTransport model.
	SocketDir string
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
	// HeartbeatInterval is how often in-process workers (local/ssh)
	// emit a heartbeat audit event. Defaults to 60s when zero. 0
	// (or a negative value) disables.
	HeartbeatInterval time.Duration
	// JobBodyCap is the per-event truncation cap for prompt + output
	// strings. Defaults to 64 KiB when zero.
	JobBodyCap int
	// ArchetypeSeqPath, when non-empty, names a JSON file the spawner
	// persists per-role instance-id counters to. Restored at
	// NewSpawner time so daemon restarts don't reuse instance ids
	// (`worker-3` after restart stays distinct from a historical
	// `worker-3`). Empty disables persistence.
	ArchetypeSeqPath string
	// InFlight is the per-team in-flight job log. When set, the
	// spawner hands it to every Worker so start/end records get
	// written for each job. Used by the daemon's restart-reconcile
	// path to emit job_interrupted audit events for orphans.
	InFlight *inflight.Log
	// LoadArchetypeMemory, when non-nil, is called at worker
	// construction to fetch the role's persisted memory markdown.
	// Returned body is snapshot once onto Worker.BaselineContext so
	// every job the worker runs carries the same long-term context.
	// Errors are logged and treated as empty.
	LoadArchetypeMemory func(role string) (string, error)
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
	s := &Spawner{
		team:         t,
		bus:          b,
		registry:     r,
		cfg:          cfg,
		baseCtx:      ctx,
		baseCancel:   cancel,
		workers:      map[string]*Worker{},
		jobs:         map[string]*jobRecord{},
		subs:         map[string]context.CancelFunc{},
		provisioned:  map[string]provisionedAgent{},
		archetypeSeq: map[string]int{},
	}
	// Restore the per-role counter from disk so daemon restarts
	// don't reuse instance ids. Best-effort: a missing/corrupt file
	// is treated as "start from zero," because the audit-history
	// belt-and-suspenders below will catch any drift.
	if loaded, err := readArchetypeSeq(cfg.ArchetypeSeqPath); err == nil {
		for role, n := range loaded {
			s.archetypeSeq[role] = n
		}
	}
	// Belt-and-suspenders: walk the audit log for max `<role>-N`
	// already used per archetype. If anything is higher than the
	// persisted counter, use that. Combined, the next spawn always
	// produces a fresh id even if the state file is missing/stale.
	if cfg.AuditSink != nil {
		for role, n := range maxArchetypeIDFromAudit(cfg.AuditSink, t) {
			if n > s.archetypeSeq[role] {
				s.archetypeSeq[role] = n
			}
		}
	}
	return s
}

// readArchetypeSeq loads the persisted per-role counter map. Returns
// an empty map (and nil error) when the file is missing.
func readArchetypeSeq(path string) (map[string]int, error) {
	if path == "" {
		return map[string]int{}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int{}, nil
		}
		return nil, err
	}
	out := map[string]int{}
	if err := json.Unmarshal(body, &out); err != nil {
		return map[string]int{}, nil // tolerate corruption — audit fallback recovers
	}
	return out, nil
}

// writeArchetypeSeq atomically persists the counter map. Best-effort:
// errors are not fatal because the audit-history scan reconstructs the
// state on the next start.
func writeArchetypeSeq(path string, seq map[string]int) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(seq, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// maxArchetypeIDFromAudit scans the audit log for any agent id of the
// form `<role>-N` matching a known archetype role and returns the max
// N seen per role. Used to recover the counter if the state file is
// gone or stale.
func maxArchetypeIDFromAudit(sink audit.Sink, t *team.Team) map[string]int {
	out := map[string]int{}
	roles := map[string]struct{}{}
	for _, a := range t.SnapshotArchetypes() {
		roles[a.Role] = struct{}{}
	}
	if len(roles) == 0 {
		return out
	}
	events, err := sink.Query("", time.Time{}, 0)
	if err != nil {
		return out
	}
	for _, e := range events {
		role, n, ok := parseInstanceID(e.AgentID)
		if !ok {
			continue
		}
		if _, isRole := roles[role]; !isRole {
			continue
		}
		if n > out[role] {
			out[role] = n
		}
	}
	return out
}

// parseInstanceID splits `<role>-<N>` into (role, N, true). Anything
// that doesn't fit returns (_, _, false).
func parseInstanceID(id string) (string, int, bool) {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' {
			role := id[:i]
			if role == "" {
				return "", 0, false
			}
			n := 0
			for j := i + 1; j < len(id); j++ {
				c := id[j]
				if c < '0' || c > '9' {
					return "", 0, false
				}
				n = n*10 + int(c-'0')
			}
			if i+1 == len(id) {
				return "", 0, false
			}
			return role, n, true
		}
	}
	return "", 0, false
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
			// collide with the reconciled id. Persist to keep the
			// state file in sync.
			s.mu.Lock()
			if s.archetypeSeq[arch.Role] < n {
				s.archetypeSeq[arch.Role] = n
			}
			snap := make(map[string]int, len(s.archetypeSeq))
			for k, v := range s.archetypeSeq {
				snap[k] = v
			}
			s.mu.Unlock()
			_ = writeArchetypeSeq(s.cfg.ArchetypeSeqPath, snap)
			connected++
			s.publishLog(id, "reconciled — persistent worker reused")
		}
	}
	return connected
}

// ReconcileLocalSockets walks the per-team socket directory and, for
// every existing socket file, registers the worker as a running
// agent without re-spawning. Used at daemon start so workers that
// outlived a previous daemon are immediately addressable. Best-effort:
// a socket whose /healthz doesn't answer is removed as stale; a
// surviving worker's audit outbox will drain its accumulated events
// against the freshly-started daemon as soon as the leader URL
// resolves.
func (s *Spawner) ReconcileLocalSockets(ctx context.Context) int {
	if s.cfg.SocketDir == "" || s.cfg.WorkerToken == "" {
		return 0
	}
	entries, err := os.ReadDir(s.cfg.SocketDir)
	if err != nil {
		return 0
	}
	connected := 0
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		agentID := strings.TrimSuffix(name, ".sock")
		socketPath := filepath.Join(s.cfg.SocketDir, name)
		client := executor.NewUnixClient(socketPath)
		exec := executor.NewHTTP(client, "http://unix", s.cfg.WorkerToken)
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := exec.CheckHealth(hctx)
		cancel()
		if err != nil {
			_ = os.Remove(socketPath)
			continue
		}
		role, _, ok := parseInstanceID(agentID)
		if !ok {
			continue
		}
		a := &provisioner.Agent{
			ID:         agentID,
			Role:       role,
			Backend:    provisioner.BackendLocal,
			SocketPath: socketPath,
		}
		w := &Worker{Agent: a, Bus: s.bus, Executor: exec}
		if err := w.Start(s.baseCtx); err != nil {
			continue
		}
		s.registry.Add(mcpsrv.AgentEntry{
			ID:        agentID,
			Role:      role,
			State:     mcpsrv.StateRunning,
			Backend:   string(provisioner.BackendLocal),
			StartedAt: time.Now(),
		})
		s.subscribeResults(agentID)
		// Use a real LocalProvisioner for teardown so stop_agent on a
		// reattached worker actually POSTs /shutdown and removes the
		// socket. A no-op would silently leak the process.
		p, _ := s.selectProvisioner(provisioner.BackendLocal)
		if p == nil {
			p = noopTeardownProvisioner{}
		}
		s.mu.Lock()
		s.workers[agentID] = w
		s.provisioned[agentID] = provisionedAgent{provisioner: p, agent: a}
		s.mu.Unlock()
		connected++
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
// Persists the bumped counter so daemon restart doesn't reuse ids.
func (s *Spawner) nextArchetypeID(role string) string {
	s.mu.Lock()
	s.archetypeSeq[role]++
	id := fmt.Sprintf("%s-%d", role, s.archetypeSeq[role])
	// Snapshot the map under the lock; persist outside the critical
	// section so disk latency doesn't pile up on the spawn path.
	snap := make(map[string]int, len(s.archetypeSeq))
	for k, v := range s.archetypeSeq {
		snap[k] = v
	}
	s.mu.Unlock()
	if err := writeArchetypeSeq(s.cfg.ArchetypeSeqPath, snap); err != nil {
		// Disk failure isn't fatal — next start will pick up the
		// audit fallback. Surface to stderr so the operator notices.
		fmt.Fprintf(os.Stderr, "[spawner] persist archetype-seq: %v\n", err)
	}
	return id
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
	// In-process workers (local/ssh — Transport != nil) need to emit
	// their own audit events. Fargate workers have a remote
	// teem-worker daemon that does the emitting, so we skip the
	// in-process path to avoid double-emitting.
	if a.Transport != nil {
		w.Audit = s.cfg.AuditSink
		w.Registry = s.registry
		w.HeartbeatInterval = s.heartbeatInterval()
		w.BodyCap = s.cfg.JobBodyCap
		w.InFlight = s.cfg.InFlight
	}
	// Archetype memory snapshot: bake the role's long-term context
	// into the Worker at construction so it rides along with every
	// job the leader assigns. Best-effort — a load failure leaves
	// BaselineContext empty.
	if s.cfg.LoadArchetypeMemory != nil && a.Role != "" {
		if body, err := s.cfg.LoadArchetypeMemory(a.Role); err == nil {
			w.BaselineContext = body
		} else {
			fmt.Fprintf(os.Stderr, "[spawner] load archmem for %q: %v\n", a.Role, err)
		}
	}
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

// heartbeatInterval returns the configured cadence for in-process
// worker heartbeats, or the 60s default when Config left it zero. A
// negative value disables — returned as 0 so Worker.Start sees "no
// heartbeat goroutine".
func (s *Spawner) heartbeatInterval() time.Duration {
	if s.cfg.HeartbeatInterval == 0 {
		return 60 * time.Second
	}
	if s.cfg.HeartbeatInterval < 0 {
		return 0
	}
	return s.cfg.HeartbeatInterval
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
			// Agent has stopped on the backend. Fail outstanding jobs
			// first (HandleWorkerStopped tears down subs and the
			// provisioner but doesn't surface pending-job failures),
			// then fully reconcile via the same idempotent helper
			// used by the worker_stopped audit path — otherwise
			// s.workers/s.provisioned leak and MaxConcurrent keeps
			// counting the dead agent.
			s.failOutstandingJobs(subCtx, a.ID, "agent stopped")
			s.publishLog(a.ID, "agent stopped on backend; reconciling")
			s.HandleWorkerStopped(subCtx, a.ID)
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
		// Subprocess mode requires SocketDir + WorkerToken. When
		// SocketDir is unset (tests, --no-tailnet smoke flows) the
		// provisioner falls back to in-process LocalTransport.
		return provisioner.NewLocalProvisionerForSubprocess(
			s.cfg.RepoRoot,
			s.cfg.WorktreeBase,
			s.cfg.SocketDir,
			s.cfg.LeaderURL,
			s.cfg.WorkerToken,
		), nil
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

// executorFor builds the Executor for a provisioned agent. It picks
// by Agent shape:
//
//   - SocketPath != "" → unix-socket HTTPExecutor (local subprocess
//     teem-worker). The default for ephemeral local agents after the
//     subprocess migration.
//   - TailnetHost != ""  → tailnet HTTPExecutor (SSH-spawned remote,
//     Fargate, or persistent local that the operator manages).
//   - Transport != nil   → legacy in-process ProcessExecutor. Today
//     used only by SSH (which still wraps an exec.Cmd transport).
//
// Any of these means the daemon and worker are independent processes
// at the network layer; the difference is the dialer.
func (s *Spawner) executorFor(a *provisioner.Agent) (executor.Executor, error) {
	if a.SocketPath != "" {
		if s.cfg.WorkerToken == "" {
			return nil, fmt.Errorf("agent %s: unix-socket executor needs WorkerToken", a.ID)
		}
		client := executor.NewUnixClient(a.SocketPath)
		return executor.NewHTTP(client, "http://unix", s.cfg.WorkerToken), nil
	}
	if a.TailnetHost != "" {
		if s.cfg.HTTPClient == nil || s.cfg.WorkerToken == "" {
			return nil, fmt.Errorf("agent %s: tailnet executor needs HTTPClient + WorkerToken (tailnet must be enabled)", a.ID)
		}
		return executor.NewHTTP(s.cfg.HTTPClient, "http://"+a.TailnetHost+":7780", s.cfg.WorkerToken), nil
	}
	if a.Transport != nil {
		return executor.NewProcess(a.Transport, a.WorkingDir, a.MCPs), nil
	}
	return nil, fmt.Errorf("agent %s: provisioner returned no executor handle", a.ID)
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

// HandleWorkerStopped reconciles leader state with a worker that
// terminated under its own steam. Mirrors StopAgent's bookkeeping
// (cancel subscriptions, flip registry to Stopped, drop from internal
// maps), then calls provisioner.Teardown with Agent.Stopped=true so
// the provisioner skips the /shutdown POST / SIGTERM path that would
// otherwise hang waiting for an already-dead process.
//
// No-op when the agent is unknown — duplicate worker_stopped events
// can arrive (audit posts are at-least-once) and the second one
// finds nothing to clean up.
func (s *Spawner) HandleWorkerStopped(ctx context.Context, agentID string) {
	s.mu.Lock()
	_, hasWorker := s.workers[agentID]
	pa, hasProv := s.provisioned[agentID]
	cancelLiveness := s.subs["liveness:"+agentID]
	cancelResults := s.subs[agentID]
	delete(s.workers, agentID)
	delete(s.provisioned, agentID)
	delete(s.subs, "liveness:"+agentID)
	delete(s.subs, agentID)
	s.mu.Unlock()

	if !hasWorker && !hasProv {
		return
	}
	if cancelLiveness != nil {
		cancelLiveness()
	}
	if cancelResults != nil {
		cancelResults()
	}
	_ = s.registry.SetState(agentID, mcpsrv.StateStopped)

	if !hasProv {
		return
	}
	pa.agent.Stopped = true
	tdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pa.provisioner.Teardown(tdCtx, pa.agent); err != nil {
		s.publishLog(agentID, fmt.Sprintf("post-stop teardown: %v", err))
	}
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

// TotalInFlight returns the sum of in-flight job counts across every
// active worker. Used by the daemon's graceful-shutdown drain to
// decide whether the team is idle yet.
func (s *Spawner) TotalInFlight() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, w := range s.workers {
		n += w.inFlight.Load()
	}
	return n
}

// Drain blocks until either TotalInFlight reaches zero or ctx
// expires. Polled rather than condition-variable based because the
// in-flight signals come from many workers and the check is cheap.
// Returns nil on clean drain, ctx.Err() on timeout.
func (s *Spawner) Drain(ctx context.Context) error {
	if s.TotalInFlight() == 0 {
		return nil
	}
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if s.TotalInFlight() == 0 {
			return nil
		}
	}
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
