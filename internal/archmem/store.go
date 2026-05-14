// Package archmem persists a per-archetype memory file the leader
// injects as baseline context for every freshly-spawned worker.
//
// Layout (one file per role under <dir>/<role>.md):
//
//	---
//	role: worker
//	digest_updated: 2026-05-13T14:00Z
//	digest_window_days: 14
//	---
//	# Digest
//	<rolling LLM summary>
//
//	# Recent entries
//	- 2026-05-13T13:55:00Z worker-12 job=abc done — touched executor.go
//	- ...
//
// Appends are atomic O_APPEND writes serialised on a per-role mutex.
// Rewrite (used by the summarizer) goes through tmp+rename under the
// same lock so an interrupted regeneration can't truncate the file.
package archmem

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	recentHeader = "# Recent entries"
	digestHeader = "# Digest"

	// LeaderRole is the reserved role name for per-team leader memory.
	// Always considered valid by the Store regardless of the team's
	// declared archetype set.
	LeaderRole = "leader"
)

// roleRE bounds a role to a path-safe slug. Same shape as the team
// archetype role validator and tight enough to keep the on-disk file
// name (`<role>.md`) inside the memory directory.
var roleRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// IsValidRoleName reports whether s matches the role slug grammar
// (`^[a-z][a-z0-9_-]*$`). Exposed so the CLI can reject bad input
// before touching the filesystem.
func IsValidRoleName(s string) bool { return roleRE.MatchString(s) }

// Entry is one append-only line in the "Recent entries" section.
type Entry struct {
	Timestamp time.Time
	AgentID   string
	JobID     string
	// Status is one of:
	//   "done"  — job completed successfully (workers, summarizer)
	//   "error" — job ended in failure
	//   "note"  — operator annotation added via `teem memory append`;
	//             not a job-complete log, so JobID is typically empty
	Status  string
	Summary string // single-line human summary (no newlines)
}

// Frontmatter is the YAML-ish header at the top of each file. Kept
// minimal so a hand-rolled parser handles it.
type Frontmatter struct {
	Role             string
	DigestUpdated    time.Time
	DigestWindowDays int
}

// RolesFunc returns the current set of archetype roles. Append calls
// reject a role not in this set so a deleted archetype doesn't grow a
// memory file. nil disables validation (handy for tests).
type RolesFunc func() []string

// Store is the leader-side handle to the per-team memory directory.
// Safe for concurrent use.
type Store struct {
	dir   string
	roles RolesFunc

	mu     sync.Mutex
	roleMu map[string]*sync.Mutex
}

// New returns a Store rooted at dir. dir is created on first write.
// roles may be nil; when supplied, AppendEntry validates the role
// against the returned slice on every call so stale callers can't
// resurrect a removed archetype.
func New(dir string, roles RolesFunc) *Store {
	return &Store{
		dir:    dir,
		roles:  roles,
		roleMu: map[string]*sync.Mutex{},
	}
}

// Dir returns the on-disk root directory the Store writes to.
func (s *Store) Dir() string { return s.dir }

// SweepTmp removes any stale "*.md.tmp" files left over from an
// interrupted Rewrite. Called at daemon startup.
func (s *Store) SweepTmp() {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasSuffix(name, ".md.tmp") {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}

func (s *Store) lockRole(role string) func() {
	s.mu.Lock()
	m, ok := s.roleMu[role]
	if !ok {
		m = &sync.Mutex{}
		s.roleMu[role] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// path returns the on-disk path for role; "" if role is invalid.
func (s *Store) path(role string) string {
	if !roleRE.MatchString(role) {
		return ""
	}
	return filepath.Join(s.dir, role+".md")
}

// validRole reports whether role is in the current archetype set (or
// no RolesFunc was supplied). The reserved leader role is always
// considered valid — it is per-team memory, not an archetype.
func (s *Store) validRole(role string) bool {
	if role == LeaderRole {
		return true
	}
	if s.roles == nil {
		return true
	}
	for _, r := range s.roles() {
		if r == role {
			return true
		}
	}
	return false
}

// ErrUnknownRole is returned by AppendEntry when role is not in the
// team's archetype list.
var ErrUnknownRole = errors.New("archmem: role not in team archetypes")

// AppendEntry adds one line to the role's "Recent entries" section.
// Creates the file with default frontmatter if it doesn't exist.
func (s *Store) AppendEntry(role string, e Entry) error {
	p := s.path(role)
	if p == "" {
		return fmt.Errorf("archmem: bad role %q", role)
	}
	if !s.validRole(role) {
		return ErrUnknownRole
	}
	unlock := s.lockRole(role)
	defer unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("archmem: mkdir: %w", err)
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		if err := s.initFile(p, role); err != nil {
			return err
		}
	}
	line := formatEntry(e) + "\n"
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("archmem: open append: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("archmem: append: %w", err)
	}
	return nil
}

// Load returns the full markdown body for role, or "" if the file
// doesn't exist yet.
func (s *Store) Load(role string) (string, error) {
	p := s.path(role)
	if p == "" {
		return "", fmt.Errorf("archmem: bad role %q", role)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(body), nil
}

// LoadDigest returns just the "# Digest" section text, or the whole
// file body if no digest section exists.
func (s *Store) LoadDigest(role string) (string, error) {
	body, err := s.Load(role)
	if err != nil || body == "" {
		return body, err
	}
	_, digest, _, err := parse(body)
	if err != nil {
		return body, nil
	}
	if strings.TrimSpace(digest) == "" {
		return body, nil
	}
	return digest, nil
}

// LoadParsed returns the parsed digest text and entries for role. The
// digest is the literal text between "# Digest" and "# Recent entries"
// (trimmed); a missing or empty digest section returns "". Unlike
// LoadDigest, this never falls back to returning the whole file body —
// callers who need to know whether the file has any real content can
// check (digest == "" && len(entries) == 0).
func (s *Store) LoadParsed(role string) (string, []Entry, error) {
	body, err := s.Load(role)
	if err != nil || body == "" {
		return "", nil, err
	}
	_, digest, entries, perr := parse(body)
	if perr != nil {
		return "", nil, perr
	}
	return strings.TrimSpace(digest), entries, nil
}

// LoadEntries parses the role's "Recent entries" section into Entry
// records. Lines we don't understand are skipped.
func (s *Store) LoadEntries(role string) ([]Entry, error) {
	body, err := s.Load(role)
	if err != nil || body == "" {
		return nil, err
	}
	_, _, entries, err := parse(body)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// loadEntriesLocked reads entries from a path without taking the role
// lock — callers must hold it. Used by MutateUnderLock so the
// read+rewrite happens inside one critical section.
func (s *Store) loadEntriesLocked(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("archmem: read: %w", err)
	}
	_, _, entries, perr := parse(string(data))
	if perr != nil {
		return nil, perr
	}
	return entries, nil
}

// LoadFrontmatter returns the parsed frontmatter; zero value if absent.
func (s *Store) LoadFrontmatter(role string) (Frontmatter, error) {
	body, err := s.Load(role)
	if err != nil || body == "" {
		return Frontmatter{}, err
	}
	fm, _, _, err := parse(body)
	return fm, err
}

// Rewrite replaces the role's file atomically with the supplied
// frontmatter, digest, and entries. Acquires the per-role lock so
// concurrent appends serialise behind it.
func (s *Store) Rewrite(role string, fm Frontmatter, digest string, entries []Entry) error {
	p := s.path(role)
	if p == "" {
		return fmt.Errorf("archmem: bad role %q", role)
	}
	unlock := s.lockRole(role)
	defer unlock()
	return s.rewriteLocked(role, p, fm, digest, entries)
}

// MutateUnderLock acquires the per-role lock, loads the current entry
// list from disk, hands it to fn, and atomically rewrites the file with
// fn's returned (frontmatter, digest, entries). This is the safe shape
// for read-modify-write callers (Summarizer) whose computation must
// happen outside the lock: do the slow work first, then call this with
// a fn that merges newly-arrived appends against the slow result.
//
// Returning an error from fn aborts the rewrite without changes.
func (s *Store) MutateUnderLock(role string, fn func(current []Entry) (Frontmatter, string, []Entry, error)) error {
	p := s.path(role)
	if p == "" {
		return fmt.Errorf("archmem: bad role %q", role)
	}
	unlock := s.lockRole(role)
	defer unlock()
	current, _ := s.loadEntriesLocked(p)
	fm, digest, entries, err := fn(current)
	if err != nil {
		return err
	}
	return s.rewriteLocked(role, p, fm, digest, entries)
}

func (s *Store) rewriteLocked(role, p string, fm Frontmatter, digest string, entries []Entry) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("archmem: mkdir: %w", err)
	}
	if fm.Role == "" {
		fm.Role = role
	}
	body := render(fm, digest, entries)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("archmem: write tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("archmem: rename: %w", err)
	}
	return nil
}

// initFile writes an empty file with default frontmatter + an empty
// Recent entries section. Called inside the per-role lock.
func (s *Store) initFile(path, role string) error {
	body := render(Frontmatter{Role: role}, "", nil)
	return os.WriteFile(path, []byte(body), 0o600)
}

// formatEntry renders one Entry as the single-line bullet stored under
// "# Recent entries". Newlines in Summary are flattened so the line
// stays parseable.
func formatEntry(e Entry) string {
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	agent := e.AgentID
	if agent == "" {
		agent = "-"
	}
	status := e.Status
	if status == "" {
		status = "done"
	}
	summary := strings.ReplaceAll(e.Summary, "\n", " ")
	summary = strings.ReplaceAll(summary, "\r", " ")
	job := e.JobID
	if job == "" {
		job = "-"
	}
	return fmt.Sprintf("- %s %s job=%s %s — %s",
		ts.UTC().Format(time.RFC3339),
		agent,
		job,
		status,
		summary,
	)
}

// render produces the on-disk markdown for a (frontmatter, digest,
// entries) tuple.
func render(fm Frontmatter, digest string, entries []Entry) string {
	var b strings.Builder
	b.WriteString("---\n")
	if fm.Role != "" {
		fmt.Fprintf(&b, "role: %s\n", fm.Role)
	}
	if !fm.DigestUpdated.IsZero() {
		fmt.Fprintf(&b, "digest_updated: %s\n", fm.DigestUpdated.UTC().Format(time.RFC3339))
	}
	if fm.DigestWindowDays > 0 {
		fmt.Fprintf(&b, "digest_window_days: %d\n", fm.DigestWindowDays)
	}
	b.WriteString("---\n")
	b.WriteString(digestHeader + "\n")
	d := strings.TrimSpace(digest)
	if d != "" {
		b.WriteString(d)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(recentHeader + "\n")
	for _, e := range entries {
		b.WriteString(formatEntry(e))
		b.WriteString("\n")
	}
	return b.String()
}

// parse splits a memory file body into (frontmatter, digest, entries).
// Tolerant: a missing frontmatter or section is fine. Unparseable lines
// in "Recent entries" are skipped so a half-written line can't poison
// the whole file.
func parse(body string) (Frontmatter, string, []Entry, error) {
	fm := Frontmatter{}
	rest := body
	if strings.HasPrefix(body, "---\n") {
		end := strings.Index(body[4:], "\n---\n")
		if end >= 0 {
			head := body[4 : 4+end]
			rest = body[4+end+len("\n---\n"):]
			fm = parseFrontmatter(head)
		}
	}
	digest := ""
	entriesText := ""
	if i := strings.Index(rest, recentHeader); i >= 0 {
		// Optional leading "# Digest\n..."
		before := rest[:i]
		entriesText = rest[i+len(recentHeader):]
		if di := strings.Index(before, digestHeader); di >= 0 {
			digest = strings.TrimSpace(before[di+len(digestHeader):])
		} else {
			digest = strings.TrimSpace(before)
		}
	} else if di := strings.Index(rest, digestHeader); di >= 0 {
		digest = strings.TrimSpace(rest[di+len(digestHeader):])
	}
	entries := parseEntries(entriesText)
	return fm, digest, entries, nil
}

func parseFrontmatter(s string) Frontmatter {
	fm := Frontmatter{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "role":
			fm.Role = v
		case "digest_updated":
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				fm.DigestUpdated = t
			}
		case "digest_window_days":
			n := 0
			for i := 0; i < len(v); i++ {
				c := v[i]
				if c < '0' || c > '9' {
					n = 0
					break
				}
				n = n*10 + int(c-'0')
			}
			fm.DigestWindowDays = n
		}
	}
	return fm
}

// parseEntries reads bullet lines and reconstructs Entry records.
// Skips any malformed line — we'd rather lose a recent-entries line
// than fail the whole load.
func parseEntries(s string) []Entry {
	var out []Entry
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		rest := strings.TrimPrefix(line, "- ")
		// Format: <ts> <agent> job=<job> <status> — <summary>
		fields := strings.SplitN(rest, " ", 4)
		if len(fields) < 4 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		agent := fields[1]
		jobField := fields[2]
		if !strings.HasPrefix(jobField, "job=") {
			continue
		}
		job := strings.TrimPrefix(jobField, "job=")
		statusAndSummary := fields[3]
		status := ""
		summary := ""
		if i := strings.Index(statusAndSummary, " — "); i >= 0 {
			status = statusAndSummary[:i]
			summary = strings.TrimSpace(statusAndSummary[i+len(" — "):])
		} else {
			status = statusAndSummary
		}
		out = append(out, Entry{
			Timestamp: ts,
			AgentID:   agent,
			JobID:     job,
			Status:    status,
			Summary:   summary,
		})
	}
	return out
}

// EntriesNewerThan returns the subset of entries whose Timestamp is at
// or after cutoff. Result preserves order.
func EntriesNewerThan(entries []Entry, cutoff time.Time) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !e.Timestamp.Before(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

// SortEntriesByTime sorts entries oldest-first. Stable so equal
// timestamps preserve insertion order.
func SortEntriesByTime(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})
}
