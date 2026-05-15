package executor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/usage"
)

// canned stream-json mirrors the shape Claude Code emits with
// --output-format stream-json: a system init, an assistant message with
// a text content block, and a final 'result' event carrying the same
// text. Trailing newline is significant — ParseClaudeStreamJSON
// rebuilds it.
const cannedStream = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]}}
{"type":"result","result":"hello world"}
`

// cannedStreamWithUsage carries the same shape plus an init `model`
// and a result `usage` rollup so the capture path can be asserted.
const cannedStreamWithUsage = `{"type":"system","subtype":"init","model":"claude-opus-4-7"}
{"type":"assistant","message":{"model":"claude-opus-4-7","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":15,"output_tokens":7}}}
{"type":"result","result":"hi","usage":{"input_tokens":15,"output_tokens":7},"total_cost_usd":0.0012}
`

func TestParseClaudeStreamJSON_SinkCapturesVerbatim(t *testing.T) {
	var sink bytes.Buffer
	res, err := ParseClaudeStreamJSON(strings.NewReader(cannedStream), &sink, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.FinalText != "hello world" {
		t.Errorf("final text: got %q want %q", res.FinalText, "hello world")
	}
	if res.EventCount != 3 {
		t.Errorf("event count: got %d want 3", res.EventCount)
	}
	if got := sink.String(); got != cannedStream {
		t.Errorf("sink not verbatim:\n got: %q\nwant: %q", got, cannedStream)
	}
}

func TestParseClaudeStreamJSON_NilSinkParsesOnly(t *testing.T) {
	res, err := ParseClaudeStreamJSON(strings.NewReader(cannedStream), nil, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.FinalText != "hello world" || res.EventCount != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParseClaudeStreamJSON_FeedsUsageCapture(t *testing.T) {
	cap := usage.NewCapture(time.Now())
	_, err := ParseClaudeStreamJSON(strings.NewReader(cannedStreamWithUsage), nil, cap)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := cap.Summary()
	if s.Model != "claude-opus-4-7" {
		t.Errorf("model: %q", s.Model)
	}
	if s.InputTokens != 15 || s.OutputTokens != 7 {
		t.Errorf("tokens: in=%d out=%d", s.InputTokens, s.OutputTokens)
	}
	if s.Partial {
		t.Errorf("Partial should be false (result rollup landed)")
	}
}

func TestParseClaudeStreamJSON_TolerateBadLine(t *testing.T) {
	stream := `{"type":"system"}
not json — should be skipped
{"type":"result","result":"after the garbage"}
`
	var sink bytes.Buffer
	res, err := ParseClaudeStreamJSON(strings.NewReader(stream), &sink, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.FinalText != "after the garbage" {
		t.Errorf("final text: got %q", res.FinalText)
	}
	// Sink still gets every line verbatim — including the bad one.
	if !strings.Contains(sink.String(), "not json") {
		t.Errorf("sink should preserve bad lines verbatim")
	}
}
