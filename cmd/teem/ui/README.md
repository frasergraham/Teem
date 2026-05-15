# Teem dashboard SPA

Vite + React + TypeScript single-page app. Embedded into the `teem`
binary via `//go:embed all:ui/dist` in `cmd/teem/ui_embed.go`. Phase 1
of the migration described in `docs/dashboard-spa.md`.

## Prerequisites

- Node 20+ (Vite 5 supports Node 18+; 20+ recommended).
- npm 10+.

## Build

From the repo root:

```sh
make ui            # installs deps and writes cmd/teem/ui/dist
make build         # equivalent to `make ui && go build ./cmd/teem`
```

`go build ./cmd/teem` on its own fails on a clean checkout because the
`//go:embed all:ui/dist` directive refuses to compile if the directory
does not exist. Run `make ui` first.

## Dev server

```sh
npm install
npm run dev        # Vite dev server on http://localhost:5173
```

The dev server proxies `/api`, `/control`, and `/teams` to
`http://localhost:7777` (the daemon's default port). Start the teem
daemon separately; Vite handles HMR for the SPA, the daemon handles
data.

URL shape during phase 1: open `http://localhost:5173/teams/<team-id>/v2/`
to load the SPA pointed at a real team.

## Layout

- `index.html` — Vite entry; mounts `<div id="root"/>`.
- `src/main.tsx` — React 18 root; fetches `/api/teams/<id>/state` and
  renders `hello, <name>`.
- `src/styles/tokens.css` — minimal CSS custom properties for the
  light/dark surface colours and the Inter+system font stack. Will
  grow in phase 2 to mirror the SSR dashboard's full token set.

## What's not here yet

- Routing (single page in phase 1).
- WebSocket push and event reducers (phase 2 — see
  `docs/dashboard-spa.md` §6, §7).
- Components beyond the hello-world fetch.
- Replacement of the SSR dashboard at `/teams/<id>` — that page is
  untouched in phase 1.
