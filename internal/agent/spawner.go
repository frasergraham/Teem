package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/bus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/team"
)

// jobRecord tracks an outstanding job for the get_results MCP tool.
type jobRecord struct {
	status string
	output string
}

// Spawner satisfies mcp.Spawner. It owns the workers it spawns and the
// outstanding job table.
type Spawner struct {
	team     *team.Team
	bus      bus.Bus
	registry *mcpsrv.Registry

	mu      sync.Mutex
	workers map[string]*Worker
	jobs    map[string]*jobRecord
	subs    map[string]context.CancelFunc
}

// NewSpawner constructs a Spawner. Call Stop to tear it down.
func NewSpawner(t *team.Team, b bus.Bus, r *mcpsrv.Registry) *Spawner {
	return &Spawner{
		team:     t,
		bus:      b,
		registry: r,
		workers:  map[string]*Worker{},
		jobs:     map[string]*jobRecord{},
		subs:     map[string]context.CancelFunc{},
	}
}

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
	p, err := provisioner.Select(pSpec)
	if err != nil {
		return "", err
	}
	a, err := p.Provision(ctx, pSpec)
	if err != nil {
		return "", err
	}

	w := &Worker{Agent: a, Bus: s.bus}
	if err := w.Start(ctx); err != nil {
		return "", err
	}

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
	s.mu.Unlock()
	return a.ID, nil
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
	s.jobs[jobID] = &jobRecord{status: "pending"}
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

// Stop tears down all workers and result subscribers.
func (s *Spawner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.subs {
		cancel()
	}
	for id := range s.workers {
		s.registry.SetState(id, mcpsrv.StateStopped)
	}
}
