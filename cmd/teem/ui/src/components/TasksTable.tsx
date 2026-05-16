import { DashboardTask, useTeamStore } from '../store/team';

// TasksTable renders snapshot.tasks.open — the per-team open-task list.
// Server-side teamSnapshot already sorts by stage order (see
// stageOrder in cmd/teem/ui.go); within-stage ordering is preserved
// from plan.Plan iteration. The spec asks for "stage_entered_at desc
// within stage" — we'll add that ordering when the JSON payload
// surfaces the timestamp (currently only `stage_ago` is exposed, which
// is a rendered duration string that can't be sorted).
export function TasksTable() {
  const open = useTeamStore((s) => s.snapshot?.tasks.open ?? emptyTasks);
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
              <th>Task</th>
              <th>Title</th>
              <th>Stage</th>
              <th>Assignee</th>
              <th>Age</th>
            </tr>
          </thead>
          <tbody>
            {open.map((t) => (
              <TaskRow key={t.id} task={t} />
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function TaskRow({ task }: { task: DashboardTask }) {
  const assigneeClass = ['assignee']
    .concat(task.assigned_to && !task.assignee_active ? ['gone'] : [])
    .concat(task.assignee_derived ? ['derived'] : [])
    .join(' ');
  const titleNode = task.url ? <a href={task.url}>{task.title}</a> : <>{task.title}</>;
  const idNode = task.url ? <a href={task.url}>{task.id}</a> : <>{task.id}</>;
  return (
    <tr>
      <td className="id">{idNode}</td>
      <td>{titleNode}</td>
      <td>
        {task.stage && <span className={`stage ${task.stage}`}>{task.stage}</span>}
        {task.stale && (
          <span
            className="stale-pill"
            title="active stage but the assignee is no longer running — reassign or move the task"
          >
            STALE
          </span>
        )}
      </td>
      <td
        className={assigneeClass}
        title={task.assignee_derived ? 'inferred from the latest evidence job' : undefined}
      >
        {task.assigned_to || '—'}
      </td>
      <td className="when">{task.stage_ago}</td>
    </tr>
  );
}

const emptyTasks: DashboardTask[] = [];
