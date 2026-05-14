# Leader Wake Strategy and Channels-Fallback Design

Status: design (proposed). Tracks task `t-b06d74ba`. Unblocks `t-50458567` (pulse/channels consolidation) and is referenced by `t-63fb0676` (dashboard manual-ping button).

## 1. Context

The Teem leader is a long-running `claude` session that takes turns when *something* tells it to. The "something" is a wake signal. Wake signals exist on a spectrum from event-precise (a worker just finished, wake now) to coarse (5-minute heartbeat).

Today the most event-precise wake path is **Claude Code channels**: the daemon publishes events on `channelbus`, a stdio shim (`cmd/teem-channel/main.go`) advertises `experimental.claude/channel` capability to Claude Code, and Claude Code delivers a `<channel>` block into the leader's running session. End-to-end latency is ~hundreds of milliseconds.

**Channels are experimental.** They are gated behind `--dangerously-load-development-channels` and an allow-list. They may be removed, may not work for every operator, and the API may shift. Every part of Teem that relies on channels needs a documented fallback, because:

- Operators outside the allow-list run without the flag → channel notifications are silently dropped by Claude Code.
- A future Claude Code release may remove the capability entirely.
- The `teem-channel` stdio shim can crash or stop consuming SSE.
- The leader's chat TUI may not surface the channel block fast enough (regression mode previously seen as `t-60fcb4f5`).

This document specifies the wake-path layers, how the daemon detects whether channels are live, what happens on transitions, and the decisions that fall out of the above.

## 2. Wake paths in Teem today

| Layer | Path | Latency | Needs operator? | Needs channels? |
|-------|------|---------|-----------------|-----------------|
| Channels | `channelbus.Publish` → `teem-channel` shim → Claude Code `<channel>` block → leader session | ~hundreds of ms | Yes (chat TUI connected) | Yes |
| Pulse audit-nudge | Audit event → `Pulse.NudgeFromAudit` → `DebounceWindow` (default 500ms) → `Pulse.Tick(ctx, "event:<kind>@<agent>")` → `claude -p --resume` subprocess | ~500ms + claude cold start | No | No |
| Pulse timer | `Pulse.run` → `effectiveInterval` (default 5m, idle backoff doubles it) → `Pulse.Tick(ctx, "timer")` | up to `Interval` | No | No |
| Manual ping | Operator clicks dashboard button / runs CLI → POST to daemon → `Pulse.Tick(ctx, "manual:<who>")` | sub-second | Yes (the operator) | No |

Audit-nudge "interesting" kinds today (`isInterestingKind` in `internal/pulse/pulse.go:358-364`): `KindJobComplete`, `KindJobError`, `KindNote`. **`KindWorkerStopped` is defined (`internal/audit/audit.go:59`) but NOT in this set** — a worker exiting cleanly does not currently wake the leader off-cycle. This is a real gap: roster cleanup and "decide next steps" want this signal. **Implementation note for `t-50458567`**: add `audit.KindWorkerStopped` to the `isInterestingKind` switch. One line, same file, same package.

Source references: `internal/pulse/pulse.go` (timer, debouncer, `Tick`, `NudgeFromAudit`, `isInterestingKind`, `MaxPerHour`, `DebounceWindow`), `internal/channelbus/channelbus.go` (publish/subscribe bus, `Bus.Len()`), `cmd/teem-channel/main.go` (stdio shim that subscribes to `/teams/<team>/channel-events` SSE and re-emits as `experimental.claude/channel` notifications to Claude Code).

## 3. Fallback layers in priority order

The strategy is **defence in depth**: each layer is independently sufficient to make forward progress. Higher-priority layers are more event-precise; lower-priority layers are the ones that survive when everything else has failed.

### L1 — Pulse timer (baseline floor, always on)

- `Pulse.run` ticks every `Interval` (default 5m, doubled by idle backoff).
- Independent of channels, operator presence, audit events, MCP capability, network.
- Cost: one `claude -p` subprocess per tick. Capped by `MaxPerHour` (default 30).
- **Status**: works today. This is the contract: the leader will always get at least one turn per `Interval`, full stop.

### L2 — Pulse audit-nudge (event precision without chat)

- `Pulse.NudgeFromAudit` is called from the audit-hook chain in `cmd/teem/daemon.go`. Interesting events (today: `job_complete`, `job_error`, `note`; proposed addition for `t-50458567`: `worker_stopped` — see §2) are debounced for `DebounceWindow` and then fire an off-cycle `Pulse.Tick(ctx, "event:<kind>@<agent>")`.
- Gives operator-disconnected sessions event-driven precision without depending on Claude Code channels.
- Cost: one subprocess per nudge burst.
- **Status**: works today. Should be **suppressed** while channels are confirmed live (to avoid double-waking when the chat-connected leader will get the channel event anyway).

### L3 — Operator-visible degradation signal

- `teem status` must show one of `channels: live | fallback | disabled` per team so the operator knows *which* wake path is active right now.
- The dashboard renders the same state, prominently, on the team-detail page.
- The daemon writes a one-line audit event on every state transition (`channels_live`, `channels_fallback`).
- **Status**: not yet built. Implementation follows once the state machine in §5 lands.

### L4 — Manual wake (operator-triggered)

- `teem nudge [--team <id>]` CLI: forces a single `Pulse.Tick(ctx, "manual:cli")`.
- Dashboard "Ping leader" button (`t-63fb0676`): forces a single `Pulse.Tick(ctx, "manual:dashboard")`.
- Respects the pause flag (`internal/pulse/pulse.go` `Pause`/`Resume`) — if pulse is paused the manual ping reports "pulse paused, not pinging" rather than bypassing. Channels don't bypass pause either: a paused leader stays paused regardless of which wake layer fires.
- **Status**: dashboard button is `t-63fb0676`; CLI is a small follow-up.

## 4. Channels-live detection

The daemon needs an answer to "are channels currently working?" Channels do **not** flow through the daemon's HTTP MCP server — that path carries the leader's tool calls only. Channels flow over a separate transport: workers/daemon publish onto a per-team `channelbus.Bus`, the daemon's `/teams/<team>/channel-events` SSE endpoint (`cmd/teem/daemon.go:1670`, handler at `:1797`) subscribes one consumer per HTTP connection, and the `teem-channel` stdio shim holds that SSE connection open and re-emits as `experimental.claude/channel` notifications to Claude Code (`cmd/teem-channel/main.go:93`). A leader can be issuing MCP tool calls just fine while its channel shim has crashed or never spawned, so inspecting MCP-client capabilities tells us nothing about whether channel notifications are actually reaching the leader.

The candidates we considered:

1. **MCP capability advertisement on the HTTP MCP server.** Rejected — wrong transport, as above. Tells us "this client knows the experimental key exists" at best, not "channel events are being delivered."
2. **Heartbeat ping** — periodically publish a synthetic channel event and watch for an ack from the leader. Adds protocol surface and burns a leader turn every heartbeat interval. **Rejected** — too invasive.
3. **Channelbus subscriber count.** Direct measurement of the actual transport: `channelbus.Bus.Len() > 0` (already exposed at `internal/channelbus/channelbus.go:151`) tells us the SSE endpoint has at least one connected subscriber. The only way to be a subscriber is to be the `teem-channel` shim holding an SSE connection open (or, in tests, a synthetic subscriber). **Recommended.**

**Recommendation: channelbus subscriber count, observed via subscribe/unsubscribe hooks on the SSE handler.**

Algorithm:

- On entry to `handleChannelEvents` after `rt.channelBus.Subscribe()` (`daemon.go:1813`): if this is the first subscriber for the team (`Bus.Len() == 1` post-subscribe), transition the team to `channels-live`. Emit one audit event (`audit.KindChannelsState`, `state=live`).
- On exit from `handleChannelEvents` (defer after `cancel()`): if this was the last subscriber (`Bus.Len() == 0` post-unsubscribe), transition to `channels-fallback`. Emit one audit event (`state=fallback`).
- Per-team `channels-live` flag stored on the team runtime struct (next to `rt.channelBus`), guarded by a small mutex. `pulse` and the dashboard read this flag; they don't compute it.

Rationale:
- **Edge-triggered**, not poll-based: the subscribe and cancel paths are the exact moments transport health changes, so we observe them directly rather than guessing via TTL.
- **No new traffic, no leader turns burned, no new protocol surface.**
- **The multi-session question dissolves**: any subscriber suffices because `channelbus.Bus.Publish` fans out to *every* subscriber. As long as at least one SSE consumer is connected, every channel event is being delivered to the shim and onward to Claude Code.
- **Failure modes covered**: shim crashes → SSE connection closes → defer fires → fallback. Operator quits chat TUI → shim exits → SSE closes → fallback. Daemon restart → bus has zero subscribers at boot → fallback (see §5 startup).

**Edge cases:**
- Slow-subscriber drops (`channelbus.go:140-147` logs dropped events for slow subscribers): subscriber is still connected, so we remain "live." A future refinement could flip to fallback after N consecutive drops, but v1 treats "connected" as "healthy."
- Stale subscriber (TCP connection wedged, no read activity): kept open by the 25s keepalive write (`daemon.go:1816, 1824-1828`). If the write fails the handler returns and defer fires → fallback. So a wedged TCP socket converts to a fallback transition within ~25s naturally; no extra TTL needed.

**Where the detection lives:** in `handleChannelEvents` (subscribe/cancel hooks), not in the MCP server bootstrap, not in `pulse`, not in `channelbus` itself. The bus stays a dumb pub/sub primitive; the SSE handler is the one place that already knows about subscriber lifecycle.

## 5. State machine

Two states per team: `channels-live` and `channels-fallback`. Transitions:

```
                first SSE subscribe (Bus.Len 0 → 1)
                ──────────────────────────────────►
   channels-fallback                              channels-live
                ◄──────────────────────────────────
                last SSE unsubscribe (Bus.Len 1 → 0)
```

**Startup state**: the daemon boots with `channels-fallback` for every team. The `teem-channel` shim isn't connected yet (Claude Code hasn't launched it), so `Bus.Len() == 0`. The first SSE subscribe from the shim flips the team to `live`.

### live → fallback (last subscriber disconnected)

- Daemon emits audit event: `kind=channels_state`, `state=fallback`, `team=<id>`.
- One-line log: `team=<id> channels: live → fallback (subscriber disconnected)`.
- Pulse's audit-nudge path **re-enables**: subsequent interesting events fire `NudgeFromAudit` again.
- Dashboard and `teem status` re-render to `fallback`.
- Pulse timer is unaffected (it was always running).

### fallback → live (first SSE subscriber connected)

- Daemon emits audit event: `kind=channels_state`, `state=live`, `team=<id>`.
- One-line log: `team=<id> channels: fallback → live`.
- Pulse's audit-nudge path **suppresses** (becomes a no-op until next live → fallback).
- Pulse timer continues unchanged.
- Dashboard and `teem status` re-render to `live`.

### Re-render and re-publish behaviour

- The state machine transitions are emitted as audit events (one per transition). No flapping protection is needed in v1; if rapid reconnect cycles become a problem, add a short debounce on the fallback side.
- The dashboard polls `/control/teams/<id>/status` and re-renders on change; no special push needed because the state-transition cadence is low.
- These audit transitions are **not** themselves delivered to the leader via channels (by construction): the `live → fallback` transition fires precisely *because* there is no subscriber, so the event can't reach the leader through that path; the `fallback → live` event is fired before the new subscriber has begun reading and is observed externally (dashboard, `teem status`, audit log) rather than by the leader. The leader notices degradation indirectly — via the pulse timer continuing to wake it and via operator action.

## 6. Decisions

### D1 — Is pulse-timer-only acceptable when channels are unavailable?

**Yes, always — the pulse timer is the contract.** It runs unconditionally at `Interval` (default 5m). Every other layer (channels, audit-nudge, manual ping) is precision on top of that floor. The audit-nudge path stacks for free (sub-second precision when interesting events arrive) and is suppressed while channels are live to avoid double-waking. If a future deployment needs a tighter floor, lower `Interval`; this design doesn't change.

### D2 — Should the daemon refuse to start without channels, or degrade?

**Recommendation: degrade with a prominent log line.**

Rationale: channels are experimental and gated behind a CLI flag that the operator may not have set. Hard-failing the daemon would block every operator outside the allow-list. The daemon should boot, log `channels: not available — running in fallback mode` at INFO, and proceed with pulse-timer + audit-nudge. `teem status` should reflect this. If a future operator opt-in flag wants strict-channels mode, that's a separate config knob (`require_channels: true`) — not the default.

### D3 — Where does channels-live detection live?

**Recommendation: channelbus subscriber count, observed via subscribe/cancel hooks in `handleChannelEvents`.**

Rationale: the SSE handler is the one piece of code that already owns subscriber lifecycle, and `channelbus.Bus.Len()` is already exposed. Putting the detection in `pulse` would couple pulse to channel internals; putting it on the MCP server would observe the wrong transport (see §4 — channel events do not flow through HTTP MCP); putting it inside `channelbus` would make the bus aware of its own subscribers' semantics, which inverts the dependency. The SSE handler hook keeps `channelbus` a dumb primitive, leaves `pulse` reading a flag, and observes the actual transport directly. See §4 for the algorithm.

### D4 — Non-Claude-Code-channel paths?

**Recommendation: out of scope for v1; mention as future work.**

Rationale: alternative transports (file-based wake via fsnotify on a sentinel file, named pipe, dbus, Unix socket signalling) are technically feasible but solve a problem we don't have yet. The pulse timer is a sufficient floor. If channels disappear permanently, the right next step is probably "promote audit-nudge to a first-class wake path with operator-tunable latency," not "build a new transport." Revisit if and when Claude Code drops the experimental channel API entirely.

## 7. Implementation tasks this design unblocks

- **`t-50458567`** — *Consolidate pulse audit-nudge path with channels (keep timer for headless).* Blocked on this design. Implements: subscribe/cancel hooks in `handleChannelEvents` (`cmd/teem/daemon.go`), per-team `channels-live` flag on the team runtime struct, `pulse.NudgeFromAudit` suppression while the flag is set, the one-line addition of `audit.KindWorkerStopped` to `isInterestingKind` (§2), and the new audit kind below.
- **`audit.KindChannelsState`** — new constant in `internal/audit/audit.go` (alongside `KindWorkerStopped` et al.) with value `"channels_state"`. Meta carries `{state: "live" | "fallback", team: "<id>"}`. Emitted by the §5 transitions; consumed by `teem status` and the dashboard.
- **`t-63fb0676`** — *Dashboard manual-ping button.* This design names the L4 surface area (CLI `teem nudge` and dashboard "Ping leader"); the dashboard half is `t-63fb0676`. The CLI half is a small follow-up — track separately.
- **`teem status` channels-state column** — small follow-up. Renders `channels: live | fallback | disabled` per team from the same per-team flag.

## Appendix — file map

| Concern | File |
|---------|------|
| Pulse timer, debouncer, Tick, NudgeFromAudit | `internal/pulse/pulse.go` |
| Channel publish/subscribe bus | `internal/channelbus/channelbus.go` |
| stdio shim that advertises `experimental.claude/channel` | `cmd/teem-channel/main.go` |
| Audit-hook wiring (combineHooks) | `cmd/teem/daemon.go` |
| Dashboard team-detail page (where channels-state renders) | `cmd/teem/daemon.go` (control handlers) |
