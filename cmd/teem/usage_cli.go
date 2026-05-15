package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/frasergraham/teem/internal/usage"
)

// runUsageCmd implements `teem usage`: a read-only one-shot that prints
// today's per-model usage roll-up plus the throttle status. Reads
// ~/.teem/state/usage.json and ~/.teem/usage.yaml directly so it works
// whether or not the daemon is running.
//
//	teem usage                       # default paths
//	teem usage --config <path>       # override config
//	teem usage --state <path>        # override state
func runUsageCmd(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	cfgPath := fs.String("config", usage.DefaultConfigPath(), "usage.yaml path")
	statePath := fs.String("state", usage.DefaultStatePath(), "usage.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return printUsageReport(os.Stdout, *cfgPath, *statePath, time.Now())
}

// printUsageReport renders the human-readable status to w. Factored
// out for tests so they don't depend on stdout capture.
func printUsageReport(w io.Writer, cfgPath, statePath string, now time.Time) error {
	cfg, err := usage.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(w, "config: %v (using defaults)\n", err)
		cfg = usage.Config{}
	}
	store, err := usage.OpenStore(statePath)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	snap := store.Snapshot()

	if cfg.DailyTokenBudget <= 0 {
		fmt.Fprintln(w, "no daily budget set; throttle disabled")
		fmt.Fprintf(w, "  config: %s\n", cfgPath)
		fmt.Fprintf(w, "  state:  %s\n", statePath)
		fmt.Fprintln(w)
		printModelBreakdown(w, snap)
		return nil
	}

	total := int64(0)
	for _, t := range snap.ByModel {
		total += t.Input + t.Output + t.CacheCreate
	}
	pct := float64(total) / float64(cfg.DailyTokenBudget) * 100
	threshold := cfg.EffectiveThreshold()
	status := "active"
	if float64(total) >= float64(cfg.DailyTokenBudget)*threshold {
		status = "THROTTLED"
	}
	next, err := cfg.NextReset(now)
	if err != nil {
		return fmt.Errorf("next reset: %w", err)
	}
	fmt.Fprintf(w, "status:      %s\n", status)
	fmt.Fprintf(w, "today:       %d / %d tokens (%.1f%% of cap; throttle at %.0f%%)\n",
		total, cfg.DailyTokenBudget, pct, threshold*100)
	fmt.Fprintf(w, "next reset:  %s (in %s)\n", next.Format(time.RFC3339), trunc(time.Until(next)))
	if !snap.LastReset.IsZero() {
		fmt.Fprintf(w, "last reset:  %s\n", snap.LastReset.Format(time.RFC3339))
	}
	fmt.Fprintln(w)
	printModelBreakdown(w, snap)
	return nil
}

// printModelBreakdown writes a tabular per-model summary. Empty state
// prints a placeholder line.
func printModelBreakdown(w io.Writer, snap usage.StateFile) {
	if len(snap.ByModel) == 0 {
		fmt.Fprintln(w, "no usage recorded yet today")
		return
	}
	models := make([]string, 0, len(snap.ByModel))
	for m := range snap.ByModel {
		models = append(models, m)
	}
	sort.Strings(models)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tINPUT\tOUTPUT\tCACHE_CREATE\tCACHE_READ")
	for _, m := range models {
		t := snap.ByModel[m]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n", m, t.Input, t.Output, t.CacheCreate, t.CacheRead)
	}
	tw.Flush()
}

// trunc drops sub-minute precision so "in 23h59m0s" doesn't sprawl.
func trunc(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Truncate(time.Second)
	}
	return d.Truncate(time.Minute)
}
