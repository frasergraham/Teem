import { useEffect, useMemo, useRef, useState } from 'react';

import { useTeamStore } from '../store/team';

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

type EventKind = 'system' | 'user' | 'assistant' | 'tool' | 'result' | 'unknown';

interface WatchEvent {
  kind: EventKind;
  text: string;       // glance-friendly summary line
  raw: string;        // full JSON for the title= tooltip
  ts: number;         // arrival time (wall clock, ms)
}

export function WatchTranscriptModal({ agentID, jobID, persona, onClose }: Props) {
  const teamID = useTeamStore((s) => s.teamID);
  const [events, setEvents] = useState<WatchEvent[]>([]);
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
    const es = new EventSource(url);
    const append = (parsed: WatchEvent) => {
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
              <span className="watch-event-kind">{e.kind}</span>
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

// parseStreamLine maps one claude stream-json line to a WatchEvent.
// Returns null when the line is empty or malformed JSON; the modal
// just drops those so a truncated mid-write line doesn't show up as
// an "unknown" row.
export function parseStreamLine(raw: string): WatchEvent | null {
  const trimmed = raw.trim();
  if (!trimmed) return null;
  let obj: Record<string, unknown>;
  try {
    obj = JSON.parse(trimmed) as Record<string, unknown>;
  } catch {
    return null;
  }
  const type = typeof obj.type === 'string' ? (obj.type as string) : '';
  const now = Date.now();
  switch (type) {
    case 'system':
      return {
        kind: 'system',
        text: summarizeSystem(obj),
        raw: trimmed,
        ts: now,
      };
    case 'user':
      return {
        kind: 'user',
        text: summarizeUser(obj),
        raw: trimmed,
        ts: now,
      };
    case 'assistant':
      return {
        kind: 'assistant',
        text: summarizeAssistant(obj),
        raw: trimmed,
        ts: now,
      };
    case 'result':
      return {
        kind: 'result',
        text: summarizeResult(obj),
        raw: trimmed,
        ts: now,
      };
    default:
      return {
        kind: 'unknown',
        text: type || '(unknown)',
        raw: trimmed,
        ts: now,
      };
  }
}

function summarizeSystem(obj: Record<string, unknown>): string {
  const sub = stringField(obj, 'subtype');
  if (sub) return sub;
  return 'system';
}

function summarizeUser(obj: Record<string, unknown>): string {
  // claude stream-json wraps user messages in {message: {content: ...}}
  const msg = obj.message as Record<string, unknown> | undefined;
  if (msg) {
    const text = extractText(msg);
    if (text) return truncate(text, 200);
    const toolResult = extractToolResultPreview(msg);
    if (toolResult) return `tool_result: ${truncate(toolResult, 180)}`;
  }
  return 'user turn';
}

function summarizeAssistant(obj: Record<string, unknown>): string {
  const msg = obj.message as Record<string, unknown> | undefined;
  if (msg) {
    const text = extractText(msg);
    if (text) return truncate(text, 200);
    const toolUse = extractToolUsePreview(msg);
    if (toolUse) return `tool_use: ${toolUse}`;
  }
  return 'assistant turn';
}

function summarizeResult(obj: Record<string, unknown>): string {
  const sub = stringField(obj, 'subtype');
  const cost = obj.total_cost_usd;
  const turns = obj.num_turns;
  const bits: string[] = [];
  if (sub) bits.push(sub);
  if (typeof turns === 'number') bits.push(`${turns} turn${turns === 1 ? '' : 's'}`);
  if (typeof cost === 'number' && cost > 0) bits.push(`$${cost.toFixed(4)}`);
  return bits.length ? bits.join(' · ') : 'result';
}

function extractText(msg: Record<string, unknown>): string {
  const content = msg.content;
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    for (const block of content) {
      if (block && typeof block === 'object') {
        const b = block as Record<string, unknown>;
        if (b.type === 'text' && typeof b.text === 'string') return b.text;
      }
    }
  }
  return '';
}

function extractToolUsePreview(msg: Record<string, unknown>): string {
  const content = msg.content;
  if (!Array.isArray(content)) return '';
  for (const block of content) {
    if (block && typeof block === 'object') {
      const b = block as Record<string, unknown>;
      if (b.type === 'tool_use' && typeof b.name === 'string') {
        return b.name;
      }
    }
  }
  return '';
}

function extractToolResultPreview(msg: Record<string, unknown>): string {
  const content = msg.content;
  if (!Array.isArray(content)) return '';
  for (const block of content) {
    if (block && typeof block === 'object') {
      const b = block as Record<string, unknown>;
      if (b.type === 'tool_result') {
        const inner = b.content;
        if (typeof inner === 'string') return inner;
        if (Array.isArray(inner)) {
          for (const sub of inner) {
            if (sub && typeof sub === 'object') {
              const s = sub as Record<string, unknown>;
              if (typeof s.text === 'string') return s.text;
            }
          }
        }
        return 'result';
      }
    }
  }
  return '';
}

function stringField(obj: Record<string, unknown>, key: string): string {
  const v = obj[key];
  return typeof v === 'string' ? v : '';
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 1) + '…';
}
