# `project_manager` archetype — design doc

T8 design proposal. Round 2 — operator-revised after first pass. No
code in this round; this doc covers the shape of the archetype, how it
slots into the existing system, and what v1 / v2 / v3 look like.

## Table of contents

1. Archetype declaration
2. Spawning model (on-demand + scheduled)
3. Tracker access (skill-driven)
4. Leader ↔ PM workflow integration
5. Configuration
6. Audit-trail push
7. Permissions / tool access for the spawned PM
8. Phasing
9. Open questions and risks

---

## 1. Archetype declaration

PM is registered like any other archetype in
`internal/team/defaults.go`'s `DefaultArchetypes` and follows the same
`team.ArchetypeSpec` shape (`internal/team/team.go:30`). Concretely:

```yaml
- role: project_manager
  description: |
    Partner/consultant for the leader. Owns the external bug tracker,
    files new tasks into the Teem plan, picks up tracker-side work.
  placement: local
  max_concurrent: 1
  lifecycle: ephemeral
  no_worktree: true
  skill: linear   # picks the Claude Code "linear" skill (or other tracker skill)
```

**What's different from worker / reviewer / integrator:**

- **No worktree.** Existing local archetypes get an auto-worktree on
  branch `teem/<agent-id>` (`internal/agent/spawner.go:54-58`). PM
  doesn't need one — it never reads or writes the working tree. Add a
  new `ArchetypeSpec.NoWorktree bool` flag and short-circuit worktree
  creation when set. Small, targeted, leaves every other archetype
  untouched.
- **Tracker access via a Claude Code skill.** PM loads the tracker
  skill named by `skill:` (e.g. `linear`) when its CC subprocess
  starts. The skill knows how to authenticate, what API surface it
  exposes, and what tools it ships with. This replaces the original
  "wire a tracker MCP into the archetype" idea — see §3.
- **Plan read + add_task.** PM's bread-and-butter MCP calls are
  `list_tasks`, `query_audit`, `get_results`, plus `add_task` to file
  new work it noticed on the tracker side. It can also call any other
  Teem MCP tool a worker can (see §7) — workers and PM use the same
  permission profile.

**Rationale.** The leader currently treats every archetype identically:
spawn it, give it a worktree, hand it a job. PM is the first archetype
that breaks that mould — partner, not executor — and the only minimal
change needed is the `NoWorktree` flag. Tracker access is delegated to
the skill ecosystem.

---

## 2. Spawning model

**Recommendation: on-demand AND scheduled.** Two entry points; both
spawn a fresh PM, both report back via the same `get_results` path.

### On-demand (leader-initiated)

The leader spawns a PM when it wants a consultation. Reuses the
existing `spawn_agent` + `assign_job` flow. PM runs, reports back,
retires. New PM per consultation, fresh context.

### Scheduled (daemon-initiated)

The daemon ticks the PM periodically to pick up tracker-side changes
the leader didn't ask about. Operator files an issue in Linear at
04:00 → daemon's next PM tick notices → PM files a Teem `add_task` →
leader sees it on its next pulse.

- **Cadence:** default once per hour, configurable via
  `tracker.poll_interval` in `teem.yaml`.
- **Entry point:** a new daemon ticker (sibling of pulse) that, when
  due and a `project_manager` archetype is registered for the team,
  enqueues a "scheduled tracker check" job via the same agent-spawn
  path.
- **Idempotency:** the scheduled tick should be a no-op when one is
  already in flight; daemon guards with a per-team mutex.
- **Per-tick wait timeout:** 15m. Slow tracker round-trips can exceed
  5m; 15m gives the PM headroom before the loop retires the worker
  and moves on.

**Why both, not just on-demand:**

- On-demand alone misses operator-side work between consultations.
  An issue filed externally would sit invisible to the team until the
  leader thought to consult PM — which it might not, if it's busy.
- The scheduled tick gives the tracker a non-trivial role even when
  the leader isn't actively driving — closes the "PM only matters
  when invoked" gap.

**Why on-demand is still there:**

- The leader has the richest context. End-of-major-work pushes,
  start-of-major-work alignment checks, "what should we pick up
  next?" decisions — all need the leader's narrative, which only
  the leader has fresh.
- A scheduled tick can't substitute for an intentional consultation.

### What this leaves out (deferred to v3)

- **In-daemon background sync** that doesn't spawn an LLM at all —
  a Go client that pushes audit comments and pulls tracker state
  without an LLM tick. Worth doing if scheduled-PM costs become a
  problem in practice. v3.

---

## 3. Tracker access (skill-driven)

The PM's tracker integration is a **Claude Code skill**, not a Teem-side
Go abstraction. Claude Code already ships skills for popular trackers
(Linear has one; GitHub Issues, Jira, Shortcut, Asana are reasonable
follow-ons).

**Why a skill rather than a Go interface:**

- The skill ecosystem is the right home for "how does an LLM talk to
  Linear" — it ships docs, authentication, prompt patterns, and tool
  wrapping in one place. Building a parallel Go interface in
  `internal/tracker/` would re-implement what the skill already
  encapsulates.
- A future operator who wants GitHub Issues just declares
  `skill: github-issues`. No Teem code changes; the skill handles
  everything backend-specific.
- The Go-side concept "we have a tracker plugged in" becomes one
  string field on the archetype (`skill:`) plus optional context the
  daemon passes to the skill via the consultation brief (team id,
  active milestone, etc.).

### What the archetype config looks like

```yaml
archetypes:
  - role: project_manager
    no_worktree: true
    skill: linear
```

That's the whole tracker wiring on the archetype side. The skill is
loaded by the PM's CC subprocess on startup; from PM's perspective it
just has new tools available (the skill's namespaced tool surface).

### What the daemon passes to PM

The daemon's consultation brief (see §4) supplies tracker context as
plain prose:

> Active Linear team: ENG. Active milestone: 2026-06-cut. Last PM
> consultation pushed comments up to event id ev-7c2 (see frontmatter
> in pinned issue ENG-104).

The skill consumes that prose just like it would in a normal Claude
Code session. No Go-side type sharing required.

### What about authentication?

The skill handles it. Linear's skill, for example, expects the operator
to have authenticated Claude Code with Linear via the normal flow.
Teem-side `teem.yaml` doesn't need an `auth_env` or `auth_file` field
for v1 — that complexity lives in the skill.

If a future skill turns out to need explicit per-team config (multiple
Linear workspaces on one machine, say), we add a `skill_config:` map on
the archetype that the daemon includes verbatim in the consultation
brief. Not needed for v1.

---

## 4. Leader ↔ PM workflow integration

### Where the leader's instructions live

Two places — keep them in sync (the existing
`<!-- Keep in sync ... -->` comment pattern in SKILL.md / team.go is
the precedent):

1. `internal/team/team.go:LeaderSystemPrompt()` — short block:
   "Consult the PM at the start and end of major work, plus any time
   you want a sequencing or tracker check."
2. `cmd/teem/plugin/skills/teem-orchestration/SKILL.md` — fuller
   "Working with a project_manager" section.

### Sketch of the leader-prompt block

```
--- Working with the project manager ---
If a project_manager archetype is in the roster, treat it as a
consultant — not a subordinate. Consult freely. Useful moments:

- Start of a major piece of work — confirm priorities and release fit
  against the external tracker.
- End of a major piece of work — push completed-work summaries into
  the tracker via PM.
- Any time you want to know "what's on the tracker that isn't in our
  plan yet?", or "what should we sequence next?", or "what does the
  operator's recent tracker activity look like?".

There's no rate limit on PM consultations. The daemon also ticks PM
on a schedule (default hourly) so tracker-side work shows up as
add_task entries without you having to ask.

PM files new tasks via add_task. Stage moves, decisions, and worker
dispatch remain yours.
```

### How does the leader call PM?

**Recommendation: use the existing `spawn_agent` + `assign_job` flow.**
The scheduled tick uses the same internal path (the daemon constructs
the same job shape the leader would). A dedicated `consult_pm` MCP
tool can be added later as a convenience once the prompt template has
stabilised — defer to v2.

### Consultation brief template

The leader (or daemon, for scheduled ticks) fills this template:

```
You are the project manager for {team_name}. Consultation type:
{startup | shutdown | midstream | scheduled-check}.

Current Teem state:
- Active leader status: {get_leader_status output, abridged}
- Open tasks (top 10 by stage): {list_tasks open_only=true, abridged}

Recent activity ({since}):
- Audit summary: {query_audit since=… limit=20, abridged}

Tracker scope (for the loaded {skill} skill):
- Linear team: {team_id}
- Active milestone: {operator-supplied or "auto-discover"}
- Last pushed event id per task: {bookkeeping blob}

Your task:
1. Read tracker state using the {skill} skill's tools.
2. Reconcile the tracker view against the Teem plan above. Flag:
   - Tracker issues with no matching Teem task (file via add_task).
   - Teem tasks with no matching tracker issue (recommend; leader
     decides whether to create one).
   - Stale priorities or milestone mismatches.
3. For each completed Teem task since {since}, push (or update) the
   per-task summary comment on the corresponding tracker issue. Use
   the `<!-- teem-sync v=1 task=… last_event=… -->` frontmatter
   format (see §6).
4. Report back as JSON: {priorities: […], new_tasks_filed: […],
   tracker_updates: […], open_questions: […]}.
```

For scheduled ticks, the daemon synthesises the brief from current
state and uses consultation type `scheduled-check`; PM's behaviour
narrows to "pull tracker changes, file what's new via add_task, push
audit comments for anything verified since last tick."

---

## 5. Configuration

### `teem.yaml` extensions

```yaml
team:
  name: my-team
  ...
  tracker:                          # new top-level block — optional
    type: linear                    # picks the skill (skill: linear)
    team_id: ENG                    # passed into the brief as context
    poll_interval: 1h               # scheduled-tick cadence; default 1h
  archetypes:
    - role: project_manager
      no_worktree: true
      skill: linear
```

The `tracker:` block is read by the daemon at startup and:

1. Held on `team.Team.Tracker` for read-back via `read_team`.
2. Used by the scheduled-tick ticker to know which skill / context to
   wire on each PM spawn.

`tracker.type` and `archetype.skill` are normally the same value;
splitting them lets one archetype use a non-default skill if a future
operator wants it. v1 readers can treat them as synonyms.

### Disabled-PM path — when no tracker is configured

**Recommendation: don't register the PM archetype at all.**

When `tracker:` is absent from teem.yaml, the loader silently skips
appending the PM archetype. No warning at startup, no half-broken
archetype in the roster. Rationale: there's little value in a PM
without a tracker — the whole shape of the archetype is "consultant
who owns the external tracker." Sequencing-only consultations are
better delivered by direct leader judgement.

If operators later want a tracker-less PM (purely for sequencing
discussions), we can revisit. For v1, the rule is simple: tracker
configured → PM available; no tracker → no PM.

---

## 6. Audit-trail push

The skill drives this, not Go code, but the format and idempotency
rules are still Teem's call — they're prompted into the consultation
brief.

### Which audit events become tracker comments?

| Kind                  | Push to tracker? | Why                            |
| --------------------- | ---------------- | ------------------------------ |
| `job_complete`        | Yes              | The substance of the work.     |
| `job_error`           | Yes              | Visible failures matter.       |
| `decision_note`       | Yes              | The "why" — exactly what the tracker should have. |
| `blocker_note`        | Yes              | Owners need to know.           |
| `task_stage_changed`  | Roll-up only     | Too noisy 1:1; summarised in per-task comment. |
| `job_received`        | No               | Implementation detail.         |
| `job_transcript_ready`| No               | Internal storage event.        |
| `heartbeat`           | No               | Liveness noise.                |
| `worker_stopped`      | No               | Internal.                      |
| `note`                | Filtered         | Worker free-form notes — push only when meta includes `tracker: true`. |

### Batching

**One comment per task milestone, summarising all related audit events
since the last push.** When a task moves to `verified` or
`integrating`, PM walks audit events filtered by `Meta.task_id` since
the last push and emits a single markdown comment.

- A typical task generates 10–50 audit events; per-event comments
  would be unreadable.
- Tracker APIs rate-limit comment writes; batching is the polite
  shape.
- `record_decision` already captures the narrative; the per-task
  rollup is the right granularity for an external reader.

### Format

Markdown blob with structured frontmatter PM parses to de-dupe:

```markdown
<!-- teem-sync v=1 task=t-3b9f last_event=ev-7c2 -->
## Task: Add user-deletion endpoint (verified)

**Decisions**
- Kept the soft-delete column to preserve audit trail
  (record_decision @ 2026-05-12T14:02:00Z)

**Blockers cleared**
- DB migration approval (resolved 2026-05-13)

**Work**
- worker-ada: PR #4421 (job j-1a2b3c, output 12 KiB)
- reviewer-blake: approved (job j-9f0e1d)
- integrator-cleo: merged to main (job j-aa11bb)
```

PM recognises its own prior posts via the frontmatter on the next
consultation and **edits in place** rather than re-creating — this is
the idempotency hook.

### Idempotence bookkeeping

**Track last-pushed-event-id per (task_id, tracker) pair.**

The leader includes the bookkeeping blob in PM's consultation brief
(it's read from a small daemon-managed state file —
`~/.teem/state/<team>/tracker-sync.json`). PM reports back the new
last-event-ids in its result JSON; daemon persists.

Per-push flow inside the skill:

1. Read last pushed event id for the task (from brief).
2. Pull audit events with `Meta.task_id == taskID` and `id > last`.
3. If empty, no-op.
4. Else compose markdown, search for an existing comment with the
   matching frontmatter task id, edit-or-create accordingly, report
   the new last event id back.

Crash mid-step is safe: a duplicate edit is a no-op; a duplicate
create is detectable by the marker on next push and can be cleaned
up.

---

## 7. Permissions / tool access for the spawned PM

PM gets the **standard worker tool set**. Same Teem MCP permissions
as a worker, plus the loaded skill's tools.

| Source              | Tool                     | PM access |
| ------------------- | ------------------------ | --------- |
| Tracker skill       | (skill-defined surface)  | full      |
| teem MCP            | `list_tasks`             | read      |
| teem MCP            | `read_team`              | read      |
| teem MCP            | `query_audit`            | read      |
| teem MCP            | `recall_jobs`            | read      |
| teem MCP            | `get_results`            | read      |
| teem MCP            | `get_leader_status`      | read      |
| teem MCP            | `update_leader_status`   | write     |
| teem MCP            | `add_task`               | write     |
| teem MCP            | everything else workers can call | write |

### Why no tool gating in v1

The original design carved out `AllowedTools` to deny PM the
Edit/Write/NotebookEdit CC tools and to block `assign_job` /
`spawn_agent` at the MCP layer. Operator feedback: not worried about
PM having those tools. Workers may also reasonably want to update
their own tracker tasks (so worker tracker access is fine too).

**v1 ships without tool gating.** PM has the same surface as any
worker, plus its skill. If we observe abuse (PM dispatching
`assign_job` calls that surprise the leader, say), we revisit — but
the prompt strongly directs PM away from those tools, and the leader
is supervising via the audit log anyway.

The `NoWorktree` archetype flag is the only archetype-shape change v1
needs.

---

## 8. Phasing

### v1 — minimum (the cut this T8 ticket lands)

- Register `project_manager` archetype in `DefaultArchetypes` —
  appended at team-load time **only if `tracker:` is configured**.
- On-demand spawn via existing `spawn_agent`.
- Scheduled spawn via new daemon ticker (default hourly, configurable
  via `tracker.poll_interval`).
- Tracker access via the named skill (`skill: linear` for v1).
- Leader prompt block added (consult freely; no rate limit).
- `NoWorktree` archetype flag added so PM doesn't get a useless git
  branch.
- Idempotency state file at `~/.teem/state/<team>/tracker-sync.json`,
  passed to PM via consultation brief.

### v2

- Additional tracker skills: GitHub Issues, Jira, Shortcut.
- Dedicated `consult_pm` MCP tool (optional convenience once the
  prompt template has stabilised).
- Smarter scheduling (only tick when there's been tracker-side
  activity since last tick, if the skill supports a cheap "anything
  changed?" probe).
- Daemon writes per-tick PM job lifecycle into the dashboard so
  operators can see scheduled consultations alongside on-demand ones.

### v3

- Optional in-daemon background sync — a Go client that pushes audit
  comments without spinning up an LLM each time. The scheduled-PM
  path remains for sequencing-style consultations.
- Cross-task milestone reporting ("you'll miss the 2026-06-01 cut
  unless task X is verified by 2026-05-28").
- If observed abuse warrants it, archetype-level tool gating
  (originally v1's `AllowedTools` idea — deferred indefinitely until
  there's a concrete problem to solve).

---

## 9. Open questions and risks

- **Skill availability.** v1 hinges on the Linear skill behaving as
  documented. If Claude Code's Linear skill is missing, broken, or
  has surprising auth requirements, v1's tracker push won't work.
  Mitigation: before merging the archetype-default change, smoke-test
  end-to-end with the Linear skill on the operator's setup. Failure
  mode is contained — PM job errors out, leader logs the failure,
  team continues without tracker push.
- **Scheduled-tick cost.** Hourly PM consultations consume LLM
  tokens regardless of whether anything changed tracker-side.
  Mitigation: v2's "skill supports a cheap probe" path. v1 accepts
  the cost; operator can lengthen `poll_interval` if it bites.
- **Conflict resolution.** If PM updates a Linear issue and the
  leader simultaneously updates the corresponding Teem task
  differently (PM marks done; leader moves to `reviewing`), the
  systems disagree. **Rule: Teem plan wins for stage, tracker wins
  for tracker-side metadata (milestones, priorities).** Document
  explicitly in the leader prompt and in the consultation brief. The
  audit push (§6) pushes Teem's view of stage into a tracker
  *comment*, not the tracker's issue.status field — so the leader's
  stage moves don't fight the tracker.
- **Roster pollution.** PM names land in the same wordlist roster as
  workers (`project_manager-ada`, etc.). The wordlist allocator
  already scopes by role, so no collision risk. The dashboard's
  branches-per-project view (`cmd/teem/branches.go`) just shells out
  to `git for-each-ref refs/heads/teem/` — PMs won't appear, which
  is correct (they have no branch).
- **Where does scheduled-tick state live?** The daemon's pulse loop
  already manages per-team timers (`internal/pulse/pulse.go`). The
  PM ticker is a sibling: separate timer, separate budget gate
  (skipped when pulse is paused), but reuses the same agent-spawn
  path. Implementation should mirror pulse's shape, not invent a new
  one.
