package usage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the operator-supplied throttle policy at ~/.teem/usage.yaml.
// A missing file is not an error: the resulting Config has zero values
// and AvailableQuota treats DailyTokenBudget == 0 as "no throttle"
// (the operator hasn't opted in).
type Config struct {
	// DailyTokenBudget is the cap on the sum of input + output +
	// cache_create tokens across all models in a single day. Zero
	// disables the throttle.
	DailyTokenBudget int64 `yaml:"daily_token_budget"`
	// ThrottleThreshold is the fraction of the budget at which the
	// throttle activates. Defaults to 0.80 when zero. Setting > 1 is
	// allowed (e.g. soft caps) but rounded to 1.0 for the gate
	// decision.
	ThrottleThreshold float64 `yaml:"throttle_threshold"`
	// ResetAnchor is the local clock + timezone at which the daily
	// counter resets, formatted "HH:MM <Area/Location>". Defaults to
	// "00:00 America/Los_Angeles" when empty.
	ResetAnchor string `yaml:"reset_anchor"`
}

// configFile is a one-key wrapper around Config so the on-disk shape
// is `usage: { … }`. Mirrors the operator-visible layout the task
// brief documents.
type configFile struct {
	Usage Config `yaml:"usage"`
}

// DefaultThrottleThreshold is the fraction used when the operator
// doesn't pin one. 0.80 leaves a 20% buffer for in-flight subprocesses
// that haven't yet rolled up.
const DefaultThrottleThreshold = 0.80

// DefaultResetAnchor is the daily roll-over the operator gets without
// opting in. Pacific time matches typical Anthropic billing pages.
const DefaultResetAnchor = "00:00 America/Los_Angeles"

// DefaultConfigPath returns ~/.teem/usage.yaml. Empty when $HOME is
// unreadable.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".teem", "usage.yaml")
}

// LoadConfig reads path and returns the populated Config. A missing
// file returns a zero Config (no throttle) and no error. Malformed YAML
// is an error so the operator notices.
func LoadConfig(path string) (Config, error) {
	var cf configFile
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("usage: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(body, &cf); err != nil {
		return Config{}, fmt.Errorf("usage: parse %s: %w", path, err)
	}
	c := cf.Usage
	if c.DailyTokenBudget < 0 {
		return Config{}, fmt.Errorf("usage: daily_token_budget must be ≥ 0, got %d", c.DailyTokenBudget)
	}
	if c.ThrottleThreshold < 0 {
		return Config{}, fmt.Errorf("usage: throttle_threshold must be ≥ 0, got %v", c.ThrottleThreshold)
	}
	return c, nil
}

// EffectiveThreshold returns ThrottleThreshold, falling back to the
// default when unset (zero).
func (c Config) EffectiveThreshold() float64 {
	if c.ThrottleThreshold <= 0 {
		return DefaultThrottleThreshold
	}
	return c.ThrottleThreshold
}

// EffectiveAnchor returns ResetAnchor, falling back to the default
// when empty.
func (c Config) EffectiveAnchor() string {
	if strings.TrimSpace(c.ResetAnchor) == "" {
		return DefaultResetAnchor
	}
	return c.ResetAnchor
}

// MostRecentReset returns the most recent wall-clock moment that the
// daily counter would have reset, given the anchor and now. Used both
// by the rollover decision and the CLI's "next reset" projection.
func (c Config) MostRecentReset(now time.Time) (time.Time, error) {
	hh, mm, loc, err := parseAnchor(c.EffectiveAnchor())
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, loc)
	if today.After(local) {
		// Anchor hasn't fired today yet — last reset was yesterday's anchor.
		return today.AddDate(0, 0, -1), nil
	}
	return today, nil
}

// NextReset is MostRecentReset + 24h. Tracking it as its own function
// keeps the CLI projection readable.
func (c Config) NextReset(now time.Time) (time.Time, error) {
	last, err := c.MostRecentReset(now)
	if err != nil {
		return time.Time{}, err
	}
	return last.AddDate(0, 0, 1), nil
}

// parseAnchor splits "HH:MM Area/Location" into its parts. Whitespace
// is tolerant; the location string is whatever time.LoadLocation
// accepts.
func parseAnchor(anchor string) (hh, mm int, loc *time.Location, err error) {
	parts := strings.Fields(anchor)
	if len(parts) != 2 {
		return 0, 0, nil, fmt.Errorf("usage: reset_anchor %q: want \"HH:MM Area/Location\"", anchor)
	}
	hm := strings.Split(parts[0], ":")
	if len(hm) != 2 {
		return 0, 0, nil, fmt.Errorf("usage: reset_anchor %q: bad time", anchor)
	}
	hh, err = strconv.Atoi(hm[0])
	if err != nil || hh < 0 || hh > 23 {
		return 0, 0, nil, fmt.Errorf("usage: reset_anchor %q: hour out of range", anchor)
	}
	mm, err = strconv.Atoi(hm[1])
	if err != nil || mm < 0 || mm > 59 {
		return 0, 0, nil, fmt.Errorf("usage: reset_anchor %q: minute out of range", anchor)
	}
	loc, err = time.LoadLocation(parts[1])
	if err != nil {
		return 0, 0, nil, fmt.Errorf("usage: reset_anchor %q: load zone: %w", anchor, err)
	}
	return hh, mm, loc, nil
}
