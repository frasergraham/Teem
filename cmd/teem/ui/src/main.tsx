import { StrictMode, useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import './styles/tokens.css';

type Team = { id: string; name: string };
type StateResponse = { team: Team };

const TEAM_ID_RE = /\/teams\/(t-[a-f0-9]+|[a-z0-9-]+)\/v2/;

function parseTeamID(pathname: string): string | null {
  const m = pathname.match(TEAM_ID_RE);
  return m ? m[1] : null;
}

function App() {
  const [team, setTeam] = useState<Team | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const id = parseTeamID(window.location.pathname);
    if (!id) {
      setError('no team id in URL');
      return;
    }
    let cancelled = false;
    fetch(`/api/teams/${id}/state`)
      .then(async (r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return (await r.json()) as StateResponse;
      })
      .then((s) => {
        if (!cancelled) setTeam(s.team);
      })
      .catch((e) => {
        if (!cancelled) setError(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (error) return <div>error: {error}</div>;
  if (!team) return <div>loading…</div>;
  return <div>hello, {team.name}</div>;
}

const rootEl = document.getElementById('root');
if (rootEl) {
  createRoot(rootEl).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
