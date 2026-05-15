package usage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ModelPricing is the per-million-token USD rate for one Claude model.
// All four fields are dollar amounts per 1,000,000 tokens — the same
// unit Anthropic's pricing page uses, so the operator pastes numbers in
// directly. Zero is treated as "$0", not "missing" — operators who only
// price input/output for a model leave cache fields out and the YAML
// loader defaults them to zero.
type ModelPricing struct {
	InputPerMillion       float64 `yaml:"input_per_million"`
	OutputPerMillion      float64 `yaml:"output_per_million"`
	CacheReadPerMillion   float64 `yaml:"cache_read_per_million"`
	CacheCreatePerMillion float64 `yaml:"cache_create_per_million"`
}

// Pricing is the loaded ~/.teem/pricing.yaml. Models maps the Claude
// model id (e.g. "claude-opus-4-7") to its per-million rates; ModTime
// is the file's mtime so the dashboard can surface a stale-warn pill
// when the operator hasn't refreshed in StaleAge; Stale mirrors that
// check for callers that don't want to recompute.
type Pricing struct {
	Models  map[string]ModelPricing
	ModTime time.Time
	Stale   bool
}

// pricingFile is the on-disk shape: `pricing: { <model>: { … } }`.
// Matches docs/token-cost-measurement.md so the YAML the operator
// pastes from the spec works without translation.
type pricingFile struct {
	Pricing map[string]ModelPricing `yaml:"pricing"`
}

// StaleAge is the threshold past which a pricing.yaml is treated as
// suspect. Anthropic adjusts list prices a few times a year; an
// operator who set this up a quarter ago probably has drift worth
// flagging.
const StaleAge = 60 * 24 * time.Hour

// DefaultPricingPath returns ~/.teem/pricing.yaml. Empty when $HOME is
// unreadable (the caller treats the empty path as "no pricing").
func DefaultPricingPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".teem", "pricing.yaml")
}

// LoadPricing reads path. Returns (p, true, nil) on success;
// (Pricing{}, false, nil) when the file is absent — per design the
// dashboard hides cost UI rather than rendering $0, so the missing
// file isn't an error; (Pricing{}, false, err) on parse error so the
// operator notices a typo instead of silently losing cost visibility.
func LoadPricing(path string) (Pricing, bool, error) {
	if path == "" {
		return Pricing{}, false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Pricing{}, false, nil
		}
		return Pricing{}, false, fmt.Errorf("usage: stat pricing %s: %w", path, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Pricing{}, false, fmt.Errorf("usage: read pricing %s: %w", path, err)
	}
	var pf pricingFile
	if err := yaml.Unmarshal(body, &pf); err != nil {
		return Pricing{}, false, fmt.Errorf("usage: parse pricing %s: %w", path, err)
	}
	mod := info.ModTime()
	p := Pricing{
		Models:  pf.Pricing,
		ModTime: mod,
		Stale:   time.Since(mod) > StaleAge,
	}
	if p.Models == nil {
		p.Models = map[string]ModelPricing{}
	}
	return p, true, nil
}

// HasPricing returns true when the loaded pricing actually carries at
// least one model. Empty maps (well-formed file with no entries) count
// as absent for UI purposes.
func (p Pricing) HasPricing() bool { return len(p.Models) > 0 }
