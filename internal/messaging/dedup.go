package messaging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Dedup is a daemon-global last-fired map keyed by
// (team_id, task_id, severity). Calls within Window of a prior fire for
// the same key are dropped; calls outside the window or with a different
// severity escape through. Persistence is best-effort to the configured
// path — a write failure does not block delivery.
type Dedup struct {
	mu     sync.Mutex
	path   string
	window time.Duration
	last   map[string]time.Time
	now    func() time.Time
}

// NewDedup constructs a Dedup, loading any prior state from path. A
// missing file is fine — start empty. A bad file is logged into the
// returned error and the caller may decide whether to continue.
func NewDedup(path string, window time.Duration) (*Dedup, error) {
	if window <= 0 {
		window = DefaultDedupWindow
	}
	d := &Dedup{
		path:   path,
		window: window,
		last:   map[string]time.Time{},
		now:    time.Now,
	}
	if path == "" {
		return d, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return d, fmt.Errorf("messaging: read dedup: %w", err)
	}
	if len(body) == 0 {
		return d, nil
	}
	var stored map[string]time.Time
	if err := json.Unmarshal(body, &stored); err != nil {
		return d, fmt.Errorf("messaging: parse dedup: %w", err)
	}
	for k, v := range stored {
		d.last[k] = v
	}
	return d, nil
}

// SetClock is the test seam — swap in a deterministic time source.
func (d *Dedup) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = now
}

// Allow returns true and records the fire time if msg should be
// delivered, or false if a prior fire for the same (team, task, severity)
// is still within the window.
func (d *Dedup) Allow(msg Message) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := dedupKey(msg)
	now := d.now()
	if prev, ok := d.last[key]; ok && now.Sub(prev) < d.window {
		return false
	}
	d.last[key] = now
	d.persistLocked()
	return true
}

// dedupKey is the persistence key — same shape the design doc commits to,
// so an operator can grep ~/.teem/state/messaging.json by task id.
func dedupKey(msg Message) string {
	return fmt.Sprintf("%s/%s/%s", msg.TeamID, msg.TaskID, string(msg.Severity))
}

func (d *Dedup) persistLocked() {
	if d.path == "" {
		return
	}
	body, err := json.MarshalIndent(d.last, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, d.path)
}
