import { Envelope, useTeamStore } from './team';
import { fetchState } from '../api/client';

// dispatch routes a wsbus Envelope into the Zustand store. Phase 2c-i
// keeps the per-kind mapping minimal — the doc's audit-kind →
// state-patch table (see docs/dashboard-spa.md §6) lands incrementally.
// Until that's wired, ANY `kind="audit"` envelope schedules a debounced
// refetch of /state so the dashboard reflects the change. Costs one
// /state GET per quiet window of bursty audit traffic; cheap to ship,
// and the per-kind patch path will retire this when it lands (filed
// as a follow-up task).
//
// Returning `'refetch'` tells the WebSocket loop to abort the current
// connection, refetch /state, and re-dial — that is the contract for
// `snapshot_invalidate`. Audit envelopes use the in-band refresh
// instead so we don't churn the socket on every event.
export type DispatchResult = 'ok' | 'refetch';

const auditRefreshDebounceMs = 300;
let auditRefreshTimer: ReturnType<typeof setTimeout> | null = null;

function scheduleAuditRefresh(): void {
  if (auditRefreshTimer !== null) clearTimeout(auditRefreshTimer);
  auditRefreshTimer = setTimeout(() => {
    auditRefreshTimer = null;
    const teamID = useTeamStore.getState().teamID;
    if (!teamID) return;
    fetchState(teamID)
      .then((s) => {
        // Only swap if we're still viewing the same team — a team
        // switch mid-fetch would otherwise stomp the new snapshot.
        if (useTeamStore.getState().teamID === teamID) {
          useTeamStore.getState().setSnapshot(s);
        }
      })
      .catch(() => {
        // Swallow — the next audit envelope schedules another retry,
        // and ws.ts handles hard failures via the conn-state path.
      });
  }, auditRefreshDebounceMs);
}

export function dispatch(env: Envelope): DispatchResult {
  useTeamStore.getState().applyEnvelope(env);
  if (env.kind === 'snapshot_invalidate') return 'refetch';
  if (env.kind === 'audit') scheduleAuditRefresh();
  return 'ok';
}
