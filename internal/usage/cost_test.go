package usage

import (
	"math"
	"testing"
	"time"
)

func fixturePricing() Pricing {
	return Pricing{
		Models: map[string]ModelPricing{
			"claude-opus-4-7": {
				InputPerMillion:       15,
				OutputPerMillion:      75,
				CacheReadPerMillion:   1.5,
				CacheCreatePerMillion: 18.75,
			},
			"claude-sonnet-4-6": {
				InputPerMillion:       3,
				OutputPerMillion:      15,
				CacheReadPerMillion:   0.3,
				CacheCreatePerMillion: 3.75,
			},
		},
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostFor_IncludesCacheRead(t *testing.T) {
	p := fixturePricing()
	ev := CostEvent{
		Model:             "claude-opus-4-7",
		InputTokens:       1_000_000,
		OutputTokens:      0,
		CacheCreateTokens: 0,
		CacheReadTokens:   1_000_000,
	}
	cost, ok := CostFor(ev, p)
	if !ok {
		t.Fatal("priced=false for known model")
	}
	// $15 for 1M input + $1.50 for 1M cache_read = $16.50. Cost
	// attribution INCLUDES cache_read (operators do pay for it), even
	// though the throttle path excludes it.
	if !approxEq(cost, 16.5) {
		t.Errorf("cost=%.4f want 16.50 — cache_read may have been excluded", cost)
	}
}

func TestCostFor_UnknownModel(t *testing.T) {
	_, ok := CostFor(CostEvent{Model: "claude-mystery", InputTokens: 1_000_000}, fixturePricing())
	if ok {
		t.Errorf("unknown model returned priced=true")
	}
}

func TestPerTaskCost_DeterministicSum(t *testing.T) {
	p := fixturePricing()
	now := time.Now()
	events := []CostEvent{
		{JobID: "j1", Model: "claude-opus-4-7", InputTokens: 1_000_000, OutputTokens: 500_000, Timestamp: now},
		{JobID: "j2", Model: "claude-sonnet-4-6", InputTokens: 2_000_000, OutputTokens: 1_000_000, Timestamp: now},
		{JobID: "jX", Model: "claude-opus-4-7", InputTokens: 99_000_000, OutputTokens: 99_000_000, Timestamp: now}, // not in evidence
	}
	cb, ok := PerTaskCost(events, []string{"j1", "j2"}, p)
	if !ok {
		t.Fatal("ok=false despite loaded pricing")
	}
	if len(cb.Jobs) != 2 {
		t.Fatalf("Jobs=%d want 2; got %+v", len(cb.Jobs), cb.Jobs)
	}
	// j1: 1M * 15 + 0.5M * 75 = 15 + 37.5 = 52.50
	// j2: 2M * 3 + 1M * 15    = 6 + 15  = 21.00
	// total = 73.50
	if !approxEq(cb.USD, 73.5) {
		t.Errorf("USD=%.4f want 73.50", cb.USD)
	}
	if cb.UnknownModels {
		t.Errorf("UnknownModels=true; all models priced")
	}
}

func TestPerTaskCost_HiddenWhenPricingAbsent(t *testing.T) {
	events := []CostEvent{{JobID: "j1", Model: "claude-opus-4-7", InputTokens: 1_000_000}}
	_, ok := PerTaskCost(events, []string{"j1"}, Pricing{})
	if ok {
		t.Errorf("ok=true with empty pricing; dashboard would render $0 instead of hiding")
	}
}

func TestPerTaskCost_UnknownModelFlagged(t *testing.T) {
	p := fixturePricing()
	events := []CostEvent{
		{JobID: "j1", Model: "claude-mystery", InputTokens: 1_000_000},
		{JobID: "j2", Model: "claude-opus-4-7", InputTokens: 1_000_000},
	}
	cb, ok := PerTaskCost(events, []string{"j1", "j2"}, p)
	if !ok {
		t.Fatal("ok=false")
	}
	if !cb.UnknownModels {
		t.Errorf("UnknownModels=false; expected true because j1 used unpriced model")
	}
	if !approxEq(cb.USD, 15) {
		t.Errorf("USD=%.4f want 15.00 (just j2)", cb.USD)
	}
}

// TestTodaysSpend_SeparatePath demonstrates the design point: a job
// linked to two tasks doubles in per-task sum, but TodaysSpend walks
// the raw audit stream so the daily total stays right. Without this
// separation, "today's spend" would inflate with every cross-linked
// task.
func TestTodaysSpend_SeparatePath(t *testing.T) {
	p := fixturePricing()
	since := time.Now().Add(-1 * time.Hour)
	now := time.Now()
	events := []CostEvent{
		{JobID: "shared-job", Model: "claude-opus-4-7", InputTokens: 1_000_000, Timestamp: now},
		{JobID: "other-job", Model: "claude-opus-4-7", InputTokens: 1_000_000, Timestamp: now},
	}
	// Two tasks both list shared-job in evidence — PerTaskCost over-attributes.
	taskA, _ := PerTaskCost(events, []string{"shared-job"}, p)
	taskB, _ := PerTaskCost(events, []string{"shared-job"}, p)
	naiveSum := taskA.USD + taskB.USD
	if !approxEq(naiveSum, 30) {
		t.Errorf("naive per-task sum=%.4f want 30 (over-attribution sanity)", naiveSum)
	}
	// TodaysSpend uses the raw event stream — each job counted once.
	total, ok := TodaysSpend(events, since, p)
	if !ok {
		t.Fatal("ok=false")
	}
	if !approxEq(total, 30) {
		t.Errorf("TodaysSpend=%.4f want 30 (1M opus input × 2 distinct jobs)", total)
	}
}

func TestTodaysSpend_FiltersBeforeSince(t *testing.T) {
	p := fixturePricing()
	since := time.Now().Add(-1 * time.Hour)
	events := []CostEvent{
		{JobID: "old", Model: "claude-opus-4-7", InputTokens: 1_000_000, Timestamp: time.Now().Add(-2 * time.Hour)},
		{JobID: "new", Model: "claude-opus-4-7", InputTokens: 1_000_000, Timestamp: time.Now()},
	}
	total, ok := TodaysSpend(events, since, p)
	if !ok {
		t.Fatal("ok=false")
	}
	if !approxEq(total, 15) {
		t.Errorf("TodaysSpend=%.4f want 15 (only the post-since event)", total)
	}
}

func TestTodaysSpend_HiddenWhenPricingAbsent(t *testing.T) {
	_, ok := TodaysSpend([]CostEvent{{Model: "claude-opus-4-7"}}, time.Time{}, Pricing{})
	if ok {
		t.Errorf("ok=true with empty pricing")
	}
}
