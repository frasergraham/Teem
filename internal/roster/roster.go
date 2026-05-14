// Package roster manages persistent, identity-carrying worker names.
//
// Allocation policy (T9 named workers):
//
//  1. Fresh wordlist entry. Walk Wordlist in order; the first base
//     whose `<role>-<base>` id is not yet on the roster wins. This
//     keeps freshly-spawned workers readable ("worker-ada",
//     "reviewer-blake").
//  2. Reincarnation. When the wordlist for this role is exhausted —
//     i.e. every base has been bound to the role at some point —
//     pick the least-recently-used roster entry with `Role == role`
//     and `InUse == false`. The retired worker comes back: "bob"
//     returns from retirement instead of becoming "worker-2". This
//     is the feature, not a fallback.
//  3. Numeric suffix. Only fires when every name on the roster for
//     this role is currently in use AND every wordlist base for the
//     role is also pinned to a running worker. Essentially
//     unreachable at expected scale; retained as no-collision
//     belt-and-suspenders.
//
// Names are scoped per role: `worker-ada` and `reviewer-ada` are
// independent entries, because the role prefix carries semantic
// meaning. The roster file is the source of truth for
// `which names exist`; legacy `<role>-<N>` ids surfaced by migration
// participate in reincarnation alongside wordlist names.
package roster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry records one name in the roster. ID is the full agent id
// (e.g. `worker-ada` or legacy `worker-3`). InUse == true means a
// worker with this id is currently running; false means the name is
// available for reincarnation.
type Entry struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	InUse      bool      `json:"in_use"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// Roster is the per-team allocator. Safe for concurrent use.
type Roster struct {
	path     string
	wordlist []string

	mu          sync.Mutex
	entries     map[string]Entry
	nextNumeric map[string]int
}

type onDisk struct {
	Entries     map[string]Entry `json:"entries"`
	NextNumeric map[string]int   `json:"next_numeric"`
}

// Open loads the roster file at path. A missing file is treated as
// an empty roster. Empty path disables persistence (in-memory only
// — useful in tests).
func Open(path string) (*Roster, error) {
	r := &Roster{
		path:        path,
		wordlist:    Wordlist,
		entries:     map[string]Entry{},
		nextNumeric: map[string]int{},
	}
	if path == "" {
		return r, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return r, fmt.Errorf("roster: read %s: %w", path, err)
	}
	var d onDisk
	if err := json.Unmarshal(body, &d); err != nil {
		// Treat corruption as "start empty" — better than refusing to
		// boot. Migration on the next call can repopulate from
		// transcripts/audit.
		return r, nil
	}
	if d.Entries != nil {
		r.entries = d.Entries
	}
	if d.NextNumeric != nil {
		r.nextNumeric = d.NextNumeric
	}
	return r, nil
}

// Allocate picks the next id for a freshly-spawned worker of role.
// See the package doc for the policy. The returned id is marked
// InUse and its LastUsedAt advanced.
func (r *Roster) Allocate(role string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()

	// Step 1: fresh wordlist entry.
	for _, base := range r.wordlist {
		id := role + "-" + base
		if _, exists := r.entries[id]; !exists {
			r.entries[id] = Entry{ID: id, Role: role, InUse: true, LastUsedAt: now}
			_ = r.persistLocked()
			return id
		}
	}

	// Step 2: reincarnation — LRU among same-role, not-in-use entries.
	var bestID string
	var bestTime time.Time
	first := true
	for id, e := range r.entries {
		if e.Role != role || e.InUse {
			continue
		}
		if first || e.LastUsedAt.Before(bestTime) {
			bestID = id
			bestTime = e.LastUsedAt
			first = false
		}
	}
	if bestID != "" {
		e := r.entries[bestID]
		e.InUse = true
		e.LastUsedAt = now
		r.entries[bestID] = e
		_ = r.persistLocked()
		return bestID
	}

	// Step 3: numeric suffix. Skip ids that collide with already-
	// registered entries (legacy IDs).
	for {
		r.nextNumeric[role]++
		id := fmt.Sprintf("%s-%d", role, r.nextNumeric[role])
		if _, exists := r.entries[id]; exists {
			continue
		}
		r.entries[id] = Entry{ID: id, Role: role, InUse: true, LastUsedAt: now}
		_ = r.persistLocked()
		return id
	}
}

// Release marks id as no longer in use. Bumps LastUsedAt so future
// reincarnation prefers other entries before reaching for this one.
// No-op for unknown ids.
func (r *Roster) Release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return
	}
	e.InUse = false
	e.LastUsedAt = time.Now().UTC()
	r.entries[id] = e
	_ = r.persistLocked()
}

// MarkInUse records id as currently running. Creates the entry if
// absent. Used for reconciliation of persistent agents and for
// reattaching to subprocess workers that outlived the daemon.
func (r *Roster) MarkInUse(id, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	e.ID = id
	if role != "" {
		e.Role = role
	}
	e.InUse = true
	e.LastUsedAt = time.Now().UTC()
	r.entries[id] = e
	if base, ok := numericSuffix(id, e.Role); ok {
		if base > r.nextNumeric[e.Role] {
			r.nextNumeric[e.Role] = base
		}
	}
	_ = r.persistLocked()
}

// Register inserts id as not-in-use, leaving its LastUsedAt at the
// supplied timestamp (so older retirements lose the LRU coin-flip).
// If id already exists, this is a no-op. Used by migration.
func (r *Roster) Register(id, role string, lastUsed time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; ok {
		return
	}
	if lastUsed.IsZero() {
		lastUsed = time.Now().UTC()
	}
	r.entries[id] = Entry{ID: id, Role: role, InUse: false, LastUsedAt: lastUsed}
	if base, ok := numericSuffix(id, role); ok {
		if base > r.nextNumeric[role] {
			r.nextNumeric[role] = base
		}
	}
	_ = r.persistLocked()
}

// Snapshot returns a copy of the roster sorted by id for stable
// dashboards / list_roster output.
func (r *Roster) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// IsKnown reports whether id is in the roster (in use or retired).
func (r *Roster) IsKnown(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[id]
	return ok
}

// numericSuffix returns the trailing integer in `<role>-<N>`, if
// any. Used to keep the numeric-fallback counter monotonic against
// historical legacy ids.
func numericSuffix(id, role string) (int, bool) {
	prefix := role + "-"
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	rest := id[len(prefix):]
	if rest == "" {
		return 0, false
	}
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// RoleFromID extracts the role prefix from an agent id of the form
// `<role>-<suffix>`. Returns "" when id has no '-'. Works for both
// named ("worker-ada") and numeric ("worker-3", "security-reviewer-7")
// id shapes.
func RoleFromID(id string) string {
	i := strings.LastIndexByte(id, '-')
	if i <= 0 {
		return ""
	}
	return id[:i]
}

// persistLocked writes the roster to disk. Called under r.mu. Best-
// effort; errors are returned to the caller but the in-memory state
// is already updated.
func (r *Roster) persistLocked() error {
	if r.path == "" {
		return nil
	}
	d := onDisk{Entries: r.entries, NextNumeric: r.nextNumeric}
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// MigrateLegacy seeds the roster from pre-T9 state so historical
// ids participate in reincarnation. Calls are idempotent — entries
// that already exist are not overwritten. Sources:
//
//   - legacyArchetypeSeqPath: ~/.teem/state/<team>/archetype-seq.json
//     produced by the old per-role counter. Each `{role: N}` pair
//     becomes N register-only entries `<role>-1..<role>-N`.
//   - transcriptsDir: ~/.teem/state/<team>/transcripts/<id>/ — every
//     subdir name is a historical agent id. Role inferred via
//     RoleFromID; ids whose role doesn't match any known role are
//     skipped (e.g. a stray `leader-<jobid>` directory).
//   - extraIDs: caller-supplied (e.g. from audit log scan).
//
// Returns the count of newly-registered ids. Errors are best-effort
// — a missing file just means "nothing to migrate from there."
func (r *Roster) MigrateLegacy(legacyArchetypeSeqPath, transcriptsDir string, knownRoles []string, extraIDs []string) int {
	roleSet := map[string]struct{}{}
	for _, role := range knownRoles {
		roleSet[role] = struct{}{}
	}
	added := 0

	// 1. Legacy archetype-seq.json counter.
	if legacyArchetypeSeqPath != "" {
		if body, err := os.ReadFile(legacyArchetypeSeqPath); err == nil {
			counts := map[string]int{}
			if err := json.Unmarshal(body, &counts); err == nil {
				for role, n := range counts {
					if _, ok := roleSet[role]; !ok && len(roleSet) > 0 {
						// Tolerate roles the operator removed —
						// still register them so any surviving
						// transcripts are correlatable.
					}
					for i := 1; i <= n; i++ {
						id := fmt.Sprintf("%s-%d", role, i)
						if !r.IsKnown(id) {
							r.Register(id, role, time.Time{})
							added++
						}
					}
				}
			}
		}
	}

	// 2. Transcripts dir — one subdir per historical agent id.
	if transcriptsDir != "" {
		if entries, err := os.ReadDir(transcriptsDir); err == nil {
			for _, ent := range entries {
				if !ent.IsDir() {
					continue
				}
				id := ent.Name()
				role := RoleFromID(id)
				if role == "" {
					continue
				}
				if _, ok := roleSet[role]; !ok && len(roleSet) > 0 {
					// Unknown role — could be a removed archetype.
					// Still register; future reincarnation only
					// fires when that role exists again.
				}
				if !r.IsKnown(id) {
					ts := time.Time{}
					if info, err := ent.Info(); err == nil {
						ts = info.ModTime().UTC()
					}
					r.Register(id, role, ts)
					added++
				}
			}
		}
	}

	// 3. Caller-supplied extras (e.g. audit-log scan).
	for _, id := range extraIDs {
		role := RoleFromID(id)
		if role == "" {
			continue
		}
		if !r.IsKnown(id) {
			r.Register(id, role, time.Time{})
			added++
		}
	}

	return added
}
