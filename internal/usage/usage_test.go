package usage

import (
	"strings"
	"testing"
	"time"
)

// Realistic-looking init / assistant / result events. The exact
// shape mirrors the cannedStream in internal/executor/process_test.go
// plus the usage fields docs/usage-capture.md §2 calls out.
const inputOnlyStream = `{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1000,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"result","result":"done","usage":{"input_tokens":1000,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"total_cost_usd":0.01}
`

const outputOnlyStream = `{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"assistant","message":{"usage":{"output_tokens":2500}}}
{"type":"result","usage":{"output_tokens":2500}}
`

const cacheHitStream = `{"type":"system","subtype":"init","model":"claude-haiku-4-5-20251001"}
{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":100,"cache_creation_input_tokens":4000,"cache_read_input_tokens":12000}}}
{"type":"result","usage":{"input_tokens":50,"output_tokens":100,"cache_creation_input_tokens":4000,"cache_read_input_tokens":12000},"total_cost_usd":0.0034}
`

// Two assistant turns and a final result rollup. We expect the
// result rollup to win over the per-turn sum (the rollup is
// authoritative — docs/usage-capture.md §2).
const multiTurnStream = `{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":50}}}
{"type":"assistant","message":{"usage":{"input_tokens":200,"output_tokens":75}}}
{"type":"result","usage":{"input_tokens":300,"output_tokens":125},"total_cost_usd":0.05}
`

// Stream truncated mid-flight: no result rollup, just one assistant
// turn. Capture should report what it saw and flag Partial.
const truncatedStream = `{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"assistant","message":{"usage":{"input_tokens":42,"output_tokens":21}}}
`

// No init event: model identification has to fall back to the
// assistant.message.model field, and the rest works as normal.
const missingInitStream = `{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":20}}}
{"type":"result","usage":{"input_tokens":10,"output_tokens":20}}
`

const malformedStream = `{"type":"system","subtype":"init","model":"claude-opus-4-7"}
not valid json — should be skipped
{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":2}}}
{"type":"result","usage":{"input_tokens":1,"output_tokens":2}}
`

func TestCapture_InputOnly(t *testing.T) {
	c := NewCapture(time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	s, err := c.Observe(strings.NewReader(inputOnlyStream))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if s.Model != "claude-opus-4-7" {
		t.Errorf("model: got %q", s.Model)
	}
	if s.InputTokens != 1000 || s.OutputTokens != 0 {
		t.Errorf("tokens: in=%d out=%d", s.InputTokens, s.OutputTokens)
	}
	if s.TotalCostUSD != 0.01 {
		t.Errorf("total_cost_usd: %v", s.TotalCostUSD)
	}
	if s.Partial {
		t.Errorf("Partial should be false when result rollup present")
	}
	if s.EndedAt.Before(s.StartedAt) {
		t.Errorf("EndedAt %v should be ≥ StartedAt %v", s.EndedAt, s.StartedAt)
	}
}

func TestCapture_OutputOnly(t *testing.T) {
	c := NewCapture(time.Now())
	s, _ := c.Observe(strings.NewReader(outputOnlyStream))
	if s.OutputTokens != 2500 || s.InputTokens != 0 {
		t.Errorf("tokens: in=%d out=%d", s.InputTokens, s.OutputTokens)
	}
	if s.Model != "claude-opus-4-7" {
		t.Errorf("model: %q", s.Model)
	}
}

func TestCapture_CacheHit(t *testing.T) {
	c := NewCapture(time.Now())
	s, _ := c.Observe(strings.NewReader(cacheHitStream))
	if s.CacheCreateTokens != 4000 {
		t.Errorf("cache create: %d", s.CacheCreateTokens)
	}
	if s.CacheReadTokens != 12000 {
		t.Errorf("cache read: %d", s.CacheReadTokens)
	}
	if s.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model: %q", s.Model)
	}
	if s.TotalCostUSD == 0 {
		t.Errorf("total_cost_usd should be populated")
	}
}

func TestCapture_MultiTurnPrefersResultRollup(t *testing.T) {
	c := NewCapture(time.Now())
	s, _ := c.Observe(strings.NewReader(multiTurnStream))
	// Per-turn would sum to 300/125; the result rollup is also 300/125.
	// Verify we end up with the rollup numbers (not 600/250 from double-counting).
	if s.InputTokens != 300 || s.OutputTokens != 125 {
		t.Errorf("tokens: in=%d out=%d (rollup must replace per-turn sum)", s.InputTokens, s.OutputTokens)
	}
	if s.Partial {
		t.Errorf("Partial should be false")
	}
}

func TestCapture_TruncatedMarkedPartial(t *testing.T) {
	c := NewCapture(time.Now())
	s, _ := c.Observe(strings.NewReader(truncatedStream))
	if !s.Partial {
		t.Errorf("Partial must be true when no result rollup landed")
	}
	if s.InputTokens != 42 || s.OutputTokens != 21 {
		t.Errorf("partial tokens still recorded: in=%d out=%d", s.InputTokens, s.OutputTokens)
	}
}

func TestCapture_MissingInitFallsBackToAssistantModel(t *testing.T) {
	c := NewCapture(time.Now())
	s, _ := c.Observe(strings.NewReader(missingInitStream))
	if s.Model != "claude-sonnet-4-6" {
		t.Errorf("model fallback: got %q want claude-sonnet-4-6", s.Model)
	}
}

func TestCapture_MalformedLinesSkipped(t *testing.T) {
	c := NewCapture(time.Now())
	s, err := c.Observe(strings.NewReader(malformedStream))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if s.InputTokens != 1 || s.OutputTokens != 2 {
		t.Errorf("tokens after bad line: in=%d out=%d", s.InputTokens, s.OutputTokens)
	}
}

func TestCapture_EmptyStream(t *testing.T) {
	c := NewCapture(time.Now())
	s, _ := c.Observe(strings.NewReader(""))
	if s.InputTokens != 0 || s.OutputTokens != 0 || s.Model != "" {
		t.Errorf("empty stream should produce zero summary: %+v", s)
	}
	if !s.Partial {
		t.Errorf("empty stream must mark Partial")
	}
}

func TestAuditMeta_OmitsJobIDWhenEmpty(t *testing.T) {
	s := UsageSummary{
		Model:        "claude-opus-4-7",
		InputTokens:  10,
		OutputTokens: 20,
		StartedAt:    time.Now(),
		EndedAt:      time.Now(),
	}
	m := AuditMeta(s, "leader", "")
	if _, ok := m["job_id"]; ok {
		t.Errorf("job_id should be omitted when empty")
	}
	if m["agent_id"] != "leader" {
		t.Errorf("agent_id: %v", m["agent_id"])
	}
	if m["input_tokens"] != int64(10) {
		t.Errorf("input_tokens: %T %v", m["input_tokens"], m["input_tokens"])
	}
}

func TestAuditMeta_IncludesPartialAndCostWhenSet(t *testing.T) {
	s := UsageSummary{
		Model:        "x",
		TotalCostUSD: 0.42,
		Partial:      true,
		StartedAt:    time.Now(),
		EndedAt:      time.Now(),
	}
	m := AuditMeta(s, "worker-1", "j7")
	if m["job_id"] != "j7" {
		t.Errorf("job_id: %v", m["job_id"])
	}
	if m["total_cost_usd"] != 0.42 {
		t.Errorf("total_cost_usd: %v", m["total_cost_usd"])
	}
	if m["partial"] != true {
		t.Errorf("partial: %v", m["partial"])
	}
}

// Feed should be safe to call before/after Observe — useful for
// callers that interleave Capture into their own line scanners.
func TestCapture_FeedThenSummary(t *testing.T) {
	c := NewCapture(time.Now())
	c.Feed([]byte(`{"type":"system","subtype":"init","model":"claude-opus-4-7"}`))
	c.Feed([]byte(`{"type":"assistant","message":{"usage":{"input_tokens":7,"output_tokens":9}}}`))
	c.Feed([]byte(`{"type":"result","usage":{"input_tokens":7,"output_tokens":9}}`))
	s := c.Summary()
	if s.InputTokens != 7 || s.OutputTokens != 9 || s.Model != "claude-opus-4-7" {
		t.Errorf("summary: %+v", s)
	}
}
