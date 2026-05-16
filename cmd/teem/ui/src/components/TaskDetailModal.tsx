import { useEffect, useRef, useState } from 'react';
import { marked } from 'marked';

import { DashboardTask, useTeamStore } from '../store/team';
import {
  TaskAgentRollup,
  TaskDetailPayload,
  TaskJob,
  TaskTimelineEvent,
  fetchTaskDetail,
} from '../api/task_detail';
import { APIError } from '../api/client';

// TaskDetailModal restores click-to-see-details for the per-team task
// rows. Phase 4 deleted the SSR /teams/<id>/tasks/<id> task-flow page;
// this modal is the replacement. The plan-side `task` prop renders
// the header + notes immediately; in parallel we fetch
// /api/teams/<id>/tasks/<task-id> to layer in the audit timeline,
// per-agent rollup, and per-evidence transcript links.
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
  const teamID = useTeamStore((s) => s.teamID);
  const [detail, setDetail] = useState<TaskDetailPayload | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

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

  // Fetch the detail payload when the modal opens (or when the open
  // task changes — streamed snapshot updates that re-resolve modalTask
  // to a different id should refetch).
  useEffect(() => {
    if (!teamID) return;
    const ac = new AbortController();
    setDetail(null);
    setLoadError(null);
    fetchTaskDetail(teamID, task.id, ac.signal)
      .then((p) => setDetail(p))
      .catch((err) => {
        if (ac.signal.aborted) return;
        setLoadError(err instanceof APIError ? err.message : String(err));
      });
    return () => ac.abort();
  }, [teamID, task.id]);

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
        <TaskDetailLoaded detail={detail} loadError={loadError} />
      </div>
    </div>
  );
}

// TaskDetailLoaded renders the three extra sections once the server
// response arrives: agent rollup, evidence jobs (with transcript
// links), and the audit timeline. Each section short-circuits to an
// empty state when its slice is empty — a brand-new task with no
// audit activity yet should not show three placeholders.
function TaskDetailLoaded({
  detail,
  loadError,
}: {
  detail: TaskDetailPayload | null;
  loadError: string | null;
}) {
  if (loadError) {
    return (
      <section className="task-modal-detail" aria-label="task detail">
        <div className="task-modal-error">Couldn't load task detail: {loadError}</div>
      </section>
    );
  }
  if (!detail) {
    return (
      <section className="task-modal-detail" aria-label="task detail">
        <div className="task-modal-loading">Loading audit history…</div>
      </section>
    );
  }
  return (
    <section className="task-modal-detail" aria-label="task detail">
      <AgentRollupBlock agents={detail.agents} />
      <EvidenceBlock jobs={detail.jobs} />
      <TimelineBlock events={detail.timeline} />
    </section>
  );
}

function AgentRollupBlock({ agents }: { agents: TaskAgentRollup[] }) {
  if (agents.length === 0) return null;
  return (
    <div className="task-modal-agents">
      <h3 className="task-modal-h3">
        Agents involved <span className="count">{agents.length}</span>
      </h3>
      <table className="task-modal-agent-table">
        <thead>
          <tr>
            <th>Agent</th>
            <th>Jobs</th>
            <th>Done</th>
            <th>Error</th>
            <th>Pending</th>
            <th>Last seen</th>
          </tr>
        </thead>
        <tbody>
          {agents.map((a) => (
            <tr key={a.agent_id}>
              <td className="agent-id">{a.agent_id}</td>
              <td>{a.job_count}</td>
              <td>{a.done}</td>
              <td className={a.errored > 0 ? 'has-error' : undefined}>{a.errored}</td>
              <td>{a.pending}</td>
              <td className="when" title={a.last_seen_at}>
                {a.last_seen_ago || '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function EvidenceBlock({ jobs }: { jobs: TaskJob[] }) {
  if (jobs.length === 0) return null;
  return (
    <div className="task-modal-evidence">
      <h3 className="task-modal-h3">
        Evidence <span className="count">{jobs.length}</span>
      </h3>
      <ul className="task-modal-job-list">
        {jobs.map((j) => (
          <li key={j.job_id} className={`job-row status-${j.status}`}>
            <div className="job-row-head">
              <span className="job-id" title={j.job_id}>
                {j.job_id}
              </span>
              {j.agent_id && <span className="job-agent">{j.agent_id}</span>}
              <span className={`job-status status-${j.status}`}>{j.status}</span>
              {typeof j.duration_ms === 'number' && j.duration_ms > 0 && (
                <span className="job-duration">{formatDuration(j.duration_ms)}</span>
              )}
              {j.transcript_url ? (
                <a
                  className="job-transcript-link"
                  href={j.transcript_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  title={
                    typeof j.transcript_bytes === 'number'
                      ? `${j.transcript_bytes} bytes — opens raw NDJSON in a new tab`
                      : 'opens raw NDJSON in a new tab'
                  }
                >
                  transcript ↗
                </a>
              ) : (
                <span className="job-transcript-missing" title="no transcript on disk">
                  no transcript
                </span>
              )}
            </div>
            {j.summary && <div className="job-summary">{j.summary}</div>}
          </li>
        ))}
      </ul>
    </div>
  );
}

function TimelineBlock({ events }: { events: TaskTimelineEvent[] }) {
  if (events.length === 0) return null;
  return (
    <div className="task-modal-timeline">
      <h3 className="task-modal-h3">
        Audit timeline <span className="count">{events.length}</span>
      </h3>
      <ul className="task-modal-timeline-list">
        {events.map((e, i) => (
          <li key={`${e.ts}-${i}`} className={`timeline-row source-${e.source}`}>
            <span className="timeline-time" title={e.ts}>
              {formatTime(e.ts)}
            </span>
            <span className={`timeline-kind kind-${e.kind}`}>{e.kind}</span>
            {e.agent_id && <span className="timeline-agent">{e.agent_id}</span>}
            {e.message && <span className="timeline-msg">{e.message}</span>}
          </li>
        ))}
      </ul>
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

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(1)}s`;
  const minutes = seconds / 60;
  if (minutes < 60) return `${minutes.toFixed(1)}m`;
  const hours = minutes / 60;
  return `${hours.toFixed(1)}h`;
}

function formatTime(ts: string): string {
  // Display HH:MM:SS in the operator's local TZ. Falls back to the raw
  // string if parsing fails so corrupt timestamps stay diagnosable.
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleTimeString();
}
