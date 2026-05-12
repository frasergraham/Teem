# Teem

Orchestrate Claude Code subprocesses as a coordinated team of agents. A
**Leader** Claude session chats with you and delegates work to **Worker**
Claude sessions placed on local or remote hosts. The Leader talks to
Teem through an MCP server that exposes tools for spawning agents,
assigning jobs, and inspecting bus traffic.

## Quickstart

```sh
export ANTHROPIC_API_KEY=...           # used by `teem llm ping` (Leader uses the CLI)
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
internal/team/            YAML loader + roster
internal/bus/             message bus interface + MemBus
internal/tailnet/         tsnet wrapper
internal/mcp/             orchestrator MCP server (mark3labs/mcp-go)
internal/transport/       Local + SSH process transports
internal/provisioner/     local / ssh / railway (stub) backends
internal/llm/             Anthropic SDK wrapper (for utility code paths)
internal/agent/           Worker + Spawner glue
internal/leader/          Leader runtime (transport-pluggable)
config/team.example.yaml  example team
```

## What's not built yet

- **Railway cloud provisioning** — `RailwayProvisioner` ships as a stub
  returning `ErrNotImplemented`. The Railway GraphQL client, the
  `teem-worker` container image, and a worker→leader bus transport
  (HTTP-over-tailnet) all land together in the next PR.
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

# Anthropic SDK round-trip
ANTHROPIC_API_KEY=sk-... go run ./cmd/teem llm ping --prompt "say hi"

# Full chat (joins your tailnet)
TS_AUTHKEY=tskey-... go run ./cmd/teem chat --team config/team.example.yaml

# Or without the tailnet, on 127.0.0.1
go run ./cmd/teem chat --tailnet=false --team config/team.example.yaml
```
