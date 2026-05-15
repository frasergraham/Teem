package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig_MissingFile(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("missing file should be zero config, got err: %v", err)
	}
	if c.DailyTokenBudget != 0 {
		t.Errorf("DailyTokenBudget: got %d, want 0", c.DailyTokenBudget)
	}
}

func TestLoadConfig_Populated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.yaml")
	body := []byte("usage:\n  daily_token_budget: 50000000\n  throttle_threshold: 0.9\n  reset_anchor: \"04:00 UTC\"\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DailyTokenBudget != 50_000_000 {
		t.Errorf("budget: %d", c.DailyTokenBudget)
	}
	if c.ThrottleThreshold != 0.9 {
		t.Errorf("threshold: %v", c.ThrottleThreshold)
	}
	if c.ResetAnchor != "04:00 UTC" {
		t.Errorf("anchor: %q", c.ResetAnchor)
	}
}

func TestLoadConfig_Malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.yaml")
	if err := os.WriteFile(path, []byte(":\n- not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Errorf("expected parse error")
	}
}

func TestLoadConfig_NegativeBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.yaml")
	if err := os.WriteFile(path, []byte("usage:\n  daily_token_budget: -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Errorf("expected validation error for negative budget")
	}
}

func TestEffectiveDefaults(t *testing.T) {
	c := Config{}
	if c.EffectiveThreshold() != DefaultThrottleThreshold {
		t.Errorf("threshold default: %v", c.EffectiveThreshold())
	}
	if c.EffectiveAnchor() != DefaultResetAnchor {
		t.Errorf("anchor default: %q", c.EffectiveAnchor())
	}
}

func TestMostRecentReset_BeforeAnchorToday(t *testing.T) {
	c := Config{ResetAnchor: "04:00 UTC"}
	now := time.Date(2026, 5, 15, 2, 30, 0, 0, time.UTC) // before 04:00
	got, err := c.MostRecentReset(now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 5, 14, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestMostRecentReset_AfterAnchorToday(t *testing.T) {
	c := Config{ResetAnchor: "04:00 UTC"}
	now := time.Date(2026, 5, 15, 14, 30, 0, 0, time.UTC) // after 04:00
	got, err := c.MostRecentReset(now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 5, 15, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestNextReset(t *testing.T) {
	c := Config{ResetAnchor: "04:00 UTC"}
	now := time.Date(2026, 5, 15, 14, 30, 0, 0, time.UTC)
	got, err := c.NextReset(now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 5, 16, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestParseAnchor_BadInput(t *testing.T) {
	// "" falls back to the default anchor — that's the operator's
	// no-config path and must keep working. Other malformed values
	// must surface as errors so the operator notices a typo.
	for _, in := range []string{"noon", "25:00 UTC", "00:99 UTC", "00:00 Mars/Olympus"} {
		c := Config{ResetAnchor: in}
		if _, err := c.MostRecentReset(time.Now()); err == nil {
			t.Errorf("anchor %q should fail to parse", in)
		}
	}
}
