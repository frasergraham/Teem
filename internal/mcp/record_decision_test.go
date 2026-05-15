package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/plan"
)

// TestRecordDecision_SeverityParam covers the new optional severity
// parameter. Default = "info" (silent journal entry); "question" tags
// the audit event for the messaging filter; bad values are rejected.
func TestRecordDecision_SeverityParam(t *testing.T) {
	srv, p, _, a := newTestServerFull(t)
	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})

	type want struct {
		isErr    bool
		severity string
	}
	cases := []struct {
		name string
		args map[string]any
		want want
	}{
		{
			name: "default = info",
			args: map[string]any{"task_id": task.ID, "text": "vendoring foo"},
			want: want{severity: "info"},
		},
		{
			name: "explicit info",
			args: map[string]any{"task_id": task.ID, "text": "x", "severity": "info"},
			want: want{severity: "info"},
		},
		{
			name: "question accepted",
			args: map[string]any{"task_id": task.ID, "text": "?", "severity": "question"},
			want: want{severity: "question"},
		},
		{
			name: "bad value rejected",
			args: map[string]any{"task_id": task.ID, "text": "x", "severity": "bogus"},
			want: want{isErr: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Name = "record_decision"
			req.Params.Arguments = c.args
			res, err := srv.handleRecordDecision(context.Background(), req)
			if err != nil {
				t.Fatalf("call err: %v", err)
			}
			if res.IsError != c.want.isErr {
				t.Fatalf("IsError = %v; text = %s", res.IsError, textOf(t, res))
			}
			if c.want.isErr {
				return
			}
			// Find the most recent decision_note and check its severity.
			events, _ := a.Query("", parseZero(), 100)
			var got *audit.Event
			for i := range events {
				if events[i].Kind == audit.KindDecisionNote {
					got = &events[i]
				}
			}
			if got == nil {
				t.Fatal("no decision_note emitted")
			}
			sev, _ := got.Meta["severity"].(string)
			if sev != c.want.severity {
				t.Errorf("severity = %q, want %q", sev, c.want.severity)
			}
		})
	}
}
