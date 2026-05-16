import { create } from 'zustand';

// Shape pinned from docs/dashboard-spa.md §6. These are deliberately
// loose where Phase 2b (t-e2da3b70) has not landed the JSON-tagged Go
// structs yet; the doc is the authoritative contract until codegen
// replaces these hand-typed mirrors.
//
// TODO(t-e2da3b70 codegen): regenerate from `dashboardTeam` json tags
// once Phase 2b lands so component subscribers get exhaustive fields.

export interface TeamMeta {
  id: string;
  name: string;
  registered_at?: string;
}

export interface AuditEvent {
  ts: string;
  agent_id: string;
  job_id?: string;
  kind: string;
  message?: string;
  meta?: Record<string, unknown>;
}

export interface Envelope {
  kind: 'audit' | 'snapshot_invalidate' | 'ping';
  seq: number;
  ts: string;
  event?: AuditEvent;
  reason?: string;
}

// Snapshot payload — see docs/dashboard-spa.md §6. Treated as a loose
// projection until Phase 2b nails the Go-side json tags; components
// that need a field reach for it through a selector and accept
// `unknown`-ish until then.
export interface StateSnapshot {
  team: TeamMeta;
  hero?: unknown;
  agents?: unknown[];
  workers?: unknown[];
  tasks?: {
    open?: unknown[];
    awaiting_approval?: unknown[];
    shelved?: unknown[];
    recent_done?: unknown[];
  };
  decisions?: unknown[];
  leader_status?: unknown;
  other_statuses?: unknown[];
  pulse?: unknown;
  usage?: unknown | null;
  branches?: { count: number; rows?: unknown[] };
  channels_state?: 'live' | 'fallback';
  now?: string;
  etag?: string;
}

export type ConnState =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'connecting'; attempt: number }
  | { kind: 'live'; seq: number; lastPingAt?: string }
  | { kind: 'reconnecting'; attempt: number; nextDelayMs: number }
  | { kind: 'error'; message: string };

export interface TeamState {
  teamID: string | null;
  snapshot: StateSnapshot | null;

  // Most-recent envelope seq we have observed (audit or ping). Used as
  // ?since_seq on reconnect so the daemon can backfill from its
  // wsbus ring buffer.
  lastSeq: number;
  lastPingAt: string | null;

  conn: ConnState;

  // Ring of the last 50 audit events, newest first. Components like
  // EventsLog (Phase 2c-ii+) will subscribe to this slice.
  events: AuditEvent[];

  // Mutators (called by dispatch.ts / ws.ts / client.ts).
  setTeamID(id: string): void;
  setSnapshot(s: StateSnapshot): void;
  setConn(c: ConnState): void;
  applyEnvelope(env: Envelope): void;
  reset(): void;
}

const eventsRingMax = 50;

export const useTeamStore = create<TeamState>((set) => ({
  teamID: null,
  snapshot: null,
  lastSeq: 0,
  lastPingAt: null,
  conn: { kind: 'idle' },
  events: [],

  setTeamID(id) {
    set({ teamID: id });
  },
  setSnapshot(s) {
    // A fresh snapshot resets lastSeq to 0 — the WebSocket will
    // backfill from the daemon's ring buffer on reconnect. The doc
    // model has the snapshot's `etag` carrying the equivalent of a
    // seq cursor; until the Go side stamps that, 0 means "give me the
    // default backfill window."
    set({ snapshot: s, lastSeq: 0 });
  },
  setConn(c) {
    set({ conn: c });
  },
  applyEnvelope(env) {
    set((state) => {
      const next: Partial<TeamState> = {};
      if (env.seq > state.lastSeq) next.lastSeq = env.seq;
      if (env.kind === 'ping') {
        next.lastPingAt = env.ts;
        if (state.conn.kind === 'live') {
          next.conn = { kind: 'live', seq: env.seq, lastPingAt: env.ts };
        }
        return next;
      }
      if (env.kind === 'audit' && env.event) {
        const ring = [env.event, ...state.events].slice(0, eventsRingMax);
        next.events = ring;
        if (state.conn.kind === 'live') {
          next.conn = {
            kind: 'live',
            seq: env.seq,
            lastPingAt: state.conn.lastPingAt,
          };
        }
        return next;
      }
      // snapshot_invalidate is handled by ws.ts (it triggers a refetch
      // + reconnect); we don't mutate state here beyond bumping seq.
      return next;
    });
  },
  reset() {
    set({
      teamID: null,
      snapshot: null,
      lastSeq: 0,
      lastPingAt: null,
      conn: { kind: 'idle' },
      events: [],
    });
  },
}));
