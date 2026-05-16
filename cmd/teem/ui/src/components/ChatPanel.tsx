import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  KeyboardEvent,
} from 'react';
import { marked } from 'marked';
import { useTeamStore } from '../store/team';
import { streamChat } from '../api/chat';

// ChatPanel is a component-local, focus-stable replacement for the SSR
// chat block in cmd/teem/ui_dashboard.html. The SSR version loses
// focus/draft/scroll on every 10s meta-refresh; this one mounts once
// for the lifetime of the SPA, owns its own `messages` / `draft` /
// `streaming` state (NOT in the Zustand store, to avoid re-rendering
// other panels), and persists the draft to sessionStorage so it
// survives an accidental reload.
//
// Visual: reuses the SSR CSS class names (.chat-panel, .chat-log,
// .chat-msg, .chat-compose, etc.) — those classes are already defined
// in ui_dashboard.html and scoped under body.team-detail-page, which
// DashboardLayout adds to <body> on mount.

type Role = 'user' | 'leader';

interface ChatMessage {
  id: string;
  role: Role;
  text: string;
  stamp: string;
  pending?: boolean;
  error?: boolean;
}

const SCROLL_BOTTOM_SLOP_PX = 24;

export function ChatPanel() {
  const teamID = useTeamStore((s) => s.teamID);
  if (!teamID) return null;
  return <ChatPanelInner teamID={teamID} />;
}

function ChatPanelInner({ teamID }: { teamID: string }) {
  const draftKey = `teem.chat.draft.${teamID}`;
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [draft, setDraft] = useState<string>(() => loadDraft(draftKey));
  const [streaming, setStreaming] = useState(false);

  const logRef = useRef<HTMLDivElement | null>(null);
  const inputRef = useRef<HTMLTextAreaElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  // Tracks whether the user was pinned to the bottom *before* the
  // latest DOM update — we only auto-scroll if they were.
  const wasAtBottomRef = useRef(true);

  // Persist draft on every keystroke. sessionStorage is per-tab, so
  // closing the tab clears it — matching the SSR behaviour.
  useEffect(() => {
    try {
      if (draft) sessionStorage.setItem(draftKey, draft);
      else sessionStorage.removeItem(draftKey);
    } catch {
      // ignore quota / private-mode failures
    }
  }, [draftKey, draft]);

  // Reload the draft when the team changes (mount). Other panels in
  // the SPA never unmount this component, so this only fires on first
  // mount in practice.
  useEffect(() => {
    setDraft(loadDraft(draftKey));
  }, [draftKey]);

  // Before the DOM updates, capture whether the log is at the bottom.
  useLayoutEffect(() => {
    const el = logRef.current;
    if (!el) return;
    wasAtBottomRef.current =
      el.scrollHeight - el.scrollTop - el.clientHeight < SCROLL_BOTTOM_SLOP_PX;
  });

  // After the DOM updates, if the user was at the bottom, keep them
  // pinned. This is the "scroll discipline" requirement — never yank
  // the viewport away from a user who has scrolled up to read history.
  useEffect(() => {
    const el = logRef.current;
    if (!el) return;
    if (wasAtBottomRef.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [messages]);

  // Abort any in-flight stream on unmount so we don't leak a fetch
  // body reader or fire setState into a dead component.
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
    };
  }, []);

  const onCancel = useCallback(() => {
    const ctrl = abortRef.current;
    if (!ctrl) return;
    ctrl.abort();
    abortRef.current = null;
    setMessages((prev) =>
      prev.map((m) =>
        m.pending && m.role === 'leader'
          ? {
              ...m,
              pending: false,
              stamp: fmtStamp(new Date()) + ' · cancelled',
              error: true,
              text: (m.text || '') + (m.text ? '\n\n' : '') + 'cancelled',
            }
          : m,
      ),
    );
    setStreaming(false);
  }, []);

  const onSend = useCallback(() => {
    const text = draft.trim();
    if (!text) return;
    // `/done` is a client-side cancel verb — it never reaches the
    // server. It only does anything while a stream is in flight.
    if (text === '/done') {
      setDraft('');
      if (streaming) onCancel();
      return;
    }
    if (streaming) return;

    const userMsg: ChatMessage = {
      id: nextID(),
      role: 'user',
      text,
      stamp: fmtStamp(new Date()) + ' · sent',
    };
    const leaderMsg: ChatMessage = {
      id: nextID(),
      role: 'leader',
      text: '',
      stamp: 'thinking…',
      pending: true,
    };
    const leaderID = leaderMsg.id;

    setMessages((prev) => [...prev, userMsg, leaderMsg]);
    setDraft('');
    setStreaming(true);

    const controller = new AbortController();
    abortRef.current = controller;

    void streamChat(teamID, text, controller.signal, {
      onText(chunk) {
        setMessages((prev) =>
          prev.map((m) =>
            m.id === leaderID
              ? { ...m, text: m.text + chunk, pending: false }
              : m,
          ),
        );
      },
      onError(msg) {
        setMessages((prev) =>
          prev.map((m) =>
            m.id === leaderID
              ? {
                  ...m,
                  text:
                    (m.text ? m.text + '\n\n' : '') + 'error: ' + msg,
                  pending: false,
                  error: true,
                  stamp: fmtStamp(new Date()) + ' · failed',
                }
              : m,
          ),
        );
        finalize(controller);
      },
      onDone() {
        setMessages((prev) =>
          prev.map((m) =>
            m.id === leaderID
              ? {
                  ...m,
                  pending: false,
                  stamp: fmtStamp(new Date()) + ' · turn complete',
                }
              : m,
          ),
        );
        finalize(controller);
      },
    });

    function finalize(ctrl: AbortController) {
      if (abortRef.current === ctrl) abortRef.current = null;
      setStreaming(false);
      // Restore focus to the input so the operator can keep typing
      // without a manual click — matches SSR behaviour.
      inputRef.current?.focus();
    }
  }, [draft, streaming, teamID, onCancel]);

  const onKeyDown = useCallback(
    (e: KeyboardEvent<HTMLTextAreaElement>) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
        e.preventDefault();
        onSend();
      }
    },
    [onSend],
  );

  return (
    <section className="chat-panel" aria-label="chat with leader">
      <details className="section-collapsible" open>
        <summary>
          <h3 className="panel-label">
            Chat with leader <span className="count">one-shot · live</span>
          </h3>
        </summary>
        <div className="chat-log" ref={logRef} aria-live="polite">
          {messages.map((m) => (
            <MessageRow key={m.id} msg={m} />
          ))}
        </div>
        <form
          className="chat-compose"
          onSubmit={(e) => {
            e.preventDefault();
            onSend();
          }}
        >
          <textarea
            ref={inputRef}
            className="chat-input"
            placeholder="Ask the leader something — or hand it a one-shot directive. Each send is a fresh turn."
            rows={2}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={onKeyDown}
          />
          {streaming ? (
            <button
              type="button"
              className="chat-send"
              onClick={onCancel}
              title="Cancel the in-flight turn"
            >
              CANCEL
            </button>
          ) : (
            <button
              type="submit"
              className="chat-send"
              disabled={draft.trim().length === 0}
            >
              SEND
            </button>
          )}
        </form>
        <div className="chat-hint">
          Each send opens a fresh leader turn (no resumed session).{' '}
          <kbd>&#8984;</kbd>+<kbd>&#8629;</kbd> to submit. Type{' '}
          <code>/done</code> on its own line to cancel an in-flight turn.
        </div>
      </details>
    </section>
  );
}

function MessageRow({ msg }: { msg: ChatMessage }) {
  const cls = `chat-msg ${msg.role}${msg.pending ? ' pending' : ''}${
    msg.error ? ' error' : ''
  }`;
  return (
    <div className={cls}>
      <div className="avatar">{msg.role === 'user' ? 'YOU' : 'LDR'}</div>
      <div>
        {msg.role === 'leader' ? (
          <div
            className="body"
            // Leader output is markdown. marked emits HTML; we trust
            // the leader's own output since the daemon is the only
            // writer on this channel (localhost / tailnet boundary).
            dangerouslySetInnerHTML={{ __html: renderLeaderMarkdown(msg.text) }}
          />
        ) : (
          // Operator turns are plain text — preserves whitespace and
          // avoids accidental markdown rendering of paths / code.
          <div className="body" style={{ whiteSpace: 'pre-wrap' }}>
            {msg.text}
          </div>
        )}
        <div className="stamp">{msg.stamp}</div>
      </div>
    </div>
  );
}

function renderLeaderMarkdown(text: string): string {
  if (!text) return '';
  try {
    // marked.parse with async:false returns a string synchronously.
    return marked.parse(text, { async: false }) as string;
  } catch {
    return escapeHTML(text);
  }
}

function escapeHTML(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

function fmtStamp(d: Date): string {
  const pad = (n: number) => (n < 10 ? `0${n}` : `${n}`);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

let idCounter = 0;
function nextID(): string {
  idCounter += 1;
  return `m${Date.now().toString(36)}${idCounter.toString(36)}`;
}

function loadDraft(key: string): string {
  try {
    return sessionStorage.getItem(key) ?? '';
  } catch {
    return '';
  }
}
