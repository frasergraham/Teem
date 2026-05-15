package usage

import "time"

// CostEvent is a flat record describing one KindUsageEvent worth of
// subprocess usage. Callers (the dashboard) project audit.Event.Meta
// into this shape so internal/usage doesn't have to import
// internal/audit — keeping the cycle-free boundary the rest of the
// package observes.
type CostEvent struct {
	JobID             string
	Model             string
	InputTokens       int64
	OutputTokens      int64
	CacheCreateTokens int64
	CacheReadTokens   int64
	Timestamp         time.Time
}

// JobCost is one row in a CostBreakdown — the per-event contribution
// surfaced by the dashboard's drill-in <details>. Priced is false when
// the event's model wasn't in pricing.yaml; the row is still kept so
// the operator can see which model isn't yet priced.
type JobCost struct {
	JobID             string
	Model             string
	InputTokens       int64
	OutputTokens      int64
	CacheCreateTokens int64
	CacheReadTokens   int64
	USD               float64
	Priced            bool
}

// CostBreakdown is the per-task rollup. USD is the sum of every
// contributing event; Jobs is the per-event detail; UnknownModels is
// true when at least one contributing event ran on a model that wasn't
// in pricing.yaml (the dashboard renders a small "?" so the operator
// knows the number is a lower bound).
type CostBreakdown struct {
	USD           float64
	Jobs          []JobCost
	UnknownModels bool
}

// CostFor computes the dollar cost of one event under pricing p. The
// cache_read fraction IS included here. That diverges from
// Store.TotalBillable (which excludes cache_read for the
// throttle/rate-limit budget) because cost attribution is a different
// concern — the operator does pay for cache reads, just at the
// discounted rate, and excluding them would understate spend. Returns
// (USD, true) when the model is in p.Models; (0, false) otherwise.
func CostFor(ev CostEvent, p Pricing) (float64, bool) {
	mp, ok := p.Models[ev.Model]
	if !ok {
		return 0, false
	}
	const million = 1_000_000.0
	cost := float64(ev.InputTokens)/million*mp.InputPerMillion +
		float64(ev.OutputTokens)/million*mp.OutputPerMillion +
		float64(ev.CacheCreateTokens)/million*mp.CacheCreatePerMillion +
		float64(ev.CacheReadTokens)/million*mp.CacheReadPerMillion
	return cost, true
}

// PerTaskCost sums the cost of every event whose JobID appears in
// evidence. Returns (CostBreakdown{}, false) when pricing isn't loaded
// — the caller hides the cost cell rather than rendering $0.
//
// v1 caveat: a job linked to multiple tasks is counted under every
// linked task. That's why TodaysSpend does its own scan rather than
// folding PerTaskCost across all tasks — the daily total stays correct
// even when per-task numbers over-attribute. The dashboard surfaces
// the caveat in a tooltip on the cost cell.
func PerTaskCost(events []CostEvent, evidence []string, p Pricing) (CostBreakdown, bool) {
	if !p.HasPricing() {
		return CostBreakdown{}, false
	}
	if len(evidence) == 0 {
		return CostBreakdown{}, true
	}
	want := make(map[string]bool, len(evidence))
	for _, id := range evidence {
		if id != "" {
			want[id] = true
		}
	}
	var out CostBreakdown
	for _, e := range events {
		if e.JobID == "" || !want[e.JobID] {
			continue
		}
		cost, priced := CostFor(e, p)
		out.Jobs = append(out.Jobs, JobCost{
			JobID:             e.JobID,
			Model:             e.Model,
			InputTokens:       e.InputTokens,
			OutputTokens:      e.OutputTokens,
			CacheCreateTokens: e.CacheCreateTokens,
			CacheReadTokens:   e.CacheReadTokens,
			USD:               cost,
			Priced:            priced,
		})
		out.USD += cost
		if !priced {
			out.UnknownModels = true
		}
	}
	return out, true
}

// TodaysSpend sums the cost of every event at or after `since`. Uses
// the raw audit stream — not task evidence — so the daily total stays
// correct under PerTaskCost's over-attribution caveat. Returns
// (0, false) when pricing isn't loaded.
func TodaysSpend(events []CostEvent, since time.Time, p Pricing) (float64, bool) {
	if !p.HasPricing() {
		return 0, false
	}
	var total float64
	for _, e := range events {
		if e.Timestamp.Before(since) {
			continue
		}
		cost, _ := CostFor(e, p)
		total += cost
	}
	return total, true
}
