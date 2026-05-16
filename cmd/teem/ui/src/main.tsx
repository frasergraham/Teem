import { StrictMode, useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import './styles/tokens.css';

import { connect } from './api/ws';
import { useTeamStore } from './store/team';
import { DashboardLayout } from './components/DashboardLayout';

const TEAM_ID_RE = /\/teams\/(t-[a-f0-9]+|[a-z0-9-]+)\/v2/;

function parseTeamID(pathname: string): string | null {
  const m = pathname.match(TEAM_ID_RE);
  return m ? m[1] : null;
}

function App() {
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

const rootEl = document.getElementById('root');
if (rootEl) {
  createRoot(rootEl).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
