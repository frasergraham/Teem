import { useState } from 'react';

import { useTeamStore, Worker } from '../store/team';
import { WatchTranscriptModal } from './WatchTranscriptModal';

// WorkersPanel renders the active-workers manifest under the hero.
// One row per worker the daemon's teamSnapshot decided is active
// (Persona, Role tag, Activity, Age). Subscribes to snapshot.workers
// only — pulse/usage/event-log changes leave this panel untouched.
//
// Watch button: rows whose snapshot.current_job_id is non-empty get a
// "Watch" affordance that opens WatchTranscriptModal against
// /api/teams/<id>/transcripts/<agent>/<job>/watch.
export function WorkersPanel() {
  const workers = useTeamStore((s) => s.snapshot?.workers ?? emptyWorkers);
  const [watching, setWatching] = useState<Worker | null>(null);
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
            <WorkerRow key={w.agent_id} worker={w} onWatch={() => setWatching(w)} />
          ))}
        </div>
      )}
      {watching && watching.current_job_id && (
        <WatchTranscriptModal
          agentID={watching.agent_id}
          jobID={watching.current_job_id}
          persona={watching.persona}
          onClose={() => setWatching(null)}
        />
      )}
    </section>
  );
}

function WorkerRow({ worker, onWatch }: { worker: Worker; onWatch: () => void }) {
  const activityClass = worker.activity ? 'worker-doing' : 'worker-doing empty';
  return (
    <div className="worker-row">
      <div className="worker-name">{worker.persona}</div>
      <span className={`role-tag ${worker.role_colour_class}`}>{worker.role_tag}</span>
      <div className={activityClass}>{worker.activity || '—'}</div>
      <span className="worker-age">{worker.age || '—'}</span>
      {worker.current_job_id ? (
        <button
          type="button"
          className="worker-watch-btn"
          onClick={onWatch}
          aria-label={`watch live transcript for ${worker.persona}`}
          title="Stream this worker's live transcript"
        >
          Watch
        </button>
      ) : (
        <span className="worker-watch-placeholder" aria-hidden="true" />
      )}
    </div>
  );
}

const emptyWorkers: Worker[] = [];
