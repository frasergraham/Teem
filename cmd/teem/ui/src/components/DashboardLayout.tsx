import { useEffect } from 'react';
import { ConnState, useTeamStore } from '../store/team';
import { HeroPanel } from './HeroPanel';
import { WorkersPanel } from './WorkersPanel';
import { TasksTable } from './TasksTable';

// DashboardLayout is the top-level SPA frame: sticky header (team name,
// leader status text, connection-state dot) plus the three Phase 2c-ii
// panels (hero / workers / tasks). The body picks up the
// bridge-console palette from tokens.css via the team-detail-page
// class, so the SPA visually tracks the SSR per-team page.
export function DashboardLayout() {
  // Carry the SSR class onto <body> for the duration of the SPA mount —
  // tokens.css scopes the bridge-console palette under this selector
  // (mirrors the SSR `<body class="team-detail-page">` rule).
  useEffect(() => {
    document.body.classList.add('team-detail-page');
    return () => document.body.classList.remove('team-detail-page');
  }, []);

  const snapshotReady = useTeamStore((s) => s.snapshot != null);
  if (!snapshotReady) {
    return <LoadingFrame />;
  }
  return (
    <>
      <Header />
      <main className="spa-grid">
        <HeroPanel />
        <WorkersPanel />
        <TasksTable />
      </main>
    </>
  );
}

function Header() {
  const teamName = useTeamStore((s) => s.snapshot?.team.name ?? '(no team)');
  const conn = useTeamStore((s) => s.conn);
  const lastSeq = useTeamStore((s) => s.lastSeq);
  return (
    <header className="spa-header">
      <h1>{teamName}</h1>
      <span className="spa-conn">
        <span className={`dot ${conn.kind}`} aria-hidden="true" />
        <span>{describeConn(conn, lastSeq)}</span>
      </span>
    </header>
  );
}

function LoadingFrame() {
  const conn = useTeamStore((s) => s.conn);
  const lastSeq = useTeamStore((s) => s.lastSeq);
  return (
    <header className="spa-header">
      <h1>(loading)</h1>
      <span className="spa-conn">
        <span className={`dot ${conn.kind}`} aria-hidden="true" />
        <span>{describeConn(conn, lastSeq)}</span>
      </span>
    </header>
  );
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
      return `live · seq=${conn.seq}`;
    case 'reconnecting':
      return `reconnecting in ${Math.round(conn.nextDelayMs / 1000)}s (attempt ${conn.attempt}), last seq=${seq}`;
    case 'error':
      return `error: ${conn.message}`;
  }
}
