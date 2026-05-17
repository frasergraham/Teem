import { useEffect, useRef, useState } from 'react';

import { DashboardTask, useTeamStore } from '../store/team';
import { renderMarkdownSafe } from '../lib/markdown';
import {
  TaskDetailPayload,
  TaskJob,
  TaskTimelineEvent,
  TaskTokens,
  fetchTaskDetail,
} from '../api/task_detail';
import { APIError } from '../api/client';

// TaskDetailModal restores click-to-see-details for the per-team task
// rows. The body is a chronological participation log built from the
// audit timeline — one row per significant event (stage transitions,
// job lifecycle, operator decisions). Rows are oldest-first so the
// reader can follow the task from the moment it was proposed through
// every contributor.
//
// Personas: agent_id `<role>-<name>` is rendered as `<RoleLabel>
// <CapitalizedName>` so the reader sees "Coder Ada" instead of
// "worker-ada". The role label is the persona, not the raw role:
// worker → Coder, reviewer → Reviewer, integrator → Integrator,
// project_manager → PM. The synthetic ids "leader" and "operator"
// render unchanged.
//
// Verbs are past-tense, kind-keyed: job_complete picks a role-specific
// verb (coded / reviewed / integrated / consulted) so a glance at
// the row tells you what kind of work landed. There is no "gone" or
// "stale" styling — the log is a record of what happened, not a
// commentary on the agent's current health.

interface Props {
  task: DashboardTask;
  onClose: () => void;
}

export function TaskDetailModal({ task, onClose }: Props) {
  const closeRef = useRef<HTMLButtonElement | null>(null);
  const teamID = useTeamStore((s) => s.teamID);
  const [detail, setDetail] = useState<TaskDetailPayload | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

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

  const notesHTML = renderMarkdownSafe(task.notes);

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
        <div className="task-modal-body">
          <TaskMeta task={task} />
          <TaskDetailLog detail={detail} loadError={loadError} />
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
          <TaskTokensPanel tokens={detail?.tokens} />
        </div>
      </div>
    </div>
  );
}

// TaskMeta renders the small key/value strip under the header.
// Terminal stages (verified/done) hide the Assignee row entirely —
// the work is finished and the assignee adds no signal.
function TaskMeta({ task }: { task: DashboardTask }) {
  const terminal = task.stage === 'verified' || task.stage === 'done';
  return (
    <dl className="task-modal-meta">
      {!terminal && (
        <div>
          <dt>Assignee</dt>
          <dd>
            {task.assigned_to || '—'}
            {task.assignee_derived && <em> (derived)</em>}
          </dd>
        </div>
      )}
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
  );
}

function TaskTokensPanel({ tokens }: { tokens: TaskTokens | undefined }) {
  if (!tokens) return null;
  const total = tokens.input + tokens.output + tokens.cache_create + tokens.cache_read;
  if (total === 0 && tokens.jobs.length === 0) return null;
  return (
    <section className="task-modal-tokens" aria-label="token usage for this task">
      <h3 className="task-modal-h3">Token usage</h3>
      <div className="usage-stats">
        <UsageStat label="tokens in" value={tokens.input} />
        <UsageStat label="tokens out" value={tokens.output} />
        <UsageStat label="cache hits" value={tokens.cache_read} />
        <UsageStat label="cache writes" value={tokens.cache_create} />
      </div>
      {tokens.jobs.length > 0 && (
        <details className="usage-models">
          <summary>per-job breakdown ({tokens.jobs.length})</summary>
          <table className="usage-models-table">
            <thead>
              <tr>
                <th>agent · job</th>
                <th>model</th>
                <th>input</th>
                <th>output</th>
                <th>cache write</th>
                <th>cache hit</th>
              </tr>
            </thead>
            <tbody>
              {tokens.jobs.map((j) => (
                <tr key={j.job_id}>
                  <td>
                    {j.agent_id || '—'} · {j.job_id.slice(0, 8)}
                  </td>
                  <td>{j.model || '—'}</td>
                  <td>{j.input.toLocaleString()}</td>
                  <td>{j.output.toLocaleString()}</td>
                  <td>{j.cache_create.toLocaleString()}</td>
                  <td>{j.cache_read.toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}
    </section>
  );
}

function UsageStat({ label, value }: { label: string; value: number }) {
  return (
    <div className="usage-stat">
      <span className="n">{value.toLocaleString()}</span>
      <span className="lbl">{label}</span>
    </div>
  );
}

// TaskDetailLog sits at the top of the modal body as a collapsible
// section — closed by default so the notes/tokens are the first thing
// the operator sees, and the chronological history is one click away.
function TaskDetailLog({
  detail,
  loadError,
}: {
  detail: TaskDetailPayload | null;
  loadError: string | null;
}) {
  const rows = detail ? buildLogRows(detail) : [];
  const summary = loadError
    ? 'Task log (failed to load)'
    : !detail
      ? 'Task log (loading…)'
      : rows.length === 0
        ? 'Task log (empty)'
        : `Task log (${rows.length})`;

  return (
    <details className="task-modal-detail" aria-label="task log">
      <summary className="task-modal-detail-summary">{summary}</summary>
      {loadError ? (
        <div className="task-modal-error">Couldn't load task detail: {loadError}</div>
      ) : !detail ? (
        <div className="task-modal-loading">Loading audit history…</div>
      ) : rows.length === 0 ? (
        <div className="task-modal-log-empty">No audit activity yet for this task.</div>
      ) : (
        <ol className="task-modal-log">
          {rows.map((r, i) => (
            <li key={`${r.ts}-${i}`} className="task-log-row">
              <span className="log-time" title={r.ts}>
                [{formatTime(r.ts)}]
              </span>
              <span className="log-persona">{r.persona}</span>
              <span className="log-verb">{r.verb}</span>
              {r.jobID && (
                <>
                  <span className="log-sep">·</span>
                  <span className="log-job-id">job {shortJobID(r.jobID)}</span>
                </>
              )}
              {r.transcriptURL && (
                <>
                  <span className="log-sep">·</span>
                  <a
                    className="log-transcript"
                    href={r.transcriptURL}
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    transcript{r.transcriptBytes ? ` (${formatBytes(r.transcriptBytes)})` : ''}
                  </a>
                </>
              )}
              {r.detail && <div className="log-detail">{r.detail}</div>}
            </li>
          ))}
        </ol>
      )}
    </details>
  );
}

// LogRow is one row in the chronological participation log. ts is
// ISO-8601 from the server; persona is already human-formatted
// ("Coder Ada", "Operator"); verb is past-tense.
interface LogRow {
  ts: string;
  persona: string;
  verb: string;
  jobID?: string;
  transcriptURL?: string;
  transcriptBytes?: number;
  detail?: string;
}

// buildLogRows folds detail.timeline into a chronological row list.
// Returns oldest-first. Skips event kinds that don't represent a
// significant participation moment (heartbeats, channels_state,
// usage_event — none of which carry task-scoped meaning here).
//
// The first row is always synthetic: "<Origin> created this task". It
// uses the audit task_created event's timestamp when present (so the
// row reflects when the audit log saw the create), otherwise falls
// back to task.created_at. The task_created audit row itself is then
// suppressed from the rest of the log so it doesn't appear twice.
export function buildLogRows(detail: TaskDetailPayload): LogRow[] {
  const jobByID = new Map<string, TaskJob>();
  for (const j of detail.jobs) {
    jobByID.set(j.job_id, j);
  }
  const rows: LogRow[] = [];
  for (const e of detail.timeline) {
    if (e.kind === 'task_created') continue;
    if (!shouldShow(e.kind)) continue;
    const row = renderEvent(e, jobByID);
    if (row) rows.push(row);
  }
  // detail.timeline is newest-first per buildTaskTimeline; flip to
  // oldest-first so the participation log reads top-down by time.
  rows.sort((a, b) => (a.ts < b.ts ? -1 : a.ts > b.ts ? 1 : 0));

  const creation = buildCreationRow(detail);
  if (creation) rows.unshift(creation);
  return rows;
}

// buildCreationRow synthesises the first log entry: "<Persona> created
// this task". Origin → persona mapping:
//   - operator        → Operator
//   - leader          → Leader
//   - project_manager → PM, with a " (via project_manager)" suffix
//   - system          → System
// A leader-origin task with a parent_id gets "(follow-up from <parent>)"
// appended; the parent's title isn't on the wire so we fall back to the
// raw parent_id, which is still unambiguous and immutable.
//
// Legacy tasks loaded from disk without an Origin field still come back
// from the server defaulted to "operator" — but if a very old snapshot
// pre-dates that backfill and arrives blank, we apply the same fallback
// here so the SPA never renders an empty creator.
function buildCreationRow(detail: TaskDetailPayload): LogRow | null {
  const task = detail.task;
  if (!task) return null;
  const created = findTaskCreatedEvent(detail.timeline);
  const ts = created?.ts || task.created_at;
  if (!ts) return null;
  const origin = (task.origin || 'operator') as string;
  let persona = 'Operator';
  let suffix = '';
  switch (origin) {
    case 'operator':
      persona = 'Operator';
      break;
    case 'leader':
      persona = 'Leader';
      if (task.parent_id) {
        suffix = ` (follow-up from ${task.parent_id})`;
      }
      break;
    case 'project_manager':
      persona = 'PM';
      suffix = ' (via project_manager)';
      break;
    case 'system':
      persona = 'System';
      break;
    default:
      persona = capitalize(origin);
      break;
  }
  return {
    ts,
    persona,
    verb: `created this task${suffix}`,
  };
}

function findTaskCreatedEvent(
  timeline: TaskTimelineEvent[],
): TaskTimelineEvent | undefined {
  for (const e of timeline) {
    if (e.kind === 'task_created') return e;
  }
  return undefined;
}

function shouldShow(kind: string): boolean {
  switch (kind) {
    case 'job_received':
    case 'job_complete':
    case 'job_error':
    case 'job_interrupted':
    case 'task_stage_changed':
    case 'decision_note':
    case 'blocker_note':
      return true;
    default:
      // job_transcript_ready, heartbeat, channels_state, usage_event,
      // pulse_tick, pm_tick — informative but not participation rows.
      return false;
  }
}

function renderEvent(
  e: TaskTimelineEvent,
  jobByID: Map<string, TaskJob>,
): LogRow | null {
  const persona = formatPersona(e.agent_id);
  const role = roleFrom(e.agent_id);
  const job = e.job_id ? jobByID.get(e.job_id) : undefined;

  switch (e.kind) {
    case 'job_received':
      return {
        ts: e.ts,
        persona,
        verb: 'started a job',
        jobID: e.job_id,
      };
    case 'job_complete': {
      const verb = completeVerb(role);
      const row: LogRow = { ts: e.ts, persona, verb, jobID: e.job_id };
      if (job?.transcript_url) {
        row.transcriptURL = job.transcript_url;
        row.transcriptBytes = job.transcript_bytes;
      }
      return row;
    }
    case 'job_error': {
      const row: LogRow = {
        ts: e.ts,
        persona,
        verb: 'errored on a job',
        jobID: e.job_id,
      };
      if (job?.transcript_url) {
        row.transcriptURL = job.transcript_url;
        row.transcriptBytes = job.transcript_bytes;
      }
      const msg = stringMeta(e.meta, 'error') || e.message;
      if (msg) row.detail = msg;
      return row;
    }
    case 'job_interrupted':
      return {
        ts: e.ts,
        persona,
        verb: 'was interrupted on a job',
        jobID: e.job_id,
      };
    case 'task_stage_changed': {
      const to = stringMeta(e.meta, 'stage') || stringMeta(e.meta, 'to');
      const from = stringMeta(e.meta, 'from');
      let verb = 'moved task';
      if (to && from) verb = `moved task from ${from} to ${to}`;
      else if (to) verb = `moved task to ${to}`;
      return { ts: e.ts, persona, verb };
    }
    case 'decision_note': {
      const decision = stringMeta(e.meta, 'decision');
      const sev = stringMeta(e.meta, 'severity');
      let verb = 'recorded a decision';
      if (decision === 'approve') verb = 'approved';
      else if (decision === 'reject') verb = 'rejected';
      else if (decision === 'comment') verb = 'commented';
      else if (sev === 'question') verb = 'flagged a question';
      const detail = e.message || stringMeta(e.meta, 'comment');
      return { ts: e.ts, persona, verb, detail: detail || undefined };
    }
    case 'blocker_note':
      return {
        ts: e.ts,
        persona,
        verb: 'raised a blocker',
        detail: e.message || undefined,
      };
    default:
      return null;
  }
}

// roleFrom returns the role component of an agent_id ("worker-ada"
// → "worker"). Synthetic ids ("leader", "operator") and bare ids
// fall through.
function roleFrom(agentID?: string): string {
  if (!agentID) return '';
  const i = agentID.indexOf('-');
  if (i <= 0) return agentID;
  return agentID.slice(0, i);
}

// formatPersona renders agent_id as "<RoleLabel> <CapitalizedName>".
// Unknown roles render as the raw agent_id so corrupt or future
// roles stay diagnosable.
function formatPersona(agentID?: string): string {
  if (!agentID) return '—';
  if (agentID === 'leader') return 'Leader';
  if (agentID === 'operator') return 'Operator';
  const dash = agentID.indexOf('-');
  if (dash <= 0) return capitalize(agentID);
  const role = agentID.slice(0, dash);
  const name = agentID.slice(dash + 1);
  const label = roleLabel(role);
  if (!label) return agentID;
  return `${label} ${capitalize(name)}`;
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

function completeVerb(role: string): string {
  switch (role) {
    case 'worker':
      return 'coded';
    case 'reviewer':
      return 'reviewed';
    case 'integrator':
      return 'integrated';
    case 'project_manager':
      return 'consulted';
    default:
      return 'completed a job';
  }
}

function capitalize(s: string): string {
  if (!s) return s;
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function stringMeta(meta: Record<string, unknown> | undefined, key: string): string {
  if (!meta) return '';
  const v = meta[key];
  return typeof v === 'string' ? v : '';
}

function shortJobID(id?: string): string {
  if (!id) return '';
  return id.length > 8 ? id.slice(0, 8) : id;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function formatTime(ts: string): string {
  // The participation log is a record, not a live tail — show the
  // full UTC date+time on every row so a session pieced together
  // hours later is unambiguous.
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  const y = d.getUTCFullYear();
  const m = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  const hh = String(d.getUTCHours()).padStart(2, '0');
  const mm = String(d.getUTCMinutes()).padStart(2, '0');
  return `${y}-${m}-${day} ${hh}:${mm} UTC`;
}

