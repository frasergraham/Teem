# Dashboard SPA — design

> Re-implements the team dashboard as a single-page React app embedded
> in the Teem binary via `go:embed`. Replaces the 2200-line server-rendered
> template (`cmd/teem/ui_dashboard.html`) and its meta-refresh polling
> loop with a Vite + React + TypeScript bundle driven by a JSON API and
> a WebSocket push channel.

## 1. Motivation

The current dashboard is a Go `html/template` rendered server-side on
every request. To stay "live" it forces a full page reload every 10s
via `<meta http-equiv="refresh" content="10">` (see
`cmd/teem/ui_dashboard.html:666` for `summary` and `:1601` for
`team_detail`). That works as long as the user is read-only — the
moment any UI carries client-side state the reload destroys it.

Concrete pain we already have to paper over:

- **Chat focus / scroll lost on reload.** The chat panel SSE-streams
  from `POST /control/teams/<id>/chat`
  (`cmd/teem/chat_handler.go:60`), maintains an in-memory `history`
  array, and serializes the whole thing to `sessionStorage` under
  `teem.chat.<teamID>` on every turn
  (`cmd/teem/ui_dashboard.html:2089-2104`) just so a refresh
  mid-conversation doesn't blank the log. Even with that workaround,
  the input field's caret position, draft text, and scroll position
  are gone every 10s. A reply that takes longer than the next refresh
  loses its place.
- **Collapse state synthesized via storage.** Every `<details
  id="details-...">` block has a JS shim
  (`cmd/teem/ui_dashboard.html:2065-2084`) that mirrors `open` /
  `closed` into `localStorage` under `teem.ui.collapse.<id>` so the
  next refresh re-applies it. One-time migration code in that same
  block forwards legacy `sessionStorage` entries. It works but it's
  load-bearing UI plumbing for what should just be component state.
- **Approval / pulse forms are HTML POST + redirect.** Approval
  buttons post a form, the handler 303s back to `/teams/<id>?flash=...`,
  the template re-renders the whole page to show a banner. Latency is
  ≥ one full DOM rebuild on top of whatever the daemon took. Same
  pattern for `/pulse/{start,stop,config}` form posts.
- **No partial update path.** A new audit event, a stage transition,
  or a pulse tick can't surface until the next 10s tick. We already
  fan out audit events to one consumer (channelbus →
  `/teams/<id>/channel-events` SSE, see `cmd/teem/daemon.go:2680`);
  the dashboard is the natural second consumer.

Goal: kill the meta-refresh loop, kill the storage hacks, push deltas.

## 2. Stack decision

**Vite + React 18 + TypeScript**, building to static assets, no SSR.

- **Vite** for the dev-server / HMR experience and for its trivial
  static build (`dist/` is a folder of hashed `.js` / `.css` / one
  `index.html`). The build output has no runtime requirements beyond
  serving the files; nothing the Go binary can't do.
- **React 18** because we already think in components and Suspense /
  concurrent rendering matters for streaming WebSocket updates.
- **TypeScript** because the JSON contract between the daemon and the
  SPA is going to be wide (every type in `dashboardTeam`, every audit
  Kind), and codegen / hand-typed mirrors will rot fast without
  compile-time enforcement.

Considered and rejected:

- **Next.js / Remix.** Both assume a Node runtime for SSR or RSC; we
  embed in a Go binary with no Node at deploy time. Their
  static-export modes throw away the framework's reason for being.
- **Plain React + esbuild.** Doable, but we'd rebuild the dev server,
  the env-var loader, the React fast-refresh wiring, and the asset
  hashing by hand. Vite gives all of that for free.
- **htmx.** Tempting because it'd let us keep the Go templates and
  swap fragments instead of full pages. Rejected: it doesn't solve
  the chat / collapse / draft-input problem (the fragments still
  blow away DOM state inside them), and we'd still need a real
  client framework for the chat panel itself. Half-measure.

The constraint that pins this: **the build must produce a directory
of static files that `//go:embed` can pick up at compile time.** Vite
does this natively. SSR frameworks need a runtime and don't fit.

## 3. Directory layout

```
cmd/teem/
├── ui/                      # NEW — SPA source root
│   ├── package.json
│   ├── package-lock.json
│   ├── tsconfig.json
│   ├── vite.config.ts
│   ├── index.html           # Vite entry — single <div id="root"/>
│   ├── .gitignore           # node_modules/, dist/ — but see below
│   └── src/
│       ├── main.tsx         # ReactDOM.createRoot + Router
│       ├── api/             # fetch wrappers + WS client
│       │   ├── client.ts
│       │   ├── events.ts    # event envelope types
│       │   └── ws.ts
│       ├── store/           # state mgmt — see §8
│       │   ├── team.ts
│       │   └── selectors.ts
│       ├── pages/
│       │   ├── Index.tsx           # / — team tile index
│       │   ├── TeamDetail.tsx      # /teams/<id>
│       │   ├── AgentJobs.tsx       # /teams/<id>/agents/<id>/jobs
│       │   ├── JobDetail.tsx       # /teams/<id>/jobs/<id>
│       │   └── TaskFlow.tsx        # /teams/<id>/tasks/<id>
│       ├── components/      # see §9
│       │   ├── HeroPanel.tsx
│       │   ├── WorkersPanel.tsx
│       │   ├── TasksTable.tsx
│       │   ├── ChatPanel.tsx
│       │   ├── DecisionsList.tsx
│       │   ├── UsageCard.tsx
│       │   ├── PulseControls.tsx
│       │   └── ApprovalCard.tsx
│       └── styles/
│           └── tokens.css   # carries forward the existing :root vars
├── ui_embed.go              # NEW — //go:embed all:ui/dist
└── ui.go                    # existing SSR — stays during migration
```

`dist/` is committed to **not** be in `.gitignore` for the
release-tag commit only — see §4 for the CI story. During day-to-day
development it's ignored.

## 4. Build & embed

`go:embed` reads files relative to the package directory at compile
time. The plan:

**(a) Prerequisite: `npm run build`** writes `cmd/teem/ui/dist/{index.html,assets/...}`.

**(b) Glue.** A new file `cmd/teem/ui_embed.go`:

```go
package main

import "embed"

//go:embed all:ui/dist
var spaFS embed.FS
```

A `go:generate` directive at the top of the same file:

```go
//go:generate sh -c "cd ui && npm ci && npm run build"
```

Plus a `Makefile` target:

```make
ui:
	cd cmd/teem/ui && npm ci && npm run build

build: ui
	go build ./cmd/teem
```

`go build ./cmd/teem` does **not** invoke `go generate` automatically.
The two safety nets:

- Local dev: developers run `make build` (or `make ui` once and `go
  build` thereafter).
- A small `init()` in `ui_embed.go` reads `ui/dist/index.html` from
  `spaFS` and panics on startup if the bundle is missing — fast,
  obvious failure rather than "the dashboard serves a 404."

**(c) CI.** The release workflow runs `make ui` before `go build`
(or invokes `go generate ./...`). The committed `dist/` exists only
on release-tag commits so `go install
github.com/frasergraham/teem/cmd/teem@<tag>` works for users without
Node. (Alternative: don't commit `dist/`, require Node in CI and in
release builds. Decide in open question O5.)

**(d) Dev mode.** In dev, the SPA runs under Vite's dev server on
`localhost:5173` with HMR, and proxies any `/api/*`, `/control/*`,
`/audit`, `/teams/*/channel-events`, and `/teams/*/events` to the
running teem daemon (default `localhost:51000` or whatever
`endpoint` is configured to). `vite.config.ts`:

```ts
server: {
  proxy: {
    '/api':     { target: 'http://localhost:51000', ws: true },
    '/control': { target: 'http://localhost:51000' },
    '/teams':   { target: 'http://localhost:51000', ws: true },
  }
}
```

The Go daemon doesn't know it's being proxied; the SPA sees a single
origin.

## 5. Routing model

The current `cmd/teem/daemon.go:handleTeamRoute` (line 2462) dispatches
five SSR routes plus several action-form POSTs. The SPA mirrors the
read paths with React Router and keeps the action POSTs as JSON
endpoints.

| Current URL                             | SSR handler                        | New SPA route                | New JSON endpoint                              |
| --------------------------------------- | ---------------------------------- | ---------------------------- | ---------------------------------------------- |
| `/` , `/ui`                             | `daemon.renderDashboard`           | `/`                          | `GET /api/teams` (lightweight summary)         |
| `/teams/<id>`                           | `daemon.renderTeamPage`            | `/teams/:id`                 | `GET /api/teams/:id/state`                     |
| `/teams/<id>/agents/<aid>/jobs`         | `daemon.renderAgentJobs`           | `/teams/:id/agents/:aid/jobs`| `GET /api/teams/:id/agents/:aid/jobs`          |
| `/teams/<id>/jobs/<jid>`                | `daemon.renderJobDetail`           | `/teams/:id/jobs/:jid`       | `GET /api/teams/:id/jobs/:jid`                 |
| `/teams/<id>/tasks/<tid>`               | `daemon.renderTaskFlow`            | `/teams/:id/tasks/:tid`      | `GET /api/teams/:id/tasks/:tid`                |
| `POST /tasks/<tid>/{approve,reject,..}` | `handleTaskActionForm`             | (in-page mutation)           | `POST /api/teams/:id/tasks/:tid/:action` (JSON)|
| `POST /decisions/<tid>/{reply,..}`      | `handleDecisionActionForm`         | (in-page mutation)           | `POST /api/teams/:id/decisions/:tid/:action`   |
| `POST /control/.../pulse/{start,..}`    | `handlePulseControl`               | (in-page mutation)           | reuse current endpoint, return JSON not 303    |
| `POST /control/.../chat`                | `handleChatTeam`                   | reuse                        | reuse — already SSE, see §9 chat panel         |
| `GET /teams/<id>/channel-events`        | `handleChannelEvents`              | (consumer remains the shim)  | unchanged                                      |
| `GET /teams/<id>/events`                | n/a — NEW                          | WS source                    | **new** WebSocket; see §6, §7                  |

**What stays SSR.** The `/` and `/teams/<id>` HTML handlers stay
mounted through phase 4 of the migration (see §10) and serve the
unchanged Go template. The SPA mounts under `/teams/<id>/v2` during
phase 1, then takes the canonical URLs in phase 3.

**What becomes JSON-only.** The per-page state endpoints in the table
above are net-new; they read from the same sources the current
`teamSnapshot` reads from (`rt.registry.List()`, `rt.plan.List`,
`rt.auditSink`, `rt.pulse`, the `usage.Aggregator`) and return the
already-shaped Go structs — `dashboardTeam`, `awaitingApprovalTask`,
`decisionRow`, `usageSnapshot`, `pulseSnapshot`, `workerRow`, etc. —
serialized straight to JSON. The structs already have `json:` tags
where they matter; we make those exhaustive and the Go shapes become
the API contract.

## 6. Data layer / API

Two-channel design: **one big snapshot fetch on page load**, then
**deltas over WebSocket**.

### Snapshot — `GET /api/teams/:id/state`

Returns everything the team-detail page needs in one round trip:

```jsonc
{
  "team":      { "id": "t-abc", "name": "main", "registered_at": "..." },
  "hero":      { /* teamHero */ },
  "agents":    [ /* dashboardAgent */ ],
  "workers":   [ /* workerRow */ ],
  "tasks": {
    "open":              [ /* dashboardTask */ ],
    "awaiting_approval": [ /* awaitingApprovalTask */ ],
    "shelved":           [ /* dashboardTask */ ],
    "recent_done":       [ /* dashboardTask */ ]
  },
  "decisions": [ /* decisionRow */ ],
  "leader_status": { "text": "...", "updated_at": "...", "agent_id": "leader" },
  "other_statuses": [ /* leaderRow */ ],
  "pulse":     { /* pulseSnapshot */ },
  "usage":     null | { /* usageSnapshot */ },
  "branches":  { "count": 3, "rows": [ /* dashboardBranch */ ] },
  "channels_state": "live" | "fallback",
  "now":       "2026-05-15T20:55:00Z",
  "etag":      "sha256:abc123…"   // see §7 reconnect
}
```

The shape is the JSON projection of `dashboardTeam` plus a couple of
top-level fields. Implementation reuses `teamSnapshot` verbatim,
adding `json:` tags where missing.

### Deltas — `GET /api/teams/:id/events` (WebSocket)

After the initial snapshot the client opens a WebSocket. Every event
delivered is one of a small set of typed envelopes:

```ts
type Envelope =
  | { kind: "audit",              ts: string, seq: number, event: AuditEvent }
  | { kind: "snapshot_invalidate", ts: string, seq: number, reason: string }
  | { kind: "ping",               ts: string, seq: number }
```

The `event` payload for `kind: "audit"` is the existing
`audit.Event` (from `internal/audit/audit.go:121`) — `agent_id`,
`job_id`, `kind`, `message`, `meta` — and the SPA store maps known
audit `Kind`s to UI state changes:

- `task_stage_changed` → patch task in `tasks.*`
- `decision_note` (severity=question) → add to `decisions`
- `blocker_note` → add to `decisions` and set task stage=blocked
- `job_received` / `job_complete` / `job_error` /
  `job_interrupted` / `job_transcript_ready` → update worker
  in-flight state, recent events list
- `heartbeat` → bump agent `LastSeen`
- `worker_stopped` → drop worker from manifest
- `channels_state` → update top-of-page channels indicator
- `pm_tick` → log line in events panel, no UI change
- `usage_event` → patch `usage.Used` and per-model breakdown
- `usage_throttle` → set/clear throttle banner

Kinds we don't recognise are appended to the recent-events list as
opaque rows.

`snapshot_invalidate` is the daemon's "I lost confidence that your
projection is correct, refetch" signal — emitted, for instance, when
a team is unregistered and re-registered, or when the daemon
restarted between this connection and the previous one. Client
behaviour: re-call `GET /api/teams/:id/state`.

`ping` is a 30s keepalive carrying the latest `seq`. The client uses
this to detect missed events on reconnect (compare last seen `seq`
against the most recent ping's `seq`).

## 7. WebSocket push

The fan-out plumbing is the parallel of the existing channelbus
(`internal/channelbus/channelbus.go`). That bus has the shape we
want — `Subscribe()` returns `(id, <-chan Event, cancel)`, `Publish`
non-blocking with per-listener drop and rate-limited drop logging —
but its `Event` type is the Claude Code channel notification, not
audit. We add a sibling.

**Proposed package: `internal/wsbus`**

```go
package wsbus

type Envelope struct {
    Kind  string         `json:"kind"`     // "audit" | "snapshot_invalidate" | "ping"
    Seq   uint64         `json:"seq"`
    TS    time.Time      `json:"ts"`
    Event *audit.Event   `json:"event,omitempty"`
    Reason string        `json:"reason,omitempty"`
}

type Bus struct { /* same shape as channelbus.Bus */ }

func (b *Bus) Subscribe() (id int, ch <-chan Envelope, cancel func())
func (b *Bus) Publish(e Envelope)
func (b *Bus) Recent(n int) []Envelope   // ring buffer of the last N envelopes
```

The daemon wires it in the same place it already wires channelbus:
inside `registeredTeam`. The audit hook chain
(`newAuditHandlerWithHooks` in `cmd/teem/daemon.go:2379`) gets one
more hook that wraps the incoming `audit.Event` in an `Envelope`,
stamps a monotonic `Seq`, and `Publish`es to `rt.wsbus`. The chat
handler, pulse loop, and PM loop already route through `auditSink`,
so any audit hook sees them; we don't need bespoke publish calls.

**HTTP endpoint: `GET /teams/<id>/events`** (mounted in
`handleTeamRoute`, line 2480 area). Upgrades to WebSocket via
`golang.org/x/net/websocket` or `nhooyr.io/websocket` (operator
choice — see open question O4). Connection lifecycle:

1. Validate auth. Same scheme as the chat panel and `/ping`:
   tailnet boundary, no bearer required — the dashboard already
   relies on this. The endpoint refuses external (non-tailnet)
   origins via the daemon's existing localhost-bind detection. (If
   we ever move past tailnet boundary, this is one of three places
   that needs auth — see open question O3.)
2. On connect, if the client sent `?since_seq=<N>`, the server
   walks `wsbus.Recent(...)` and replays envelopes with `Seq > N`
   before going live. If `N` is older than the ring buffer's
   horizon, send `snapshot_invalidate` instead.
3. On connect with no `since_seq`, replay the last 50 envelopes so
   the events panel has immediate content.
4. Stream live envelopes until client disconnect or `Bus.Publish`
   drops us for being slow (same drop policy as channelbus —
   wedged listener doesn't back-pressure the publisher).
5. Server sends `{ kind: "ping" }` every 30s to keep idle
   connections through tailnet / proxy idle timeouts and to give
   clients a clean "I'm caught up at seq=N" handshake.

**Reconnect.** Client wraps its `WebSocket` in a small reconnect
loop with exponential backoff (1s / 2s / 4s / max 30s) plus jitter.
On each reconnect it sends `?since_seq=<last seen seq>` so the
server can either backfill or invalidate.

**Backfill horizon.** Ring buffer sized for ~1h of activity at
nominal load (a couple thousand envelopes). After that horizon a
reconnect gets `snapshot_invalidate` and refetches `/state`. This
matches the channelbus approach of "favour the wake-signal, drop
replay completeness."

## 8. State management

**Zustand.** One store per team, mounted at `<TeamDetail
teamId="..."/>` mount. Reasons:

- Audit-event-driven updates are dozens of small patches per
  minute. Redux's reducer-per-action ergonomics buy us nothing here
  beyond ceremony; we want `store.setState(s => patchTask(s, ev))`
  written in one place.
- The store is the natural seat for the WebSocket subscriber:
  `useTeamStore.getState().wsConnect(teamId)` opens the socket once
  per mount and dispatches into `setState`.
- Optimistic updates fit cleanly: a `markAwaitingApprovalApproved`
  action sets a local `pendingAction` flag, fires the POST, then
  either lets the server's `task_stage_changed` audit envelope
  confirm the change (clearing `pendingAction`) or reverts on POST
  error.

Shape sketch:

```ts
interface TeamState {
  team: TeamMeta | null
  agents: Agent[]
  workers: WorkerRow[]
  tasks: { open: Task[]; awaiting: AwaitingApprovalTask[]; shelved: Task[]; recentDone: Task[] }
  decisions: DecisionRow[]
  pulse: PulseSnapshot
  usage: UsageSnapshot | null
  events: AuditEvent[]      // ring of last 50
  channelsState: 'live' | 'fallback'
  pending: Record<string, { kind: 'approve'|'reject'|'comment'; at: number }>
  wsSeq: number
  wsConnected: boolean
}
```

Selectors live in `store/selectors.ts` so components subscribe at
the slice they care about — `<TasksTable>` doesn't re-render on a
`usage_event`.

Considered and rejected:

- **React Context + useReducer.** Fine until ~5 components share
  the store; then re-renders fan out and we'd be writing
  custom selectors anyway. Zustand gets us there from the start.
- **Redux Toolkit.** Heavier; the action-creator boilerplate is
  paying for tooling (devtools, time travel) we don't currently
  need on a single-machine internal tool.

## 9. Component breakdown

The team-detail page composition mirrors the SSR layout, one
component per existing visual section. Store-slice subscriptions
are listed alongside.

| Component         | Subscribes to                                          | Notes                                                                                |
| ----------------- | ------------------------------------------------------ | ------------------------------------------------------------------------------------ |
| `DashboardLayout` | `team`, `wsConnected`                                  | Shell: header, connection-lost banner, footer.                                       |
| `HeroPanel`       | `agents`, `tasks.open`, `pulse`, `usage`               | The big-numbers band — counters and stage-bar (`teamHero` today).                    |
| `WorkersPanel`    | `workers`, `agents`                                    | "Active workers" manifest. Updates on `heartbeat`, `job_received`, `worker_stopped`. |
| `TasksTable`      | `tasks.open`, `tasks.shelved`, `tasks.recentDone`      | Open / shelved / recent rows. Stage pills via the existing CSS class names.          |
| `DecisionsList`   | `tasks.awaiting`, `decisions`                          | Unified operator-action panel (approval + question + blocker). Hosts `ApprovalCard`. |
| `ApprovalCard`    | one `AwaitingApprovalTask` via prop                    | Plan-artifact rendering, APPROVE / REJECT / COMMENT — POSTs to JSON endpoint.        |
| `ChatPanel`       | local component state + POST `/control/.../chat`       | Same SSE stream as today; this time chat history lives in component state and       |
|                   |                                                        | survives WS reconnects / page navigation within the SPA without storage hacks.       |
| `UsageCard`       | `usage`                                                | Today's token bar + per-model breakdown. Throttle banner on `usage_throttle`.        |
| `PulseControls`   | `pulse`                                                | Lamp toggle + interval input + wake-prompt textarea. POSTs to existing endpoints.    |
| `EventsLog`       | `events` (last 50)                                     | Bottom-of-page raw audit feed. Auto-scrolls unless user scrolled away.               |
| `BranchesPanel`   | `branches` (initial fetch only; no WS-driven updates)  | Static-ish; refetched on `snapshot_invalidate`.                                      |

**Chat panel.** This is the headline UX win. The current panel
already speaks SSE (`cmd/teem/ui_dashboard.html:2086`+). In the SPA
we drop the `sessionStorage` mirror — the panel is just a mounted
React component carrying its own `messages: ChatMessage[]` state,
and the surrounding store doesn't churn it on every audit event.
The Cmd/Ctrl+Enter binding and pending-state styling port directly.

**Approval optimistic update.** Operator clicks APPROVE:
`store.optimisticApprove(taskId)` flips `pending[taskId] = 'approve'`
and dims the card. The POST returns 200; we wait for the
`task_stage_changed` envelope (or a 5s timeout) to remove the card
from `tasks.awaiting`. POST failure reverts the dim.

## 10. Migration strategy

Four sequential phases, each a follow-up task. This doc ships none
of them — it only specifies them.

**Phase 1 — Scaffold (follow-up: `t-XX`).**
Stand up `cmd/teem/ui/` with Vite + React + TS, wire `go:embed all:ui/dist`,
mount the SPA at `/teams/<id>/v2`. SPA renders a one-line "hello"
and the team name fetched from a stub `/api/teams/<id>/state` that
returns `{ team: { id, name }}`. Goal: prove the build chain, the
embed wiring, and the dev-server proxy. No feature parity.

**Phase 2 — Feature parity (follow-up).**
Build out `/api/teams/<id>/state` to return the full
`dashboardTeam`-equivalent payload, ship the WebSocket push at
`/api/teams/<id>/events`, port every component listed in §9, and
verify side-by-side against the SSR page. The old page stays at
`/teams/<id>`; the SPA lives at `/teams/<id>/v2`. Operators flip
between the two via a link in the header until parity is signed
off.

**Phase 3 — Swap the default (follow-up).**
The SPA takes `/teams/<id>`. The SSR template moves to
`/teams/<id>/legacy` for one release as the bailout. The
`meta-refresh` lines disappear.

**Phase 4 — Delete the legacy (follow-up).**
Remove `cmd/teem/ui_dashboard.html`, `ui_agent_jobs.html`,
`ui_job_detail.html`, `ui_task_flow.html`, the `renderDashboard` /
`renderTeamPage` / `renderAgentJobs` / `renderJobDetail` /
`renderTaskFlow` handlers, and the `html/template` parsing in
`cmd/teem/ui.go`. Form-action handlers (`handleTaskActionForm`,
`handleDecisionActionForm`, `handlePulseControl`) become
JSON-shaped where they aren't already.

Each phase is independently revertable. Phase 1 is risk-free (the
new route doesn't shadow anything). Phase 3 is the one where we
notice if the SPA missed a corner case, hence the one-release
legacy bailout.

## 11. Open questions

These need an operator decision before phase 1 starts.

- **O1. Theme strategy.** Today's CSS keeps colour tokens as
  `--bg` / `--fg` / `--accent` etc., and `@media (prefers-color-scheme:
  dark)` swaps them (`cmd/teem/ui_dashboard.html:11-43`). Question:
  in the SPA, do we (a) keep the same CSS-variables-only approach
  and let `prefers-color-scheme` continue to drive it, (b) add a
  JS-toggled `data-theme="dark"` class so we can support a manual
  override, or (c) something else? Recommendation: (a) for phase
  1, (b) when manual override is requested.

- **O2. Fonts.** The current dashboard uses `system-ui` and JetBrains
  Mono (with Google Fonts loaded inline). The bridge-console redesign
  draft (`docs/dashboard-redesign.html`) loads Fraunces + Bricolage
  Grotesque + JetBrains Mono. Question: which font stack does the
  SPA carry forward? Recommendation: ship Phase 1 with `system-ui` to
  avoid coupling the SPA work to the redesign work; let typography
  land separately.

- **O3. Auth.** The existing dashboard is unauth on the tailnet
  boundary (`cmd/teem/daemon.go:630-635`). The new `/api/*` and
  `/api/teams/:id/events` WebSocket inherit that boundary, but
  WebSockets cross more proxies and the failure mode is silent. Do
  we (a) keep tailnet-only and document it, or (b) gate `/api/*`
  behind the existing bearer token (which the dashboard would need
  a session cookie to carry)? Recommendation: (a) for parity, with
  a TODO to revisit when we add multi-user access.

- **O4. WebSocket library.** `nhooyr.io/websocket` (single-author,
  small, modern context-aware API) vs `gorilla/websocket` (de facto
  standard, larger surface). Recommendation: `nhooyr.io/websocket`
  — context-aware reads / writes line up with our existing daemon
  context handling and the API is smaller to wrap.

- **O5. `dist/` committed or built in CI?** Committing keeps `go
  install @<tag>` working without Node. Not committing avoids
  generated-file churn in PRs. Recommendation: commit `dist/` only
  on release-tag commits via a release workflow that builds and
  amends; ignore `dist/` on `main`.

- **O6. Collapse-state persistence.** The current dashboard saves
  every `<details>` open/closed state to `localStorage` so it
  survives the 10s refresh (`ui_dashboard.html:2065-2084`). In the
  SPA there's no refresh, so the state survives natively in
  component state. Question: do we keep `localStorage` so the
  preference survives a hard reload or browser restart, or treat
  collapse as ephemeral? Recommendation: keep `localStorage` keyed
  the same way (`teem.ui.collapse.<id>`) — operator muscle memory.
