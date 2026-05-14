// Package pulse is the autonomous-leader heartbeat. A Pulse owns a
// goroutine that periodically invokes `claude -p --resume <session>`
// with a freshly-composed context snapshot, so the leader can take
// turns when no human is connected. Event-driven wakes are handled by
// the channels mechanism (teem-channel stdio shim) when a chat client
// is attached; Pulse covers the disconnected case.
package pulse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/claudeflags"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/plan"
)

// SessionLoader resolves the team's persistent Claude session id.
// Implementation lives in cmd/teem (loadLeaderSession); we accept a
// function value to avoid an import cycle.
type SessionLoader func() (sessionID string, ok bool, err error)

// Config bundles everything one team's Pulse needs.
type Config struct {
	TeamName    string
	LoadSession SessionLoader // returns the session id or (false) if no chat has run yet
	PauseFile   string        // path; presence skips ticks
	RunningFile string        // path; presence signals daemon to auto-resume on restart
	MCPConfig   string        // path to ~/.teem/state/<team>/pulse-mcp.json
	RepoRoot    string        // CWD for the claude subprocess
	Plan        *plan.Plan
	Audit       audit.Sink
	Registry    *mcpsrv.Registry
	Interval    time.Duration // timer cadence; default 5m
	BodyCap     int           // truncation cap for assistant text in audit; default 64 KiB
	ClaudePath  string        // optional override (default: exec.LookPath("claude"))
	TickTimeout time.Duration // per-tick deadline; default 5m
	// MaxPerHour caps autonomous ticks; default 30. Exceeded ticks
	// are skipped and counted but emit no claude invocation. A pause
	// flag and stop() are the override knobs.
	MaxPerHour int
	// IdleBackoffAfter is how many no-tool-call ticks in a row before
	// the effective interval doubles. Default 3.
	IdleBackoffAfter int
}

// Pulse runs the autonomous leader loop for a single team.
type Pulse struct {
	cfg Config

	mu       sync.Mutex // serializes tick execution; never two at once
	running  atomic.Bool
	cancel   context.CancelFunc
	lastTick atomic.Value // time.Time
	tickN    atomic.Int64

	// Sliding window of recent tick timestamps for budget enforcement.
	budgetMu  sync.Mutex
	tickTimes []time.Time

	// Idle backoff: tracks consecutive no-tool-call ticks. Doubles
	// the effective interval after IdleBackoffAfter; reset on the
	// next tick that calls a tool.
	idleStreak atomic.Int32
}

// New constructs a Pulse from a Config. Sensible defaults are applied
// for missing fields.
func New(cfg Config) *Pulse {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.BodyCap == 0 {
		cfg.BodyCap = 64 * 1024
	}
	if cfg.TickTimeout == 0 {
		cfg.TickTimeout = 5 * time.Minute
	}
	if cfg.MaxPerHour == 0 {
		cfg.MaxPerHour = 30
	}
	if cfg.IdleBackoffAfter == 0 {
		cfg.IdleBackoffAfter = 3
	}
	return &Pulse{cfg: cfg}
}

// Start kicks off the periodic loop. Idempotent — calling Start on an
// already-running Pulse is a no-op. The loop exits when Stop is called
// or the parent context is cancelled. Writes a "running" flag file so
// a daemon restart can auto-resume Pulse without operator
// intervention.
func (p *Pulse) Start(parent context.Context) {
	if !p.running.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	_ = p.writeRunningFlag()
	go p.run(ctx)
}

// Stop ends the loop. Safe to call multiple times; safe to call before
// Start. Removes the running-flag file so daemon restarts don't
// auto-resume an explicitly-stopped Pulse.
func (p *Pulse) Stop() {
	if !p.running.CompareAndSwap(true, false) {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	_ = p.clearRunningFlag()
}

// WasRunning checks the persisted running-flag file. Used by the
// daemon at startup to decide whether to auto-Start a freshly-built
// Pulse instance. Does not affect the in-memory running atomic.
func (p *Pulse) WasRunning() bool {
	if p.cfg.RunningFile == "" {
		return false
	}
	_, err := os.Stat(p.cfg.RunningFile)
	return err == nil
}

func (p *Pulse) writeRunningFlag() error {
	if p.cfg.RunningFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.RunningFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.cfg.RunningFile, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func (p *Pulse) clearRunningFlag() error {
	if p.cfg.RunningFile == "" {
		return nil
	}
	err := os.Remove(p.cfg.RunningFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Running reports whether the loop is active.
func (p *Pulse) Running() bool { return p.running.Load() }

// LastTick returns the timestamp of the most recent tick.
func (p *Pulse) LastTick() time.Time {
	v, _ := p.lastTick.Load().(time.Time)
	return v
}

// TickCount returns how many ticks have been attempted (including
// skipped paused ones).
func (p *Pulse) TickCount() int64 { return p.tickN.Load() }

// Interval returns the current timer cadence.
func (p *Pulse) Interval() time.Duration { return p.cfg.Interval }

// SetInterval changes the cadence. If Pulse is already running, the
// change takes effect on the next tick wakeup.
func (p *Pulse) SetInterval(d time.Duration) {
	if d > 0 {
		p.cfg.Interval = d
	}
}

// Paused returns true if the pause flag file exists.
func (p *Pulse) Paused() bool {
	if p.cfg.PauseFile == "" {
		return false
	}
	_, err := os.Stat(p.cfg.PauseFile)
	return err == nil
}

// Pause writes the pause flag file with the supplied reason.
func (p *Pulse) Pause(reason string) error {
	if p.cfg.PauseFile == "" {
		return errors.New("pulse: PauseFile not configured")
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.PauseFile), 0o700); err != nil {
		return err
	}
	if reason == "" {
		reason = time.Now().UTC().Format(time.RFC3339)
	}
	return os.WriteFile(p.cfg.PauseFile, []byte(reason+"\n"), 0o600)
}

// Resume removes the pause flag file.
func (p *Pulse) Resume() error {
	if p.cfg.PauseFile == "" {
		return nil
	}
	err := os.Remove(p.cfg.PauseFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p *Pulse) run(ctx context.Context) {
	// Fire one tick immediately so a freshly-started Pulse doesn't
	// wait Interval before doing anything.
	if err := p.Tick(ctx, "timer"); err != nil && !errors.Is(err, context.Canceled) {
		p.logErr(err)
	}
	for {
		// effective interval honors idle backoff: after a streak of
		// no-tool-call ticks, slow down until the next interesting
		// event nudges us.
		wait := p.effectiveInterval()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := p.Tick(ctx, "timer"); err != nil && !errors.Is(err, context.Canceled) {
			p.logErr(err)
		}
	}
}

// effectiveInterval doubles the configured interval once the idle
// streak crosses IdleBackoffAfter, capped at 8× so we never go fully
// silent (still want a periodic check-in).
func (p *Pulse) effectiveInterval() time.Duration {
	streak := int(p.idleStreak.Load())
	if streak <= p.cfg.IdleBackoffAfter {
		return p.cfg.Interval
	}
	mult := 1 << ((streak - p.cfg.IdleBackoffAfter) - 1)
	if mult > 8 {
		mult = 8
	}
	return p.cfg.Interval * time.Duration(mult)
}

// Tick performs a single autonomous turn. Idempotent under concurrent
// callers (mutex). Returns nil even when paused — pause is "skip
// quietly," not "error."
func (p *Pulse) Tick(ctx context.Context, trigger string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tickN.Add(1)
	// Capture the previous tick time *before* bumping, so the context
	// snapshot can show "Last tick: 4m ago" rather than "0s ago".
	priorLastRaw := p.lastTick.Load()
	priorLast, _ := priorLastRaw.(time.Time)
	now := time.Now().UTC()
	p.lastTick.Store(now)

	if p.Paused() {
		return nil
	}

	// Budget gate: skip (but emit a synthetic audit event) when over
	// quota. Operators see this in `teem audit` if pulse is running
	// too hot.
	if !p.consumeBudget(now) {
		_ = p.cfg.Audit.Write(audit.Event{
			Timestamp: now,
			AgentID:   "leader",
			Kind:      audit.Kind("pulse_budget_exceeded"),
			Message:   fmt.Sprintf("over %d ticks/hour", p.cfg.MaxPerHour),
			Meta:      map[string]any{"trigger": trigger},
		})
		return nil
	}

	sessionID, ok, err := p.cfg.LoadSession()
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if !ok {
		// No human chat has happened yet — there's no session to
		// resume. Skip silently; pulse will pick up the first time
		// the user runs `teem chat`.
		return nil
	}

	contextBody := p.buildContextSnapshot(trigger, priorLast)
	tickCtx, cancel := context.WithTimeout(ctx, p.cfg.TickTimeout)
	defer cancel()

	start := time.Now()
	result, err := p.invokeClaude(tickCtx, sessionID, contextBody)
	dur := time.Since(start)

	ev := audit.Event{
		Timestamp: now,
		AgentID:   "leader",
		Kind:      audit.Kind("pulse_tick"),
		Meta: map[string]any{
			"trigger":     trigger,
			"duration_ms": int(dur.Milliseconds()),
			"tool_calls":  result.toolCalls,
		},
	}
	if err != nil {
		ev.Message = err.Error()
		ev.Meta["error"] = true
	} else {
		ev.Message = truncate(result.text, p.cfg.BodyCap)
		ev.Meta["assistant_bytes"] = len(result.text)
	}
	_ = p.cfg.Audit.Write(ev)

	// Idle streak: only count toward backoff on successful ticks
	// (errors might be transient). Tool calls reset the streak;
	// no-tool-call success bumps it.
	if err == nil {
		if result.toolCalls > 0 {
			p.idleStreak.Store(0)
		} else {
			p.idleStreak.Add(1)
		}
	}

	if err != nil {
		return fmt.Errorf("pulse tick (%s): %w", trigger, err)
	}
	return nil
}

// tickResult captures everything Pulse needs from a claude invocation
// to log + decide what to do next.
type tickResult struct {
	text      string
	toolCalls int
}

// consumeBudget atomically checks whether a new tick fits in the
// hour-window quota. On success, records the timestamp and returns
// true. On failure, returns false and leaves the window unchanged.
func (p *Pulse) consumeBudget(now time.Time) bool {
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	cutoff := now.Add(-time.Hour)
	// Drop expired timestamps.
	fresh := p.tickTimes[:0]
	for _, t := range p.tickTimes {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	p.tickTimes = fresh
	if len(p.tickTimes) >= p.cfg.MaxPerHour {
		return false
	}
	p.tickTimes = append(p.tickTimes, now)
	return true
}

// buildContextSnapshot composes the system-prompt addendum that gets
// passed to claude on a tick. Includes trigger reason, recent audit
// events, open tasks, and currently-running agents — enough for the
// leader to take a useful turn without re-querying everything itself.
func (p *Pulse) buildContextSnapshot(trigger string, priorLast time.Time) string {
	var b strings.Builder
	b.WriteString("This is an autonomous tick — you are taking a turn without a human prompt.\n")
	fmt.Fprintf(&b, "Trigger: %s\n", trigger)
	if !priorLast.IsZero() {
		fmt.Fprintf(&b, "Last tick: %s ago\n", roundDuration(time.Since(priorLast)))
	}
	b.WriteString("\n")

	// Recent audit activity since the last tick. On first tick
	// priorLast is zero — fall back to one Interval window.
	if p.cfg.Audit != nil {
		since := priorLast
		if since.IsZero() {
			since = time.Now().Add(-p.cfg.Interval)
		}
		events, err := p.cfg.Audit.Query("", since, 30)
		if err == nil && len(events) > 0 {
			b.WriteString("Recent activity:\n")
			for _, e := range events {
				if e.Kind == "pulse_tick" {
					continue
				}
				job := ""
				if e.JobID != "" {
					job = " job=" + e.JobID
				}
				msg := e.Message
				if msg != "" {
					msg = " " + truncate(msg, 80)
				}
				fmt.Fprintf(&b, "  [%s] %s%s %s%s\n", e.Timestamp.UTC().Format("15:04:05"), e.AgentID, job, e.Kind, msg)
			}
			b.WriteString("\n")
		}
	}

	// Open tasks.
	if p.cfg.Plan != nil {
		open := p.cfg.Plan.List(plan.Filter{OpenOnly: true})
		if len(open) > 0 {
			b.WriteString("Open tasks:\n")
			for _, t := range open {
				assigned := ""
				if t.AssignedTo != "" {
					assigned = " (assigned: " + t.AssignedTo + ")"
				}
				ev := ""
				if len(t.Evidence) > 0 {
					ev = " evidence: " + strings.Join(t.Evidence, ",")
				}
				fmt.Fprintf(&b, "  - %s %-12s %s%s%s\n", t.ID, t.Status, t.Title, assigned, ev)
			}
			b.WriteString("\n")
		}
	}

	// Running agents.
	if p.cfg.Registry != nil {
		agents := p.cfg.Registry.List()
		if len(agents) > 0 {
			b.WriteString("Active agents:\n")
			for _, a := range agents {
				lastSeen := ""
				if !a.LastSeen.IsZero() {
					lastSeen = fmt.Sprintf(" last_seen=%s ago", roundDuration(time.Since(a.LastSeen)))
				}
				fmt.Fprintf(&b, "  - %s (%s) %s%s\n", a.ID, a.Role, a.State, lastSeen)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("Take one turn: verify completed work, assign new jobs as needed, update task statuses.\n")
	b.WriteString("If there's nothing useful to do, call no tools and say so briefly — that's a valid turn.\n")
	return b.String()
}

// invokeClaude runs `claude -p --resume <session>` with the supplied
// context appended to the system prompt and a short user-side prompt.
// Returns the final assistant text plus how many tool_use blocks the
// model emitted (used by the idle-backoff logic).
func (p *Pulse) invokeClaude(ctx context.Context, sessionID, contextBody string) (tickResult, error) {
	claudePath := p.cfg.ClaudePath
	if claudePath == "" {
		path, err := exec.LookPath("claude")
		if err != nil {
			return tickResult{}, fmt.Errorf("claude CLI not on PATH: %w", err)
		}
		claudePath = path
	}
	args := []string{
		"-p",
		"--resume", sessionID,
		"--output-format", "stream-json",
		"--verbose",
		"--mcp-config", p.cfg.MCPConfig,
		"--append-system-prompt", contextBody,
		"--dangerously-skip-permissions",
	}
	args = append(args, claudeflags.ChannelFlags()...)
	args = append(args, "Take your next turn.")
	cmd := exec.CommandContext(ctx, claudePath, args...)
	cmd.Dir = p.cfg.RepoRoot
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return tickResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return tickResult{}, fmt.Errorf("start claude: %w", err)
	}
	res, parseErr := parseTickStream(stdout)
	if err := cmd.Wait(); err != nil {
		return res, fmt.Errorf("claude exit: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if parseErr != nil {
		return res, parseErr
	}
	return res, nil
}

// parseTickStream consumes Claude Code's stream-json and returns the
// final assistant text plus a count of tool_use content blocks across
// every assistant message. We need our own parser instead of using
// executor.ParseClaudeStreamJSON because we care about the
// (intermediate) tool-call shape that the executor variant
// intentionally ignores.
func parseTickStream(r io.Reader) (tickResult, error) {
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type assistantMsg struct {
		Content []contentBlock `json:"content"`
	}
	type ev struct {
		Type    string          `json:"type"`
		Result  string          `json:"result"`
		Message assistantMsg    `json:"message"`
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var res tickResult
	for sc.Scan() {
		var e ev
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			for _, c := range e.Message.Content {
				if c.Type == "tool_use" {
					res.toolCalls++
				}
				if c.Type == "text" {
					res.text = c.Text
				}
			}
		case "result":
			if e.Result != "" {
				res.text = e.Result
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("read stream: %w", err)
	}
	return res, nil
}

func (p *Pulse) logErr(err error) {
	fmt.Fprintf(os.Stderr, "[pulse %s] %v\n", p.cfg.TeamName, err)
}

func truncate(s string, cap int) string {
	if cap <= 0 || len(s) <= cap {
		return s
	}
	return s[:cap] + "\n…<truncated>"
}

func roundDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return d.Round(time.Minute).String()
	}
}

// WriteMCPConfig writes the per-team MCP config that Pulse passes to
// claude. Pulse needs the same MCP config the human chat uses, with
// two servers registered: the HTTP "teem" orchestrator (tools), and a
// stdio "teem-channel" shim that forwards channel notifications from
// the daemon's SSE endpoint into the claude subprocess. teamName +
// daemonEndpoint drive the shim's argv. shimPath is the absolute path
// to the teem-channel binary, or empty to fall back to a bare
// "teem-channel" lookup on PATH.
func WriteMCPConfig(path, mcpURL, teamName, daemonEndpoint, shimPath string) error {
	if shimPath == "" {
		shimPath = "teem-channel"
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"teem": map[string]any{
				"type": "http",
				"url":  mcpURL,
			},
			"teem-channel": map[string]any{
				"type":    "stdio",
				"command": shimPath,
				"args":    []string{"--team", teamName, "--endpoint", daemonEndpoint},
			},
		},
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicWrite(path, body)
}

func atomicWrite(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Compile-time guard that we don't drift from the audit.Sink shape we
// rely on.
var _ io.Writer = (*bytes.Buffer)(nil)
