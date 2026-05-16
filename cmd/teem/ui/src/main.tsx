import { StrictMode, useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import './styles/tokens.css';

import { connect } from './api/ws';
import { ConnState, useTeamStore } from './store/team';

const TEAM_ID_RE = /\/teams\/(t-[a-f0-9]+|[a-z0-9-]+)\/v2/;

function parseTeamID(pathname: string): string | null {
  const m = pathname.match(TEAM_ID_RE);
  return m ? m[1] : null;
}

function describeConn(conn: ConnState, seq: number): string {
  switch (conn.kind) {
    case 'idle':
      return 'idle';
    case 'loading':
      return 'loading…';
    case 'connecting':
      return `connecting (attempt ${conn.attempt})…`;
    case 'live':
      return `connected, seq=${conn.seq}`;
    case 'reconnecting':
      return `reconnecting in ${Math.round(conn.nextDelayMs / 1000)}s (attempt ${conn.attempt}), last seq=${seq}`;
    case 'error':
      return `error: ${conn.message}`;
  }
}

function App() {
  const conn = useTeamStore((s) => s.conn);
  const snapshot = useTeamStore((s) => s.snapshot);
  const lastSeq = useTeamStore((s) => s.lastSeq);
  const eventsCount = useTeamStore((s) => s.events.length);

  useEffect(() => {
    const id = parseTeamID(window.location.pathname);
    if (!id) {
      useTeamStore.getState().setConn({ kind: 'error', message: 'no team id in URL' });
      return;
    }
    const dispose = connect(id);
    return dispose;
  }, []);

  const teamName = snapshot?.team.name ?? '(loading)';
  return (
    <div style={{ fontFamily: 'system-ui, sans-serif', padding: '1rem' }}>
      <header style={{ display: 'flex', gap: '1rem', alignItems: 'baseline' }}>
        <strong>{teamName}</strong>
        <span style={{ opacity: 0.7, fontSize: '0.9rem' }}>{describeConn(conn, lastSeq)}</span>
      </header>
      <p style={{ opacity: 0.6, fontSize: '0.85rem', marginTop: '0.5rem' }}>
        events buffered: {eventsCount}
      </p>
    </div>
  );
}

const rootEl = document.getElementById('root');
if (rootEl) {
  createRoot(rootEl).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
