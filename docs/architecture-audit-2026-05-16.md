# Architecture audit — 2026-05-16

Read-only walkthrough of the Teem Go daemon + SPA. Method: package-size
survey, per-package skim with `file:line` citations, cross-cutting
themes, and a follow-up queue at the end. No code changes outside this
file.

## Headline findings (TL;DR)

- **`cmd/teem/daemon.go` is the codebase's god object** — 3417 lines, 86 top-level functions, importing every internal package. It mixes HTTP routing, registration, restore, channel-detection, audit-hook wiring, messaging, usage, orphan-sweep startup, and team-services construction. Splitting it into `daemon_lifecycle.go` / `daemon_routes.go` / `daemon_services.go` / `daemon_register.go` is the single highest-leverage refactor in the repo.
- **`cmd/teem/ui.go` is the SSR-era response-builder kept alive only to feed `/api/teams/<id>/state`** — 1546 lines, 154 JSON-tagged fields on `dashboardTeam`. With SSR retired (`cmd/teem/ui_dashboard.html` deleted @ `d57dfa4`), this file is now misnamed and conceptually a snapshot-DTO builder.
- **Audit `FileSink.Query` is O(file) per call and is called 5+ times per state snapshot** (`cmd/teem/ui.go:500,563,736,1100,1364`, `peeraware_loop.go:204`, `chat_history.go:236`, `cost.go:23`, `orphan_sweep.go:61`). As JSONL grows, dashboard latency grows linearly with audit history.
- **`registeredTeam.channelsLive` is a plain `bool` read without the documented `detectionMu`** at `api_state.go:122` (with lock), `task_approval.go:386` (without), `daemon.go:1735` (without). Either the lock-free reads are intentional and the type should be `atomic.Bool`, or they're bugs.
- **Pulse and spawner are healthy but oversized** (1198 / 1189 lines). The current shape is *correct* (one mutex serializes ticks, multi-map state under one lock in spawner) but reading the code requires a lot of context — both deserve internal package splits.

## Package walkthroughs

### `internal/audit`

What it does: defines `Event` + `Kind` (20+ constants), the `FileSink` JSONL store, the HTTP ingestion endpoint, and the new `JobTaskIndex`.

Health: **smelly** — sink scans, kind proliferation, no streaming reader.

- **`audit.go:32` Kind set is open, but in practice closed-by-convention.** 20 constants spanning lifecycle, decisions, pulse, usage, channels, leader-status. No registry, no `String()` validation, no test that asserts every kind is consumed somewhere. Pulse's `isInterestingKind` (`pulse.go:696`) silently no-ops on a kind it doesn't recognise — a new kind that *should* wake the leader fails silently until someone notices.
- **`audit.go:210–243` `Query` is O(file).** Opens, scans from byte 0, decodes JSONL, filters in Go. The five UI callers above hit this on every snapshot build; orphan-sweep adds one Query every 5 min over a 4h window. There is no seek-from-tail and no index.
- **`audit.go:222` scanner buffer cap is 4 MiB per line.** One pathological log line truncates a query silently (scanner returns false on overflow). No metric or warning.
- **`jobtask.go` is clean.** RWMutex-guarded map, nil-safe, idempotent `Clear`, `ClearByAgent` returns a count for tests. No issues.
- **Suggested follow-ups:**
  - "audit: stream-tail Query path" — wrap `FileSink` with a small tail-cache so `Query(since=last 30m)` is O(window) instead of O(file). Half-day, big snapshot-latency win.
  - "audit: closed-kind registry + lint" — register every `Kind` constant in a package-level set and add a test that fails if a constant is missing from `isInterestingKind`'s switch (so the wake set stays in sync).

### `internal/plan`

What it does: append-only JSONL task store; in-memory plan with a 12-stage transition matrix and a coarser `Status`.

Health: **smelly** (size + stage proliferation), but mutation paths are race-safe.

- **`plan.go` is 701 lines mixing concerns:** CRUD, transition validation, replay, evidence list management, legacy normalization (`normalizePair`). Single `mu` guards everything (correct, but coarse).
- **`stage.go:11–45` defines 12 stages** (proposed/ready/specced/awaiting_approval/planning/coding/reviewing/integrating/blocked/verified/abandoned/shelved). The transition matrix is hand-maintained and forgiving — many stages have 8+ outgoing edges. No mechanically-derived rule.
- **Replay-time silent repair (`plan.go`):** `normalizePair` silently corrects contradictory `(stage, status)` pairs without logging which entries were rewritten — operators lose forensic ability if a bug introduces a bad pair.
- **Suggested follow-ups:**
  - "plan: extract `stagerepo` package" — move `Stage`, transition matrix, and `normalizePair` out of `internal/plan` into `internal/plan/stage` (subpackage) so plan.go shrinks to CRUD/replay only.
  - "plan: log normalize-repair at replay" — when `normalizePair` adjusts a contradictory pair, emit a `note` audit event so the repair is visible to ops.

### `internal/pulse`

What it does: leader autonomous-tick loop with idle backoff, debouncer, channels-live gate, persistent interval/wake-prompt.

Health: **smelly** — correct but 1198 lines, lots of mixed state (atomics + `mu` + `persistMu` + nudge channel + channels-live atomic + `running` atomic).

- **`pulse.go:531–546` `run`'s timer loop does not check `Paused()`** — pause is checked inside `Tick` (not shown), so a paused pulse still wakes the goroutine, evaluates `effectiveInterval`, and enters `fireTick` before bailing. Cost is small but the design intent ("a paused pulse is idle") would read more clearly with the guard at the top of the loop.
- **`pulse.go:556–569` `fireTick` channels-live branch silently returns when `OnChannelNudge` is nil.** No log, no counter. If the wiring ever regresses (e.g. a refactor forgets to pass `OnChannelNudge`), the timer goes dark and no metric surfaces.
- **`pulse.go:680–686` `NudgeFromAudit` drops on full buffer** with a bare `default:` and an early `return`. Comment says "already enough nudges queued; drop" but a burst that overflows is invisible — no counter, no rate-limited log.
- **`pulse.go:452–458` `Busy` uses `TryLock`** with a documented race window. This is fine, but `Busy` is wired into dashboard `/control/teams/<id>/ping` (`daemon.go:1735`) — UI may report "not busy" the instant before a tick starts. The doc-comment already calls this out; no fix needed, just be aware.
- **`pulse_test.go` has 12+ `time.Sleep` calls** (226, 325, 333, 767, 777, 789, 817, 827, 859, 874, 898, 936) ranging 50–500 ms. The whole-suite race-count for this file approaches a second of pure sleep. Flake-prone under CI load.
- **Suggested follow-ups:**
  - "pulse: split into timer / debouncer / persist files" — same package, three files. Reduces cognitive load when reading one concern.
  - "pulse: counter for dropped nudges + nil channel-nudge callback" — expose via existing daemon status payload so silent-death modes become visible.
  - "pulse: replace `time.Sleep` with done-channel/clock injection in tests" — borrow the small `clockwork`-style fake the codebase already needs.

### `internal/mcp`

What it does: MCP server, tool registrations (spawn, assign_job, record_decision, set_task_stage, update_leader_status, query_audit, etc.), registry of running agents.

Health: **smelly** — `tools.go` is 901 lines with a flat handler-per-tool layout, error swallowing on audit writes is widespread.

- **`tools.go` is a single file containing all tool handlers** for ~20 tools. Each handler is reasonable individually; collectively the file is hard to navigate.
- **Audit-write errors are typically swallowed**: most callers use `_ = s.audit.Write(...)`. If the disk fills or the FD goes bad, the in-memory mutation succeeds but the audit trail diverges and no operator-visible signal fires.
- **`registry.go` is clean.** RWMutex registry, GC routine has a stop channel, idempotent close.
- **Suggested follow-ups:**
  - "mcp: split `tools.go` per tool family" — `tools_jobs.go`, `tools_plan.go`, `tools_status.go`, `tools_archmem.go`. Pure file move + tests.
  - "mcp: surface audit.Write errors" — wrap audit writes in a helper that logs once per minute with a rate limit; consider returning a `meta.audit_lost=true` field on the MCP tool result for the leader's benefit.

### `internal/agent`

What it does: spawner (1189 lines) — drives subprocess workers via socket adoption, named-roster allocation, quota, drain.

Health: **clean** (locking is correct) but oversized.

- **`spawner.go`** holds five maps (workers, jobs, subs, provisioned, roster) under a single `mu`. Stop/Spawn/AssignJob all hold the lock; no obvious data race.
- **Worker socket reconciliation (`spawner.go:864`)** uses `s.cfg.AuditSink.Query("", time.Time{}, 1000)` — a full-history Query bounded by 1000. As the audit log grows past a few thousand recent events, this becomes a noticeable startup cost.
- **No GC of `jobs` map.** Entries are only removed on explicit `CancelJob`. A worker that crashes between `job_received` and a terminal event leaks an entry until the daemon restarts. (The new orphan-sweep in `cmd/teem/orphan_sweep.go` emits the *audit* terminal but does not call back into the spawner.)
- **Suggested follow-ups:**
  - "spawner: TTL sweep on `jobs` map" — drop entries older than 24h on every Spawn so a long-lived daemon doesn't grow unbounded.
  - "spawner: bound `Query` window in `ReconcileLocalSockets`" — pass `since = now - 24h` instead of `time.Time{}`.

### `internal/team`

What it does: team config (archetypes, leader.system_prompt, tracker), prompt assembly, persona mapping, ID minting.

Health: **smelly** — `LeaderSystemPrompt` is a 60-line string-builder with seven inline policy sections, three of which carry `NOTE: keep in sync with cmd/teem/plugin/skills/teem-orchestration/SKILL.md` comments.

- **`team.go:459–522` `LeaderSystemPrompt`** has hand-managed `--- Section ---` blocks for: dashboard-honesty, status-messages (shared `StatusMessageGuidance`), integrator workflow, project manager, memory hygiene, project brief. The three "keep in sync" comments are a documented drift hazard — there is no test that the SKILL.md sections match.
- **System prompt has no version field.** A change mid-daemon-run produces no audit entry; running workers continue with stale framing until respawned, with no record that the framing changed.
- **`team.go:31–37` `NewID` panics on `crypto/rand` failure** with a bare message — fine, but the panic message doesn't say *which team / which caller* triggered it.
- **Suggested follow-ups:**
  - "team: assert `LeaderSystemPrompt` sections match SKILL.md" — golden-file test that loads SKILL.md, extracts the four shared sections, and compares to the builder output. Fails the build when drift starts.
  - "team: audit a `system_prompt_changed` event" — emit on every effective change, with a short hash of the old/new prompt. Cheap, makes drift visible.

### `internal/messaging`

What it does: notifier abstraction, Telegram backend, dedup, reply-tokens, webhook listener, format.go.

Health: **clean-ish** — the per-file decomposition is good; one rough edge:

- **`telegram.go`** maintains a manual LRU (`list.List` + `messageIDIdx` map) for outbound message IDs with TTL but no background reaper — stale entries persist until the next Publish runs the eviction pass.
- **Webhook listener startup is in `cmd/teem/daemon.go`**, not in this package, so the lifecycle (port-reuse, funnel-enable) is tangled with daemon startup ordering.
- **Suggested follow-up:**
  - "messaging: reap-on-timer for outbound message-id LRU" — small goroutine, every 6h, drops TTL-expired entries even when traffic is quiet.

### `internal/channelbus` / `internal/wsbus`

What it does: in-memory pub/sub. `channelbus` for leader-chat channel blocks; `wsbus` for SPA WebSocket envelopes (with ring buffer for backfill on reconnect).

Health: **clean.** Both packages are short, well-tested, mutex-guarded, drop-on-overflow with documented semantics.

- `wsbus_test.go:90,98` uses 1–5 ms `time.Sleep` for sequencing — flake-prone but cheap; not worth refactoring unless CI flakes.
- No suggested follow-ups.

### `internal/leaderstatus`

What it does: per-agent status text board with atomic-rename file persistence.

Health: **clean.** Single mutex, UTF-8 safe truncation, file roundtrip tested. No follow-ups.

### `internal/usage`

What it does: cost / quota / capture pipeline. Decomposed into `usage.go`, `state.go`, `quota.go`, `pricing.go`, `cost.go`, `config.go` — each ~150 lines with a paired test file.

Health: **clean.** Best-decomposed package in the tree; should be a model for future extractions.

- `cost.go:23` does a Query with `limit=50000` — combined with the O(file) sink, that's an entire-log scan. The 50k bound is effectively infinite for the realistic log size, so it functions as a "scan everything since `since`".
- No suggested follow-ups beyond the cross-cutting audit-Query work below.

### `internal/roster` / `internal/pruner`

What it does: name allocation (fresh / reincarnated / numeric) and branch pruning (live / merged / orphan classification).

Health: **clean.** Pruner is purely functional (`Classify` + side-effecting `Apply`); roster has wordlist + LRU reincarnation; both well-tested.

- No suggested follow-ups.

### `cmd/teem` (the god directory)

What it lives there: daemon entrypoint, every HTTP handler, audit-hook plumbing, dashboard DTO assembly, CLI subcommands, embed wrappers. 68 `.go` files.

Health: **risky** — sprawl + leakiness.

- **`daemon.go` is 3417 lines / 86 funcs.** Concerns I can list from `grep -nE '^func '`: parse-flags, runStart/Stop/Status, daemonHomeDir, atomicWrite, snapshotTeam, serveDaemon (the giant HTTP setup), handler routing, control/teams/* dispatch, channel-detection (`observeChannelSubscribe`), audit-hook combinators (`combineHooks`, `makeChannelHook`, `makeUsageHook`), messaging init, webhook auto-register, funnel enable, restore from disk, build team services (one function spanning ~470 lines), transcripts handler. Every one of those concerns has a clear cleavage plane.
- **`cmd/teem/ui.go` is now misnamed.** SSR templates and `renderTeamPage` are gone (zara's @ `d57dfa4`/`67583ab`); what remains is the snapshot-builder that feeds `/api/teams/<id>/state` (`api_state.go:116`). The file should be renamed to `snapshot.go` and the doc-comments on each type updated — many still reference "the team-detail page" as if it were rendered server-side.
- **154 JSON-tagged fields on `dashboardTeam` (`ui.go:20`).** The SPA at `cmd/teem/ui/src/store/team.ts` consumes a subset via the `apiTeamStatePayload` wrapper. Some fields (`PulseInterval`, `PulseLastTick`, `PulseTickCount` at the top level, *duplicated* inside `Pulse pulseSnapshot`) are explicitly described in comments as legacy. Worth a pruning pass.
- **`registeredTeam.channelsLive` lock discipline is uneven.** `daemon.go:401` documents that `detectionMu` guards the flag; `api_state.go:121` follows the rule; `task_approval.go:386` and `daemon.go:1735` do bare reads. Likely benign for a single-word bool but technically a data race; should be `atomic.Bool`.
- **`cmd/teem/orphan_sweep.go:25` 4h window is documented but unbounded for very long daemons.** A daemon up for weeks with crashed jobs orphaned >4h ago will never sweep them — `inFlightLog.Outstanding()` at startup is the only safety net, and a long-running daemon never hits startup. Comment acknowledges this; a separate quarterly sweep with a larger window would close the gap.
- **Suggested follow-ups:**
  - "daemon: split `daemon.go` into 4–5 files" — `daemon_lifecycle.go` (run/stop/status/PID/state file), `daemon_http.go` (handler + middleware), `daemon_register.go` (register/restore/migrate), `daemon_services.go` (buildTeamServices), `daemon_audit_hooks.go` (channel/usage/messaging hooks + combinators).
  - "ui.go: rename to `snapshot.go`, drop SSR-era comments" — pure rename + comment cleanup; no behavior change.
  - "daemon: make `registeredTeam.channelsLive` an `atomic.Bool`" — kill the bare-read TOCTOU; delete `detectionMu` if the only state it protects is this one flag.
  - "ui: cull the duplicate Pulse* top-level fields on `dashboardTeam`" — SPA now reads from `Pulse pulseSnapshot`; the four mirror fields can go.

### `cmd/teem/ui/src/` (the SPA)

What it lives there: React 18 + Vite + TS, Zustand store, WebSocket envelope handlers, components per panel (Hero, Workers, Tasks, Approvals, Pulse, Usage, Decisions, EventLog, Chat, WatchTranscript), TaskDetailModal.

Health: **clean — for now.** Tightest package layout in the project. Audit-envelope patches (`audit_patches.ts`, 414 lines) and the Zustand `team.ts` store (336 lines) are the heaviest files; both are flat patch-tables that map well.

- `audit_patches.ts:39–53` handler-map only covers 12 of audit's 20+ kinds. Missing kinds (e.g. `pm_tick`, `worker_started`, `task_added`) fall through to the no-op branch and rely on the next `snapshot_invalidate` to reconcile. Comment at line 14 acknowledges this.
- No tests in `cmd/teem/ui/src/` — `vitest` isn't wired. Snapshot-patching logic in `audit_patches.ts` is exactly the kind of code that benefits from unit tests.
- **Suggested follow-up:**
  - "ui: vitest + tests for `audit_patches.ts`" — wire vitest into `make ui`, add fixtures for each handler. Covers the highest-bug-risk file in the SPA.

## Cross-cutting themes

1. **Audit reads are the dominant snapshot cost.** Every UI snapshot triggers 3–5 `FileSink.Query` calls scanning the entire JSONL. A small "tail-cached" wrapper (e.g., last 24h kept in a ring) would convert linear scans into bounded reads with no API churn.
2. **God-objects in two places.** `cmd/teem/daemon.go` (3417 lines) and `internal/pulse/pulse.go` (1198 lines). Both are split-ready — clear cleavage planes, no circular concerns.
3. **Audit `Kind` proliferation without a registry.** 20+ kinds, each consumed by a hand-maintained switch in pulse, channel hook, usage hook, audit-patches.ts. A registry pattern (or a lint test enforcing each Kind appears in every required switch) would close the silent-no-op risk.
4. **SSR→SPA cutover is not fully cleaned up.** `cmd/teem/ui.go` is a server-rendered DTO builder by historical accident; `dashboardTeam` retains legacy mirror fields and doc-comments that reference templates that no longer exist.
5. **Subprocess error-swallowing.** Audit writes (`mcp/tools.go`), usage parses, messaging notifies — many call sites use `_ = ...` with no surface for failure. A package-wide convention for "rate-limited log + counter" would let ops see silent-death modes.
6. **Test hygiene: sleep-based sequencing.** `internal/pulse/pulse_test.go` is the worst offender (12+ `time.Sleep` calls, 50–500 ms each); `cmd/teem/pm_loop_test.go`, `messaging_webhook_test.go`, `task_approval_test.go` also use 10–50 ms sleeps. A small clock-fake + signal-channel idiom would deflake all of them.
7. **Lock discipline drift on `channelsLive`.** Documented mutex bypassed in two of three read sites. Migrate to `atomic.Bool` and delete the mutex if that's its only job.
8. **PM scaffold still unwired.** `cmd/teem/pm_loop.go` exists (@ `179d1a3`) but is not started in `buildTeamServices`. The leader prompt already advertises a project_manager archetype (`team.go:499`); the gap between prompt and runtime should be flagged in the queue.

## Proposed task queue

| Task title | Area | Rough size | Priority |
|---|---|---|---|
| daemon: split `daemon.go` into 4–5 files (lifecycle / http / register / services / audit-hooks) | `cmd/teem` | 1d | High |
| audit: stream-tail Query wrapper so `since=now-30m` is O(window) | `internal/audit` | half-day | High |
| ui.go: rename to `snapshot.go`, prune SSR-era comments + duplicate Pulse* fields on `dashboardTeam` | `cmd/teem` | half-day | High |
| daemon: `registeredTeam.channelsLive` → `atomic.Bool`, drop `detectionMu` if obsolete | `cmd/teem` | 1h | High |
| audit: closed-Kind registry + lint test (each Kind appears in pulse switch or is explicitly opted-out) | `internal/audit` | half-day | High |
| pulse: split `pulse.go` into timer / debouncer / persist files (same package) | `internal/pulse` | half-day | Medium |
| pulse: counter for dropped nudges + nil OnChannelNudge | `internal/pulse` | 2h | Medium |
| pulse: replace `time.Sleep` in `pulse_test.go` with clock-fake + done channels | `internal/pulse` | 1d | Medium |
| mcp: split `tools.go` per tool family (jobs / plan / status / archmem) | `internal/mcp` | half-day | Medium |
| mcp: surface audit.Write errors via rate-limited logger + counter | `internal/mcp` | half-day | Medium |
| plan: extract `internal/plan/stage` subpackage (transition matrix + `normalizePair`) | `internal/plan` | 1d | Medium |
| plan: emit audit `note` when `normalizePair` repairs a contradictory pair | `internal/plan` | 2h | Low |
| team: golden-file test that asserts `LeaderSystemPrompt` sections match SKILL.md | `internal/team` | half-day | Medium |
| team: audit `system_prompt_changed` event with old/new hash on mutation | `internal/team` | 2h | Low |
| spawner: TTL sweep on `jobs` map (drop entries older than 24h on Spawn) | `internal/agent` | 2h | Medium |
| spawner: bound `Query` window in `ReconcileLocalSockets` to 24h | `internal/agent` | 1h | Low |
| messaging: timer-based reaper for outbound message-id LRU (every 6h) | `internal/messaging` | 2h | Low |
| daemon: PM loop hookup in `buildTeamServices` (currently dangling — prompt advertises a feature the daemon doesn't run) | `cmd/teem` | half-day | High |
| ui (SPA): vitest + unit tests for `audit_patches.ts` (handler coverage per Kind) | `cmd/teem/ui/src` | 1d | Medium |
| orphan-sweep: secondary "very-stale" pass with a much larger query window (e.g. 30d) for long-running daemons | `cmd/teem` | 2h | Low |
