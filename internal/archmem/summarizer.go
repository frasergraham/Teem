package archmem

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/llm"
)

// Summarizer periodically regenerates each role's "# Digest" and prunes
// the "Recent entries" list to the retention window. Owned by the
// daemon; one per team.
type Summarizer struct {
	Store           *Store
	Client          llm.Client
	Roles           RolesFunc
	RetentionWindow time.Duration // entries older than this are dropped at digest time (default 14d)
	Interval        time.Duration // ticker cadence (default 24h)
	InitialDelay    time.Duration // first run after Start (default 30s)
	Model           string        // optional model override
	LogPrefix       string        // stderr log prefix (default "[archmem]")
	// MaxEntriesPerDigest caps how many recent entries are sent to the
	// LLM in one prompt. Zero = no cap.
	MaxEntriesPerDigest int
	// LLMTimeout bounds a single Complete call. Default 2 minutes.
	LLMTimeout time.Duration
}

// Run blocks until ctx is cancelled. Calls runOnce at startup (after
// InitialDelay) and then every Interval. Returns nil on clean exit.
func (s *Summarizer) Run(ctx context.Context) error {
	if s.Store == nil {
		return fmt.Errorf("archmem.Summarizer: Store is required")
	}
	if s.Interval <= 0 {
		s.Interval = 24 * time.Hour
	}
	if s.InitialDelay <= 0 {
		s.InitialDelay = 30 * time.Second
	}
	if s.RetentionWindow <= 0 {
		s.RetentionWindow = 14 * 24 * time.Hour
	}
	if s.LLMTimeout <= 0 {
		s.LLMTimeout = 2 * time.Minute
	}
	if s.LogPrefix == "" {
		s.LogPrefix = "[archmem]"
	}

	timer := time.NewTimer(s.InitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		s.runOnce(ctx)
		timer.Reset(s.Interval)
	}
}

// runOnce summarises every current role. Errors are logged and not
// fatal — the next tick retries.
func (s *Summarizer) runOnce(ctx context.Context) {
	roles := []string{}
	if s.Roles != nil {
		roles = s.Roles()
	}
	for _, role := range roles {
		if err := s.summarizeRole(ctx, role); err != nil {
			fmt.Fprintf(os.Stderr, "%s summarize role %q: %v\n", s.LogPrefix, role, err)
		}
	}
}

// summarizeRole loads the role's entries, drops anything older than the
// retention window, asks the LLM for a fresh digest, and rewrites the
// file atomically. If the LLM call fails the file is left untouched.
func (s *Summarizer) summarizeRole(ctx context.Context, role string) error {
	// 1. Load a snapshot (no lock) for the LLM call.
	snapshot, err := s.Store.LoadEntries(role)
	if err != nil {
		return fmt.Errorf("load entries: %w", err)
	}
	prevFM, _ := s.Store.LoadFrontmatter(role)
	prevDigest, _ := s.Store.LoadDigest(role)
	cutoff := time.Now().UTC().Add(-s.RetentionWindow)
	snapKept := EntriesNewerThan(snapshot, cutoff)
	SortEntriesByTime(snapKept)
	// Skip entirely when there's nothing to summarise and no existing file —
	// avoids creating empty files for unused roles.
	if len(snapKept) == 0 && strings.TrimSpace(prevDigest) == "" {
		body, _ := s.Store.Load(role)
		if body == "" {
			return nil
		}
	}

	// 2. LLM call happens outside the lock so AppendEntry isn't blocked.
	digest := prevDigest
	llmRan := false
	if s.Client != nil && len(snapKept) > 0 {
		d, err := s.callLLM(ctx, role, snapKept, prevDigest)
		if err != nil {
			return fmt.Errorf("llm: %w", err)
		}
		digest = strings.TrimSpace(d)
		llmRan = true
	}

	// 3. Rewrite under the per-role lock, merging in any entries that
	//    AppendEntry wrote during the LLM call. The merge keeps anything
	//    on disk newer than cutoff and dedups against snapKept by
	//    (timestamp, agent_id, job_id).
	return s.Store.MutateUnderLock(role, func(current []Entry) (Frontmatter, string, []Entry, error) {
		merged := mergeEntries(snapKept, EntriesNewerThan(current, cutoff))
		SortEntriesByTime(merged)
		fm := Frontmatter{
			Role:             role,
			DigestWindowDays: int(s.RetentionWindow / (24 * time.Hour)),
		}
		if fm.DigestWindowDays == 0 {
			fm.DigestWindowDays = prevFM.DigestWindowDays
		}
		// Only stamp DigestUpdated when we actually produced a new digest;
		// otherwise the file would claim to have been freshly summarised
		// while still serving prevDigest.
		if llmRan {
			fm.DigestUpdated = time.Now().UTC()
		} else {
			fm.DigestUpdated = prevFM.DigestUpdated
		}
		return fm, digest, merged, nil
	})
}

// mergeEntries returns the union of a and b, deduping by
// (Timestamp, AgentID, JobID).
func mergeEntries(a, b []Entry) []Entry {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]Entry, 0, len(a)+len(b))
	for _, e := range a {
		k := e.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + e.AgentID + "|" + e.JobID
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	for _, e := range b {
		k := e.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + e.AgentID + "|" + e.JobID
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	return out
}

// callLLM asks the configured client for a 6-line-ish digest summarising
// the supplied entries plus the prior digest.
func (s *Summarizer) callLLM(ctx context.Context, role string, entries []Entry, prevDigest string) (string, error) {
	es := entries
	if s.MaxEntriesPerDigest > 0 && len(es) > s.MaxEntriesPerDigest {
		es = es[len(es)-s.MaxEntriesPerDigest:]
	}
	var b strings.Builder
	if prevDigest != "" {
		b.WriteString("Previous digest (may be stale):\n")
		b.WriteString(prevDigest)
		b.WriteString("\n\n")
	}
	b.WriteString("Recent entries (oldest first):\n")
	for _, e := range es {
		b.WriteString(formatEntry(e))
		b.WriteString("\n")
	}
	b.WriteString("\nProduce a 4-6 line markdown digest of what this archetype has been doing — concrete files, recurring themes, open follow-ups. No preamble, no bullets longer than one line.")

	ctx2, cancel := context.WithTimeout(ctx, s.LLMTimeout)
	defer cancel()
	resp, err := s.Client.Complete(ctx2, llm.CompletionRequest{
		Model:     s.Model,
		System:    "You distill activity logs for a Teem archetype role into a short, factual digest used as long-term memory for that role's next spawned worker.",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: b.String()}},
		MaxTokens: 600,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
