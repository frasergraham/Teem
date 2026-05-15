package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ModelTotals is the per-model token accumulator inside StateFile.
// All four fields are running daily sums; they reset to zero at the
// configured anchor.
type ModelTotals struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
}

// StateFile is the on-disk shape of ~/.teem/state/usage.json. LastReset
// is the wall-clock moment the daily window most recently rolled over;
// ByModel maps the Claude model id (e.g. "claude-opus-4-7") to its
// running totals.
type StateFile struct {
	LastReset time.Time              `json:"last_reset"`
	ByModel   map[string]ModelTotals `json:"by_model"`
}

// Store wraps StateFile with a mutex and atomic file persistence. Safe
// for concurrent Record / Snapshot calls; one Store per daemon.
type Store struct {
	path string

	mu    sync.Mutex
	state StateFile
}

// DefaultStatePath returns ~/.teem/state/usage.json. Empty when $HOME
// is unreadable.
func DefaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".teem", "state", "usage.json")
}

// OpenStore opens (creating the parent dir if needed) the state file at
// path. A missing file starts empty. Malformed JSON is reported as an
// error so the operator notices.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("usage: mkdir state dir: %w", err)
	}
	s := &Store{path: path, state: StateFile{ByModel: map[string]ModelTotals{}}}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("usage: read %s: %w", path, err)
	}
	if len(body) == 0 {
		return s, nil
	}
	var loaded StateFile
	if err := json.Unmarshal(body, &loaded); err != nil {
		return nil, fmt.Errorf("usage: parse %s: %w", path, err)
	}
	if loaded.ByModel == nil {
		loaded.ByModel = map[string]ModelTotals{}
	}
	s.state = loaded
	return s, nil
}

// Path returns the on-disk path the store is persisting to. Useful for
// the CLI and tests.
func (s *Store) Path() string { return s.path }

// Snapshot returns a deep-ish copy of the current state. The ByModel
// map is freshly allocated so callers can mutate it freely.
func (s *Store) Snapshot() StateFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := StateFile{LastReset: s.state.LastReset, ByModel: make(map[string]ModelTotals, len(s.state.ByModel))}
	for k, v := range s.state.ByModel {
		out.ByModel[k] = v
	}
	return out
}

// MaybeReset checks whether `now` has crossed the configured anchor
// since LastReset and, if so, clears the by-model totals and stamps the
// new LastReset. Returns true when a reset happened. Persistence
// happens in the same call so a process restart can't lose the
// transition.
//
// Used by Record (before accumulating) and by AvailableQuota (before
// reading). Callers must NOT hold s.mu — this method takes it.
func (s *Store) MaybeReset(cfg Config, now time.Time) (bool, error) {
	anchor, err := cfg.MostRecentReset(now)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// First-ever write: stamp LastReset without clearing (nothing to clear).
	if s.state.LastReset.IsZero() {
		s.state.LastReset = anchor
		return false, s.persistLocked()
	}
	if !s.state.LastReset.Before(anchor) {
		return false, nil
	}
	s.state.ByModel = map[string]ModelTotals{}
	s.state.LastReset = anchor
	if err := s.persistLocked(); err != nil {
		return true, err
	}
	return true, nil
}

// Record accumulates a single subprocess's usage into the by-model
// totals. Reset is applied first so a subprocess that ran across the
// anchor still lands in the right window. Empty model names are
// recorded under "unknown" so the operator notices schema drift.
func (s *Store) Record(cfg Config, model string, t ModelTotals, when time.Time) error {
	if _, err := s.MaybeReset(cfg, when); err != nil {
		return err
	}
	if model == "" {
		model = "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.state.ByModel[model]
	cur.Input += t.Input
	cur.Output += t.Output
	cur.CacheRead += t.CacheRead
	cur.CacheCreate += t.CacheCreate
	s.state.ByModel[model] = cur
	return s.persistLocked()
}

// TotalBillable returns the sum across all models of input + output +
// cache_create tokens. cache_read is excluded because it's roughly
// 10% of the input price — the throttle wants to track expensive
// tokens. Callers that want a different breakdown can use Snapshot.
func (s *Store) TotalBillable() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, t := range s.state.ByModel {
		total += t.Input + t.Output + t.CacheCreate
	}
	return total
}

// persistLocked writes the current state to disk atomically (temp file
// + rename). Caller must hold s.mu.
func (s *Store) persistLocked() error {
	body, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("usage: marshal state: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".usage.json.")
	if err != nil {
		return fmt.Errorf("usage: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("usage: write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: rename tmp: %w", err)
	}
	return nil
}
