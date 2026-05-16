import { useEffect, useState } from 'react';
import { ConnState, useTeamStore } from '../store/team';
import { useSettingsStore } from '../store/settings';
import { pingLeader } from '../api/control';
import { APIError } from '../api/client';
import { HeroPanel } from './HeroPanel';
import { WorkersPanel } from './WorkersPanel';
import { TasksTable } from './TasksTable';
import { ChatPanel } from './ChatPanel';
import { ApprovalCard } from './ApprovalCard';
import { DecisionsList } from './DecisionsList';
import { UsageCard } from './UsageCard';
import { PulseControls } from './PulseControls';
import { SettingsMenu } from './SettingsMenu';

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

  const teamID = useTeamStore((s) => s.teamID);
  const hydrate = useSettingsStore((s) => s.hydrate);
  useEffect(() => {
    if (teamID) hydrate(teamID);
  }, [teamID, hydrate]);

  const snapshotReady = useTeamStore((s) => s.snapshot != null);
  if (!snapshotReady) {
    return (
      <>
        <LoadingFrame />
        <SettingsMenu />
      </>
    );
  }
  return (
    <>
      <Header />
      <DashboardBody />
      <SettingsMenu />
    </>
  );
}

function DashboardBody() {
  const visible = useSettingsStore((s) => s.visible);
  return (
    <main className="spa-grid">
      {visible.hero && <HeroPanel />}
      {visible.usage && <UsageCard />}
      {visible.tasksAwaitingApproval && <ApprovalCard />}
      {visible.workers && <WorkersPanel />}
      {visible.pulse && <PulseControls />}
      {visible.decisions && <DecisionsList />}
      {visible.tasksOpen && <TasksTable />}
      {visible.chat && <ChatPanel />}
    </main>
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
      <PingLeaderButton />
      <SettingsGear />
    </header>
  );
}

// PingLeaderButton fires a one-shot manual pulse-tick via
// POST /control/teams/<id>/ping. Visual feedback: a short "pinged" state
// after success, "..." while in flight, otherwise the default label.
// On error, the title= surfaces the message for hover-debug.
function PingLeaderButton() {
  const teamID = useTeamStore((s) => s.teamID);
  const [state, setState] = useState<'idle' | 'sending' | 'sent' | 'error'>('idle');
  const [error, setError] = useState<string | null>(null);
  if (!teamID) return null;
  async function onClick() {
    if (state === 'sending') return;
    setState('sending');
    setError(null);
    try {
      await pingLeader(teamID!);
      setState('sent');
      setTimeout(() => setState((s) => (s === 'sent' ? 'idle' : s)), 2000);
    } catch (e) {
      setError(e instanceof APIError ? e.message : String(e));
      setState('error');
      setTimeout(() => setState((s) => (s === 'error' ? 'idle' : s)), 3500);
    }
  }
  const label =
    state === 'sending' ? 'pinging…' : state === 'sent' ? 'pinged ✓' : state === 'error' ? 'failed' : 'Ping leader';
  return (
    <button
      type="button"
      className={`ping-btn ${state}`}
      onClick={onClick}
      disabled={state === 'sending'}
      title={error ?? 'Fire a one-shot pulse tick — the leader wakes and takes a turn now.'}
      aria-label="ping the leader"
    >
      {label}
    </button>
  );
}

function SettingsGear() {
  const open = useSettingsStore((s) => s.openMenu);
  return (
    <button
      type="button"
      className="settings-gear"
      onClick={open}
      aria-label="Dashboard settings"
      title="Dashboard settings"
    >
      <svg
        width="18"
        height="18"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <circle cx="12" cy="12" r="3" />
        <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33h.01a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82v.01a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
      </svg>
    </button>
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
      <SettingsGear />
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
