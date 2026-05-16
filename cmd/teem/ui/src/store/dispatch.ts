import { Envelope, useTeamStore } from './team';

// dispatch routes a wsbus Envelope into the Zustand store. Phase 2c-i
// keeps the mapping intentionally minimal — the doc's audit-kind →
// state-patch table (see docs/dashboard-spa.md §6) lands incrementally
// as Phase 2c-ii+ components arrive. For now every envelope updates
// `lastSeq` and the events ring; component-shaped patches (task stage,
// usage, pulse, decisions) are TODO until Phase 2b nails the snapshot
// shape.
//
// Returning `'refetch'` tells the WebSocket loop to abort the current
// connection, refetch /state, and re-dial — that is the contract for
// `snapshot_invalidate`.
export type DispatchResult = 'ok' | 'refetch';

export function dispatch(env: Envelope): DispatchResult {
  useTeamStore.getState().applyEnvelope(env);
  if (env.kind === 'snapshot_invalidate') return 'refetch';
  return 'ok';
}
