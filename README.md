# Teem

Orchestrate Claude Code subprocesses as a coordinated team of agents. A
**Leader** Claude session chats with you and delegates work to **Worker**
Claude sessions placed on local or remote hosts. The Leader talks to
Teem through an MCP server that exposes tools for spawning agents,
assigning jobs, and inspecting bus traffic.

## Getting started

```sh
teem init
```

The wizard asks for a team name, then offers one-keystroke defaults: the
standard archetypes (**worker**, **reviewer**, **integrator**, all local)
and a leader brief that frames the leader as a delegator. If a
`CLAUDE.md` (or `.claude/CLAUDE.md`) is present in the current directory,
its contents are folded into the leader brief so the leader inherits your
project conventions. Decline the defaults to walk through the custom
archetype builder, or edit the resulting `teem.yaml` directly afterwards.

## Quickstart

```sh
export ANTHROPIC_API_KEY=...           # optional; enables archmem role digests
export TS_AUTHKEY=tskey-...            # tailnet auth key (or skip; tsnet prints a login URL)

go run ./cmd/teem chat --team config/team.example.yaml
> who is on your team?
> ask backend to print its working directory
```

`Ctrl-D` to quit.

For local dev without a tailnet:

```sh
go run ./cmd/teem chat --tailnet=false
```

To run the Leader on another machine and chat as if it were local:

```sh
go run ./cmd/teem chat --leader-host=user@dev-box
```

The terminal feels identical — same prompt, same streaming assistant
text — because the chat UI only talks to the `Leader` interface, not the
process behind it. `claude -p --input-format stream-json --output-format
stream-json` is the Anthropic Agent SDK exposed as a stdio process; the
SSH transport just relays that stdio.

## Architecture

```
+----------------+        stream-json stdio        +----------------+
|  Terminal REPL | <-----------------------------> | Leader (claude)|
+----------------+                                  +-------+--------+
                                                            | MCP/HTTP
                                                            v
+-------------+ tools (spawn/assign/...) +---------------------------+
|  Operator   | -----------------------> | Orchestrator MCP server   |
+-------------+                          | (internal/mcp)            |
                                          +-----------+---------------+
                                                      |
                          +---------------------------+--------+
                          |                                    |
                          v                                    v
                +-------------------+               +----------------------+
                |  Spawner          |  publishes    |  In-process bus      |
                |  (internal/agent) | ------------> |  (internal/bus)      |
                +---------+---------+               +----------+-----------+
                          | provisions                          | subscribe
                          v                                     |
                  +-------+--------+         +------------------+
                  | Provisioner    |         |  Worker goroutines
                  | local / ssh /  |         |  each owns a claude -p
                  | railway (stub) |         |  subprocess via Transport
                  +----------------+         +-------------------------------+
```

All hosts (Leader + workers) sit on the user's tailnet, joined by an
embedded `tsnet` node so no system-wide `tailscaled` install is needed.

## Repository layout

```
cmd/teem/                 CLI entry (chat, llm ping, version)
cmd/teem-worker/          Daemon that runs inside Fargate worker containers
internal/team/            YAML loader + roster
internal/bus/             message bus interface + MemBus
internal/tailnet/         tsnet wrapper
internal/mcp/             orchestrator MCP server (mark3labs/mcp-go)
internal/transport/       Local + SSH process transports
internal/executor/        ProcessExecutor (local/ssh) + HTTPExecutor (cloud)
internal/provisioner/     local / ssh / fargate backends
internal/llm/             Anthropic SDK wrapper (for utility code paths)
internal/agent/           Worker + Spawner glue
internal/leader/          Leader runtime (transport-pluggable)
Dockerfile                Builds the teem-worker container image
config/team.example.yaml  example team
```

## Local agents and git worktrees

Local agents run inside per-agent git worktrees branched off the leader's
repo. Each spawn creates a worktree at
`~/.teem/worktrees/<team-slug>/<agent-id>` on a new branch
`teem/<agent-id>`, branched from the leader's current `HEAD`. The branch
survives shutdown so the agent's work is a reviewable artifact across
sessions; the worktree itself is removed on `Stop` / Ctrl-D.

Notes:

- The leader must be running inside a git repo. If it isn't, local-agent
  spawns fail with a clear message unless the agent's YAML supplies an
  explicit `working_dir`.
- Uncommitted changes in the leader's checkout are **not** carried over.
  Worktrees branch off the committed HEAD; commit or stash first if you
  want the agent to see in-progress work.
- Setting `working_dir` in YAML opts out — the agent runs in that path
  raw, with no worktree, matching the pre-worktree behavior.
- Submodules are not auto-initialized inside agent worktrees.
- Re-spawning the same agent id reuses its branch (the agent picks up
  where it left off).

## Cloud agents (ECS Fargate)

Agents tagged `backend: fargate` in the team YAML are spun up on demand as
ephemeral Fargate tasks that join your tailnet and accept jobs over HTTP.

The leader-side flow is:

1. Leader calls `spawn_agent` over MCP. Teem returns the agent id
   immediately with state `provisioning`.
2. Teem calls ECS `RunTask` with per-agent env overrides
   (`TEEM_AGENT_ID`, `TEEM_AGENT_ROLE`, `TEEM_WORKER_HOSTNAME=teem-<id>`,
   `TEEM_WORKER_TOKEN`, `TS_AUTHKEY`, `ANTHROPIC_API_KEY`), polls
   `DescribeTasks` until `RUNNING`, and flips the registry state to
   `running`.
3. `assign_job` publishes to the bus as usual; the worker's HTTPExecutor
   `POST`s the job to `http://teem-<id>:7780/jobs` and long-polls
   `GET /jobs/{id}?wait=30s` for the result. A 15s `GET /healthz`
   watchdog catches unreachable workers and surfaces `worker unreachable`
   on the job. A separate 15s `DescribeTasks` poll flips the registry to
   `stopped` if Fargate kills the task.
4. `Stop()` on shutdown calls ECS `StopTask` for every provisioned task.

### One-time AWS setup

You need (click-ops is fine for v1):

- An ECR repo (or another registry) hosting the `teem-worker` image
  built from `./Dockerfile`. Push with the usual `docker build` /
  `docker push` flow.
- An **ECS cluster** (Fargate-capable).
- A **task definition** referencing your image. Network mode `awsvpc`,
  Fargate compatibility, container name `teem-worker` (or set
  `TEEM_ECS_CONTAINER_NAME`). The container does not need to declare any
  env vars — Teem injects them via `RunTask` overrides.
- A **VPC public subnet** (single AZ) and a **security group** allowing
  outbound 443 (Anthropic API, ECR pulls, Tailscale coordination) and
  outbound UDP 41641 (Tailscale data plane). No inbound rules required —
  Tailscale handles connectivity.
- **Task execution role** with the AWS-managed
  `AmazonECSTaskExecutionRolePolicy` (pulls images, writes logs).
- **Task role** — no extra permissions needed unless your worker MCPs
  call AWS.

### TS_AUTHKEY flags

The auth key Teem injects into each worker must be **ephemeral,
reusable, and preauthorized**. Otherwise dead tasks pile up as tailnet
devices and you'll hit your device limit fast.

### Required leader env

```sh
export AWS_REGION=us-west-2
export TEEM_ECS_CLUSTER=teem
export TEEM_ECS_TASK_DEF=teem-worker:1          # family[:revision]
export TEEM_ECS_SUBNETS=subnet-aaaa             # csv
export TEEM_ECS_SECURITY_GROUPS=sg-bbbb         # csv
export TS_AUTHKEY=tskey-...                     # ephemeral+reusable+preauth
export ANTHROPIC_API_KEY=sk-...                 # injected into workers
# optional:
# export TEEM_WORKER_TOKEN=...                  # auto-generated if unset
# export TEEM_ECS_CONTAINER_NAME=teem-worker
# export TEEM_ECS_ASSIGN_PUBLIC_IP=true
# export TEEM_ECS_PROVISION_TIMEOUT=5m
```

### Source-control credentials for remote workers

Remote workers can't share the leader's local checkout, so they clone
the repo themselves on startup. Configure once at the leader; the
provisioner ships these to every worker container:

```sh
export TEEM_GIT_REPO_URL=https://github.com/owner/repo.git
export TEEM_GIT_TOKEN=ghp_...                   # PAT with repo:rw
# optional:
# export TEEM_GIT_USERNAME=x-access-token       # GitHub PAT convention (default)
# export TEEM_GIT_AUTHOR_NAME="Teem Agent"
# export TEEM_GIT_AUTHOR_EMAIL=teem-agent@noreply.local
# export TEEM_GIT_BRANCH_PREFIX=teem/           # default
# export TEEM_GIT_AUTO_PUSH=true                # default; pushes the agent's branch after each successful job
```

On startup the worker daemon:

1. Clones `$TEEM_GIT_REPO_URL` into `$TEEM_WORKER_WORKDIR/repo`.
2. Writes a credential helper script that reads `$TEEM_GIT_TOKEN` from
   env at request time (the token is never written to disk).
3. Configures local `user.name` / `user.email`.
4. Creates or checks out `teem/<agent-id>` from the default branch (or
   resumes an existing remote branch with the same name).
5. After every successful job, runs `git push -u origin teem/<agent-id>`
   so an ephemeral container's work survives teardown. Disable with
   `TEEM_GIT_AUTO_PUSH=false`.

Local agents are unaffected — they share the leader's repo via a git
worktree and inherit the operator's existing git credentials.

**Token handling**: `TEEM_GIT_TOKEN` passes through ECS `RunTask`
container overrides, so it shows up in CloudTrail and the ECS console
(same caveat as `ANTHROPIC_API_KEY`). Move both to Secrets Manager refs
on the task definition for production.

Then start chat normally:

```sh
teem chat --team config/team.example.yaml
> spawn the researcher and ask it to print "hello from fargate"
```

### Known issues / follow-ups

- `ANTHROPIC_API_KEY` is passed via `RunTask` container overrides, so it
  appears in CloudTrail and the ECS console. Move to Secrets Manager
  refs in the task definition for production use.
- The worker image is pulled from your registry on every task start; if
  you use a public registry, expect 10–30s of extra cold start. ECR with
  pull-through cache or a regional ECR repo is the production answer.
- We use `LaunchType=FARGATE` (on-demand) rather than Fargate Spot,
  because spot interruption mid-claude-job loses the work. Add a
  checkpointing story before flipping.

## Persistent vs ephemeral workers

Every agent has a lifecycle:

- **ephemeral** (default): the worker's placement is owned by the
  current `teem chat` session. Local agents get a worktree created and
  torn down per session; cloud agents get a Fargate task launched on
  spawn and stopped on shutdown.
- **persistent**: the placement outlives the leader. Local persistent
  workers are processes you start yourself; cloud persistent workers
  are tasks Teem launches once and reuses across sessions.

Set it on the agent in YAML:

```yaml
- id: bg-1
  role: background
  local: true
  lifecycle: persistent
```

### Where persistent state lives

`~/.teem/state/<team-slug>/<agent-id>.json` — one file per persistent
agent. Cloud agents record their task ARN here so the next `teem chat`
can reconcile against ECS `DescribeTasks` and reuse the running task.
Stale records (task STOPPED, or gone) are dropped automatically; the
next spawn launches a fresh task.

### Running a persistent local worker

You're responsible for starting the `teem-worker` daemon yourself.
Concretely, in another terminal (or under launchd/systemd):

```sh
TEEM_AGENT_ID=bg-1 \
TEEM_AGENT_ROLE=background \
TEEM_WORKER_HOSTNAME=teem-bg-1 \
TEEM_WORKER_TOKEN=<same token as the leader> \
TS_AUTHKEY=tskey-... \
ANTHROPIC_API_KEY=sk-... \
teem-worker
```

The leader and worker need to share `TEEM_WORKER_TOKEN`. Set it on the
leader env before `teem chat`, copy the same value into the worker's
env. (When unset, the leader auto-generates one per session — fine for
ephemeral, but for persistent workers you'll want a stable value in
both places.)

### Reconcile on startup

When `teem chat` starts, it tries to reconnect each persistent agent
listed in the team YAML:

1. Probes the worker at `http://teem-<id>:7780/healthz` over the
   tailnet.
2. On success: registers the agent as `running` in the registry — the
   leader's Claude sees it via `list_agents` without an explicit
   spawn.
3. On failure: the agent is left in the unregistered pool. The leader
   can still `spawn_agent` it; for persistent Fargate this reuses an
   existing live task (state lookup) or launches a fresh one.

Persistent agents are skipped in the shutdown teardown loop — that's
the whole point.

## What's not built yet

- **In-process Agent SDK Leader** (`SDKLeader`) — calls the Python or
  TypeScript `claude-agent-sdk` library directly. Stubbed; the CLI-backed
  `ClaudeLeader` already handles "Leader runs remotely, feels local" via
  the SSH transport.
- **Leader-hosted HTTP file server** for sharing context between
  workers. The tailnet itself is in place; the file-server endpoints
  come next.
- **File-backed bus** and on-disk job state — today everything is
  in-process channels.
- **SSH transport hardening** — v1 uses `SSH_AUTH_SOCK` only and
  `InsecureIgnoreHostKey()`. Production needs known-hosts wiring.
- **Web UI** — terminal REPL only for now.

## Verification

```sh
go vet ./...
go build ./...
go test ./...

# Full chat (joins your tailnet)
TS_AUTHKEY=tskey-... go run ./cmd/teem chat --team config/team.example.yaml

# Or without the tailnet, on 127.0.0.1
go run ./cmd/teem chat --tailnet=false --team config/team.example.yaml
```
