import { DecisionRow, useTeamStore } from '../store/team';

const PREVIEW_CHARS = 120;

// DecisionsList renders snapshot.decisions filtered to agent questions
// (severity=question). The full Decisions panel in the SSR template
// mixes approvals, questions, and blockers; in the SPA the approval
// rows are owned by ApprovalCard, so this component shows only the
// "agent asked a question" rows.
export function DecisionsList() {
  const decisions = useTeamStore((s) => s.snapshot?.decisions ?? emptyDecisions);
  const questions = decisions.filter((d) => isQuestion(d));
  return (
    <section className="decisions-section" aria-label="agent questions">
      <h3 className="panel-label">
        Open questions <span className="count">{questions.length}</span>
      </h3>
      {questions.length === 0 ? (
        <div className="decisions-empty">No open agent questions.</div>
      ) : (
        questions.map((d) => <QuestionRow key={`${d.task_id}-${d.timestamp}`} row={d} />)
      )}
    </section>
  );
}

function QuestionRow({ row }: { row: DecisionRow }) {
  const summary = row.summary || '';
  const preview =
    summary.length > PREVIEW_CHARS ? summary.slice(0, PREVIEW_CHARS).trimEnd() + '…' : summary;
  const stamp = formatStamp(row.timestamp);
  return (
    <div className={`decision-row decision-row-${row.type_class}`}>
      <div
        className={`decision-stripe ${row.type_class}`}
        style={{ background: row.stripe }}
        aria-hidden="true"
      />
      <div className="decision-body">
        <div className="decision-head">
          <span className={`decision-type ${row.type_class}`}>{row.type}</span>
          <span className="decision-title">
            {row.url ? <a href={row.url}>{row.title}</a> : row.title}
          </span>
          <a className="decision-id" href={row.url || '#'}>
            {row.task_id}
          </a>
          {row.age && <span className="decision-age">{row.age}</span>}
        </div>
        {preview && <div className="decision-summary">{preview}</div>}
        <div className="decision-meta">
          <span className="when">{stamp}</span>
          {row.url && (
            <a className="decision-view" href={row.url}>
              View task →
            </a>
          )}
        </div>
      </div>
    </div>
  );
}

function isQuestion(d: DecisionRow): boolean {
  // type_class is the CSS modifier the SSR template appends — "question"
  // for record_decision severity=question rows. Falls back to checking
  // Type because the SPA may render against older snapshots in tests.
  if (d.type_class === 'question') return true;
  return d.type.toLowerCase() === 'question';
}

function formatStamp(iso: string): string {
  if (!iso) return '';
  try {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    const pad = (n: number) => (n < 10 ? `0${n}` : `${n}`);
    return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  } catch {
    return iso;
  }
}

const emptyDecisions: DecisionRow[] = [];
