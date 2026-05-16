import { useTeamStore } from '../store/team';

// UsageCard renders snapshot.usage — the daily token-budget card.
// Visual parity with the SSR usage-panel: small headline at top
// (today's spend lives in HeroPanel, so here we show used/cap +
// percent), a colour-coded progress bar, and a collapsible per-model
// breakdown table. Hidden entirely when the daemon has no
// Aggregator wired (usage === null) — the SSR template suppresses the
// panel in that case for the same reason.
export function UsageCard() {
  const usage = useTeamStore((s) => s.snapshot?.usage);
  if (!usage) return null;
  return (
    <section className="usage-panel" aria-label="daily token usage">
      <h3 className="panel-label">
        Usage
        {usage.configured && (
          <span className="pct"> · {usage.percent_used.toFixed(1)}%</span>
        )}
        {usage.throttle && (
          <span className="throttling-badge" role="status">
            THROTTLING
          </span>
        )}
      </h3>

      {usage.configured ? (
        <>
          <div className="usage-headline-row">
            <span className="usage-numbers">
              <span className="n">{usage.used.toLocaleString()}</span> /{' '}
              {usage.cap.toLocaleString()} tokens
            </span>
          </div>
          <div
            className="usage-bar"
            role="progressbar"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={Number(usage.percent_used.toFixed(1))}
          >
            <div
              className={`usage-bar-fill ${usage.bar_colour}`}
              style={{ width: `${Math.min(100, usage.percent_used).toFixed(2)}%` }}
            />
          </div>
          <div className="usage-meta">
            {usage.next_reset_in && (
              <span>
                <span className="label">next reset</span>
                <span className="value" title={usage.next_reset_abs}>
                  in {usage.next_reset_in}
                </span>
              </span>
            )}
            {usage.last_reset_abs && (
              <span>
                <span className="label">last reset</span>
                <span className="value">{usage.last_reset_abs}</span>
              </span>
            )}
          </div>
        </>
      ) : (
        <div className="usage-hint">
          Daily token budget not configured. Add a cap to <code>~/.teem/usage.yaml</code>:
          <br />
          <code>usage:&nbsp;&nbsp;daily_token_budget:&nbsp;5000000</code>
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
                <th>cache create</th>
                <th>cache read</th>
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
