import { useTeamStore, Worker } from '../store/team';

// WorkersPanel renders the active-workers manifest under the hero.
// One row per worker the daemon's teamSnapshot decided is active
// (Persona, Role tag, Activity, Age). Subscribes to snapshot.workers
// only — pulse/usage/event-log changes leave this panel untouched.
export function WorkersPanel() {
  const workers = useTeamStore((s) => s.snapshot?.workers ?? emptyWorkers);
  return (
    <section className="workers-panel" aria-label="active workers">
      <h3 className="panel-label">
        Active workers <span className="count">{workers.length}</span>
      </h3>
      {workers.length === 0 ? (
        <div className="workers-empty">All idle — no agents running.</div>
      ) : (
        <div className="workers">
          {workers.map((w) => (
            <WorkerRow key={w.agent_id} worker={w} />
          ))}
        </div>
      )}
    </section>
  );
}

function WorkerRow({ worker }: { worker: Worker }) {
  const activityClass = worker.activity ? 'worker-doing' : 'worker-doing empty';
  return (
    <div className="worker-row">
      <div className="worker-name">{worker.persona}</div>
      <span className={`role-tag ${worker.role_colour_class}`}>{worker.role_tag}</span>
      <div className={activityClass}>{worker.activity || '—'}</div>
      <span className="worker-age">{worker.age || '—'}</span>
    </div>
  );
}

const emptyWorkers: Worker[] = [];
