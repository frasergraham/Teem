package mcp

import (
	"errors"
	"sync"
	"time"
)

// AgentState is a snapshot of an agent's lifecycle.
type AgentState string

const (
	StatePending      AgentState = "pending"
	StateProvisioning AgentState = "provisioning"
	StateRunning      AgentState = "running"
	StateBusy         AgentState = "busy"
	StateError        AgentState = "error"
	StateStopped      AgentState = "stopped"
)

// AgentEntry is a Registry entry.
type AgentEntry struct {
	ID          string     `json:"id"`
	Role        string     `json:"role"`
	State       AgentState `json:"state"`
	Backend     string     `json:"backend"`
	TailnetHost string     `json:"tailnet_host,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	// LastSeen is the timestamp of the most recent audit event from
	// this agent (heartbeat, job lifecycle, anything). Zero when the
	// agent has never reported in. The leader uses this to spot
	// stalled or unreachable workers.
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// Registry tracks active agents in-process. Thread-safe.
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*AgentEntry
}

func NewRegistry() *Registry {
	return &Registry{agents: map[string]*AgentEntry{}}
}

var ErrAgentNotFound = errors.New("registry: agent not found")

func (r *Registry) Add(e AgentEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now()
	}
	r.agents[e.ID] = &e
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.agents, id)
}

func (r *Registry) SetState(id string, s AgentState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.agents[id]
	if !ok {
		return ErrAgentNotFound
	}
	e.State = s
	return nil
}

// SetLastSeen updates the agent's LastSeen timestamp. No-op if the
// agent isn't registered (avoid creating a phantom entry from a
// stray audit event).
func (r *Registry) SetLastSeen(id string, ts time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.agents[id]; ok {
		e.LastSeen = ts
	}
}

func (r *Registry) Get(id string) (AgentEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.agents[id]
	if !ok {
		return AgentEntry{}, false
	}
	return *e, true
}

func (r *Registry) List() []AgentEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AgentEntry, 0, len(r.agents))
	for _, e := range r.agents {
		out = append(out, *e)
	}
	return out
}

// GCStopped removes registry entries that have been in StateStopped
// for longer than ttl. Returns the number of entries removed.
// ttl <= 0 is a no-op — retention is opt-in and the registry preserves
// stopped agents forever by default so audit history stays joinable.
// Callers should poll this on a timer when configured.
func (r *Registry) GCStopped(now time.Time, ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	cutoff := now.Add(-ttl)
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := 0
	for id, e := range r.agents {
		if e.State != StateStopped {
			continue
		}
		ref := e.LastSeen
		if ref.IsZero() {
			ref = e.StartedAt
		}
		if ref.IsZero() || ref.Before(cutoff) {
			delete(r.agents, id)
			removed++
		}
	}
	return removed
}
