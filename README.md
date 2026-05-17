# Teem

Teem runs a small team of Claude Code subprocesses against a git repo.
One **leader** chats with you and delegates work to **workers**,
**reviewers**, and an **integrator** — each in its own git worktree.
You watch what's happening from a dashboard or chat with the leader
from your phone.

## What you need

- A git repo you want a team to work in.
- [Claude Code](https://docs.claude.com/en/docs/claude-code) on your PATH (`claude --version`) and signed in. Workers use your subscription.
- [Tailscale](https://tailscale.com/) installed and signed in. Teem joins your tailnet so the dashboard and phone access work without exposing anything to the public internet.
- Go 1.22+ to build.

## Install

```sh
git clone https://github.com/frasergraham/teem
cd teem
make ui && go install ./cmd/teem
```

`make ui` builds the dashboard into the embedded SPA. A bare
`go build` won't work on a clean checkout until that runs once.

## Run a team

In the repo you want a team to work on:

```sh
cd ~/code/my-project
teem init     # writes ./teem.yaml and installs the orchestration skill
teem start    # daemonises in the background
teem chat     # opens Claude Code attached to the leader
```

`teem start` joins your tailnet as a node called `teem` and serves the
dashboard at `http://teem:7777/`. Open that in a browser on any
Tailscale-connected machine and you'll see the team status, worker
roster, task list, and a chat panel.

`teem stop` shuts the daemon down. In-flight workers are detached
subprocesses and keep running across a restart.

## Telegram (optional)

The leader can be chatted with from your phone. Useful when you're
away from the laptop.

1. Talk to [@BotFather](https://t.me/BotFather), create a bot, get the token.
2. Find your Telegram chat id by messaging [@userinfobot](https://t.me/userinfobot).
3. Export the token: `export TEEM_TELEGRAM_TOKEN=...` (in your shell rc).
4. Write `~/.teem/messaging.yaml`:

   ```yaml
   messaging:
     enabled: true
     telegram:
       enabled: true
       bot_token_env: TEEM_TELEGRAM_TOKEN
       chat_id: 12345678
       funnel_via_tsnet: true
   ```

5. Enable Funnel on the `teem` node in the [Tailscale admin](https://login.tailscale.com/admin/machines) (so Telegram can deliver webhooks to it).
6. `teem stop && teem start` — the daemon registers the webhook with Telegram automatically.

From your phone:

- Plain message → leader chat turn.
- `/done` → end the current session.
- `/reply <token>` → reply to a specific notification (token is included in the message).

## Remote workers (untested)

The archetype config supports SSH and Fargate placements for running
workers on other machines. That code path exists but is not currently
exercised — assume it's broken until proven otherwise. Stick to
`placement: local` for now.

## More

- [`CLAUDE.md`](CLAUDE.md) — conventions for workers and contributors.
- [`docs/getting-started.md`](docs/getting-started.md) — deeper walkthrough.
- [`docs/`](docs/) — design notes for specific subsystems.
