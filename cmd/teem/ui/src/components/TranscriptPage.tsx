import { useEffect, useMemo, useState } from 'react';
import { marked } from 'marked';

import { TranscriptEvent, parseNDJSON } from '../lib/transcript';

// TranscriptPage renders a single worker job's NDJSON transcript as a
// proper HTML page. It is served at /teams/<id>/transcripts/<a>/<j>,
// fetches the raw NDJSON from /api/teams/<id>/transcripts/<a>/<j>, and
// renders the same parse output that the live <WatchTranscriptModal>
// uses — so a participation-log link clicks into a readable page
// instead of triggering a raw-NDJSON download.
//
// Streaming is out of scope on this page (the Watch modal already
// covers the live view); pagination, diff rendering, and syntax
// highlighting are deferred per the task spec.

interface Props {
  teamID: string;
  agentID: string;
  jobID: string;
}

// rolePersona renders agent_id `<role>-<name>` as "<RoleLabel> <Name>".
// Matches TaskDetailModal's mapping so the page header and the
// participation log read the same names.
function rolePersona(agentID: string): string {
  if (!agentID) return '—';
  if (agentID === 'leader') return 'Leader';
  if (agentID === 'operator') return 'Operator';
  const dash = agentID.indexOf('-');
  if (dash <= 0) return capitalize(agentID);
  const role = agentID.slice(0, dash);
  const name = agentID.slice(dash + 1);
  return `${roleLabel(role)} ${capitalize(name)}`;
}

function roleLabel(role: string): string {
  switch (role) {
    case 'worker':
      return 'Coder';
    case 'reviewer':
      return 'Reviewer';
    case 'integrator':
      return 'Integrator';
    case 'project_manager':
      return 'PM';
    default:
      return capitalize(role);
  }
}

function capitalize(s: string): string {
  if (!s) return s;
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function shortJobID(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

export function TranscriptPage({ teamID, agentID, jobID }: Props) {
  const [state, setState] = useState<'loading' | 'ready' | 'error'>('loading');
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [events, setEvents] = useState<TranscriptEvent[]>([]);
  const [byteCount, setByteCount] = useState<number>(0);

  const persona = useMemo(() => rolePersona(agentID), [agentID]);
  const apiURL = `/api/teams/${encodeURIComponent(teamID)}/transcripts/${encodeURIComponent(
    agentID,
  )}/${encodeURIComponent(jobID)}`;
  const dashboardURL = `/teams/${encodeURIComponent(teamID)}/`;

  useEffect(() => {
    document.title = `Transcript: ${persona} · job ${shortJobID(jobID)}`;
  }, [persona, jobID]);

  useEffect(() => {
    const ac = new AbortController();
    setState('loading');
    setErrorMsg(null);
    fetch(apiURL, { signal: ac.signal })
      .then(async (resp) => {
        if (!resp.ok) {
          throw new Error(`${resp.status} ${resp.statusText}`);
        }
        const body = await resp.text();
        if (ac.signal.aborted) return;
        setByteCount(body.length);
        setEvents(parseNDJSON(body));
        setState('ready');
      })
      .catch((err: unknown) => {
        if (ac.signal.aborted) return;
        setErrorMsg(err instanceof Error ? err.message : String(err));
        setState('error');
      });
    return () => ac.abort();
  }, [apiURL]);

  return (
    <main className="transcript-page">
      <header className="transcript-header">
        <a className="transcript-back" href={dashboardURL}>
          ← Back to dashboard
        </a>
        <h1 className="transcript-title">
          Transcript: <span className="transcript-persona">{persona}</span>{' '}
          <span className="transcript-sep">·</span>{' '}
          <span className="transcript-job">job {shortJobID(jobID)}</span>
        </h1>
        <div className="transcript-sub">
          <span className="transcript-agent-id">{agentID}</span>
          <span className="transcript-sep">·</span>
          <span className="transcript-job-id">{jobID}</span>
          {state === 'ready' && (
            <>
              <span className="transcript-sep">·</span>
              <span className="transcript-count">
                {events.length} event{events.length === 1 ? '' : 's'}
              </span>
              {byteCount > 0 && (
                <>
                  <span className="transcript-sep">·</span>
                  <span className="transcript-bytes">{formatBytes(byteCount)}</span>
                </>
              )}
            </>
          )}
        </div>
      </header>
      <section className="transcript-body" aria-live="polite">
        {state === 'loading' && (
          <div className="transcript-status">Loading transcript…</div>
        )}
        {state === 'error' && (
          <div className="transcript-status transcript-error" role="alert">
            Couldn't load transcript: {errorMsg ?? 'unknown error'}
          </div>
        )}
        {state === 'ready' && events.length === 0 && (
          <div className="transcript-status transcript-empty">
            Transcript is empty. The worker may have crashed before writing any events.
          </div>
        )}
        {state === 'ready' && events.length > 0 && (
          <ol className="transcript-events">
            {events.map((ev, i) => (
              <EventRow key={i} ev={ev} />
            ))}
          </ol>
        )}
      </section>
    </main>
  );
}

function EventRow({ ev }: { ev: TranscriptEvent }) {
  return (
    <li className={`transcript-event ${ev.kind}`}>
      <span className="transcript-event-kind">{ev.kind}</span>
      <div className="transcript-event-body">
        <EventContent ev={ev} />
      </div>
    </li>
  );
}

// EventContent renders the event-specific body. The watch modal only
// shows the one-line summary; here we have the screen real estate to
// expand richer turns inline (full assistant prose via marked, tool
// input collapsed in <details>, tool_result preview), while keeping the
// raw JSON one click away via the trailing <details> block.
function EventContent({ ev }: { ev: TranscriptEvent }) {
  if (ev.kind === 'assistant') {
    return (
      <>
        <div
          className="transcript-event-text transcript-markdown"
          dangerouslySetInnerHTML={{ __html: renderMarkdown(ev.text) }}
        />
        <RawDetails raw={ev.raw} />
      </>
    );
  }
  if (ev.kind === 'tool') {
    return (
      <>
        <div className="transcript-event-text transcript-tool-head">
          → Tool: <code>{ev.toolName ?? 'unknown'}</code>
        </div>
        {ev.toolInput && (
          <details className="transcript-tool-input">
            <summary>input</summary>
            <pre>{ev.toolInput}</pre>
          </details>
        )}
        <RawDetails raw={ev.raw} />
      </>
    );
  }
  if (ev.kind === 'user' && ev.toolResultPreview) {
    return (
      <>
        <div className="transcript-event-text transcript-tool-result">
          <span className="transcript-tool-result-label">tool_result:</span>{' '}
          <code>{previewLine(ev.toolResultPreview)}</code>
        </div>
        {ev.toolResultPreview.length > 200 && (
          <details className="transcript-tool-result-full">
            <summary>full result</summary>
            <pre>{ev.toolResultPreview}</pre>
          </details>
        )}
        <RawDetails raw={ev.raw} />
      </>
    );
  }
  if (ev.kind === 'result') {
    return (
      <>
        <div className="transcript-event-text transcript-result">{ev.text}</div>
        <RawDetails raw={ev.raw} />
      </>
    );
  }
  // system / unknown / fallback user text
  return (
    <>
      <div className="transcript-event-text">{ev.text}</div>
      <RawDetails raw={ev.raw} />
    </>
  );
}

function RawDetails({ raw }: { raw: string }) {
  return (
    <details className="transcript-raw">
      <summary>raw</summary>
      <pre>{pretty(raw)}</pre>
    </details>
  );
}

function previewLine(s: string): string {
  if (s.length <= 200) return s;
  return s.slice(0, 199) + '…';
}

function pretty(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

function renderMarkdown(text: string): string {
  if (!text) return '';
  try {
    return marked.parse(text, { async: false }) as string;
  } catch {
    return escapeHTML(text);
  }
}

function escapeHTML(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}
