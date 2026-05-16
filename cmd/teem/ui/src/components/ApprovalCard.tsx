import { useCallback, useState } from 'react';
import { marked } from 'marked';
import {
  AwaitingApprovalTask,
  PlanFile,
  StateSnapshot,
  TaskBuckets,
  useTeamStore,
} from '../store/team';
import { approveTask, commentTask, rejectTask } from '../api/control';

// ApprovalCard renders the top-of-page awaiting-approval block: one
// card per task in snapshot.tasks.awaiting_approval. Buttons fire the
// matching /control/teams/<id>/tasks/<task_id>/{approve,reject,comment}
// endpoint and optimistically pull the card out of the list — on a
// 4xx/5xx we put it back so the operator sees the failure didn't
// take. The WebSocket stream is the source of truth: when the real
// state arrives it overrides our optimistic patch.
//
// Reuses the SSR .approval-card classes already defined in
// ui_dashboard.html (scoped under body.team-detail-page, which
// DashboardLayout adds on mount).

export function ApprovalCard() {
  const teamID = useTeamStore((s) => s.teamID);
  const awaiting = useTeamStore((s) => s.snapshot?.tasks.awaiting_approval ?? emptyAwaiting);
  if (!teamID) return null;
  if (awaiting.length === 0) return null;
  return (
    <section className="approvals-section" aria-label="awaiting approval">
      <h3 className="panel-label">
        Awaiting approval <span className="count">{awaiting.length}</span>
      </h3>
      {awaiting.map((t) => (
        <ApprovalRow key={t.id} task={t} teamID={teamID} />
      ))}
    </section>
  );
}

function ApprovalRow({ task, teamID }: { task: AwaitingApprovalTask; teamID: string }) {
  const patchSnapshot = useTeamStore((s) => s.patchSnapshot);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showComment, setShowComment] = useState(false);
  const [comment, setComment] = useState('');

  // Optimistically drop the card from the awaiting_approval bucket.
  // Returns a rollback that re-inserts it at its original index.
  const removeOptimistically = useCallback((): (() => void) => {
    const snap = useTeamStore.getState().snapshot;
    if (!snap) return () => {};
    const bucket = snap.tasks.awaiting_approval ?? [];
    const idx = bucket.findIndex((t) => t.id === task.id);
    if (idx < 0) return () => {};
    const next: TaskBuckets = {
      ...snap.tasks,
      awaiting_approval: bucket.filter((t) => t.id !== task.id),
    };
    patchSnapshot({ tasks: next } as Partial<StateSnapshot>);
    return () => {
      const cur = useTeamStore.getState().snapshot;
      if (!cur) return;
      const curBucket = cur.tasks.awaiting_approval ?? [];
      if (curBucket.some((t) => t.id === task.id)) return; // wsbus already healed it
      const restored = curBucket.slice();
      restored.splice(Math.min(idx, restored.length), 0, task);
      const restoredBuckets: TaskBuckets = { ...cur.tasks, awaiting_approval: restored };
      useTeamStore
        .getState()
        .patchSnapshot({ tasks: restoredBuckets } as Partial<StateSnapshot>);
    };
  }, [task, patchSnapshot]);

  const onApprove = useCallback(async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    const rollback = removeOptimistically();
    try {
      await approveTask(teamID, task.id, comment.trim());
      setComment('');
      setShowComment(false);
    } catch (e) {
      rollback();
      setError(toMessage(e));
    } finally {
      setBusy(false);
    }
  }, [busy, removeOptimistically, teamID, task.id, comment]);

  const onReject = useCallback(async () => {
    if (busy) return;
    const reason = comment.trim();
    if (!reason) {
      setError('Reject requires a reason — type one in the comment field.');
      setShowComment(true);
      return;
    }
    setBusy(true);
    setError(null);
    const rollback = removeOptimistically();
    try {
      await rejectTask(teamID, task.id, reason);
      setComment('');
      setShowComment(false);
    } catch (e) {
      rollback();
      setError(toMessage(e));
    } finally {
      setBusy(false);
    }
  }, [busy, comment, removeOptimistically, teamID, task.id]);

  const onComment = useCallback(async () => {
    if (busy) return;
    const text = comment.trim();
    if (!text) {
      setError('Comment requires a comment.');
      setShowComment(true);
      return;
    }
    setBusy(true);
    setError(null);
    // Comment doesn't transition the task out of awaiting_approval,
    // so there is no optimistic removal. We rely on the wsbus stream
    // (decision_note → snapshot_invalidate) to bring the new note
    // through. The button just closes the textarea on success.
    try {
      await commentTask(teamID, task.id, text);
      setComment('');
      setShowComment(false);
    } catch (e) {
      setError(toMessage(e));
    } finally {
      setBusy(false);
    }
  }, [busy, comment, teamID, task.id]);

  return (
    <div className="approval-card">
      <div className="head">
        <span className="id">{task.id}</span>
        <span className="title">
          {task.url ? <a href={task.url}>{task.title}</a> : task.title}
        </span>
        <span className="when">{task.stage_ago}</span>
      </div>

      {task.evidence_rows && task.evidence_rows.length > 0 && (
        <div className="plan-artifact">
          {task.has_plan_artifact && <div className="header">Plan artifact</div>}
          {task.evidence_rows.map((ev) => (
            <div key={ev.job_id || ev.agent_id}>
              {ev.plan_shaped &&
                ev.plan_files?.map((f) => <PlanFileRow key={f.path} file={f} />)}
              <div className="evidence-row">
                {ev.agent_id && <>Worker: {ev.agent_id} · </>}
                {ev.branch_url ? (
                  <>
                    branch: <a href={ev.branch_url}>{ev.branch_ref}</a> ·{' '}
                  </>
                ) : (
                  ev.branch_ref && <>branch: {ev.branch_ref} · </>
                )}
                job: <a href={ev.job_url}>{ev.job_id}</a>
              </div>
            </div>
          ))}
        </div>
      )}

      {task.notes && (
        <details className="brief-deemph">
          <summary>Brief from leader (details collapsed)</summary>
          <div
            className="brief-body"
            dangerouslySetInnerHTML={{ __html: renderMarkdown(task.notes) }}
          />
        </details>
      )}

      {error && (
        <div className="approval-error" role="alert">
          {error}
        </div>
      )}

      <div className="actions">
        <input
          type="text"
          placeholder="optional comment"
          value={comment}
          onChange={(e) => setComment(e.target.value)}
          aria-label="comment or reason"
        />
        <button
          type="button"
          className="approve"
          disabled={busy}
          onClick={() => void onApprove()}
        >
          APPROVE
        </button>
        <button
          type="button"
          className="reject"
          disabled={busy}
          onClick={() => void onReject()}
        >
          REJECT
        </button>
        <button
          type="button"
          className="comment"
          disabled={busy}
          onClick={() => {
            if (showComment && comment.trim()) {
              void onComment();
            } else {
              setShowComment(true);
            }
          }}
        >
          COMMENT
        </button>
      </div>
    </div>
  );
}

function PlanFileRow({ file }: { file: PlanFile }) {
  if (!file.is_markdown) {
    return (
      <ul className="files">
        <li>{file.path}</li>
      </ul>
    );
  }
  // The daemon already ran goldmark over the markdown body
  // (plan_artifact.go renderBranchMarkdown — goldmark escapes raw HTML
  // by default, so the output is safe to inject). Empty `rendered`
  // means a read or render failure — show a muted placeholder so the
  // operator sees the file still exists.
  return (
    <details className="plan-artifact-doc">
      <summary>
        {file.path}
        {file.truncated && <span className="muted"> (truncated)</span>}
      </summary>
      {file.rendered ? (
        <div
          className="plan-artifact-rendered markdown-body"
          dangerouslySetInnerHTML={{ __html: file.rendered }}
        />
      ) : (
        <div className="plan-artifact-rendered markdown-body">
          <em className="muted">(could not render this file)</em>
        </div>
      )}
    </details>
  );
}

function renderMarkdown(text: string): string {
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

function toMessage(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

const emptyAwaiting: AwaitingApprovalTask[] = [];
