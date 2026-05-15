package usage

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newAggregator(t *testing.T, cfg Config, onT func(ThrottleEvent)) *Aggregator {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewAggregator(cfg, store, onT)
}

func TestAvailableQuota_BudgetZeroNeverThrottles(t *testing.T) {
	a := newAggregator(t, Config{}, nil)
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 1_000_000_000, EndedAt: time.Now()})
	_, cap, throttle, _ := a.AvailableQuota(time.Now())
	if cap != 0 || throttle {
		t.Errorf("cap=%d throttle=%v — zero budget must disable throttle", cap, throttle)
	}
}

func TestAvailableQuota_BelowThreshold(t *testing.T) {
	a := newAggregator(t, Config{DailyTokenBudget: 1000, ThrottleThreshold: 0.8}, nil)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 100, EndedAt: now})
	used, capLimit, throttle, _ := a.AvailableQuota(now)
	if used != 100 || capLimit != 1000 || throttle {
		t.Errorf("got used=%d cap=%d throttle=%v", used, capLimit, throttle)
	}
}

func TestAvailableQuota_AtThresholdActivates(t *testing.T) {
	a := newAggregator(t, Config{DailyTokenBudget: 1000, ThrottleThreshold: 0.8}, nil)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 800, EndedAt: now})
	_, _, throttle, reason := a.AvailableQuota(now)
	if !throttle {
		t.Errorf("800/1000 @ 0.8 threshold should throttle")
	}
	if reason == "" {
		t.Errorf("reason should be populated when throttling")
	}
}

func TestAvailableQuota_CacheReadDoesNotCount(t *testing.T) {
	a := newAggregator(t, Config{DailyTokenBudget: 1000, ThrottleThreshold: 0.8}, nil)
	now := time.Now()
	// Cache reads alone shouldn't trip the gate.
	_ = a.Record(UsageSummary{Model: "x", CacheReadTokens: 5000, EndedAt: now})
	_, _, throttle, _ := a.AvailableQuota(now)
	if throttle {
		t.Errorf("cache_read alone must not throttle")
	}
}

func TestAvailableQuota_ResetClearsState(t *testing.T) {
	cfg := Config{DailyTokenBudget: 1000, ThrottleThreshold: 0.5, ResetAnchor: "00:00 UTC"}
	a := newAggregator(t, cfg, nil)
	day1 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 800, EndedAt: day1})
	if _, _, throttle, _ := a.AvailableQuota(day1); !throttle {
		t.Fatalf("expected throttle on day1")
	}
	day2 := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	used, _, throttle, _ := a.AvailableQuota(day2)
	if throttle || used != 0 {
		t.Errorf("after reset: used=%d throttle=%v", used, throttle)
	}
}

func TestTransitionEmittedOncePerFlip(t *testing.T) {
	var mu sync.Mutex
	var events []ThrottleEvent
	cb := func(e ThrottleEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	a := newAggregator(t, Config{DailyTokenBudget: 100, ThrottleThreshold: 0.5}, cb)
	now := time.Now()
	// Three records below threshold — no transition; first crosses
	// threshold; the next stays throttled so no further emit.
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 10, EndedAt: now})
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 10, EndedAt: now})
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 50, EndedAt: now}) // 70 ≥ 50; throttle
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 10, EndedAt: now}) // still throttled
	mu.Lock()
	defer mu.Unlock()
	// We expect exactly one "throttled" emit. The first below-threshold
	// record may also emit a single "active" baseline — that's a single
	// transition too. So total emits ∈ {1, 2}, and the last one must be
	// "throttled".
	if len(events) == 0 {
		t.Fatalf("no transition events emitted")
	}
	throttledEmits := 0
	for _, e := range events {
		if e.State == "throttled" {
			throttledEmits++
		}
	}
	if throttledEmits != 1 {
		t.Errorf("got %d throttled emits; want exactly 1 (events: %+v)", throttledEmits, events)
	}
}

func TestTransitionBothDirections(t *testing.T) {
	cfg := Config{DailyTokenBudget: 100, ThrottleThreshold: 0.5, ResetAnchor: "00:00 UTC"}
	var mu sync.Mutex
	var states []string
	cb := func(e ThrottleEvent) {
		mu.Lock()
		states = append(states, e.State)
		mu.Unlock()
	}
	a := newAggregator(t, cfg, cb)
	day1 := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 80, EndedAt: day1}) // throttled
	day2 := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)                   // reset
	_ = a.Record(UsageSummary{Model: "x", InputTokens: 5, EndedAt: day2})  // back to active
	mu.Lock()
	defer mu.Unlock()
	// Must observe both states across the two days.
	sawThrottled, sawActive := false, false
	for _, s := range states {
		if s == "throttled" {
			sawThrottled = true
		}
		if s == "active" {
			sawActive = true
		}
	}
	if !sawThrottled || !sawActive {
		t.Errorf("transitions: %v (need both throttled and active)", states)
	}
}
