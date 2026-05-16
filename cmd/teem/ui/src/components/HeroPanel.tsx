import { useTeamStore } from '../store/team';

// HeroPanel renders the "page-header" summary at the top of the team
// detail view: status headline + breathing lamp, big-number stats
// (active agents / open tasks / today's spend), agent chips per
// archetype, and the stacked stage-bar for today's pipeline. Subscribes
// to a narrow store slice (snapshot.hero + a handful of top-level
// fields) so events that don't touch the hero — events ring, ping
// envelopes — don't re-render this panel.
export function HeroPanel() {
  const hero = useTeamStore((s) => s.snapshot?.hero);
  const statusHeadline = useTeamStore((s) => s.snapshot?.status_headline ?? '');
  const hasLeaderStatus = useTeamStore((s) => Boolean(s.snapshot?.leader_status));
  const leaderUpdatedAgo = useTeamStore((s) => s.snapshot?.leader_status?.updated_ago ?? '');
  const hasPricing = useTeamStore((s) => Boolean(s.snapshot?.has_pricing));
  const pricingStale = useTeamStore((s) => Boolean(s.snapshot?.pricing_stale));
  const spendDisplay = useTeamStore((s) => s.snapshot?.hero_spend_display ?? '');

  if (!hero) return null;

  return (
    <section className="hero status-panel" aria-label="team summary">
      <div className="status-headline-row">
        <span className="status-lamp" aria-hidden="true" />
        <p className={`status-headline${!hasLeaderStatus ? ' empty' : ''}`}>
          {statusHeadline}
          {hasLeaderStatus && leaderUpdatedAgo && (
            <span className="when">{leaderUpdatedAgo}</span>
          )}
        </p>
      </div>

      <div className="hero-numbers">
        <div className="stat big">
          <span className="n">{hero.active_agents_total}</span>
          <span className="lbl">
            active agent{hero.active_agents_total === 1 ? '' : 's'}
          </span>
        </div>
        <div className="stat secondary">
          <span className="n">{hero.open_tasks_total}</span>
          <span className="lbl">
            open task{hero.open_tasks_total === 1 ? '' : 's'}
          </span>
        </div>
        {hasPricing && (
          <div
            className="stat secondary hero-spend"
            title="Today's token spend, summed from the audit stream."
          >
            <span className="n">{spendDisplay}</span>
            <span className="lbl">
              today's spend{pricingStale && <span className="muted"> (pricing stale)</span>}
            </span>
          </div>
        )}
      </div>

      <div className="hero-chips" aria-label="agents by archetype">
        {hero.agent_chips.map((c) => (
          <span key={c.role} className={`chip${c.count === 0 ? ' zero' : ''}`}>
            <span className="role">{c.role}</span>
            <span className="count">· {c.count}</span>
          </span>
        ))}
      </div>

      <div className="hero-bar-label">Today's pipeline</div>
      {hero.has_stage_activity ? (
        <div className="stage-bar" role="img" aria-label="task stage breakdown for today">
          {hero.stage_bar.map((seg) => (
            <div
              key={seg.stage}
              className="segment"
              style={{ width: `${seg.width_pct.toFixed(2)}%`, background: seg.color_hex }}
              title={`${seg.stage} (${seg.count}): ${seg.task_id_list ?? ''}`}
            >
              <span className="count">{seg.count}</span>
              <span className="stage-name">{seg.stage}</span>
            </div>
          ))}
        </div>
      ) : (
        <div className="stage-bar-empty">no stage activity today</div>
      )}
    </section>
  );
}
