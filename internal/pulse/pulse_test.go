package pulse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/plan"
)

// writeFakeClaude writes a shell script that imitates Claude Code's
// stream-json output enough for ParseClaudeStreamJSON to recover a
// final assistant text. Used as ClaudePath so Pulse tests don't burn
// real API tokens.
func writeFakeClaude(t *testing.T, finalText string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude shim is sh-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
cat <<JSON
{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"assistant","message":{"model":"claude-opus-4-7","content":[{"type":"text","text":` + jsonEscape(finalText) + `}],"usage":{"input_tokens":11,"output_tokens":22}}}
{"type":"result","result":` + jsonEscape(finalText) + `,"usage":{"input_tokens":11,"output_tokens":22},"total_cost_usd":0.0042}
JSON
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// jsonEscape returns a JSON-encoded string literal. Inline rather than
// pulling encoding/json into a test helper.
func jsonEscape(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, []byte(string(r))...)
		}
	}
	out = append(out, '"')
	return string(out)
}

func tempPlan(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.Open(filepath.Join(t.TempDir(), "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func tempSink(t *testing.T) *audit.FileSink {
	t.Helper()
	s, err := audit.OpenFile(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestPulse(t *testing.T, claudePath string, sessionOK bool) (*Pulse, *audit.FileSink) {
	t.Helper()
	sink := tempSink(t)
	dir := t.TempDir()
	mcpCfg := filepath.Join(dir, "pulse-mcp.json")
	if err := WriteMCPConfig(mcpCfg, "http://127.0.0.1:7777/teams/x/mcp", "x", "http://127.0.0.1:7777", ""); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		TeamName: "x",
		LoadSession: func() (string, bool, error) {
			if !sessionOK {
				return "", false, nil
			}
			return "00000000-0000-0000-0000-000000000001", true, nil
		},
		PauseFile:  filepath.Join(dir, "pulse.paused"),
		MCPConfig:  mcpCfg,
		RepoRoot:   dir,
		Plan:       tempPlan(t),
		Audit:      sink,
		Registry:   mcpsrv.NewRegistry(),
		Interval:   100 * time.Millisecond,
		ClaudePath: claudePath,
	}
	return New(cfg), sink
}

func TestPulse_TickEmitsAuditEvent(t *testing.T) {
	claudePath := writeFakeClaude(t, "I checked. Nothing to do this tick.")
	p, sink := newTestPulse(t, claudePath, true)

	if err := p.Tick(context.Background(), "timer"); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	events, err := sink.Query("leader", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Each successful tick emits two events: pulse_tick + usage_event
	// (the shared per-subprocess token rollup, see docs/usage-capture.md).
	tick := findEvent(events, "pulse_tick")
	if tick == nil {
		t.Fatalf("no pulse_tick event in: %v", events)
	}
	if tick.Message == "" {
		t.Errorf("expected assistant message in audit event")
	}
	if trig, _ := tick.Meta["trigger"].(string); trig != "timer" {
		t.Errorf("trigger meta = %v", tick.Meta["trigger"])
	}
	usageEv := findEvent(events, "usage_event")
	if usageEv == nil {
		t.Fatalf("no usage_event in: %v", events)
	}
	if trig, _ := usageEv.Meta["trigger"].(string); trig != "timer" {
		t.Errorf("usage_event trigger = %v", usageEv.Meta["trigger"])
	}
	if _, ok := usageEv.Meta["job_id"]; ok {
		t.Errorf("pulse usage_event must not carry a job_id")
	}
}

// findEvent returns a pointer to the first event matching kind, or nil.
func findEvent(events []audit.Event, kind audit.Kind) *audit.Event {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

func countKind(events []audit.Event, kind audit.Kind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestPulse_TickSkippedWhenPaused(t *testing.T) {
	claudePath := writeFakeClaude(t, "shouldn't see this")
	p, sink := newTestPulse(t, claudePath, true)

	if err := p.Pause("manual"); err != nil {
		t.Fatal(err)
	}
	if err := p.Tick(context.Background(), "timer"); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	events, _ := sink.Query("", time.Time{}, 0)
	if len(events) != 0 {
		t.Errorf("paused tick should emit no audit; got %d", len(events))
	}
	// Resume; next tick should work.
	if err := p.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := p.Tick(context.Background(), "timer"); err != nil {
		t.Fatalf("Tick after resume: %v", err)
	}
	events, _ = sink.Query("", time.Time{}, 0)
	// A successful tick now emits pulse_tick + usage_event.
	if got := countKind(events, "pulse_tick"); got != 1 {
		t.Errorf("pulse_tick events after resume: got %d, want 1", got)
	}
	if got := countKind(events, audit.KindUsageEvent); got != 1 {
		t.Errorf("usage_event events after resume: got %d, want 1", got)
	}
}

func TestPulse_TickSkippedWithoutSession(t *testing.T) {
	claudePath := writeFakeClaude(t, "shouldn't see this")
	p, sink := newTestPulse(t, claudePath, false /* sessionOK = no session yet */)

	if err := p.Tick(context.Background(), "timer"); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	events, _ := sink.Query("", time.Time{}, 0)
	if len(events) != 0 {
		t.Errorf("no-session tick should be silent; got %d events", len(events))
	}
	// TickCount still increments — guard rails count attempts.
	if p.TickCount() != 1 {
		t.Errorf("TickCount = %d, want 1", p.TickCount())
	}
}

func TestPulse_StartStop(t *testing.T) {
	claudePath := writeFakeClaude(t, "ok")
	p, sink := newTestPulse(t, claudePath, true)
	p.cfg.Interval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	if !p.Running() {
		t.Fatal("expected Running=true after Start")
	}
	time.Sleep(300 * time.Millisecond)
	p.Stop()
	if p.Running() {
		t.Fatal("expected Running=false after Stop")
	}
	events, _ := sink.Query("leader", time.Time{}, 0)
	// At least the immediate first tick should have completed. Exact
	// count depends on host timing — exercise the loop fires, not the
	// rate.
	if len(events) < 1 {
		t.Errorf("expected ≥1 tick after start/stop cycle, got %d", len(events))
	}
}

func TestPulse_ContextIncludesOpenTasks(t *testing.T) {
	// Probe-style check: build a snapshot with a task and verify it
	// shows up in the rendered context string.
	p, _ := newTestPulse(t, writeFakeClaude(t, "ok"), true)
	if _, err := p.cfg.Plan.AddTask(plan.NewTaskInput{Title: "Implement migration"}); err != nil {
		t.Fatal(err)
	}
	got := p.buildContextSnapshot("timer", time.Time{})
	if !contains(got, "Implement migration") {
		t.Errorf("context body missing task title:\n%s", got)
	}
	if !contains(got, "Trigger: timer") {
		t.Errorf("context body missing trigger:\n%s", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Belt-and-suspenders: make sure the test compiles independent of
// fmt's removal during edits.
var _ = fmt.Sprintf

func TestPulse_TickBudgetExceeded(t *testing.T) {
	p, sink := newTestPulse(t, writeFakeClaude(t, "ok"), true)
	p.cfg.MaxPerHour = 2

	// 3 sequential ticks; the 3rd should be skipped with a
	// pulse_budget_exceeded audit event instead of a normal tick.
	for i := 0; i < 3; i++ {
		_ = p.Tick(context.Background(), "timer")
	}
	events, _ := sink.Query("", time.Time{}, 0)
	var ticks, exceeded int
	for _, e := range events {
		switch string(e.Kind) {
		case "pulse_tick":
			ticks++
		case "pulse_budget_exceeded":
			exceeded++
		}
	}
	if ticks != 2 || exceeded != 1 {
		t.Errorf("got %d ticks + %d budget-exceeded, want 2 + 1", ticks, exceeded)
	}
}

func TestPulse_IdleBackoffMultipliesInterval(t *testing.T) {
	// Fake claude emits no tool_use blocks → every tick is "idle".
	p, _ := newTestPulse(t, writeFakeClaude(t, "idle reply"), true)
	p.cfg.Interval = 100 * time.Millisecond
	p.cfg.IdleBackoffAfter = 2

	for i := 0; i < 4; i++ {
		_ = p.Tick(context.Background(), "timer")
	}
	// Streak has been bumped 4×. After IdleBackoffAfter (2), the
	// effective interval should be larger than the base.
	eff := p.effectiveInterval()
	if eff <= p.cfg.Interval {
		t.Errorf("expected backed-off interval > %s, got %s", p.cfg.Interval, eff)
	}
}

func TestPulse_NudgeTriggersTick(t *testing.T) {
	p, sink := newTestPulse(t, writeFakeClaude(t, "ack"), true)
	p.cfg.Interval = 10 * time.Second // long enough that the timer doesn't fire during test
	p.cfg.DebounceWindow = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// Drain the immediate startup tick before checking nudge behavior.
	time.Sleep(200 * time.Millisecond)
	startEvents, _ := sink.Query("leader", time.Time{}, 0)
	startCount := len(startEvents)

	// Now nudge — should produce one more tick after the debounce.
	p.NudgeFromAudit([]audit.Event{
		{Kind: audit.KindJobComplete, AgentID: "worker-1", JobID: "j7"},
	})
	time.Sleep(300 * time.Millisecond)
	events, _ := sink.Query("leader", time.Time{}, 0)
	if len(events) <= startCount {
		t.Errorf("nudge did not produce a tick; events before=%d after=%d", startCount, len(events))
	}
}

func TestPulse_NudgeIgnoredIfNotRunning(t *testing.T) {
	p, _ := newTestPulse(t, writeFakeClaude(t, "ack"), true)
	// NudgeFromAudit should be a no-op when Pulse hasn't been Started.
	p.NudgeFromAudit([]audit.Event{{Kind: audit.KindJobComplete, AgentID: "w"}})
	// If it didn't panic and didn't tick (no audit), we're good. The
	// real check: TickCount didn't move.
	if p.TickCount() != 0 {
		t.Errorf("nudge while stopped should not tick; got TickCount=%d", p.TickCount())
	}
}

func TestPulse_RunningFlagFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		TeamName:    "x",
		LoadSession: func() (string, bool, error) { return "", false, nil }, // session-less; ticks no-op
		PauseFile:   filepath.Join(dir, "paused"),
		RunningFile: filepath.Join(dir, "running"),
		MCPConfig:   filepath.Join(dir, "mcp.json"),
		RepoRoot:    dir,
		Audit:       tempSink(t),
		Interval:    1 * time.Hour, // long; we just want the flag side-effects
	}
	_ = WriteMCPConfig(cfg.MCPConfig, "http://x/mcp", "x", "http://x", "")

	p := New(cfg)
	if p.WasRunning() {
		t.Fatal("WasRunning true before any Start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	if !p.WasRunning() {
		t.Error("WasRunning should be true while Pulse is running")
	}
	p.Stop()
	if p.WasRunning() {
		t.Error("WasRunning should be false after explicit Stop")
	}

	// Simulate "daemon restart": new Pulse instance against the same
	// state dir. Flag was cleared by Stop, so WasRunning is false.
	p2 := New(cfg)
	if p2.WasRunning() {
		t.Error("post-Stop, a fresh Pulse should not see WasRunning=true")
	}

	// Now Start + crash (no Stop). Flag stays. A fresh Pulse sees
	// WasRunning=true and the daemon would auto-resume.
	p2.Start(ctx)
	// Don't call Stop — simulate daemon crashing.
	// Reach in and clear running atomic so a fresh Pulse can stand up
	// without the cooperative shutdown machinery.
	p2.running.Store(false)
	if p2.cancel != nil {
		p2.cancel()
	}

	p3 := New(cfg)
	if !p3.WasRunning() {
		t.Error("after a Start without Stop (simulated crash), a fresh Pulse should see WasRunning=true")
	}
}

// TestPulse_StopForShutdown_PreservesFlag verifies that the daemon
// graceful-shutdown path leaves the running-flag on disk so the next
// `teem start` will auto-resume Pulse via WasRunning.
func TestPulse_StopForShutdown_PreservesFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		TeamName:    "x",
		LoadSession: func() (string, bool, error) { return "", false, nil },
		PauseFile:   filepath.Join(dir, "paused"),
		RunningFile: filepath.Join(dir, "running"),
		MCPConfig:   filepath.Join(dir, "mcp.json"),
		RepoRoot:    dir,
		Audit:       tempSink(t),
		Interval:    1 * time.Hour,
	}
	_ = WriteMCPConfig(cfg.MCPConfig, "http://x/mcp", "x", "http://x", "")

	p := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	if !p.WasRunning() {
		t.Fatal("flag should exist after Start")
	}

	p.StopForShutdown()
	if p.Running() {
		t.Error("Running() should be false after StopForShutdown")
	}
	if !p.WasRunning() {
		t.Error("StopForShutdown must NOT clear the running flag")
	}

	// Simulate a fresh daemon boot: a new Pulse instance against the
	// same state dir should see WasRunning=true and auto-resume.
	p2 := New(cfg)
	if !p2.WasRunning() {
		t.Error("post-StopForShutdown, a fresh Pulse must see WasRunning=true so the daemon auto-resumes")
	}
}

// TestPulse_Stop_ClearsFlag verifies that the operator-explicit Stop
// path removes the running-flag, so a daemon restart will NOT
// auto-resume Pulse (operator said "off").
func TestPulse_Stop_ClearsFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		TeamName:    "x",
		LoadSession: func() (string, bool, error) { return "", false, nil },
		PauseFile:   filepath.Join(dir, "paused"),
		RunningFile: filepath.Join(dir, "running"),
		MCPConfig:   filepath.Join(dir, "mcp.json"),
		RepoRoot:    dir,
		Audit:       tempSink(t),
		Interval:    1 * time.Hour,
	}
	_ = WriteMCPConfig(cfg.MCPConfig, "http://x/mcp", "x", "http://x", "")

	p := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	if !p.WasRunning() {
		t.Fatal("flag should exist after Start")
	}

	p.Stop()
	if p.Running() {
		t.Error("Running() should be false after Stop")
	}
	if p.WasRunning() {
		t.Error("Stop must clear the running flag (operator opt-out)")
	}
}

// TestPulse_BuildClaudeArgs_PromptNotSwallowedByChannels guards
// against a regression where `--channels server:teem-channel` (variadic)
// consumed the trailing prompt arg, leaving claude with no prompt.
// claude -p then errored on the missing prompt. Pulse failed every tick.
//
// Invariant: the trailing prompt must be preceded by a `--…` flag (or
// its single-arg value), never by a value that belongs to a variadic
// option like --channels.
func TestPulse_BuildClaudeArgs_PromptNotSwallowedByChannels(t *testing.T) {
	args := buildClaudeArgs("/tmp/mcp.json", "ctx")
	if len(args) == 0 {
		t.Fatal("empty args")
	}
	prompt := args[len(args)-1]
	if prompt != defaultWakePrompt {
		t.Errorf("last arg should be the defaultWakePrompt, got %q", prompt)
	}
	// Each tick is ephemeral — no session resumption.
	for i, a := range args {
		if a == "--resume" || a == "--session-id" {
			t.Errorf("buildClaudeArgs must not pass %q (each tick is ephemeral); args[%d:]=%v", a, i, args[i:])
		}
	}
	// Walk the args; locate --channels (if present) and assert there's
	// at least one non-channel-token --flag between it and the prompt.
	channelIdx := -1
	for i, a := range args {
		if a == "--channels" || a == "--dangerously-load-development-channels" {
			channelIdx = i
			break
		}
	}
	if channelIdx < 0 {
		return // no channels flag in this build; nothing to guard
	}
	// Find the next --flag after channelIdx+1. That flag must come
	// before the trailing prompt slot.
	foundTerminator := false
	for i := channelIdx + 2; i < len(args)-1; i++ { // skip channel token at +1; stop before prompt
		if strings.HasPrefix(args[i], "--") {
			foundTerminator = true
			break
		}
	}
	if !foundTerminator {
		t.Errorf("--channels variadic is followed only by positionals; the prompt %q will be swallowed.\nargs: %v", prompt, args)
	}
}

// TestPulse_DefaultWakePrompt_IsDirective guards against accidental
// regression to a generic "Take your next turn." prompt. The leader needs
// concrete scan instructions or it answers "Idle." with zero tool calls.
func TestPulse_DefaultWakePrompt_IsDirective(t *testing.T) {
	keywords := []string{
		"list_tasks",
		"list_agents",
		"awaiting_approval",
		"update_leader_status",
	}
	for _, kw := range keywords {
		if !strings.Contains(defaultWakePrompt, kw) {
			t.Errorf("defaultWakePrompt missing required keyword %q; prompt should direct the leader to scan concrete state", kw)
		}
	}
}

// TestPulse_TickEmitsUsageEvent verifies the t-cc22531c integration:
// every successful pulse tick emits a KindUsageEvent with the
// expected Meta shape, alongside the existing pulse_tick event.
func TestPulse_TickEmitsUsageEvent(t *testing.T) {
	claudePath := writeFakeClaude(t, "ok")
	p, sink := newTestPulse(t, claudePath, true)

	if err := p.Tick(context.Background(), "timer"); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	events, _ := sink.Query("leader", time.Time{}, 0)
	ev := findEvent(events, audit.KindUsageEvent)
	if ev == nil {
		t.Fatalf("no usage_event emitted; got %v", events)
	}
	if ev.AgentID != "leader" {
		t.Errorf("agent_id = %q", ev.AgentID)
	}
	if model, _ := ev.Meta["model"].(string); model != "claude-opus-4-7" {
		t.Errorf("model meta = %v", ev.Meta["model"])
	}
	// Numbers come back as float64 after JSON round-trip via FileSink.
	if in, _ := ev.Meta["input_tokens"].(float64); int64(in) != 11 {
		t.Errorf("input_tokens meta = %v", ev.Meta["input_tokens"])
	}
	if out, _ := ev.Meta["output_tokens"].(float64); int64(out) != 22 {
		t.Errorf("output_tokens meta = %v", ev.Meta["output_tokens"])
	}
	if trig, _ := ev.Meta["trigger"].(string); trig != "timer" {
		t.Errorf("trigger meta = %v", ev.Meta["trigger"])
	}
}

func TestPulse_IsInterestingKind(t *testing.T) {
	for _, k := range []audit.Kind{audit.KindJobComplete, audit.KindJobError, audit.KindNote, audit.KindWorkerStopped} {
		if !isInterestingKind(k) {
			t.Errorf("%q should be interesting", k)
		}
	}
	for _, k := range []audit.Kind{audit.KindHeartbeat, audit.KindJobReceived, "pulse_tick", audit.KindUsageEvent} {
		if isInterestingKind(k) {
			t.Errorf("%q should NOT be interesting (causes feedback loop)", k)
		}
	}
}

// TestPulse_NudgeSuppressedWhenChannelsLive verifies the t-50458567
// gate: when channels-live is set, NudgeFromAudit must not enqueue
// (and so must not produce a tick), but the timer/manual paths are
// unaffected. Acceptance: with channelsLive=true a job_complete event
// produces zero extra ticks beyond the initial startup tick.
func TestPulse_NudgeSuppressedWhenChannelsLive(t *testing.T) {
	p, sink := newTestPulse(t, writeFakeClaude(t, "ack"), true)
	p.cfg.Interval = 10 * time.Second
	p.cfg.DebounceWindow = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// Drain the immediate startup tick.
	time.Sleep(200 * time.Millisecond)
	before, _ := sink.Query("leader", time.Time{}, 0)

	p.SetChannelsLive(true)
	if !p.ChannelsLive() {
		t.Fatal("ChannelsLive after SetChannelsLive(true) should be true")
	}
	p.NudgeFromAudit([]audit.Event{
		{Kind: audit.KindJobComplete, AgentID: "worker-1", JobID: "j7"},
	})
	time.Sleep(300 * time.Millisecond)
	after, _ := sink.Query("leader", time.Time{}, 0)
	if len(after) != len(before) {
		t.Errorf("nudge while channels-live should be a no-op; ticks before=%d after=%d", len(before), len(after))
	}

	// Now flip channels back to fallback and re-nudge: a tick should
	// fire normally.
	p.SetChannelsLive(false)
	p.NudgeFromAudit([]audit.Event{
		{Kind: audit.KindJobComplete, AgentID: "worker-1", JobID: "j8"},
	})
	time.Sleep(300 * time.Millisecond)
	resumed, _ := sink.Query("leader", time.Time{}, 0)
	if len(resumed) <= len(after) {
		t.Errorf("post-fallback nudge did not produce a tick: before=%d after=%d", len(after), len(resumed))
	}
}

// TestPulse_DebouncerReChecksChannelsLive: if channels-live flips
// during the debounce window (chat reconnected just after the nudge),
// the deferred Tick must not fire. Guards the doc's "suppresses while
// channels are live" claim against the race where the nudge precedes
// the live transition by milliseconds.
func TestPulse_DebouncerReChecksChannelsLive(t *testing.T) {
	p, sink := newTestPulse(t, writeFakeClaude(t, "ack"), true)
	p.cfg.Interval = 10 * time.Second
	// Long debounce window so the test has time to flip the gate
	// before the timer fires.
	p.cfg.DebounceWindow = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	time.Sleep(200 * time.Millisecond)
	before, _ := sink.Query("leader", time.Time{}, 0)

	// Nudge while NOT live, then flip live before the debounce fires.
	p.NudgeFromAudit([]audit.Event{
		{Kind: audit.KindJobComplete, AgentID: "w", JobID: "j"},
	})
	p.SetChannelsLive(true)
	time.Sleep(500 * time.Millisecond)
	after, _ := sink.Query("leader", time.Time{}, 0)
	if len(after) != len(before) {
		t.Errorf("debouncer should re-check channels-live at fire time; ticks before=%d after=%d", len(before), len(after))
	}
}

// TestPulse_TimerSkippedWhenChannelsLive verifies the t-ee3df7c6 gate
// extension: once channels-live is set the timer loop must NOT call
// Tick (two writers on the leader session file is a concurrent-write
// hazard). Flipping back to fallback resumes the timer path on the
// next interval.
func TestPulse_TimerSkippedWhenChannelsLive(t *testing.T) {
	p, sink := newTestPulse(t, writeFakeClaude(t, "ack"), true)
	p.cfg.Interval = 50 * time.Millisecond
	// Start with channels-live so the immediate startup tick is also
	// suppressed; otherwise the loop's first tick fires before we get
	// a chance to flip the flag.
	p.SetChannelsLive(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// Several intervals' worth of wall-time. No tick should land.
	time.Sleep(300 * time.Millisecond)
	events, _ := sink.Query("leader", time.Time{}, 0)
	if len(events) != 0 {
		t.Fatalf("timer ticks under channels-live should be suppressed; got %d", len(events))
	}

	// Drop the gate: the next interval expiry should fire normally.
	// Poll up to ~1s to absorb scheduler / fake-claude jitter.
	p.SetChannelsLive(false)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events, _ = sink.Query("leader", time.Time{}, 0)
		if len(events) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(events) == 0 {
		t.Errorf("timer should resume once channels-live clears; got 0 ticks within 1s")
	}
}

// TestPulse_TickStillFiresWhenInvokedDirectlyUnderChannelsLive guards
// that the gating policy lives in the timer loop / daemon handler, not
// inside Tick itself. Direct programmatic Tick callers (tests, future
// callers) bypass the operator-chat gate; the gate is policy applied
// by the loop and the ping handler.
func TestPulse_TickStillFiresWhenInvokedDirectlyUnderChannelsLive(t *testing.T) {
	p, sink := newTestPulse(t, writeFakeClaude(t, "ack"), true)
	p.SetChannelsLive(true)

	if err := p.Tick(context.Background(), "timer"); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	events, _ := sink.Query("leader", time.Time{}, 0)
	// A successful direct Tick now emits pulse_tick + usage_event.
	if got := countKind(events, "pulse_tick"); got != 1 {
		t.Errorf("direct Tick should not be gated by channels-live; pulse_tick events=%d", got)
	}
}
