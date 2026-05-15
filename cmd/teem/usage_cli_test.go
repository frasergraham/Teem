package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrintUsageReport_EmptyState(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "usage.yaml") // doesn't exist
	statePath := filepath.Join(dir, "usage.json")
	var buf bytes.Buffer
	if err := printUsageReport(&buf, cfgPath, statePath, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no daily budget set") {
		t.Errorf("missing throttle-disabled line: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "no usage recorded yet today") {
		t.Errorf("missing empty-state line: %s", buf.String())
	}
}

func TestPrintUsageReport_WithBudgetAndState(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "usage.yaml")
	statePath := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(cfgPath, []byte("usage:\n  daily_token_budget: 1000\n  throttle_threshold: 0.5\n  reset_anchor: \"00:00 UTC\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"last_reset":"2026-05-15T00:00:00Z","by_model":{"claude-opus-4-7":{"input":300,"output":200,"cache_create":100,"cache_read":4000}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	if err := printUsageReport(&buf, cfgPath, statePath, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// 600 = 300+200+100 (excluding cache_read).
	if !strings.Contains(out, "600 / 1000") {
		t.Errorf("expected total 600 / 1000: %s", out)
	}
	if !strings.Contains(out, "THROTTLED") {
		t.Errorf("expected THROTTLED (60%% ≥ 50%% threshold): %s", out)
	}
	if !strings.Contains(out, "claude-opus-4-7") {
		t.Errorf("missing per-model row: %s", out)
	}
	if !strings.Contains(out, "next reset:  2026-05-16T00:00:00Z") {
		t.Errorf("missing next-reset line: %s", out)
	}
}
