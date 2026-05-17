package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/channelbus"
	"github.com/frasergraham/teem/internal/usage"
)

// shortSummary clamps an output string to a single-line preview safe
// for the recent-entries section. Newlines are flattened; the result
// is truncated to 200 bytes.
func shortSummary(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	const cap = 200
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}

// isWakeKind decides whether a worker event should fire a leader.wake
// publish. Different consumers (a future chat banner) may want
// different signals than channels uses.
func isWakeKind(k audit.Kind) bool {
	switch k {
	case audit.KindJobComplete, audit.KindJobError, audit.KindJobTranscriptReady, audit.KindWorkerStopped:
		return true
	}
	return false
}

// publishPulseChannelNudge fans a pulse_tick channel block out to the
// team's channelbus. Used both by Pulse.OnChannelNudge (timer-driven)
// and handlePingTeam (operator-driven) when an operator chat session
// is active: the running chat picks up the channel block and the
// leader takes a turn without paying for a fresh claude subprocess.
// Safe to call with a nil bus.
func publishPulseChannelNudge(bus *channelbus.Bus) {
	if bus == nil {
		return
	}
	bus.Publish(channelbus.Event{
		Content: "Pulse tick. Take a turn — leader status update needed.",
		Meta: map[string]string{
			"kind":   "pulse_tick",
			"source": "teem",
		},
	})
}

// channelPushFn is the narrow surface makeChannelHook calls into. The
// production binding is mcpsrv.Server.PushChannel; tests substitute a
// recorder.
type channelPushFn func(body string, meta map[string]string)

// makeChannelHook returns the auditHook that fans selected events out
// to the team MCP server as Claude Code channel notifications. Pulled
// out of buildTeamServices so it can be unit-tested without the rest
// of the per-team plumbing.
func makeChannelHook(push channelPushFn) auditHook {
	return func(events []audit.Event) {
		for _, e := range events {
			if !isChannelKind(e.Kind) {
				continue
			}
			body := formatChannelBody(e)
			meta := map[string]string{
				"agent_id": e.AgentID,
				"kind":     string(e.Kind),
			}
			if e.JobID != "" {
				meta["job_id"] = e.JobID
			}
			if tid, ok := e.Meta["task_id"].(string); ok && tid != "" {
				meta["task_id"] = tid
			}
			push(body, meta)
		}
	}
}

// isChannelKind decides whether an audit event should be pushed into
// the leader's claude channel. The set mirrors the leader-relevant
// signals the dashboard surfaces — terminal job state, blockers,
// recorded decisions, worker shutdown, daemon-killed jobs, and
// pipeline-stage movement — and intentionally excludes high-frequency
// noise (heartbeats, pulse_tick echoes).
func isChannelKind(k audit.Kind) bool {
	switch k {
	case audit.KindJobComplete,
		audit.KindJobError,
		audit.KindJobInterrupted,
		audit.KindBlockerNote,
		audit.KindDecisionNote,
		audit.KindWorkerStopped,
		audit.KindTaskStageChanged:
		return true
	}
	return false
}

// formatChannelBody renders a short, human-readable one-liner for an
// audit event suitable for surfacing inside the leader's claude
// session. Body intentionally stays terse: full detail lives in the
// audit log + query_audit tool, and the channel exists to nudge the
// leader to look.
func formatChannelBody(e audit.Event) string {
	agent := e.AgentID
	if agent == "" {
		agent = "<unknown>"
	}
	taskID := ""
	if tid, ok := e.Meta["task_id"].(string); ok {
		taskID = tid
	}
	switch e.Kind {
	case audit.KindJobComplete:
		return fmt.Sprintf("%s finished job %s", agent, e.JobID)
	case audit.KindJobError:
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = "(no message)"
		}
		return fmt.Sprintf("%s job %s errored: %s", agent, e.JobID, shortSummary(msg))
	case audit.KindJobInterrupted:
		return fmt.Sprintf("%s's job %s was interrupted", agent, e.JobID)
	case audit.KindWorkerStopped:
		return fmt.Sprintf("%s stopped", agent)
	case audit.KindBlockerNote:
		if taskID != "" {
			return fmt.Sprintf("blocker on task %s: %s", taskID, shortSummary(e.Message))
		}
		return "blocker: " + shortSummary(e.Message)
	case audit.KindDecisionNote:
		if taskID != "" {
			return fmt.Sprintf("decision on task %s: %s", taskID, shortSummary(e.Message))
		}
		return "decision: " + shortSummary(e.Message)
	case audit.KindTaskStageChanged:
		if taskID == "" {
			return shortSummary(e.Message)
		}
		stage, _ := e.Meta["stage"].(string)
		if stage == "" {
			stage, _ = e.Meta["to"].(string)
		}
		from, _ := e.Meta["from"].(string)
		switch {
		case from != "" && stage != "":
			return fmt.Sprintf("task %s: %s → %s", taskID, from, stage)
		case stage != "":
			return fmt.Sprintf("task %s moved to %s", taskID, stage)
		default:
			return "task " + taskID + " stage changed"
		}
	}
	return fmt.Sprintf("%s: %s", e.Kind, shortSummary(e.Message))
}

// spawnerQuota returns the daemon's Aggregator as an agent.QuotaChecker
// — or a nil interface when usage is unwired, so the spawner's
// `cfg.UsageQuota != nil` check disables the gate cleanly. Returning
// the typed nil directly would be a non-nil interface value and crash
// the gate check.
func (d *daemon) spawnerQuota() agent.QuotaChecker {
	if d.usageAgg == nil {
		return nil
	}
	return d.usageAgg
}

// makeUsageHook returns the auditHook that feeds the daemon-global
// usage Aggregator. KindUsageEvent events are decoded back into a
// UsageSummary and recorded; everything else passes through.
func (d *daemon) makeUsageHook() auditHook {
	if d.usageAgg == nil {
		return nil
	}
	return func(events []audit.Event) {
		for _, e := range events {
			if e.Kind != audit.KindUsageEvent {
				continue
			}
			d.usageAgg.Record(usageSummaryFromMeta(e))
		}
	}
}

// usageSummaryFromMeta reconstructs a usage.UsageSummary from a
// KindUsageEvent's Meta bag. The Meta wire shape comes from
// usage.AuditMeta; missing/typed-incorrectly fields fall back to
// zero so a malformed event is just under-counted, not fatal.
func usageSummaryFromMeta(e audit.Event) usage.UsageSummary {
	m := e.Meta
	s := usage.UsageSummary{
		Model:             metaString(m, "model"),
		InputTokens:       metaInt64(m, "input_tokens"),
		OutputTokens:      metaInt64(m, "output_tokens"),
		CacheCreateTokens: metaInt64(m, "cache_create_tokens"),
		CacheReadTokens:   metaInt64(m, "cache_read_tokens"),
	}
	if t, err := time.Parse(time.RFC3339, metaString(m, "started_at")); err == nil {
		s.StartedAt = t
	}
	if t, err := time.Parse(time.RFC3339, metaString(m, "ended_at")); err == nil {
		s.EndedAt = t
	}
	if s.EndedAt.IsZero() {
		s.EndedAt = e.Timestamp
	}
	return s
}

func metaString(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// metaInt64 tolerates JSON's float64 round-trip plus the int64 type the
// in-process emitter uses directly.
func metaInt64(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// onUsageThrottle is the Aggregator's transition callback. Each
// active↔throttled flip is fanned out to every team's audit sink so
// each leader can react locally. Best-effort: failures are logged but
// don't propagate.
func (d *daemon) onUsageThrottle(ev usage.ThrottleEvent) {
	meta := map[string]any{
		"state":  ev.State,
		"used":   ev.Used,
		"cap":    ev.Cap,
		"reason": ev.Reason,
	}
	d.mu.Lock()
	teams := make([]*registeredTeam, 0, len(d.teams))
	for _, rt := range d.teams {
		teams = append(teams, rt)
	}
	d.mu.Unlock()
	for _, rt := range teams {
		if rt.auditSink == nil {
			continue
		}
		if err := rt.auditSink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "leader",
			Kind:      audit.KindUsageThrottle,
			Meta:      meta,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] usage: throttle audit write %s: %v\n", rt.team.Name, err)
		}
	}
}

// combineHooks chains any number of audit hooks left-to-right. nil
// entries are skipped; returns nil only if every input is nil.
func combineHooks(hooks ...auditHook) auditHook {
	// hooks[:0] reuses the variadic backing array; safe today because no
	// caller passes a slice with `...`. If that changes, copy into a fresh
	// slice instead — silent mutation of a caller's slice is a footgun.
	live := hooks[:0]
	for _, h := range hooks {
		if h != nil {
			live = append(live, h)
		}
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0]
	}
	chained := make([]auditHook, len(live))
	copy(chained, live)
	return func(events []audit.Event) {
		for _, h := range chained {
			h(events)
		}
	}
}

// auditHook is a side-channel callback invoked on every accepted
// audit POST. Used by Pulse to schedule debounced event-triggered
// ticks. nil is fine — the handler just skips the call.
type auditHook func(events []audit.Event)
