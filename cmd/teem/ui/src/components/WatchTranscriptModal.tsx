import { useEffect, useMemo, useRef, useState } from 'react';

import { useTeamStore } from '../store/team';
import { TranscriptEvent, parseStreamLine } from '../lib/transcript';

// WatchTranscriptModal streams an agent's live JSONL transcript over
// SSE (GET /api/teams/<id>/transcripts/<agent>/<job>/watch). Events
// are parsed as claude stream-json shapes (system / user / assistant
// / tool_use / tool_result / result) and rendered terse — one row per
// turn, glance-friendly.
//
// Behaviour:
//   - Auto-scrolls to bottom only when the user is already at the
//     bottom (so a manual scroll-up to read history isn't yanked
//     back by a new event landing).
//   - Closes the EventSource on dismiss, on `event: done`, and on
//     `event: error` — there is no auto-reconnect (a closed run is
//     final; for a still-running run the user reopens the modal).
//   - Reuses the .task-modal-backdrop shell so the modal feels
//     visually consistent with TaskDetailModal.
//
// Out of scope (per task spec): interrupting from the modal,
// prettifying completed transcripts, multi-watch.

interface Props {
  agentID: string;
  jobID: string;
  persona: string;
  onClose: () => void;
}

export function WatchTranscriptModal({ agentID, jobID, persona, onClose }: Props) {
  const teamID = useTeamStore((s) => s.teamID);
  const [events, setEvents] = useState<TranscriptEvent[]>([]);
  const [state, setState] = useState<'streaming' | 'done' | 'error'>('streaming');
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const scrollerRef = useRef<HTMLDivElement | null>(null);
  const atBottomRef = useRef<boolean>(true);
  const closeRef = useRef<HTMLButtonElement | null>(null);

  const url = useMemo(() => {
    if (!teamID) return null;
    return `/api/teams/${encodeURIComponent(teamID)}/transcripts/${encodeURIComponent(
      agentID,
    )}/${encodeURIComponent(jobID)}/watch`;
  }, [teamID, agentID, jobID]);

  // Esc dismiss + initial focus.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.stopPropagation();
        onClose();
      }
    }
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);
  useEffect(() => {
    closeRef.current?.focus();
  }, []);

  // EventSource lifecycle. One per (teamID, agent, job) — closed on
  // unmount and on terminal events.
  useEffect(() => {
    if (!url) return;
    // Reset on URL change so a re-open (or future agent/job swap) starts
    // with a clean event list instead of inheriting the previous run.
    setEvents([]);
    const es = new EventSource(url);
    const append = (parsed: TranscriptEvent) => {
      setEvents((prev) => prev.concat(parsed));
    };
    es.onmessage = (ev) => {
      const parsed = parseStreamLine(ev.data);
      if (parsed) append(parsed);
    };
    es.addEventListener('done', () => {
      setState('done');
      es.close();
    });
    es.addEventListener('error', (ev) => {
      // EventSource error handler fires on transport errors too (no
      // text payload). The handler's `data` only exists when the
      // server explicitly emitted `event: error` with a data field.
      const msg = (ev as MessageEvent).data ? String((ev as MessageEvent).data) : 'connection lost';
      setErrorMsg(msg);
      setState('error');
      es.close();
    });
    return () => {
      es.close();
    };
  }, [url]);

  // Auto-scroll only when the user is parked at the bottom. The
  // onScroll listener flips atBottomRef; the layout effect respects
  // it on each render.
  useEffect(() => {
    const el = scrollerRef.current;
    if (!el) return;
    if (atBottomRef.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [events]);

  function onScroll(e: React.UIEvent<HTMLDivElement>) {
    const el = e.currentTarget;
    const slack = 12; // tolerate fractional px / scrollbar thumb wobble
    atBottomRef.current = el.scrollTop + el.clientHeight >= el.scrollHeight - slack;
  }

  return (
    <div
      className="task-modal-backdrop"
      role="presentation"
      onClick={onClose}
    >
      <div
        className="task-modal-card watch-modal-card"
        role="dialog"
        aria-modal="true"
        aria-labelledby="watch-modal-title"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="task-modal-header">
          <div className="task-modal-titles">
            <div className="task-modal-id">{agentID} · {jobID}</div>
            <h2 className="task-modal-title" id="watch-modal-title">
              Watching {persona}
            </h2>
          </div>
          <div className="task-modal-pills">
            <span className={`watch-state ${state}`}>{watchStateLabel(state)}</span>
          </div>
          <button
            ref={closeRef}
            type="button"
            className="task-modal-close"
            aria-label="close watch modal"
            onClick={onClose}
          >
            ×
          </button>
        </header>
        <div
          className="watch-events"
          ref={scrollerRef}
          onScroll={onScroll}
          aria-live="polite"
          aria-label="live transcript events"
        >
          {events.length === 0 && state === 'streaming' && (
            <div className="watch-empty">Waiting for the first event…</div>
          )}
          {events.map((e, i) => (
            <div key={i} className={`watch-event ${e.kind}`} title={e.raw}>
              <span className="watch-event-kind">{watchKindLabel(e)}</span>
              <span className="watch-event-text">{e.text}</span>
            </div>
          ))}
          {state === 'error' && errorMsg && (
            <div className="watch-event error" role="status">
              <span className="watch-event-kind">error</span>
              <span className="watch-event-text">{errorMsg}</span>
            </div>
          )}
          {state === 'done' && (
            <div className="watch-event result" role="status">
              <span className="watch-event-kind">done</span>
              <span className="watch-event-text">Transcript stream closed.</span>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// watchKindLabel adds a direction hint to the `tool` kind so the
// caller (tool_use, with a name) reads differently from the response
// (tool_result, with a preview).
function watchKindLabel(e: TranscriptEvent): string {
  if (e.kind !== 'tool') return e.kind;
  if (e.toolName) return `→ ${e.toolName}`;
  if (e.toolResultPreview !== undefined) return '← result';
  return 'tool';
}

function watchStateLabel(s: 'streaming' | 'done' | 'error'): string {
  switch (s) {
    case 'streaming':
      return 'live';
    case 'done':
      return 'finished';
    case 'error':
      return 'error';
  }
}

