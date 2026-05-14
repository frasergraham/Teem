package archmem

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"time"
)

// Completer is the LLM call shape the summarizer needs: text in, text
// out. Default implementation shells out to `claude -p`; tests inject
// a stub. A nil Completer means "skip the LLM call" — the summarizer
// still prunes entries by retention window.
type Completer func(ctx context.Context, prompt string) (string, error)

// Summarizer periodically regenerates each role's "# Digest" and prunes
// the "Recent entries" list to the retention window. Owned by the
// daemon; one per team.
type Summarizer struct {
	Store           *Store
	Complete        Completer
	Roles           RolesFunc
	RetentionWindow time.Duration // entries older than this are dropped at digest time (default 14d)
	Interval        time.Duration // ticker cadence (default 24h)
	InitialDelay    time.Duration // first run after Start (default 30s)
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
// fatal — the next tick retries. Each role is wrapped in a recover so
// a panic on one role (e.g. a Completer misbehaving) does not kill
// the summarizer goroutine — the next tick still happens.
func (s *Summarizer) runOnce(ctx context.Context) {
	roles := []string{}
	if s.Roles != nil {
		roles = s.Roles()
	}
	for _, role := range roles {
		s.runRoleSafely(ctx, role)
	}
}

func (s *Summarizer) runRoleSafely(ctx context.Context, role string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "%s PANIC summarising role %q: %v\n%s\n", s.LogPrefix, role, r, debug.Stack())
		}
	}()
	if err := s.summarizeRole(ctx, role); err != nil {
		fmt.Fprintf(os.Stderr, "%s summarize role %q: %v\n", s.LogPrefix, role, err)
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
	if s.Complete != nil && len(snapKept) > 0 {
		prompt := buildDigestPrompt(s.cappedEntries(snapKept), prevDigest)
		ctx2, cancel := context.WithTimeout(ctx, s.LLMTimeout)
		d, err := s.Complete(ctx2, prompt)
		cancel()
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

// cappedEntries returns the most recent MaxEntriesPerDigest entries
// (or all of them when the cap is zero). Input must already be sorted
// oldest-first.
func (s *Summarizer) cappedEntries(es []Entry) []Entry {
	if s.MaxEntriesPerDigest > 0 && len(es) > s.MaxEntriesPerDigest {
		return es[len(es)-s.MaxEntriesPerDigest:]
	}
	return es
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

// buildDigestPrompt assembles the one-shot prompt that the Completer
// receives. Kept pure (no I/O, no time.Now) so it's trivially testable
// from the input/output contract: entries + prevDigest → prompt string.
func buildDigestPrompt(entries []Entry, prevDigest string) string {
	var b strings.Builder
	b.WriteString("You distill activity logs for a Teem archetype role into a short, factual digest used as long-term memory for that role's next spawned worker.\n\n")
	if prevDigest != "" {
		b.WriteString("Previous digest (may be stale):\n")
		b.WriteString(prevDigest)
		b.WriteString("\n\n")
	}
	b.WriteString("Recent entries (oldest first):\n")
	for _, e := range entries {
		b.WriteString(formatEntry(e))
		b.WriteString("\n")
	}
	b.WriteString("\nProduce a 4-6 line markdown digest of what this archetype has been doing — concrete files, recurring themes, open follow-ups. No preamble, no bullets longer than one line.")
	return b.String()
}

// NewClaudeSubprocessCompleter returns a Completer that runs
// `claude -p` as a subprocess to produce a digest. claudePath may be
// empty to look up "claude" on PATH at call time. repoRoot becomes the
// subprocess CWD (claude inherits the team's repo for any contextual
// reads). Output is parsed from stream-json, mirroring the pattern in
// internal/pulse — we use stream-json (not plain text) so a verbose
// CLI that emits status banners on stdout doesn't leak into the digest.
func NewClaudeSubprocessCompleter(claudePath, repoRoot string) Completer {
	return func(ctx context.Context, prompt string) (string, error) {
		path := claudePath
		if path == "" {
			p, err := exec.LookPath("claude")
			if err != nil {
				return "", fmt.Errorf("claude CLI not on PATH: %w", err)
			}
			path = p
		}
		// One-shot: no --resume, no --mcp-config. stream-json + --verbose
		// gives us a clean assistant `result` event we can pluck the
		// digest from regardless of any chrome the CLI emits.
		args := []string{
			"-p",
			"--output-format", "stream-json",
			"--verbose",
			"--dangerously-skip-permissions",
			prompt,
		}
		cmd := exec.CommandContext(ctx, path, args...)
		if repoRoot != "" {
			cmd.Dir = repoRoot
		}
		cmd.Stdin = nil
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return "", err
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("start claude: %w", err)
		}
		text, parseErr := parseDigestStream(stdout)
		if waitErr := cmd.Wait(); waitErr != nil {
			return "", fmt.Errorf("claude exit: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
		}
		if parseErr != nil {
			return "", parseErr
		}
		return text, nil
	}
}

// parseDigestStream consumes Claude Code's stream-json output and
// returns the final assistant text. Prefers the `result` event when
// present; falls back to the last assistant text block otherwise.
func parseDigestStream(r io.Reader) (string, error) {
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type assistantMsg struct {
		Content []contentBlock `json:"content"`
	}
	type ev struct {
		Type    string       `json:"type"`
		Result  string       `json:"result"`
		Message assistantMsg `json:"message"`
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var (
		text   string
		result string
	)
	for sc.Scan() {
		var e ev
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			for _, c := range e.Message.Content {
				if c.Type == "text" && c.Text != "" {
					text = c.Text
				}
			}
		case "result":
			if e.Result != "" {
				result = e.Result
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read stream: %w", err)
	}
	if result != "" {
		return result, nil
	}
	return text, nil
}
