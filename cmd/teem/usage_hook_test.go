package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/usage"
)

// TestUsageHook_Aggregates exercises the daemon-side audit hook end-
// to-end: synthesize a few KindUsageEvent rows, push them through
// makeUsageHook, and confirm the Aggregator's state.json reflects the
// sum.
func TestUsageHook_Aggregates(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "usage.json")
	store, err := usage.OpenStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	agg := usage.NewAggregator(usage.Config{ResetAnchor: "00:00 UTC"}, store, nil)
	d := &daemon{usageAgg: agg}
	hook := d.makeUsageHook()
	if hook == nil {
		t.Fatal("hook must be non-nil when aggregator is wired")
	}

	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	events := []audit.Event{
		makeUsageEvent("claude-opus-4-7", 100, 50, 25, 1000, now),
		makeUsageEvent("claude-opus-4-7", 200, 75, 0, 500, now.Add(time.Second)),
		makeUsageEvent("claude-sonnet-4-6", 50, 25, 10, 200, now.Add(2*time.Second)),
		// Non-usage event — must be ignored without panicking.
		{Kind: audit.KindJobComplete, AgentID: "worker-x", Timestamp: now},
	}
	hook(events)

	snap := agg.Snapshot()
	opus := snap.ByModel["claude-opus-4-7"]
	if opus.Input != 300 || opus.Output != 125 || opus.CacheCreate != 25 {
		t.Errorf("opus accumulator: %+v", opus)
	}
	sonnet := snap.ByModel["claude-sonnet-4-6"]
	if sonnet.Input != 50 || sonnet.Output != 25 {
		t.Errorf("sonnet accumulator: %+v", sonnet)
	}
}

// TestUsageHook_Disabled confirms that a daemon without a wired
// Aggregator returns a nil hook so combineHooks drops it cleanly.
func TestUsageHook_Disabled(t *testing.T) {
	d := &daemon{}
	if d.makeUsageHook() != nil {
		t.Errorf("nil aggregator should produce nil hook")
	}
}

// makeUsageEvent shapes a KindUsageEvent the way pulse / workers do:
// usage.AuditMeta packs int64s under the right keys, and we re-decode
// them through usageSummaryFromMeta as part of the round trip.
func makeUsageEvent(model string, in, out, cc, cr int64, when time.Time) audit.Event {
	s := usage.UsageSummary{
		Model:             model,
		InputTokens:       in,
		OutputTokens:      out,
		CacheCreateTokens: cc,
		CacheReadTokens:   cr,
		StartedAt:         when.Add(-5 * time.Second),
		EndedAt:           when,
	}
	return audit.Event{
		Timestamp: when,
		AgentID:   "worker-test",
		Kind:      audit.KindUsageEvent,
		Meta:      usage.AuditMeta(s, "worker-test", "job-1"),
	}
}
