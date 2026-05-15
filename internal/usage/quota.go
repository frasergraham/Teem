package usage

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrUsageThrottle is the sentinel returned by gated entry points when
// the daily budget is at or above the configured threshold. Callers
// surface it to operators (and to the leader, via the spawn_agent MCP
// error result).
var ErrUsageThrottle = errors.New("usage: daily token budget exceeded")

// ThrottleEvent is the payload the Aggregator hands its onThrottle
// callback when the active/throttled state flips. The daemon turns it
// into a KindUsageThrottle audit event.
type ThrottleEvent struct {
	// State is "active" or "throttled".
	State  string
	Used   int64
	Cap    int64
	Reason string
}

// Aggregator is the daemon-global owner of the daily roll-up state +
// the throttle decision. Safe for concurrent use. It is the
// QuotaChecker the spawner consults before provisioning new workers.
//
// One Aggregator per daemon. The audit hook chain feeds it
// KindUsageEvent → Record; the spawner calls AvailableQuota; the
// `teem usage` CLI reads its Store directly off disk (separate
// process, so it must not depend on this struct).
type Aggregator struct {
	cfg   Config
	store *Store

	mu          sync.Mutex
	lastEmitted string // "active" | "throttled" | "" (none yet)
	onThrottle  func(ThrottleEvent)
}

// NewAggregator wires a Config + Store into a fresh Aggregator.
// onThrottle (nil-OK) is invoked on every active↔throttled transition,
// once per transition. Idempotency lives here, not in the callback.
func NewAggregator(cfg Config, store *Store, onThrottle func(ThrottleEvent)) *Aggregator {
	return &Aggregator{cfg: cfg, store: store, onThrottle: onThrottle}
}

// Record accumulates one subprocess's usage. Reset is applied first.
// Any pending throttle-state transition is emitted before returning.
func (a *Aggregator) Record(s UsageSummary) error {
	when := s.EndedAt
	if when.IsZero() {
		when = s.StartedAt
	}
	if when.IsZero() {
		when = time.Now().UTC()
	}
	if err := a.store.Record(a.cfg, s.Model, ModelTotals{
		Input:       s.InputTokens,
		Output:      s.OutputTokens,
		CacheCreate: s.CacheCreateTokens,
		CacheRead:   s.CacheReadTokens,
	}, when); err != nil {
		return err
	}
	a.maybeEmitTransition(when)
	return nil
}

// AvailableQuota is the throttle decision primitive. It applies any
// pending reset, sums the by-model state, and returns whether new
// subprocesses should be deferred.
//
//   - cap == 0  → throttle is disabled (operator hasn't opted in);
//     used is still reported.
//   - used  < cap * threshold → throttle = false, reason "".
//   - used >= cap * threshold → throttle = true, reason explains why.
func (a *Aggregator) AvailableQuota(now time.Time) (used, capLimit int64, throttle bool, reason string) {
	if _, err := a.store.MaybeReset(a.cfg, now); err != nil {
		// Reset failure is logged at the caller; treat as "no reset"
		// rather than failing the gate (silently disabling the throttle
		// on disk-IO trouble is the wrong default — surface it via
		// reason but don't bounce live workers).
		reason = fmt.Sprintf("reset check failed: %v", err)
	}
	used = a.store.TotalBillable()
	capLimit = a.cfg.DailyTokenBudget
	if capLimit <= 0 {
		return used, 0, false, ""
	}
	threshold := a.cfg.EffectiveThreshold()
	if threshold > 1 {
		threshold = 1
	}
	limit := int64(float64(capLimit) * threshold)
	if used >= limit {
		return used, capLimit, true, fmt.Sprintf("daily usage %d ≥ %.0f%% of cap %d", used, threshold*100, capLimit)
	}
	return used, capLimit, false, ""
}

// maybeEmitTransition reads the current quota state and fires the
// callback only when it differs from the last emitted state. Called
// after every Record so transitions land promptly.
func (a *Aggregator) maybeEmitTransition(now time.Time) {
	_, capLimit, throttle, reason := a.AvailableQuota(now)
	state := "active"
	if throttle {
		state = "throttled"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastEmitted == state {
		return
	}
	a.lastEmitted = state
	if a.onThrottle == nil {
		return
	}
	used := a.store.TotalBillable()
	a.onThrottle(ThrottleEvent{State: state, Used: used, Cap: capLimit, Reason: reason})
}

// Snapshot returns the current state file shape. Convenience wrapper
// so callers don't reach through Aggregator.store directly.
func (a *Aggregator) Snapshot() StateFile { return a.store.Snapshot() }

// Config returns the resolved config the Aggregator is gating on.
func (a *Aggregator) Config() Config { return a.cfg }
