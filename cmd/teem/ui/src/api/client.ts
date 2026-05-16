import { StateSnapshot } from '../store/team';

export class APIError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export async function fetchState(teamID: string, signal?: AbortSignal): Promise<StateSnapshot> {
  const r = await fetch(`/api/teams/${encodeURIComponent(teamID)}/state`, {
    signal,
    headers: { Accept: 'application/json' },
    cache: 'no-store',
  });
  if (!r.ok) throw new APIError(r.status, `GET /state → HTTP ${r.status}`);
  return (await r.json()) as StateSnapshot;
}
