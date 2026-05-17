# Teem

> See [CLAUDE.md](CLAUDE.md) for the worker quickstart. Deeper docs in [`docs/`](docs/).

Teem is a Go daemon that orchestrates a small team of Claude Code
subprocesses against a git repo. A **leader** Claude session chats with
the operator and delegates work to **workers**, **reviewers**, and an
**integrator** — each in its own git worktree — over a per-team MCP
server. Dashboard at `http://teem:7777/` on the tailnet.

## Install

```sh
make ui && go build ./cmd/teem      # or: make ui && go install ./cmd/teem
```

`make ui` builds the embedded SPA into `cmd/teem/ui/dist`. A `go build`
on a clean checkout fails until that directory exists.

You also need **Claude Code** on your PATH (`claude --version`).

## First team

```sh
cd path/to/repo
teem init        # writes ./teem.yaml, installs ~/.claude/skills/teem-orchestration
teem start       # daemonises; tsnet node "teem", dashboard :7777, MCP server, optional webhook
teem chat        # opens Claude Code attached to the leader session
```

Or just visit `http://teem:7777/` and use the dashboard chat panel.

`teem.yaml` fields that matter today:

- `team.id` — stable filesystem/routing key (auto-minted; never rename).
- `team.name` — display name; safe to rename.
- `tailnet.hostname` — tsnet node name. Defaults to `teem`.
- `leader.system_prompt` — the leader's brief. Folded with the
  operator override at `~/.teem/state/<team-id>/prompt-overrides/leader.md`.
- `archetypes` — list of `{role, placement, max_concurrent, ...}`.
  Standard roster: `worker`, `reviewer`, `integrator`. Add custom roles
  freely.
- `tracker:` (optional) — `{type, team_id, auth_env, poll_interval}`.
  When present the daemon synthesises a `project_manager` archetype.
- Messaging lives in a separate file, `~/.teem/messaging.yaml` — see below.

## Day to day

The dashboard at `http://teem:7777/teams/<id>` is the primary surface.
Panels:

- **Tasks** — proposed / ready / coding / reviewing / integrating / verified.
- **Agents** — roster + current job + last-seen.
- **Audit** — live event log.
- **Chat with leader** — same session as `teem chat`. Streaming.
- **Watch modal** — tail a worker's transcript live (renders the HTML
  page at `/teams/<id>/transcripts/<agent>/<job>`).
- **Settings** — pulse interval, prompt overrides, section visibility.

Task lifecycle: `proposed → ready → coding → reviewing → integrating →
verified`. `assign_job` requires `task_id` — the daemon links the
worker's audit events to the task and gates write-stage transitions.

Chat with the leader from the dashboard chat panel or by `teem chat` in
a terminal. The chat-history floor is 10 turns; older bursts are
included when relevant. Telegram bare DMs to the bot count as leader
chat turns (see below).

## Channels

When the operator is actively chatting with the leader, audit events
are pushed into the leader's claude session as `<channel>` blocks via
the `teem-channel` stdio MCP shim — `~hundreds of ms` from event to
"the leader knows". When channels aren't live (no active chat, or
upstream gate flipped), the pulse loop covers it — see
[`docs/wake-strategy.md`](docs/wake-strategy.md).

Channels are an experimental Claude Code capability and gated upstream.
Operators outside the allowlist can opt in with `TEEM_CHANNELS_DEV=1`.

## Telegram

The leader can be chatted with from your phone. Setup:

1. Get a bot token from BotFather; export it (`TEEM_TELEGRAM_TOKEN=...`).
2. Write `~/.teem/messaging.yaml`:

   ```yaml
   messaging:
     enabled: true
     telegram:
       enabled: true
       bot_token_env: TEEM_TELEGRAM_TOKEN
       chat_id: 12345678
       public_url: https://my-tailnet.ts.net   # required for auto-register
       # webhook_port defaults to main_port + 1 (7778 if dashboard on 7777)
       # webhook_port: 7788
       # funnel_via_tsnet: true                # auto-configure Tailscale Funnel
   ```

3. `teem start` — the daemon binds the dedicated webhook listener and
   auto-registers the URL with Telegram on startup.

From your phone: bare DMs = leader chat turn. `/done` ends the current
session. `/reply <token>` answers a specific task ping the leader sent
earlier (the token's in the message).

## Operator tips — look here for X

- **Daemon logs**: `~/.teem/daemon.log`.
- **Per-team state**: `~/.teem/state/<team-id>/` (plan.jsonl, audit/,
  leader-session.json, prompt-overrides/, pulse.paused, ...).
- **Worker transcripts**: `~/.teem/state/<team-id>/transcripts/<agent>/<job>.jsonl`.
  Click the transcript link in any task-detail participation log — the
  daemon renders the NDJSON as a human-readable HTML page.
- **Pulse interval / pause**: Settings panel on the dashboard, or
  `teem pulse {start,stop,pause,resume,tick,status}`.
- **Archetype prompts**: `teem agent {list,show,update} <archetype>
  [--prompt|--memory]`.

## Pointers

- [`CLAUDE.md`](CLAUDE.md) — worker quickstart, build commands, forbidden git ops.
- [`docs/getting-started.md`](docs/getting-started.md) — concepts walkthrough, pulse setup, troubleshooting.
- [`docs/dashboard-spa.md`](docs/dashboard-spa.md) — SPA architecture and API contract.
- [`docs/wake-strategy.md`](docs/wake-strategy.md) — channels / pulse layering and fallbacks.
- [`docs/project-manager.md`](docs/project-manager.md) — tracker integration design.
