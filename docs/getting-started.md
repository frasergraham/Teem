# Getting started with Teem

Teem orchestrates a team of Claude Code workers. You chat with a
**leader** (which is Claude Code itself), and the leader spawns and
dispatches work to **workers** — separate Claude Code processes
running locally, over SSH, or as ephemeral AWS Fargate tasks. The
leader can also keep working autonomously between your sessions.

## Concepts in 30 seconds

- **Daemon** (`teemd`) — long-running background process. Owns the
  MCP server, audit log, task plan, persistent state. One per user.
- **Team** — declared in `teem.yaml`. A name + leader brief + a list
  of **archetypes** (role templates with a `max_concurrent` cap).
- **Leader** — a Claude Code session you talk to. Started by
  `teem chat`, attached to the daemon's MCP server. Resumes the same
  conversation every time.
- **Workers** — instances spawned from archetypes. Auto-numbered
  IDs like `worker-1`, `worker-2`. Each runs its own `claude` and
  works in its own git worktree (local) or container (Fargate).
- **Plan** — the leader's task list. Stored on disk; survives
  daemon restarts.
- **Audit** — every event workers and the leader emit. Greppable
  JSONL.
- **Pulse** — optional autonomous loop. Wakes the leader on a timer
  and on worker events while you're away.

## Install

```sh
go install github.com/frasergraham/teem/cmd/teem@latest
go install github.com/frasergraham/teem/cmd/teem-worker@latest
```

Or from a checkout: `go install ./cmd/teem ./cmd/teem-worker`.

You also need **Claude Code** on your PATH (`claude --version` should
work). Get it at https://docs.claude.com/en/docs/claude-code.

## First-time setup

In the project directory you want the team to work on:

```sh
teem init
```

This:
1. Installs the Teem plugin into `~/.claude/commands/` and
   `~/.claude/skills/` — slash commands and an orchestration skill
   Claude Code auto-loads.
2. Walks you through creating `teem.yaml` — team name, leader brief,
   one or more archetypes (`worker`, `reviewer`, custom roles…) with
   a `max_concurrent` cap each.
3. Offers to start the daemon.

A minimal `teem.yaml`:

```yaml
team:
  name: my-project
  leader:
    system_prompt: |
      Lead a small team. Delegate implementation to a worker; have a
      reviewer check every change before declaring it done.
  archetypes:
    - role: worker
      placement: local
      max_concurrent: 5
      description: "Implements features in its own git worktree."
    - role: reviewer
      placement: local
      max_concurrent: 3
      description: "Reads diffs, flags risk before merge."
```

## Daily flow

```sh
teem chat
```

That's it. Under the hood `teem chat`:

1. Ensures the daemon is running (auto-starts if missing).
2. Registers your team with the daemon if it isn't already.
3. Looks up the team's persistent Claude session id.
4. Execs `claude --resume <session-id> --mcp-config ... --append-system-prompt "<team brief>"`.

You're now in Claude Code's TUI, talking to the leader. The orchestration
skill is loaded; the leader has these MCP tools available:

| Tool | What it does |
|---|---|
| `read_team` / `list_agents` | Roster + active instances |
| `spawn_agent(role)` | Provision a new instance of a role |
| `assign_job(agent_id, prompt)` | Hand a job to a worker |
| `get_results(job_id)` | Poll for job output |
| `stop_agent(agent_id)` | Tear down a running worker |
| `add_archetype` / `remove_archetype` / `update_archetype` | Mutate the role roster at runtime |
| `add_task` / `update_task` / `list_tasks` | Manage the plan |
| `recall_jobs` / `query_audit` | Recall past work |
| `write_user_note` | Leave a message for your next chat |

Slash commands the plugin ships:

- `/teem-status` — one-glance status: leader status, active workers,
  open tasks, in-flight count, pulse state.

A typical session:

```
> Plan the migration to the new auth flow and start implementing.

[leader uses add_task to break it into 5 subtasks]
[leader calls spawn_agent("worker") twice → worker-1 and worker-2]
[leader calls assign_job for the first two subtasks]
...
> What's everyone doing?

[leader calls list_agents and recall_jobs, summarises]
```

When you're done, Ctrl-D out of the chat. Workers in flight finish
their jobs; the daemon stays up.

## Autonomy: leader works while you're away

By default the leader only thinks when you're chatting with it. To
let it keep working between sessions:

```sh
teem pulse start              # default interval: 5 min
teem pulse start --interval 2m
```

Now the daemon will periodically (timer) and reactively (when
workers emit `job_complete` / `job_error`) invoke the leader to take
a turn. Each tick:

1. Builds a context snapshot — recent audit events, open tasks,
   running agents.
2. Invokes `claude -p --resume <same session>` with that snapshot
   in `--append-system-prompt` and "Take your next turn." as input.
3. Captures the assistant turn and any tool calls in the audit log
   as a `pulse_tick` event.

Guard rails (you can adjust via env vars on the daemon):

- **30 ticks/hour** ceiling (`TEEM_PULSE_MAX_PER_HOUR`).
- **Idle backoff**: 3 consecutive no-tool-call ticks doubles the
  effective interval (capped 8×).
- **Pause/resume**:
  ```sh
  teem pulse pause --reason "I'll be back later"
  teem pulse resume
  ```

Status check:

```sh
teem pulse status
```

When you return:

```sh
teem chat
```

The leader resumes the same conversation. Before Claude Code opens,
`teem chat` prints any **notes** the leader left for you:

```
[teem] 2 note(s) from the leader since you were last here:
  • May 13 14:08 — All 5 migration subtasks complete; pushed to teem/worker-1..3
  • May 13 14:42 — wk-2 hit a flaky test (TestAuthRefresh); paused work, audit ID j9c
```

Then the chat opens and you can pick up where the leader left off.

## Working with workers

When the leader spawns a worker, a few things happen automatically:

- **Local workers** get a git worktree at
  `~/.teem/worktrees/<team>/<worker-id>` branched off your repo's
  HEAD on branch `teem/<worker-id>`. The branch persists across
  sessions.
- **Remote workers** (SSH, Fargate) clone the repo themselves if
  `TEEM_GIT_REPO_URL`+`TEEM_GIT_TOKEN` are set, and auto-push their
  branch after each successful job.
- **Heartbeats** every 60s; surfaced as `last_seen` in
  `list_agents`. The leader uses this to spot stalled workers.

To see what a worker did:

- Check its branch: `git log teem/worker-1`.
- Tail the audit log: `teem audit --agent worker-1 --follow`.
- Or just ask the leader: "what did wk-1 work on?"

## Multiple projects

The daemon is multi-tenant. Each `teem.yaml` (with a unique
`team.name`) gets its own audit log, plan, worktrees, and Pulse.
You can `teem chat` from different repos and each picks up its own
team:

```sh
cd ~/project-a && teem chat   # team: project-a
cd ~/project-b && teem chat   # team: project-b
```

`teem status` shows the running daemon and its registered teams.

## Cloud workers (AWS Fargate)

To run workers as ephemeral Fargate containers, set these env vars
before `teem start`:

```sh
export AWS_REGION=us-west-2
export TEEM_ECS_CLUSTER=teem
export TEEM_ECS_TASK_DEF=teem-worker:1
export TEEM_ECS_SUBNETS=subnet-...
export TEEM_ECS_SECURITY_GROUPS=sg-...
export TS_AUTHKEY=tskey-...               # ephemeral + reusable + preauth
export ANTHROPIC_API_KEY=sk-...

# optional: clone the repo on every worker
export TEEM_GIT_REPO_URL=https://github.com/owner/repo.git
export TEEM_GIT_TOKEN=ghp_...
```

Then declare a fargate archetype:

```yaml
archetypes:
  - role: cloud-worker
    placement: fargate
    max_concurrent: 10
    description: "Burst capacity, fresh container per agent"
```

See `README.md` for the full ECS setup checklist.

## Where everything lives

```
~/.teem/
├── daemon.{pid,json,log}                 # the daemon itself
├── state/
│   └── <team-slug>/
│       ├── leader-session.json           # Claude session UUID
│       ├── plan.jsonl                    # tasks
│       ├── notes.jsonl                   # leader → user messages
│       ├── pulse-mcp.json                # MCP config Pulse hands to claude
│       ├── pulse.paused                  # flag file (present when paused)
│       └── <agent-id>.json               # persistent agent state (Fargate ARNs)
├── audit/
│   └── <team-slug>/audit.jsonl           # every event
└── worktrees/
    └── <team-slug>/<agent-id>/           # local worker checkouts
```

The plugin lives at `~/.claude/commands/teem-*.md` and
`~/.claude/skills/teem-orchestration/SKILL.md`. `teem init` and
`teem chat` install/refresh them on first run.

## Common operations

```sh
teem status                        # daemon running? endpoint? teams?
teem audit                         # last 50 audit events
teem audit --agent worker-1 --follow
teem pulse status                  # autonomy state for this team
teem pulse pause                   # halt the autonomous loop
teem pulse tick                    # force one autonomous turn now
teem prompt show --role leader     # see the assembled leader prompt
teem prompt show --role worker --raw   # see only the operator override
teem prompt append --role worker "Always run go vet before commit"
teem prompt edit --role reviewer   # opens $EDITOR on the override file
teem stop                          # shut down the daemon
```

## Customising the system prompts

The leader and each archetype get a system prompt assembled from two
layers: the team YAML and an operator-authored override on disk at
`~/.teem/state/<team-slug>/prompt-overrides/<role>.md`. Use `teem
prompt` to inspect or extend either layer without editing YAML or
restarting the daemon. Same data is available to the leader at runtime
via the `read_prompt` / `append_prompt` MCP tools.

Operator prompt overrides assemble into the leader's brief at
chat-start. A running leader session retains its existing prompt until
you exit and re-launch with `teem chat --new-session`; subsequent
Pulse ticks and plain `teem chat` invocations resume the prior session
and reuse the prompt that was active when that session was first
created. Worker prompts behave the same way per-spawn — an in-flight
worker keeps the prompt it was given at spawn; the next spawn picks up
the new override.

## Troubleshooting

**"claude CLI not found on PATH"** — install Claude Code; make sure
`claude --version` works in the same shell.

**Worker jobs sit forever** — first check `list_agents` for the
worker's `last_seen`. If it's stale, the worker process is dead; use
`stop_agent` and respawn. Check `teem audit --agent <id>` for the
last thing it reported.

**"daemon not running"** — `teem start`. The daemon doesn't survive a
reboot by default; for that, install a launchd plist or systemd unit
that runs `teem start --foreground`.

**Pulse is too noisy / burning API spend** — `teem pulse pause`, or
`teem pulse start --interval 10m`. The 30/hour cap should prevent
runaway costs, but heavy event traffic can keep the budget pinned.

**Lost the conversation thread** — `~/.teem/state/<team>/leader-session.json`
holds the UUID. If Claude Code's local session store is gone (e.g.
`~/.claude` cleared), delete the leader-session.json and the next
`teem chat` starts fresh.

## What's next

- The autonomous loop is opt-in for a reason — start with short
  sessions where you're at the terminal before leaving it
  unattended.
- Read the orchestration skill (`~/.claude/skills/teem-orchestration/SKILL.md`)
  — that's what teaches your leader how to delegate. You can edit
  it for your team's style.
