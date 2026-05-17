import { StrictMode, useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import './styles/tokens.css';

import { connect } from './api/ws';
import { useTeamStore } from './store/team';
import { DashboardLayout } from './components/DashboardLayout';
import { TranscriptPage } from './components/TranscriptPage';

const TEAM_ID_RE = /\/teams\/(t-[a-f0-9]+|[a-z0-9-]+)(?:\/v2)?(?:\/|$)/;

// TRANSCRIPT_RE matches /teams/<id>/transcripts/<agent>/<job>. The id
// segment is the same shape as the dashboard route; agent/job allow
// the same chars the server's isSafeID accepts ([A-Za-z0-9._-]+).
const TRANSCRIPT_RE = /^\/teams\/(t-[a-f0-9]+|[a-z0-9-]+)\/transcripts\/([A-Za-z0-9._-]+)\/([A-Za-z0-9._-]+)\/?$/;

function parseTeamID(pathname: string): string | null {
  const m = pathname.match(TEAM_ID_RE);
  return m ? m[1] : null;
}

interface TranscriptRoute {
  kind: 'transcript';
  teamID: string;
  agentID: string;
  jobID: string;
}

interface DashboardRoute {
  kind: 'dashboard';
}

type Route = TranscriptRoute | DashboardRoute;

export function parseRoute(pathname: string): Route {
  const m = pathname.match(TRANSCRIPT_RE);
  if (m) {
    return { kind: 'transcript', teamID: m[1], agentID: m[2], jobID: m[3] };
  }
  return { kind: 'dashboard' };
}

function Dashboard() {
  useEffect(() => {
    const id = parseTeamID(window.location.pathname);
    if (!id) {
      useTeamStore.getState().setConn({ kind: 'error', message: 'no team id in URL' });
      return;
    }
    const dispose = connect(id);
    return dispose;
  }, []);

  return <DashboardLayout />;
}

function App() {
  const route = parseRoute(window.location.pathname);
  if (route.kind === 'transcript') {
    return (
      <TranscriptPage teamID={route.teamID} agentID={route.agentID} jobID={route.jobID} />
    );
  }
  return <Dashboard />;
}

const rootEl = document.getElementById('root');
if (rootEl) {
  createRoot(rootEl).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
