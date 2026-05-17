import { useTeamStore } from '../store/team';

// UsageCard renders snapshot.usage — running token totals only. No
// daily quota, no throttle, no countdown; just tokens in / out / cache
// activity, with the per-model breakdown available as a <details>.
export function UsageCard() {
  const usage = useTeamStore((s) => s.snapshot?.usage);
  if (!usage) return null;
  return (
    <section className="usage-panel" aria-label="token usage">
      <h3 className="panel-label">Usage</h3>
      <div className="usage-stats">
        <UsageStat label="tokens in" value={usage.input} />
        <UsageStat label="tokens out" value={usage.output} />
        <UsageStat label="cache hits" value={usage.cache_read} />
        <UsageStat label="cache writes" value={usage.cache_create} />
      </div>
      {usage.last_reset_abs && (
        <div className="usage-meta">
          <span>
            <span className="label">since</span>
            <span className="value">{usage.last_reset_abs}</span>
          </span>
        </div>
      )}
      {usage.per_model && usage.per_model.length > 0 && (
        <details className="usage-models">
          <summary>per-model breakdown ({usage.per_model.length})</summary>
          <table className="usage-models-table">
            <thead>
              <tr>
                <th>model</th>
                <th>input</th>
                <th>output</th>
                <th>cache write</th>
                <th>cache hit</th>
              </tr>
            </thead>
            <tbody>
              {usage.per_model.map((row) => (
                <tr key={row.model}>
                  <td>{row.model}</td>
                  <td>{row.input.toLocaleString()}</td>
                  <td>{row.output.toLocaleString()}</td>
                  <td>{row.cache_create.toLocaleString()}</td>
                  <td>{row.cache_read.toLocaleString()}</td>
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
