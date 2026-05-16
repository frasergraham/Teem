import { create } from 'zustand';

// Shape mirrors cmd/teem/api_state.go (apiTeamStatePayload) and the
// dashboardTeam / hero / worker / task structs in cmd/teem/ui.go. Phase
// 2c-ii adds enough typed fields for HeroPanel / WorkersPanel /
// TasksTable to render without `unknown` casts. Anything the components
// don't read yet stays loose.

export interface TeamMeta {
  id: string;
  name: string;
  registered_ago?: string;
}

export interface AgentChip {
  role: string;
  count: number;
}

export interface StageBarSegment {
  stage: string;
  count: number;
  width_pct: number;
  color_hex: string;
  task_ids?: string[];
  task_id_list?: string;
}

export interface TeamHero {
  active_agents_total: number;
  open_tasks_total: number;
  agent_chips: AgentChip[];
  stage_bar: StageBarSegment[];
  has_stage_activity: boolean;
}

export interface Worker {
  agent_id: string;
  persona: string;
  role: string;
  role_tag: string;
  role_colour_class: string;
  activity: string;
  age: string;
}

export interface DashboardTask {
  id: string;
  title: string;
  status: string;
  stage: string;
  stage_ago: string;
  assigned_to: string;
  assignee_active?: boolean;
  assignee_derived?: boolean;
  stale?: boolean;
  url?: string;
}

export interface TaskBuckets {
  open: DashboardTask[];
  awaiting_approval?: unknown[];
  shelved?: DashboardTask[];
  recent_done?: DashboardTask[];
}

export interface LeaderStatus {
  agent_id: string;
  text: string;
  updated_ago: string;
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

export interface StateSnapshot {
  team: TeamMeta;
  hero: TeamHero;
  agents?: unknown[];
  workers: Worker[];
  tasks: TaskBuckets;
  decisions?: unknown[];
  leader_status: LeaderStatus | null;
  other_statuses?: unknown[];
  pulse?: unknown;
  usage?: unknown | null;
  branches?: { count: number; rows?: unknown[] };
  channels_state?: 'live' | 'fallback';
  status_headline?: string;
  has_pricing?: boolean;
  pricing_stale?: boolean;
  hero_spend_usd?: number;
  hero_spend_display?: string;
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
  // EventsLog (Phase 2c-iii+) will subscribe to this slice.
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
