package executor

import (
	"bytes"
	"strings"
	"testing"
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

func TestParseClaudeStreamJSON_SinkCapturesVerbatim(t *testing.T) {
	var sink bytes.Buffer
	res, err := ParseClaudeStreamJSON(strings.NewReader(cannedStream), &sink)
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
	res, err := ParseClaudeStreamJSON(strings.NewReader(cannedStream), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.FinalText != "hello world" || res.EventCount != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParseClaudeStreamJSON_TolerateBadLine(t *testing.T) {
	stream := `{"type":"system"}
not json — should be skipped
{"type":"result","result":"after the garbage"}
`
	var sink bytes.Buffer
	res, err := ParseClaudeStreamJSON(strings.NewReader(stream), &sink)
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
