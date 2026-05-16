// Package pulse is the autonomous-leader heartbeat. A Pulse owns a
// goroutine that periodically (and, in later phases, in response to
// audit events) invokes `claude -p` with a freshly-composed context
// snapshot, so the leader can take turns between human chats. Each
// tick is ephemeral — pulse does not resume a saved Claude session;
// the leader's system prompt + appended leader memory + MCP tool
// access is the entire context. Status persistence flows through
// audit events / update_leader_status, not chat history.
//
// Phase 3 scope: timer-only ticks. No event triggers, no guard rails
// beyond a mutex (no overlapping ticks) and a pause file. Phase 4
// adds the CLI/control surface; phase 5 adds debounced event triggers
// and tick budget.
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
	"github.com/frasergraham/teem/internal/usage"
)

// SessionLoader reports whether the operator has run `teem chat` at
// least once for this team. Pulse uses this as an opt-in gate — until
// the operator engages, autonomous ticks stay quiet. The sessionID is
// no longer passed to claude (ticks are ephemeral) but is recorded in
// audit meta for traceability. Implementation lives in cmd/teem
// (loadLeaderSession); we accept a function value to avoid an import
// cycle.
type SessionLoader func() (sessionID string, ok bool, err error)

// Config bundles everything one team's Pulse needs.
type Config struct {
	// TeamName is for display / log lines only. TeamID is the canonical
	// id used for state-dir paths and (since T33) for the teem-channel
	// shim's --team flag.
	TeamName    string
	TeamID      string
	LoadSession SessionLoader // returns the session id or (false) if no chat has run yet
	PauseFile   string        // path; presence skips ticks
	RunningFile string        // path; presence signals daemon to auto-resume on restart
	// WakePromptFile, when non-empty, is the per-team operator
	// override for the wake-prompt text passed to claude on every
	// tick. Pulse reads it on construction; non-empty contents wins,
	// otherwise defaultWakePrompt is used. SetWakePrompt rewrites it
	// so daemon restarts pick up overrides.
	WakePromptFile string
	MCPConfig      string // path to ~/.teem/state/<team>/pulse-mcp.json
	RepoRoot       string // CWD for the claude subprocess
	Plan           *plan.Plan
	Audit          audit.Sink
	Registry       *mcpsrv.Registry
	Interval       time.Duration // timer cadence; default 5m
	BodyCap        int           // truncation cap for assistant text in audit; default 64 KiB
	ClaudePath     string        // optional override (default: exec.LookPath("claude"))
	TickTimeout    time.Duration // per-tick deadline; default 5m
	// MaxPerHour caps autonomous ticks; default 30. Exceeded ticks
	// are skipped and counted but emit no claude invocation. A pause
	// flag and stop() are the override knobs.
	MaxPerHour int
	// DebounceWindow groups audit nudges into one tick. Default 500ms.
	DebounceWindow time.Duration
	// IdleBackoffAfter is how many no-tool-call ticks in a row before
	// the effective interval doubles. Default 3.
	IdleBackoffAfter int
	// OnUsage, when non-nil, is invoked with the per-tick usage rollup
	// after pulse has written the KindUsageEvent audit event. The
	// daemon wires this to the global usage Aggregator so pulse ticks
	// contribute to the daily budget (HTTP audit hooks don't fire for
	// pulse — it writes the sink directly).
	OnUsage func(usage.UsageSummary)
	// OnChannelNudge, when non-nil, is invoked in place of starting a
	// fresh claude subprocess on ticks that fire while channels-live is
	// true. The daemon wires it to a channelbus.Publish of a pulse_tick
	// channel block so the operator's running chat session takes the
	// turn instead. nil = no channel route — pulse falls back to the
	// pre-t-d753f950 behavior of skipping when channels are live.
	OnChannelNudge func(ctx context.Context)
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
	// Protected by budgetMu so the nudger and the timer loop don't
	// race on the slice.
	budgetMu  sync.Mutex
	tickTimes []time.Time

	// Idle backoff: tracks consecutive no-tool-call ticks. Doubles
	// the effective interval after IdleBackoffAfter; reset on the
	// next tick that calls a tool.
	idleStreak atomic.Int32

	// Event-trigger debouncer: a single goroutine consumes nudges and
	// fires Tick after DebounceWindow of quiet. Buffered so callers
	// never block.
	nudgeCh chan string

	// channelsLive gates ALL pulse wake paths (timer, audit-nudge,
	// manual ping). When true, an operator chat session is active:
	// Claude Code is delivering wake events to the leader directly,
	// and the operator's TUI is already writing the leader session
	// file. Two writers on that file is a concurrent-write hazard, so
	// pulse stays out of the way while the chat is live. The timer
	// loop, debouncer and NudgeFromAudit each consult this flag; the
	// daemon's /control/teams/<id>/ping handler refuses manual pings
	// the same way. On disconnect the flag clears and pulse resumes
	// as the headless fallback. See docs/wake-strategy.md §5.
	channelsLive atomic.Bool

	// wakePrompt is the active first-turn message handed to claude on
	// each tick. Initialized from WakePromptFile in New (or
	// defaultWakePrompt when the file is missing/empty); SetWakePrompt
	// swaps it atomically and persists to disk. Stored as a plain
	// string via atomic.Pointer-style atomic.Value so concurrent
	// invokeClaude callers see a consistent snapshot per tick.
	wakePrompt atomic.Value // string

	// intervalNs holds the current cadence in nanoseconds. Stored
	// atomically so the timer loop's effectiveInterval read doesn't
	// race the dashboard's SetInterval / UpdateConfig writes.
	intervalNs atomic.Int64
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
	if cfg.DebounceWindow == 0 {
		cfg.DebounceWindow = 500 * time.Millisecond
	}
	if cfg.IdleBackoffAfter == 0 {
		cfg.IdleBackoffAfter = 3
	}
	p := &Pulse{cfg: cfg, nudgeCh: make(chan string, 32)}
	p.wakePrompt.Store(loadWakePromptFile(cfg.WakePromptFile))
	p.intervalNs.Store(int64(cfg.Interval))
	return p
}

// loadWakePromptFile returns the file's trimmed contents when present
// and non-empty; otherwise returns defaultWakePrompt. A missing file or
// any read error counts as "use the default" — the override file is
// strictly opt-in.
func loadWakePromptFile(path string) string {
	if path == "" {
		return defaultWakePrompt
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return defaultWakePrompt
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return defaultWakePrompt
	}
	return trimmed
}

// WakePrompt returns the message currently passed to claude as the
// leader's first turn on each tick. Used by the dashboard's pulse
// panel to render the textarea pre-filled with the active value.
func (p *Pulse) WakePrompt() string {
	v, _ := p.wakePrompt.Load().(string)
	if v == "" {
		return defaultWakePrompt
	}
	return v
}

// IsCustomWakePrompt reports whether the active prompt differs from
// the built-in default — a UX hint for the dashboard ("custom" vs
// "using default"). Cheap; no I/O.
func (p *Pulse) IsCustomWakePrompt() bool {
	return p.WakePrompt() != defaultWakePrompt
}

// DefaultWakePrompt exposes the built-in default so callers (the
// dashboard panel, tests) can render it as a placeholder when the
// operator hasn't supplied an override.
func DefaultWakePrompt() string { return defaultWakePrompt }

// SetWakePrompt swaps the active wake prompt and persists the change
// so a daemon restart picks it up. An empty (whitespace-only) value
// clears the override and returns to defaultWakePrompt — the file is
// removed from disk in that case so a fresh New() also reads the
// default. Safe to call while pulse is running; the next tick's
// invokeClaude call observes the new value.
func (p *Pulse) SetWakePrompt(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		p.wakePrompt.Store(defaultWakePrompt)
		if p.cfg.WakePromptFile == "" {
			return nil
		}
		err := os.Remove(p.cfg.WakePromptFile)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	p.wakePrompt.Store(trimmed)
	if p.cfg.WakePromptFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.WakePromptFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.cfg.WakePromptFile, []byte(trimmed+"\n"), 0o600)
}

// Start kicks off the periodic loop AND the audit-event debouncer.
// Idempotent — calling Start on an already-running Pulse is a no-op.
// The loops exit when Stop is called or the parent context is
// cancelled. Writes a "running" flag file so a daemon restart can
// auto-resume Pulse without operator intervention.
func (p *Pulse) Start(parent context.Context) {
	if !p.running.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	_ = p.writeRunningFlag()
	go p.run(ctx)
	go p.runDebouncer(ctx)
}

// Stop halts the loop AND clears the on-disk running-flag, so a
// subsequent daemon restart will NOT auto-resume Pulse. Use this for
// operator-explicit stops (`teem pulse stop`, team removal) — anywhere
// the intent is "don't pulse this team again until told to." Safe to
// call multiple times; safe to call before Start.
//
// For daemon graceful shutdown, use StopForShutdown instead so the
// flag survives the bounce and the next `teem start` resumes Pulse.
func (p *Pulse) Stop() {
	if !p.stopLoop() {
		return
	}
	_ = p.clearRunningFlag()
}

// StopForShutdown halts the loop but preserves the on-disk running
// flag, so the next daemon startup auto-resumes Pulse via WasRunning.
// Use only from the daemon's graceful-shutdown path — `teem stop`
// should not look like an operator opt-out.
func (p *Pulse) StopForShutdown() { p.stopLoop() }

// stopLoop performs the actual loop-cancel. Returns true if Pulse was
// running (so flag-touching callers know they have meaningful work to
// do), false on a no-op call against an already-stopped Pulse.
func (p *Pulse) stopLoop() bool {
	if !p.running.CompareAndSwap(true, false) {
		return false
	}
	if p.cancel != nil {
		p.cancel()
	}
	return true
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
func (p *Pulse) Interval() time.Duration { return time.Duration(p.intervalNs.Load()) }

// Busy peeks at the tick mutex and reports whether a tick is currently
// executing. Useful for HTTP handlers that want a quick "already in
// flight" response without blocking on a full tick.
//
// There is an inherent race: Busy may return false and a tick may
// start before the caller acts on the result. Callers should treat
// the answer as a UX hint, not a hard guarantee — the mutex inside
// Tick is what actually serializes execution.
func (p *Pulse) Busy() bool {
	if !p.mu.TryLock() {
		return true
	}
	p.mu.Unlock()
	return false
}

// SetInterval changes the cadence. If Pulse is already running, the
// change takes effect on the next tick wakeup.
func (p *Pulse) SetInterval(d time.Duration) {
	if d > 0 {
		p.intervalNs.Store(int64(d))
	}
}

// UpdateConfig changes the cadence and/or wake prompt on a running
// pulse without bouncing. Either argument may be left at its zero
// value to leave the corresponding field unchanged: a zero interval
// keeps the existing cadence; a nil wakePrompt pointer keeps the
// existing prompt. Returns the wake-prompt persistence error (if any)
// — the interval change is in-memory only and cannot fail.
func (p *Pulse) UpdateConfig(interval time.Duration, wakePrompt *string) error {
	if interval > 0 {
		p.intervalNs.Store(int64(interval))
	}
	if wakePrompt == nil {
		return nil
	}
	return p.SetWakePrompt(*wakePrompt)
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
	p.fireTick(ctx, "timer")
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
		p.fireTick(ctx, "timer")
	}
}

// fireTick is the timer-loop / debouncer entry that picks the right
// wake path for a tick. When channels are live and OnChannelNudge is
// wired, the tick is routed as a channel block into the operator's
// running chat session (no new claude subprocess). When channels are
// live but no callback is configured, we fall back to the original
// skip-when-live behavior so two writers can't race on the leader
// session file. Otherwise the normal session-spawning Tick runs.
func (p *Pulse) fireTick(ctx context.Context, trigger string) {
	if p.channelsLive.Load() {
		if p.cfg.OnChannelNudge == nil {
			return
		}
		if err := p.tickViaChannel(ctx, trigger); err != nil && !errors.Is(err, context.Canceled) {
			p.logErr(err)
		}
		return
	}
	if err := p.Tick(ctx, trigger); err != nil && !errors.Is(err, context.Canceled) {
		p.logErr(err)
	}
}

// effectiveInterval doubles the configured interval once the idle
// streak crosses IdleBackoffAfter, capped at 8× so we never go fully
// silent (still want a periodic check-in).
func (p *Pulse) effectiveInterval() time.Duration {
	base := p.Interval()
	streak := int(p.idleStreak.Load())
	if streak <= p.cfg.IdleBackoffAfter {
		return base
	}
	mult := 1 << ((streak - p.cfg.IdleBackoffAfter) - 1)
	if mult > 8 {
		mult = 8
	}
	return base * time.Duration(mult)
}

// runDebouncer collects nudges (audit-event triggers) and fires one
// Tick per debounce window. Multiple nudges inside the window
// coalesce — important when a worker emits a flurry of events on a
// fast job.
func (p *Pulse) runDebouncer(ctx context.Context) {
	var (
		pending bool
		reason  string
		timer   *time.Timer
	)
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-p.nudgeCh:
			if !pending {
				pending = true
				reason = r
				timer = time.NewTimer(p.cfg.DebounceWindow)
			} else {
				// Already armed — reset window so a busy burst
				// produces exactly one tick after the burst ends.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(p.cfg.DebounceWindow)
			}
		case <-debounceFire(timer):
			pending = false
			r := reason
			reason = ""
			timer = nil
			// Re-check the channels-live gate at fire time, not just
			// at nudge time: a chat may have connected during the
			// debounce window, in which case fireTick routes via the
			// channel callback (or, when no callback is configured,
			// falls back to the original skip-when-live behavior).
			p.fireTick(ctx, "event:"+r)
		}
	}
}

// debounceFire returns the timer's channel when armed, or nil (which
// blocks forever in a select). Avoids a ranges-over-nil-channel
// special case.
func debounceFire(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// SetChannelsLive flips the pulse-wide gate. When true the daemon has
// observed at least one teem-channel SSE subscriber (an operator chat
// session is active), so the timer loop skips, NudgeFromAudit is a
// no-op, and the daemon refuses manual pings — all three to avoid
// racing the chat's writer on the leader session file. Callers: the
// daemon's channel-events SSE handler on subscribe/unsubscribe
// transitions.
func (p *Pulse) SetChannelsLive(live bool) { p.channelsLive.Store(live) }

// ChannelsLive reports the current gate state. Exposed for status
// rendering and tests.
func (p *Pulse) ChannelsLive() bool { return p.channelsLive.Load() }

// NudgeFromAudit is the entry point the daemon's audit-handler wrapper
// calls every time workers POST events. Pulse inspects the events for
// "interesting" kinds (job lifecycle, errors) and, if Pulse is
// running, schedules a debounced tick.
//
// When channels-live is set, this path is a no-op — the leader is
// receiving the same events via the Claude Code channel block and
// firing pulse here would race the chat's session writer. The timer
// loop and the daemon's manual-ping handler suppress in the same
// situation (see docs/wake-strategy.md).
func (p *Pulse) NudgeFromAudit(events []audit.Event) {
	if !p.Running() {
		return
	}
	if p.channelsLive.Load() {
		return
	}
	for _, e := range events {
		if !isInterestingKind(e.Kind) {
			continue
		}
		reason := string(e.Kind)
		if e.AgentID != "" {
			reason = string(e.Kind) + "@" + e.AgentID
		}
		select {
		case p.nudgeCh <- reason:
		default:
			// Channel full — already enough nudges queued; drop.
		}
		return // one nudge per batch is enough
	}
}

// isInterestingKind decides whether an audit event should wake the
// leader. Lifecycle and error signals matter; heartbeats and
// pulse_tick echoes don't (we'd loop on our own audit writes).
//
// KindUsageEvent is deliberately excluded: every pulse tick now emits
// one of those, and waking on it would loop forever (tick → usage →
// nudge → tick → …). See docs/usage-capture.md §10.
func isInterestingKind(k audit.Kind) bool {
	switch k {
	case audit.KindJobComplete, audit.KindJobError, audit.KindNote, audit.KindWorkerStopped:
		return true
	}
	return false
}

// Tick performs a single autonomous turn. Idempotent under concurrent
// callers (mutex). Returns nil even when paused — pause is "skip
// quietly," not "error."
//
// Phase 3 keeps trigger as a free-form string ("timer"); phase 5 will
// use it to record what woke us up (e.g. "event:job_complete").
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
	result, err := p.invokeClaude(tickCtx, contextBody)
	dur := time.Since(start)

	ev := audit.Event{
		Timestamp: now,
		AgentID:   "leader",
		Kind:      audit.KindPulseTick,
		Meta: map[string]any{
			"trigger":         trigger,
			"duration_ms":     int(dur.Milliseconds()),
			"tool_calls":      result.toolCalls,
			"chat_session_id": sessionID,
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

	// Emit the per-tick usage rollup as its own audit event so the
	// usage-monitor throttle and the token-cost attribution code can
	// read one canonical shape across pulse + worker subprocesses
	// (see docs/usage-capture.md). One event per tick, never per
	// assistant turn. Pulse ticks carry no job_id; trigger is
	// surfaced in Meta for monitor consumers.
	usageMeta := usage.AuditMeta(result.usage, "leader", "")
	usageMeta["trigger"] = trigger
	_ = p.cfg.Audit.Write(audit.Event{
		Timestamp: now,
		AgentID:   "leader",
		Kind:      audit.KindUsageEvent,
		Meta:      usageMeta,
	})
	if p.cfg.OnUsage != nil {
		p.cfg.OnUsage(result.usage)
	}

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

// tickViaChannel records a pulse_tick audit event and fires the
// OnChannelNudge callback to deliver the tick as a channel block in
// the operator's running chat session, instead of starting a fresh
// claude subprocess. Used when channels-live is set and a nudge
// callback is wired (see fireTick). Honors pause / budget / tickN
// bookkeeping the same way Tick does so the audit + messaging path
// (t-9a89f05e) sees the same single pulse_tick event per tick.
func (p *Pulse) tickViaChannel(ctx context.Context, trigger string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tickN.Add(1)
	now := time.Now().UTC()
	p.lastTick.Store(now)

	if p.Paused() {
		return nil
	}
	if !p.consumeBudget(now) {
		_ = p.cfg.Audit.Write(audit.Event{
			Timestamp: now,
			AgentID:   "leader",
			Kind:      audit.Kind("pulse_budget_exceeded"),
			Message:   fmt.Sprintf("over %d ticks/hour", p.cfg.MaxPerHour),
			Meta:      map[string]any{"trigger": trigger, "route": "channel"},
		})
		return nil
	}

	_ = p.cfg.Audit.Write(audit.Event{
		Timestamp: now,
		AgentID:   "leader",
		Kind:      audit.Kind("pulse_tick"),
		Message:   "routed as channel nudge",
		Meta: map[string]any{
			"trigger": trigger,
			"route":   "channel",
		},
	})
	p.cfg.OnChannelNudge(ctx)
	return nil
}

// tickResult captures everything Pulse needs from a claude invocation
// to log + decide what to do next.
type tickResult struct {
	text      string
	toolCalls int
	usage     usage.UsageSummary
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
			since = time.Now().Add(-p.Interval())
		}
		events, err := p.cfg.Audit.Query("", since, 30)
		if err == nil && len(events) > 0 {
			b.WriteString("Recent activity:\n")
			for _, e := range events {
				if e.Kind == audit.KindPulseTick {
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

// invokeClaude runs `claude -p` with the supplied context appended to
// the system prompt and a short user-side prompt. Each invocation is
// ephemeral — no --resume; the leader's persona comes entirely from
// the system prompt + leader memory + MCP tool access. Returns the
// final assistant text plus how many tool_use blocks the model
// emitted (used by the idle-backoff logic).
func (p *Pulse) invokeClaude(ctx context.Context, contextBody string) (tickResult, error) {
	claudePath := p.cfg.ClaudePath
	if claudePath == "" {
		path, err := exec.LookPath("claude")
		if err != nil {
			return tickResult{}, fmt.Errorf("claude CLI not on PATH: %w", err)
		}
		claudePath = path
	}
	args := buildClaudeArgs(p.cfg.MCPConfig, contextBody, p.WakePrompt())
	cmd := exec.CommandContext(ctx, claudePath, args...)
	cmd.Dir = p.cfg.RepoRoot
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return tickResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	startedAt := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		return tickResult{}, fmt.Errorf("start claude: %w", err)
	}
	cap := usage.NewCapture(startedAt)
	res, parseErr := parseTickStream(stdout, cap)
	res.usage = cap.Summary()
	if err := cmd.Wait(); err != nil {
		return res, fmt.Errorf("claude exit: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if parseErr != nil {
		return res, parseErr
	}
	return res, nil
}

// defaultWakePrompt is the message pulse passes as the leader's first turn.
// It must direct the leader to actively SCAN current state — not assume
// nothing changed since the last tick. Lazy "Idle." replies happen when the
// prompt is generic; an active scan picks up operator approvals, stuck
// workers, and decision queues that arrived between ticks.
const defaultWakePrompt = `You're being woken on a pulse tick. Two things are non-negotiable this turn:

1. ACTIVELY SCAN state. Do not assume the previous turn's "idle" still holds.
2. CALL update_leader_status before ending your turn — every tick, even an idle one. The status line is the canonical "what's the leader doing"; a stale status erodes operator trust.

Scan in this order:
- list_tasks(open_only=true) — tasks that transitioned out of awaiting_approval (operator approvals append [APPROVED …] to notes) need implementation work dispatched or sub-tasks filed.
- list_agents — stuck workers (state=busy with stale last_seen) or unexpectedly-stopped agents need reincarnation or escalation.
- query_audit — recent operator decisions, blockers, unusual events.

Then take the next action: dispatch waiting work, reincarnate stuck workers, escalate decisions you can't make alone, or fast-forward main if an integrator branch is ready. Only conclude "idle" AFTER an actual scan shows nothing actionable.

Required before ending your turn (no exceptions): call update_leader_status with one or two sentences in the human-readable style (Coder/Reviewer/Integrator/PM personas, natural prose, no bare task IDs in the prose). Cover what's in flight, what just landed, and what's next. If genuinely idle, say so explicitly and name what you scanned.`

// buildClaudeArgs assembles the argv passed to `claude` for one tick.
//
// Ticks are ephemeral: no --resume / --session-id. Each invocation
// starts fresh; the leader's persona + memory ride in via
// --append-system-prompt.
//
// Arg order matters: `--channels <channels...>` is variadic and will
// swallow any positional argument that follows it (including the
// trailing prompt). ChannelFlags are placed BEFORE another flag so the
// next `--…` token terminates the variadic, leaving the prompt at the
// end intact.
func buildClaudeArgs(mcpConfig, contextBody, wakePrompt string) []string {
	if wakePrompt == "" {
		wakePrompt = defaultWakePrompt
	}
	return assembleClaudeArgs(mcpConfig, contextBody, wakePrompt)
}

// BuildChatArgs is the exported chat-panel sibling of buildClaudeArgs:
// the dashboard's POST /control/teams/<id>/chat handler calls it to
// build the leader subprocess argv with the operator's message as the
// final prompt instead of the autonomous wake prompt. Same MCP config,
// same channel-flag ordering, same --append-system-prompt context body
// — only the trailing prompt changes.
func BuildChatArgs(mcpConfig, contextBody, userMessage string) []string {
	return assembleClaudeArgs(mcpConfig, contextBody, userMessage)
}

func assembleClaudeArgs(mcpConfig, contextBody, prompt string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--mcp-config", mcpConfig,
	}
	args = append(args, claudeflags.ChannelFlags()...)
	args = append(args,
		"--append-system-prompt", contextBody,
		"--dangerously-skip-permissions",
	)
	args = append(args, prompt)
	return args
}

// parseTickStream consumes Claude Code's stream-json and returns the
// final assistant text plus a count of tool_use content blocks across
// every assistant message. We need our own parser instead of using
// executor.ParseClaudeStreamJSON because we care about the
// (intermediate) tool-call shape that the executor variant
// intentionally ignores.
//
// Each line is also fed through the supplied usage.Capture so the
// shared usage-extraction code in internal/usage stays the single
// source of truth for stream-json schema decisions (see
// docs/usage-capture.md). cap may be nil for tests that don't care.
func parseTickStream(r io.Reader, cap *usage.Capture) (tickResult, error) {
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
	var res tickResult
	for sc.Scan() {
		line := sc.Bytes()
		if cap != nil {
			cap.Feed(line)
		}
		var e ev
		if err := json.Unmarshal(line, &e); err != nil {
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
// the daemon's SSE endpoint into the claude subprocess. teamID +
// daemonEndpoint drive the shim's argv (the shim's --team value
// becomes the URL path segment for the SSE endpoint). shimPath is the
// absolute path to the teem-channel binary, or empty to fall back to
// a bare "teem-channel" lookup on PATH.
func WriteMCPConfig(path, mcpURL, teamID, daemonEndpoint, shimPath string) error {
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
				"args":    []string{"--team", teamID, "--endpoint", daemonEndpoint},
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
