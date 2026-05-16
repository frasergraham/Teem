import { useEffect, useRef } from 'react';
import { marked } from 'marked';

import { DashboardTask } from '../store/team';

// TaskDetailModal restores click-to-see-details for the per-team task
// rows. Phase 4 deleted the SSR /teams/<id>/tasks/<id> task-flow page;
// the rows still rendered but the link 404'd, then commit d2bd1a4
// dropped the dead URL so clicking did nothing. This modal is the
// replacement — driven entirely by client state (`task` prop), so the
// row click can show the plan-side notes verbatim without a server
// round-trip.
//
// Markdown render path: reuses the ChatPanel pattern — `marked.parse`
// with `async: false`, output piped through `dangerouslySetInnerHTML`.
// `plan.Task.Notes` is operator/leader/PM-authored on the local
// tailnet boundary, same trust model as leader chat replies.

interface Props {
  task: DashboardTask;
  onClose: () => void;
}

export function TaskDetailModal({ task, onClose }: Props) {
  const closeRef = useRef<HTMLButtonElement | null>(null);

  // Escape closes the modal. Bound on document so the listener works
  // even when focus is outside the dialog (e.g. operator clicked
  // backdrop and lost focus before pressing Escape).
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

  // Focus the close button on mount so the operator can hit Enter or
  // Space to dismiss without reaching for the mouse.
  useEffect(() => {
    closeRef.current?.focus();
  }, []);

  const notesHTML = renderNotes(task.notes);

  return (
    <div
      className="task-modal-backdrop"
      role="presentation"
      onClick={onClose}
    >
      <div
        className="task-modal-card"
        role="dialog"
        aria-modal="true"
        aria-labelledby="task-modal-title"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="task-modal-header">
          <div className="task-modal-titles">
            <div className="task-modal-id">{task.id}</div>
            <h2 className="task-modal-title" id="task-modal-title">
              {task.title}
            </h2>
          </div>
          <div className="task-modal-pills">
            {task.stage && <span className={`stage ${task.stage}`}>{task.stage}</span>}
            {task.status && (
              <span className={`task-modal-status ${task.status}`}>{task.status}</span>
            )}
          </div>
          <button
            ref={closeRef}
            type="button"
            className="task-modal-close"
            aria-label="close task details"
            onClick={onClose}
          >
            ×
          </button>
        </header>
        <dl className="task-modal-meta">
          <div>
            <dt>Assignee</dt>
            <dd className={task.assignee_active === false ? 'gone' : undefined}>
              {task.assigned_to || '—'}
              {task.assignee_derived && <em> (derived)</em>}
            </dd>
          </div>
          <div>
            <dt>In stage</dt>
            <dd>{task.stage_ago || '—'}</dd>
          </div>
          {task.stale && (
            <div>
              <dt>Health</dt>
              <dd>
                <span className="stale-pill">STALE</span>
              </dd>
            </div>
          )}
        </dl>
        <section className="task-modal-notes" aria-label="task notes">
          {notesHTML ? (
            <div
              className="task-modal-notes-body"
              dangerouslySetInnerHTML={{ __html: notesHTML }}
            />
          ) : (
            <div className="task-modal-notes-empty">No notes recorded for this task.</div>
          )}
        </section>
      </div>
    </div>
  );
}

function renderNotes(text: string | undefined): string {
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
