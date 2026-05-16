package main

import (
	"fmt"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/usage"
)

// buildCostEvents projects the last `since` cutoff of audit events into
// the flat CostEvent shape internal/usage operates on. Only
// KindUsageEvent rows are kept; numeric fields are dug out of the Meta
// bag, where JSON round-trips them as float64.
//
// Returns nil when no sink is configured (a freshly-spun-up daemon
// that hasn't seen any usage events) so the caller's downstream
// PerTaskCost / TodaysSpend safely return zero values.
func buildCostEvents(sink audit.Sink, since time.Time) []usage.CostEvent {
	if sink == nil {
		return nil
	}
	events, err := sink.Query("", since, 50000)
	if err != nil {
		return nil
	}
	out := make([]usage.CostEvent, 0, len(events))
	for _, e := range events {
		if e.Kind != audit.KindUsageEvent {
			continue
		}
		out = append(out, usage.CostEvent{
			JobID:             metaString(e.Meta, "job_id"),
			Model:             metaString(e.Meta, "model"),
			InputTokens:       metaInt64(e.Meta, "input_tokens"),
			OutputTokens:      metaInt64(e.Meta, "output_tokens"),
			CacheCreateTokens: metaInt64(e.Meta, "cache_create_tokens"),
			CacheReadTokens:   metaInt64(e.Meta, "cache_read_tokens"),
			Timestamp:         e.Timestamp,
		})
	}
	return out
}

// formatUSD renders a dollar amount with two decimals. Negative values
// shouldn't happen but are passed through so a bug doesn't silently
// vanish the cell. Sub-cent amounts round to "$0.00".
func formatUSD(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
