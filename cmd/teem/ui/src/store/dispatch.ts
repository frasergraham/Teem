import { Envelope, useTeamStore } from './team';
import { applyAuditEnvelope } from './audit_patches';

// dispatch routes a wsbus Envelope into the Zustand store.
//
// - kind="snapshot_invalidate" returns 'refetch' so the WebSocket loop
//   drops the connection, GETs /state, and redials with sinceSeq=0.
// - kind="audit" is patched into the store in-place by
//   applyAuditEnvelope (audit_patches.ts) — no /state round-trip.
//   Unhandled audit kinds fall through silently; the next plan-touching
//   event or snapshot_invalidate eventually reconciles.
// - kind="ping" is handled inside applyEnvelope (lastPingAt + conn).
export type DispatchResult = 'ok' | 'refetch';

export function dispatch(env: Envelope): DispatchResult {
  useTeamStore.getState().applyEnvelope(env);
  if (env.kind === 'snapshot_invalidate') return 'refetch';
  if (env.kind === 'audit') applyAuditEnvelope(env);
  return 'ok';
}
