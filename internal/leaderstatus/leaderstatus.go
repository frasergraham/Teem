// Package leaderstatus is the "what is each leader-tier agent doing
// right now" board. The dashboard pins the human-readable status the
// Leader (and any PM-style worker) wrote most recently, plus which
// task ids each is currently working.
//
// One file per team at ~/.teem/state/<team>/leader_status.json:
//
//	{
//	  "leader":     {"text": "Reviewing T1+T6 diff", "updated_at": "...", "current_task_ids": ["t-aa"]},
//	  "worker-12":  {"text": "Spawning reviewer-7", "updated_at": "...", "current_task_ids": []}
//	}
//
// Writes are atomic (write-tmp, rename) so a crashed process can't
// leave a half-written file. Stored as a per-agent map from day one
// so we don't have to migrate when PM workers start writing.
package leaderstatus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one agent's most recent status report.
type Entry struct {
	AgentID        string    `json:"agent_id"`
	Text           string    `json:"text"`
	UpdatedAt      time.Time `json:"updated_at"`
	CurrentTaskIDs []string  `json:"current_task_ids,omitempty"`
}

// MaxTextBytes bounds the per-entry status text. Status is meant to
// answer "what are you doing right now" — long planning prose belongs
// in record_decision, not here. Trimmed (with ellipsis) on Set.
const MaxTextBytes = 240

// Store owns the on-disk per-agent status map. Safe for concurrent
// callers.
type Store struct {
	path string

	mu      sync.Mutex
	entries map[string]Entry
}

// Open loads the file at path (or starts an empty map if it doesn't
// exist) and returns a Store ready to read/write.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("leaderstatus: mkdir: %w", err)
	}
	s := &Store{path: path, entries: map[string]Entry{}}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("leaderstatus: read: %w", err)
	}
	if len(body) == 0 {
		return s, nil
	}
	var loaded map[string]Entry
	if err := json.Unmarshal(body, &loaded); err != nil {
		// Forward-compat: a corrupt/old shape shouldn't bring the
		// daemon down. Start empty and overwrite on first Set.
		return s, nil
	}
	for k, v := range loaded {
		if v.AgentID == "" {
			v.AgentID = k
		}
		s.entries[k] = v
	}
	return s, nil
}

// Set replaces the entry for agentID. Text is truncated to
// MaxTextBytes. UpdatedAt is set to time.Now() if zero.
func (s *Store) Set(agentID, text string, currentTaskIDs []string) error {
	if agentID == "" {
		return fmt.Errorf("leaderstatus: agent_id is required")
	}
	if len(text) > MaxTextBytes {
		text = text[:MaxTextBytes] + "…"
	}
	taskIDs := append([]string(nil), currentTaskIDs...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[agentID] = Entry{
		AgentID:        agentID,
		Text:           text,
		UpdatedAt:      time.Now().UTC(),
		CurrentTaskIDs: taskIDs,
	}
	return s.persistLocked()
}

// Get returns the entry for agentID and a bool indicating whether
// it existed.
func (s *Store) Get(agentID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[agentID]
	if !ok {
		return Entry{}, false
	}
	// Defensive copy so the caller can't mutate our slice.
	if len(e.CurrentTaskIDs) > 0 {
		e.CurrentTaskIDs = append([]string(nil), e.CurrentTaskIDs...)
	}
	return e, true
}

// All returns every entry, sorted by AgentID for stable rendering.
func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if len(e.CurrentTaskIDs) > 0 {
			e.CurrentTaskIDs = append([]string(nil), e.CurrentTaskIDs...)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// persistLocked writes the current map to disk atomically. Caller
// must hold s.mu.
func (s *Store) persistLocked() error {
	body, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("leaderstatus: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("leaderstatus: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("leaderstatus: rename: %w", err)
	}
	return nil
}
