# Teem — Worker Quickstart

Teem is a Go daemon + CLI that orchestrates a small team of Claude Code worker
subprocesses against a git repo. Workers run in their own git worktrees and
report back via an HTTP audit stream.

## Layout

- `cmd/teem/` — daemon + CLI entrypoints.
- `cmd/teem/ui/` — Phase 1 SPA scaffold (Vite + React + TS), embedded via `//go:embed all:ui/dist`.
- `internal/<pkg>/` — `team`, `audit`, `messaging`, `tailnet`, `channelbus`, `plan`, `pulse`, `roster`, `executor`, `usage`, `pruner`, etc.
- `docs/` — design docs (start at `docs/getting-started.md`).

## Build & test

- Build: `make ui && go build ./cmd/teem`. `make ui` is required before any `go build` — the embed refuses to compile if `cmd/teem/ui/dist` is empty.
- Test: `make test` (depends on `ui`). Bare `go test ./...` fails on the embed from a clean checkout.

## Daemon lifecycle

`teem start` daemonises; `teem stop` graceful-shutdowns. Workers are detached subprocesses, so a daemon bounce does not kill in-flight workers.

## Tailnet & ports

The daemon runs as its own `tsnet` node named `teem` — it does NOT use the host's `tailscaled`. Dashboard at `http://teem:7777` on the tailnet. Telegram webhook on `:7778` (default = main port + 1).

## State

`~/.teem/` holds daemon state, audit logs, worker transcripts, and leader memory. Repo-root `teem.yaml` defines the team (id, archetypes, tracker, tailnet).

## Worker conventions

- Each worker runs in `~/.teem/worktrees/<team-id>/<agent-id>/` on branch `teem/<agent-id>`.
- Integrators squash-merge into `teem/integrator-<name>`. The leader fast-forwards `main` from the primary worktree.
- **Workers MUST NEVER touch `main` directly.** The only ref you may move is `refs/heads/teem/<your-name>`. Full forbidden-git-ops list lives in the leader prompt; see `docs/getting-started.md`.

## Tests-first norm

Every logic change ships with a test. Prefer extracting pure helpers (e.g. `effectiveWebhookPort`) and unit-testing them over driving the daemon end-to-end.

## Tracker

If `teem.yaml` has a `tracker:` block (e.g. Linear), a `project_manager` archetype is auto-synthesised and the leader consults it for sequencing.

## Required checks before reporting DONE

- `gofmt -l ./...` empty
- `go vet ./...` empty
- `go build ./...` clean
- `go test ./cmd/teem/... ./internal/... -race -count=1` green
