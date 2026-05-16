import { useMemo } from 'react';
import { AuditEvent, RecentEvent, useTeamStore } from '../store/team';

const MAX_ROWS = 20;
const MESSAGE_PREVIEW = 160;

// EventLogPanel renders the most-recent audit events, newest first.
// Two sources feed it:
//
//   1. snapshot.recent_events — server-formatted backfill on /state
//      load (cmd/teem/ui.go dashboardEvent, last 30 min, cap 20).
//   2. store.events — the live AuditEvent ring populated by every WS
//      audit envelope (store/team.ts applyEnvelope).
//
// We merge both by `ts`, prefer the live ring on collision, sort
// newest-first, then trim to MAX_ROWS. Messages are truncated to a
// single-line preview to keep rows scannable.
export function EventLogPanel() {
  const liveEvents = useTeamStore((s) => s.events);
  const backfill = useTeamStore((s) => s.snapshot?.recent_events);
  const rows = useMemo(() => mergeEvents(backfill, liveEvents), [backfill, liveEvents]);
  return (
    <section className="events-panel" aria-label="recent audit events">
      <h3 className="panel-label">
        Recent events <span className="count">{rows.length}</span>
      </h3>
      {rows.length === 0 ? (
        <div className="events-empty">quiet for now</div>
      ) : (
        <div className="events-list">
          {rows.map((r) => (
            <EventRow key={`${r.ts}-${r.kind}-${r.agent_id}`} row={r} />
          ))}
        </div>
      )}
    </section>
  );
}

function EventRow({ row }: { row: DisplayEvent }) {
  const preview =
    row.message.length > MESSAGE_PREVIEW
      ? row.message.slice(0, MESSAGE_PREVIEW).trimEnd() + '…'
      : row.message;
  return (
    <div className="event-row">
      <span className="time">{row.time}</span>
      <span className="agent">{row.agent_id || '—'}</span>
      <span className="kind">{row.kind}</span>
      <span className="msg" title={row.message}>
        {preview}
      </span>
    </div>
  );
}

// DisplayEvent is the merged row shape rendered by the panel.
export interface DisplayEvent {
  ts: string;
  time: string;
  agent_id: string;
  kind: string;
  message: string;
}

// mergeEvents merges the snapshot backfill with the live audit ring,
// newest-first, deduped by ts+kind+agent_id. The live entry wins on
// collision (it has more meta to work with if we ever surface it). Cap
// to MAX_ROWS. Exported for unit-test access.
export function mergeEvents(
  backfill: RecentEvent[] | undefined,
  live: AuditEvent[],
): DisplayEvent[] {
  const seen = new Set<string>();
  const out: DisplayEvent[] = [];
  for (const e of live) {
    const key = dedupeKey(e.ts, e.kind, e.agent_id);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({
      ts: e.ts,
      time: formatClock(e.ts),
      agent_id: e.agent_id || '',
      kind: e.kind,
      message: messageFromAudit(e),
    });
  }
  if (backfill) {
    for (const e of backfill) {
      const key = dedupeKey(e.ts, e.kind, e.agent_id);
      if (seen.has(key)) continue;
      seen.add(key);
      out.push({
        ts: e.ts,
        time: e.time || formatClock(e.ts),
        agent_id: e.agent_id || '',
        kind: e.kind,
        message: e.message,
      });
    }
  }
  // Newest first: ISO strings sort lexicographically in UTC, so a
  // reverse-sort by `ts` is correct as long as the wire uses RFC3339
  // (audit.Event JSON does — internal/audit/audit.go).
  out.sort((a, b) => (a.ts < b.ts ? 1 : a.ts > b.ts ? -1 : 0));
  return out.slice(0, MAX_ROWS);
}

function dedupeKey(ts: string, kind: string, agent: string): string {
  return `${ts}|${kind}|${agent}`;
}

// messageFromAudit recovers a human-readable summary for a raw audit
// event. Mirrors cmd/teem/ui.go eventSummary's two special cases
// (KindJobReceived → meta.prompt, KindJobComplete → meta.output) so
// live events read consistently with the server-formatted backfill.
export function messageFromAudit(e: AuditEvent): string {
  const meta = e.meta ?? {};
  if (e.kind === 'job_received') {
    const p = meta['prompt'];
    if (typeof p === 'string' && p !== '') return p;
  }
  if (e.kind === 'job_complete') {
    const o = meta['output'];
    if (typeof o === 'string' && o !== '') return o;
    const n = meta['output_bytes'];
    if (typeof n === 'number' && Number.isFinite(n)) return `${Math.trunc(n)} bytes returned`;
  }
  return e.message ?? '';
}

// formatClock renders an ISO timestamp as local HH:MM:SS. Falls back
// to the raw string on parse failure so a malformed wire value still
// surfaces in the UI rather than collapsing to an empty cell.
export function formatClock(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const pad = (n: number) => (n < 10 ? `0${n}` : `${n}`);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
