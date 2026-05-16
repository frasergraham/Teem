package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/usage"
)

// newUsageAgg constructs a real usage.Aggregator backed by a fresh
// temp-dir state file. Tests that need state pre-populated can call
// Record on the returned Aggregator before invoking the handler.
func newUsageAgg(t *testing.T, cfg usage.Config) *usage.Aggregator {
	t.Helper()
	store, err := usage.OpenStore(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatalf("usage store: %v", err)
	}
	return usage.NewAggregator(cfg, store, nil)
}

func TestTeamPage_UsageCard_RendersWhenConfigured(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.usageAgg = newUsageAgg(t, usage.Config{
		DailyTokenBudget: 1_000_000,
		ResetAnchor:      "00:00 UTC",
	})
	// 30% usage — green bar.
	if err := d.usageAgg.Record(usage.UsageSummary{
		Model: "claude-opus-4-7", InputTokens: 200_000, OutputTokens: 100_000,
		EndedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="usage-panel"`,
		`<div class="usage-bar"`,
		`<div class="usage-bar-fill green"`,
		"300000",  // used
		"1000000", // cap
		"30.0%",   // percent
		"next reset",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in usage card; body excerpt around 'usage-panel':\n%s", want, excerptAround(body, "usage-panel", 600))
		}
	}
	// THROTTLING badge must NOT render at 30%. The literal label only
	// appears inside the rendered <span>, not the CSS rule.
	if strings.Contains(body, ">THROTTLING<") {
		t.Errorf("throttling badge rendered at 30%%: %s", excerptAround(body, "THROTTLING", 200))
	}
}

func TestTeamPage_UsageCard_RendersHintWhenUnconfigured(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	// daily_token_budget = 0 → hint, not bar.
	d.usageAgg = newUsageAgg(t, usage.Config{ResetAnchor: "00:00 UTC"})

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "usage-panel") {
		t.Fatalf("usage panel missing entirely")
	}
	if !strings.Contains(body, `class="usage-hint"`) {
		t.Errorf("hint missing when unconfigured: %s", excerptAround(body, "usage-panel", 800))
	}
	if !strings.Contains(body, "~/.teem/usage.yaml") {
		t.Errorf("config path missing from hint")
	}
	// .usage-bar-fill appears in the <style> block as a CSS rule; only the
	// instantiated <div class="usage-bar-fill …"> means a real bar is drawn.
	if strings.Contains(body, `<div class="usage-bar-fill`) {
		t.Errorf("progress bar should NOT render when unconfigured")
	}
}

func TestTeamPage_UsageCard_ThrottlingBadge(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.usageAgg = newUsageAgg(t, usage.Config{
		DailyTokenBudget:  1_000_000,
		ThrottleThreshold: 0.8,
		ResetAnchor:       "00:00 UTC",
	})
	// 90% usage — above the 80% threshold.
	if err := d.usageAgg.Record(usage.UsageSummary{
		Model: "claude-opus-4-7", InputTokens: 500_000, OutputTokens: 400_000,
		EndedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, `class="throttling-badge"`) {
		t.Errorf("throttling badge missing at 90%%: %s", excerptAround(body, "usage-panel", 600))
	}
	if !strings.Contains(body, ">THROTTLING<") {
		t.Errorf("throttling badge label missing")
	}
	if !strings.Contains(body, `<div class="usage-bar-fill red"`) {
		t.Errorf("bar should be red above 80%%")
	}
}

func TestTeamPage_UsageCard_PerModelBreakdown(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.usageAgg = newUsageAgg(t, usage.Config{
		DailyTokenBudget: 10_000_000,
		ResetAnchor:      "00:00 UTC",
	})
	now := time.Now()
	if err := d.usageAgg.Record(usage.UsageSummary{
		Model: "claude-opus-4-7", InputTokens: 100, OutputTokens: 50,
		CacheCreateTokens: 25, CacheReadTokens: 1000, EndedAt: now,
	}); err != nil {
		t.Fatalf("record opus: %v", err)
	}
	if err := d.usageAgg.Record(usage.UsageSummary{
		Model: "claude-sonnet-4-6", InputTokens: 200, OutputTokens: 75,
		CacheCreateTokens: 0, CacheReadTokens: 500, EndedAt: now,
	}); err != nil {
		t.Fatalf("record sonnet: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		`id="details-usage-models"`,
		"per-model breakdown",
		"claude-opus-4-7",
		"claude-sonnet-4-6",
		"usage-models-table",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in per-model breakdown", want)
		}
	}
}

func TestTeamPage_UsageCard_BarColours(t *testing.T) {
	cases := []struct {
		pct     int
		colour  string
		barTxt  string
		noThrot bool
	}{
		{pct: 30, colour: "green", barTxt: `<div class="usage-bar-fill green"`, noThrot: true},
		{pct: 65, colour: "amber", barTxt: `<div class="usage-bar-fill amber"`, noThrot: true},
		{pct: 85, colour: "red", barTxt: `<div class="usage-bar-fill red"`, noThrot: false},
	}
	for _, c := range cases {
		t.Run(c.colour, func(t *testing.T) {
			d := &daemon{teams: map[string]*registeredTeam{}}
			rt := newFullTestTeam(t, "alpha")
			d.teams["alpha"] = rt
			d.usageAgg = newUsageAgg(t, usage.Config{
				DailyTokenBudget:  1_000_000,
				ThrottleThreshold: 0.8,
				ResetAnchor:       "00:00 UTC",
			})
			used := int64(c.pct * 10_000)
			if err := d.usageAgg.Record(usage.UsageSummary{
				Model: "claude-opus-4-7", InputTokens: used, EndedAt: time.Now(),
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
			w := httptest.NewRecorder()
			d.handler().ServeHTTP(w, req)
			body := w.Body.String()
			if !strings.Contains(body, c.barTxt) {
				t.Errorf("bar colour at %d%%: missing %q", c.pct, c.barTxt)
			}
			if c.noThrot && strings.Contains(body, ">THROTTLING<") {
				t.Errorf("throttling badge should be absent at %d%%", c.pct)
			}
			if !c.noThrot && !strings.Contains(body, ">THROTTLING<") {
				t.Errorf("throttling badge should be present at %d%%", c.pct)
			}
		})
	}
}

// excerptAround returns a substring of body centered on the first
// occurrence of needle, useful for failure messages.
func excerptAround(body, needle string, span int) string {
	i := strings.Index(body, needle)
	if i < 0 {
		return "(needle not in body)"
	}
	start := i - span/2
	if start < 0 {
		start = 0
	}
	end := i + span/2
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}
