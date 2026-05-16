import { Envelope, useTeamStore } from '../store/team';
import { dispatch } from '../store/dispatch';
import { fetchState } from './client';

// Reconnect backoff: 1s, 2s, 4s, 8s, 16s, capped at 30s, + ~25% jitter.
const baseDelays = [1000, 2000, 4000, 8000, 16000, 30000];

function delayForAttempt(attempt: number): number {
  const base = baseDelays[Math.min(attempt, baseDelays.length - 1)];
  const jitter = Math.floor(Math.random() * (base * 0.25));
  return base + jitter;
}

function wsURL(teamID: string, sinceSeq: number): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const host = window.location.host;
  const id = encodeURIComponent(teamID);
  const seq = encodeURIComponent(String(sinceSeq));
  return `${proto}//${host}/api/teams/${id}/events?since_seq=${seq}`;
}

// connect manages a single team's lifecycle: snapshot fetch → WS open
// → live dispatch → reconnect on close. Returns a disposer; the caller
// (main.tsx) invokes it on unmount or team-change.
export function connect(teamID: string): () => void {
  const store = useTeamStore.getState();
  store.setTeamID(teamID);

  let cancelled = false;
  let ws: WebSocket | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  let attempt = 0;
  // Set by the message handler when we observe snapshot_invalidate.
  // Drains into the close handler so it knows to refetch /state before
  // dialling again.
  let pendingRefetch = false;

  const clearReconnect = () => {
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
  };

  const scheduleReconnect = () => {
    if (cancelled) return;
    const ms = delayForAttempt(attempt);
    useTeamStore.getState().setConn({
      kind: 'reconnecting',
      attempt: attempt + 1,
      nextDelayMs: ms,
    });
    reconnectTimer = setTimeout(() => {
      attempt += 1;
      void boot();
    }, ms);
  };

  const openSocket = () => {
    if (cancelled) return;
    const sinceSeq = useTeamStore.getState().lastSeq;
    useTeamStore.getState().setConn({ kind: 'connecting', attempt: attempt + 1 });
    let sock: WebSocket;
    try {
      sock = new WebSocket(wsURL(teamID, sinceSeq));
    } catch (err) {
      useTeamStore.getState().setConn({ kind: 'error', message: String(err) });
      scheduleReconnect();
      return;
    }
    ws = sock;

    sock.onopen = () => {
      if (cancelled) {
        sock.close();
        return;
      }
      attempt = 0; // success resets backoff
      const seq = useTeamStore.getState().lastSeq;
      useTeamStore.getState().setConn({ kind: 'live', seq });
    };

    sock.onmessage = (ev) => {
      let env: Envelope;
      try {
        env = JSON.parse(ev.data) as Envelope;
      } catch {
        return; // malformed — drop, the next ping will resync seq
      }
      const result = dispatch(env);
      if (result === 'refetch') {
        pendingRefetch = true;
        sock.close();
      }
    };

    sock.onerror = () => {
      // onclose runs right after; let it handle the reconnect.
    };

    sock.onclose = () => {
      if (cancelled) return;
      ws = null;
      if (pendingRefetch) {
        pendingRefetch = false;
        // Refetch /state, then reconnect with sinceSeq=0 (set by
        // setSnapshot). No backoff penalty — this is an expected
        // server-driven invalidation.
        attempt = 0;
        void boot();
        return;
      }
      scheduleReconnect();
    };
  };

  // Single boot path: fetch snapshot (if we don't have one yet OR a
  // refetch is pending), then open the socket. Errors in the snapshot
  // fetch are surfaced via conn-state and retried on the same backoff
  // schedule as WebSocket failures.
  const boot = async () => {
    if (cancelled) return;
    const snap = useTeamStore.getState().snapshot;
    const needsFetch = snap == null || snap.team.id !== teamID;
    if (needsFetch || pendingRefetch) {
      useTeamStore.getState().setConn({ kind: 'loading' });
      try {
        const s = await fetchState(teamID);
        if (cancelled) return;
        useTeamStore.getState().setSnapshot(s);
      } catch (err) {
        if (cancelled) return;
        useTeamStore.getState().setConn({
          kind: 'error',
          message: `state fetch: ${(err as Error).message}`,
        });
        scheduleReconnect();
        return;
      }
    }
    openSocket();
  };

  void boot();

  return () => {
    cancelled = true;
    clearReconnect();
    if (ws) {
      try {
        ws.close();
      } catch {
        // ignore
      }
      ws = null;
    }
  };
}
