import { useCallback, useEffect, useRef, useState } from 'react';
import { PulseSnapshot, StateSnapshot, useTeamStore } from '../store/team';
import { configPulse, startPulse, stopPulse } from '../api/control';

// PulseControls drives the bridge-console pulse-management panel:
// start/stop the loop, change the interval, edit the wake-prompt
// override. Optimistic on start/stop — the lamp flips immediately and
// the wsbus stream (which the daemon emits a snapshot_invalidate on
// every pulse state change) confirms within one round-trip.
//
// The "next tick" countdown updates client-side via a 1s setInterval;
// the source-of-truth is the snapshot's pulse.last_tick string ("X ago")
// plus the interval. We only project forward — when the real tick fires
// the daemon pushes a new snapshot and we re-derive from it.

export function PulseControls() {
  const teamID = useTeamStore((s) => s.teamID);
  const pulse = useTeamStore((s) => s.snapshot?.pulse);
  if (!teamID || !pulse) return null;
  return <PulseControlsInner teamID={teamID} pulse={pulse} />;
}

function PulseControlsInner({ teamID, pulse }: { teamID: string; pulse: PulseSnapshot }) {
  const patchSnapshot = useTeamStore((s) => s.patchSnapshot);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Form state seeded from the snapshot and kept in sync when the
  // snapshot's interval changes (e.g. another operator edited it).
  const [intervalValue, setIntervalValue] = useState<number>(pulse.interval_value || 5);
  const [intervalUnit, setIntervalUnit] = useState<string>(pulse.interval_unit || 'm');
  const [wakePromptDraft, setWakePromptDraft] = useState<string>(
    pulse.use_default_wake_prompt ? '' : pulse.wake_prompt ?? '',
  );

  useEffect(() => {
    setIntervalValue(pulse.interval_value || 5);
    setIntervalUnit(pulse.interval_unit || 'm');
    setWakePromptDraft(pulse.use_default_wake_prompt ? '' : pulse.wake_prompt ?? '');
  }, [pulse.interval_value, pulse.interval_unit, pulse.use_default_wake_prompt, pulse.wake_prompt]);

  // 1s tick to refresh the countdown derived from the snapshot. We
  // store the "now" timestamp in component state so the render
  // function stays pure of Date.now() during reconciliation.
  const [nowMs, setNowMs] = useState<number>(() => Date.now());
  useEffect(() => {
    if (!pulse.running || pulse.paused) return;
    const t = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(t);
  }, [pulse.running, pulse.paused]);

  // snapshotLoadedMs is the wall-clock time at which we received the
  // current pulse.last_tick string. The string itself is a coarse "X
  // ago" phrase that only changes when a new snapshot arrives, so we
  // anchor here and add (nowMs - snapshotLoadedMs) drift in
  // computeCountdown to actually decrement each second.
  const snapshotLoadedMsRef = useRef<number>(Date.now());
  useEffect(() => {
    snapshotLoadedMsRef.current = Date.now();
  }, [pulse.last_tick]);

  const optimisticPatch = useCallback(
    (next: Partial<PulseSnapshot>) => {
      const snap = useTeamStore.getState().snapshot;
      if (!snap || !snap.pulse) return () => {};
      const prev = snap.pulse;
      patchSnapshot({ pulse: { ...prev, ...next } } as Partial<StateSnapshot>);
      return () => {
        if (!useTeamStore.getState().snapshot) return;
        useTeamStore
          .getState()
          .patchSnapshot({ pulse: prev } as Partial<StateSnapshot>);
      };
    },
    [patchSnapshot],
  );

  const onStart = useCallback(async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    const rollback = optimisticPatch({ running: true, paused: false });
    try {
      await startPulse(teamID);
    } catch (e) {
      rollback();
      setError(toMessage(e));
    } finally {
      setBusy(false);
    }
  }, [busy, optimisticPatch, teamID]);

  const onStop = useCallback(async () => {
    if (busy) return;
    setBusy(true);
    setError(null);
    const rollback = optimisticPatch({ running: false, paused: false });
    try {
      await stopPulse(teamID);
    } catch (e) {
      rollback();
      setError(toMessage(e));
    } finally {
      setBusy(false);
    }
  }, [busy, optimisticPatch, teamID]);

  const onSaveConfig = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      if (busy) return;
      const interval = `${intervalValue}${intervalUnit}`;
      // Empty draft means "clear the override and revert to default";
      // a non-empty draft is the new override. configPulse passes
      // through both cases via the wake_prompt field.
      const wakePrompt = wakePromptDraft.trim() === '' ? '' : wakePromptDraft;
      setBusy(true);
      setError(null);
      try {
        await configPulse(teamID, { interval, wakePrompt });
      } catch (err) {
        setError(toMessage(err));
      } finally {
        setBusy(false);
      }
    },
    [busy, intervalValue, intervalUnit, wakePromptDraft, teamID],
  );

  const lampLabel = pulse.running ? 'ON' : 'OFF';
  const stateText = !pulse.running ? 'off' : pulse.paused ? 'paused' : 'on';
  const countdown = computeCountdown(pulse, nowMs, snapshotLoadedMsRef.current);

  return (
    <section className="pulse-panel" aria-label="pulse management">
      <h3 className="panel-label">
        Pulse <span className={`state ${stateText}`}>{stateText}</span>
      </h3>
      <div className="pulse-grid">
        <div className="pulse-toggle-cell">
          <button
            type="button"
            className={`pulse-lamp${pulse.running ? ' on' : ''}${pulse.paused ? ' paused' : ''}`}
            onClick={() => void (pulse.running ? onStop() : onStart())}
            disabled={busy}
            aria-label={pulse.running ? 'stop pulse' : 'start pulse'}
          >
            <span className="lamp-orb" />
            <span className="lamp-label">{lampLabel}</span>
          </button>
          <div className="pulse-meta">
            <div>last tick: {pulse.last_tick}</div>
            <div>
              {pulse.tick_count} tick{pulse.tick_count === 1 ? '' : 's'} · interval{' '}
              {pulse.interval}
            </div>
            {pulse.running && !pulse.paused && countdown && (
              <div className="pulse-countdown">next tick: {countdown}</div>
            )}
          </div>
        </div>

        <form className="pulse-config-form" onSubmit={onSaveConfig}>
          <div className="pulse-row interval-row">
            <label htmlFor="spa-pulse-interval-value">Interval</label>
            <input
              type="number"
              id="spa-pulse-interval-value"
              min={1}
              max={9999}
              required
              value={intervalValue}
              onChange={(e) => setIntervalValue(Number(e.target.value) || 1)}
            />
            <select
              aria-label="interval unit"
              value={intervalUnit}
              onChange={(e) => setIntervalUnit(e.target.value)}
            >
              <option value="s">sec</option>
              <option value="m">min</option>
              <option value="h">hr</option>
            </select>
          </div>
          <div className="pulse-row prompt-row">
            <label htmlFor="spa-pulse-wake-prompt">
              Wake prompt{' '}
              <span className="hint">
                {pulse.use_default_wake_prompt
                  ? 'using default — leave blank to keep, or override below'
                  : 'custom override active — clear to revert to default'}
              </span>
            </label>
            <textarea
              id="spa-pulse-wake-prompt"
              rows={6}
              placeholder={pulse.default_wake_prompt}
              value={wakePromptDraft}
              onChange={(e) => setWakePromptDraft(e.target.value)}
            />
          </div>
          {error && (
            <div className="pulse-error" role="alert">
              {error}
            </div>
          )}
          <div className="pulse-row actions-row">
            <button type="submit" className="pulse-save" disabled={busy}>
              Save changes
            </button>
          </div>
        </form>
      </div>
    </section>
  );
}

// computeCountdown projects forward from the snapshot's last_tick. The
// daemon ships last_tick as a relative phrase ("(never)", "12s ago",
// "3m ago", "1h 4m ago"); we parse the duration back to ms, add the
// interval (also parsed from pulse.interval like "5m0s"), then subtract
// elapsed wall time. snapshotLoadedMs is the wall-clock at which the
// current last_tick string arrived, so (nowMs - snapshotLoadedMs)
// is the drift to add on top of the parsed "X ago" value. Returns ""
// when last_tick is "(never)" or we can't parse — the SSR template
// doesn't show a countdown either, so degraded output is fine.
function computeCountdown(
  pulse: PulseSnapshot,
  nowMs: number,
  snapshotLoadedMs: number,
): string {
  if (!pulse.last_tick || pulse.last_tick.startsWith('(')) return '';
  const intervalMs = parseDurationMs(pulse.interval);
  const sinceLastMs = parseAgoMs(pulse.last_tick);
  if (intervalMs <= 0 || sinceLastMs < 0) return '';
  const driftMs = Math.max(0, nowMs - snapshotLoadedMs);
  const remaining = intervalMs - sinceLastMs - driftMs;
  if (remaining <= 0) return 'imminent';
  return formatDurationMs(remaining);
}

// parseDurationMs accepts Go-formatted durations: "5m0s", "1h30m",
// "45s". Returns 0 on parse failure.
function parseDurationMs(s: string): number {
  if (!s) return 0;
  const re = /(\d+(?:\.\d+)?)(h|m|s|ms)/g;
  let total = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    const n = Number(m[1]);
    if (!Number.isFinite(n)) continue;
    switch (m[2]) {
      case 'h':
        total += n * 3600_000;
        break;
      case 'm':
        total += n * 60_000;
        break;
      case 's':
        total += n * 1000;
        break;
      case 'ms':
        total += n;
        break;
    }
  }
  return total;
}

// parseAgoMs reads phrases like "12s ago", "3m ago", "1h 4m ago", "now".
// Returns 0 for "now" and -1 on parse failure.
function parseAgoMs(s: string): number {
  const trimmed = s.trim();
  if (trimmed === 'now' || trimmed === '0s ago' || trimmed === '') return 0;
  if (!trimmed.endsWith(' ago')) return -1;
  const body = trimmed.slice(0, -' ago'.length);
  const re = /(\d+)(h|m|s)/g;
  let total = 0;
  let m: RegExpExecArray | null;
  let matched = false;
  while ((m = re.exec(body)) !== null) {
    matched = true;
    const n = Number(m[1]);
    switch (m[2]) {
      case 'h':
        total += n * 3600_000;
        break;
      case 'm':
        total += n * 60_000;
        break;
      case 's':
        total += n * 1000;
        break;
    }
  }
  return matched ? total : -1;
}

function formatDurationMs(ms: number): string {
  if (ms < 1000) return '<1s';
  const totalSec = Math.round(ms / 1000);
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

function toMessage(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}
