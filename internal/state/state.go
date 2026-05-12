// Package state persists the leader's view of persistent agents.
//
// One JSON file per agent at <Root>/<agent-id>.json. The Store is the
// only thing in Teem that survives across `teem chat` invocations beyond
// the team YAML itself — it's the bridge between an interactive session
// and the long-running worker placements (Fargate tasks, manually-run
// teem-worker daemons) the operator wants to reuse.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is the per-agent persistent state.
type Record struct {
	AgentID     string    `json:"agent_id"`
	Role        string    `json:"role"`
	Backend     string    `json:"backend"`
	Lifecycle   string    `json:"lifecycle"`
	TailnetHost string    `json:"tailnet_host,omitempty"`
	TaskARN     string    `json:"task_arn,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Store is the on-disk registry of persistent agents.
type Store struct {
	Root string
}

// NewStore returns a Store rooted at dir. The directory is created on
// first write so an unused Store is free.
func NewStore(dir string) *Store {
	return &Store{Root: dir}
}

// Save writes r atomically. The file is rewritten on every call so any
// field updates (e.g. a new TaskARN after a reuse-or-recreate) take
// effect.
func (s *Store) Save(r Record) error {
	if r.AgentID == "" {
		return errors.New("state: AgentID required")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("state: mkdir: %w", err)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := s.path(r.AgentID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load returns the record for agentID, or (zero, false, nil) if no such
// record exists. A real read error is returned as a non-nil err.
func (s *Store) Load(agentID string) (Record, bool, error) {
	body, err := os.ReadFile(s.path(agentID))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	var r Record
	if err := json.Unmarshal(body, &r); err != nil {
		return Record{}, false, fmt.Errorf("state: decode %s: %w", agentID, err)
	}
	return r, true, nil
}

// Delete removes the record. Missing files are not an error.
func (s *Store) Delete(agentID string) error {
	err := os.Remove(s.path(agentID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// List returns every record under Root. Useful for reconcile-on-startup
// flows; callers cross-reference against the team YAML to drop entries
// for agents that no longer exist.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		r, ok, err := s.Load(id)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) path(agentID string) string {
	return filepath.Join(s.Root, agentID+".json")
}
