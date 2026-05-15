// Package usage parses Claude Code's stream-json output to extract
// per-subprocess token usage. It is the shared plumbing under
// docs/usage-capture.md — pulse, the local/SSH ProcessExecutor, and
// the teem-worker daemon all run `claude -p --output-format
// stream-json` and now route the `usage` fields through one Capture.
//
// The package only knows the wire format; emitting the resulting
// UsageSummary as an audit.KindUsageEvent is the caller's job. That
// keeps usage importable from anywhere (no audit cycle) while still
// letting both consumers (usage-monitor throttle + token-cost
// attribution) read the same on-disk event shape.
package usage

import (
	"bufio"
	"encoding/json"
	"io"
	"time"
)

// UsageSummary is the per-subprocess rollup. One emit per claude
// invocation — per-turn audit events are intentionally not used
// (audit volume + the executor can't kill a job mid-stream today,
// see docs/usage-capture.md §10).
type UsageSummary struct {
	Model             string
	InputTokens       int64
	OutputTokens      int64
	CacheCreateTokens int64
	CacheReadTokens   int64
	// TotalCostUSD is a raw passthrough from Claude Code's "result"
	// rollup. Zero when absent. Cost attribution computes its own
	// number from pricing.yaml so this is a drift-indicator only.
	TotalCostUSD float64
	StartedAt    time.Time
	EndedAt      time.Time
	// Partial is true if the stream ended without a "result" rollup
	// — usually because the subprocess was killed mid-run. Consumers
	// may choose to skip these or render them visibly.
	Partial bool
}

// Capture is a streaming parser. Callers that already scan the
// stream themselves (pulse's parseTickStream, executor's
// ParseClaudeStreamJSON) call Feed line-by-line. Callers that only
// need usage can use Observe to drain a reader in one shot.
//
// Not goroutine-safe; one Capture per subprocess.
type Capture struct {
	s         UsageSummary
	sawResult bool
}

// NewCapture stamps the start time. Use the moment you spawn the
// subprocess so StartedAt reflects wall clock cost, not stream
// arrival.
func NewCapture(started time.Time) *Capture {
	return &Capture{s: UsageSummary{StartedAt: started.UTC()}}
}

// streamEvent is a union of the fields we care about across the
// stream-json event types Claude Code emits. Unknown fields are
// silently dropped by json.Unmarshal, which is the schema-drift
// policy from docs/usage-capture.md §7.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Message struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// Feed parses one stream-json line. Bad JSON or unknown event types
// are silently skipped — the schema evolves, and the parsers we
// share with already tolerate noise lines.
func (c *Capture) Feed(line []byte) {
	if len(line) == 0 {
		return
	}
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "system":
		// The init event arrives as type="system" subtype="init" and
		// carries the chosen model name. Only take the first one —
		// /model mid-session would surface on assistant messages.
		if ev.Subtype == "init" && ev.Model != "" && c.s.Model == "" {
			c.s.Model = ev.Model
		}
	case "assistant":
		if ev.Message.Model != "" && c.s.Model == "" {
			c.s.Model = ev.Message.Model
		}
		// Sum per-turn usage. When a "result" rollup arrives later we
		// replace these numbers — the rollup is authoritative because
		// it skips tool-result echoes.
		if !c.sawResult {
			c.s.InputTokens += ev.Message.Usage.InputTokens
			c.s.OutputTokens += ev.Message.Usage.OutputTokens
			c.s.CacheCreateTokens += ev.Message.Usage.CacheCreationInputTokens
			c.s.CacheReadTokens += ev.Message.Usage.CacheReadInputTokens
		}
	case "result":
		u := ev.Usage
		if u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheCreationInputTokens != 0 || u.CacheReadInputTokens != 0 {
			c.s.InputTokens = u.InputTokens
			c.s.OutputTokens = u.OutputTokens
			c.s.CacheCreateTokens = u.CacheCreationInputTokens
			c.s.CacheReadTokens = u.CacheReadInputTokens
			c.sawResult = true
		}
		if ev.TotalCostUSD != 0 {
			c.s.TotalCostUSD = ev.TotalCostUSD
		}
	}
}

// Summary returns the accumulated rollup with EndedAt stamped to
// now. Partial is true iff no "result" rollup was seen.
func (c *Capture) Summary() UsageSummary {
	s := c.s
	if s.EndedAt.IsZero() {
		s.EndedAt = time.Now().UTC()
	}
	s.Partial = !c.sawResult
	return s
}

// Observe drains r, feeding every line through Feed, and returns the
// rolled-up summary. Convenience for callers that don't have their
// own parser. Errors from the underlying scanner are returned
// alongside whatever summary we managed to build.
func (c *Capture) Observe(r io.Reader) (UsageSummary, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		c.Feed(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		return c.Summary(), err
	}
	return c.Summary(), nil
}

// AuditMeta returns the canonical Meta map for a KindUsageEvent
// audit event. agent_id is duplicated into Meta so consumers can
// query directly on the Meta bag without re-joining against
// Event.AgentID. job_id is omitted from Meta when empty (pulse
// ticks have no job).
func AuditMeta(s UsageSummary, agentID, jobID string) map[string]any {
	m := map[string]any{
		"agent_id":            agentID,
		"model":               s.Model,
		"input_tokens":        s.InputTokens,
		"output_tokens":       s.OutputTokens,
		"cache_create_tokens": s.CacheCreateTokens,
		"cache_read_tokens":   s.CacheReadTokens,
		"started_at":          s.StartedAt.UTC().Format(time.RFC3339),
		"ended_at":            s.EndedAt.UTC().Format(time.RFC3339),
	}
	if jobID != "" {
		m["job_id"] = jobID
	}
	if s.TotalCostUSD != 0 {
		m["total_cost_usd"] = s.TotalCostUSD
	}
	if s.Partial {
		m["partial"] = true
	}
	return m
}
