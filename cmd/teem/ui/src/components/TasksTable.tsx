import { useEffect, useState, KeyboardEvent, MouseEvent } from 'react';

import { APIError } from '../api/client';
import { markReady } from '../api/control';
import { DashboardTask, useTeamStore } from '../store/team';
import { TaskDetailModal } from './TaskDetailModal';

// TasksTable renders snapshot.tasks.open — the per-team open-task list.
// Server-side teamSnapshot already sorts by stage order (see
// stageOrder in cmd/teem/ui.go); within-stage ordering is preserved
// from plan.Plan iteration. The spec asks for "stage_entered_at desc
// within stage" — we'll add that ordering when the JSON payload
// surfaces the timestamp (currently only `stage_ago` is exposed, which
// is a rendered duration string that can't be sorted).
export function TasksTable() {
  const open = useTeamStore((s) => s.snapshot?.tasks.open ?? emptyTasks);
  const recentDone = useTeamStore((s) => s.snapshot?.tasks.recent_done ?? emptyTasks);
  const [openTaskID, setOpenTaskID] = useState<string | null>(null);

  // Resolve the modal's task from the live snapshot so streamed updates
  // (stage flips, notes edits) re-render the open modal without
  // requiring the operator to reopen it.
  const modalTask =
    openTaskID === null
      ? null
      : open.find((t) => t.id === openTaskID) ??
        recentDone.find((t) => t.id === openTaskID) ??
        null;

  // If the task disappears from both buckets (e.g. shelved while the
  // modal is open), close the modal so we don't render a dangling id.
  useEffect(() => {
    if (openTaskID !== null && modalTask === null) setOpenTaskID(null);
  }, [openTaskID, modalTask]);

  return (
    <section className="tasks-panel" aria-label="open tasks">
      <h3 className="panel-label">
        Open tasks <span className="count">{open.length}</span>
      </h3>
      {open.length === 0 ? (
        <div className="tasks-empty">no open tasks — the leader hasn't broken work down yet</div>
      ) : (
        <table className="tasks">
          <thead>
            <tr>
              <th>Title</th>
              <th>Stage</th>
              <th>Assignee</th>
              <th>In stage</th>
            </tr>
          </thead>
          <tbody>
            {open.map((t) => (
              <TaskRow key={t.id} task={t} onOpen={setOpenTaskID} />
            ))}
          </tbody>
        </table>
      )}
      {recentDone.length > 0 && (
        <details className="tasks-done">
          <summary>
            Recently completed <span className="count">{recentDone.length}</span>
          </summary>
          <table className="tasks">
            <thead>
              <tr>
                <th>Title</th>
                <th>Stage</th>
                <th>Assignee</th>
                <th>Verified</th>
              </tr>
            </thead>
            <tbody>
              {recentDone.map((t) => (
                <TaskRow key={t.id} task={t} onOpen={setOpenTaskID} />
              ))}
            </tbody>
          </table>
        </details>
      )}
      {modalTask && (
        <TaskDetailModal task={modalTask} onClose={() => setOpenTaskID(null)} />
      )}
    </section>
  );
}

function TaskRow({ task, onOpen }: { task: DashboardTask; onOpen: (id: string) => void }) {
  // Hide assignee on terminal stages — once a task is verified/done the
  // assignee adds no signal (and a crossed-out "(gone)" worker is just
  // visual noise on a completed row).
  const terminalStage = task.stage === 'verified' || task.stage === 'done';
  const assigneeClass = ['assignee']
    .concat(!terminalStage && task.assigned_to && !task.assignee_active ? ['gone'] : [])
    .concat(!terminalStage && task.assignee_derived ? ['derived'] : [])
    .join(' ');
  const assigneeText = terminalStage ? '—' : task.assigned_to || '—';

  function handleClick(e: MouseEvent<HTMLTableRowElement>) {
    // Don't intercept clicks on interactive children — the "→ ready"
    // button has its own handler, and a future link in the row should
    // win over the row-level open.
    const target = e.target as HTMLElement;
    if (target.closest('button, a, input, select, textarea')) return;
    onOpen(task.id);
  }

  function handleKey(e: KeyboardEvent<HTMLTableRowElement>) {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      onOpen(task.id);
    }
  }

  return (
    <tr
      className="task-row"
      tabIndex={0}
      role="button"
      aria-label={`open details for ${task.id}`}
      onClick={handleClick}
      onKeyDown={handleKey}
    >
      <td className="title" data-label="Title">{task.title}</td>
      <td data-label="Stage">
        {task.stage && <span className={`stage ${task.stage}`}>{task.stage}</span>}
        {task.stale && (
          <span
            className="stale-pill"
            title="active stage but the assignee is no longer running — reassign or move the task"
          >
            STALE
          </span>
        )}
        <ReadyAffordance task={task} />
      </td>
      <td
        className={assigneeClass}
        data-label="Assignee"
        title={!terminalStage && task.assignee_derived ? 'inferred from the latest evidence job' : undefined}
      >
        {assigneeText}
      </td>
      <td className="when" data-label="In stage">{task.stage_ago}</td>
    </tr>
  );
}

// ReadyAffordance is the per-row "→ ready" control. For proposed /
// specced tasks it renders a small button that flips the stage to
// `ready` via POST /control/teams/<id>/tasks/<task_id>/ready. The
// optimistic patch lands instantly; on HTTP error we roll back and
// surface the message via title= so the operator can hover for context
// (no toast infra yet). For `ready` tasks we render a lit indicator —
// no button — so the operator can see the signal is already set.
function ReadyAffordance({ task }: { task: DashboardTask }) {
  const teamID = useTeamStore((s) => s.teamID);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (task.stage === 'ready') {
    return (
      <span className="ready-dot" title="operator marked this task ready for dispatch" aria-label="ready">
        ●
      </span>
    );
  }
  if (task.stage !== 'proposed' && task.stage !== 'specced') return null;
  if (!teamID) return null;

  async function onClick() {
    if (submitting) return;
    setSubmitting(true);
    setError(null);
    const prevStage = task.stage;
    // Optimistic patch: flip the stage in-place so the row repaints
    // immediately. Roll back on HTTP error.
    useTeamStore.setState((state) => {
      if (!state.snapshot) return {};
      const open = state.snapshot.tasks.open.map((row) =>
        row.id === task.id ? { ...row, stage: 'ready' } : row,
      );
      return {
        snapshot: {
          ...state.snapshot,
          tasks: { ...state.snapshot.tasks, open },
        },
      };
    });
    try {
      await markReady(teamID!, task.id);
    } catch (err) {
      // Roll the optimistic patch back to whatever stage the row had.
      useTeamStore.setState((state) => {
        if (!state.snapshot) return {};
        const open = state.snapshot.tasks.open.map((row) =>
          row.id === task.id ? { ...row, stage: prevStage } : row,
        );
        return {
          snapshot: {
            ...state.snapshot,
            tasks: { ...state.snapshot.tasks, open },
          },
        };
      });
      if (err instanceof APIError) setError(err.message);
      else setError(String(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <button
      type="button"
      className="mark-ready-btn"
      onClick={onClick}
      disabled={submitting}
      title={error ?? 'mark ready for dispatch — leader picks it up on the next pulse tick'}
      aria-label={`mark ${task.id} ready`}
    >
      → ready
    </button>
  );
}

const emptyTasks: DashboardTask[] = [];
