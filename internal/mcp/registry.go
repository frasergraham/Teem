package mcp

import (
	"errors"
	"sync"
	"time"
)

// AgentState is a snapshot of an agent's lifecycle.
type AgentState string

const (
	StatePending  AgentState = "pending"
	StateRunning  AgentState = "running"
	StateBusy     AgentState = "busy"
	StateError    AgentState = "error"
	StateStopped  AgentState = "stopped"
)

// AgentEntry is a Registry entry.
type AgentEntry struct {
	ID          string     `json:"id"`
	Role        string     `json:"role"`
	State       AgentState `json:"state"`
	Backend     string     `json:"backend"`
	TailnetHost string     `json:"tailnet_host,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
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
