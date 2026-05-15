package messaging

import (
	"strings"
	"testing"

	"github.com/frasergraham/teem/internal/audit"
)

func TestIsMessagingKind(t *testing.T) {
	cases := []struct {
		name string
		e    audit.Event
		want bool
	}{
		{
			name: "awaiting_approval stage move fires",
			e:    audit.Event{Kind: audit.KindTaskStageChanged, Meta: map[string]any{"to": "awaiting_approval"}},
			want: true,
		},
		{
			name: "ordinary stage move skips",
			e:    audit.Event{Kind: audit.KindTaskStageChanged, Meta: map[string]any{"to": "reviewing"}},
			want: false,
		},
		{
			name: "blocker note fires",
			e:    audit.Event{Kind: audit.KindBlockerNote},
			want: true,
		},
		{
			name: "info decision skips",
			e:    audit.Event{Kind: audit.KindDecisionNote, Meta: map[string]any{"severity": "info"}},
			want: false,
		},
		{
			name: "decision with no severity skips",
			e:    audit.Event{Kind: audit.KindDecisionNote},
			want: false,
		},
		{
			name: "question decision fires",
			e:    audit.Event{Kind: audit.KindDecisionNote, Meta: map[string]any{"severity": "question"}},
			want: true,
		},
		{
			name: "worker error skips",
			e:    audit.Event{Kind: audit.KindJobError, AgentID: "worker-una"},
			want: false,
		},
		{
			name: "leader error fires",
			e:    audit.Event{Kind: audit.KindJobError, AgentID: "leader"},
			want: true,
		},
		{
			name: "heartbeat skips",
			e:    audit.Event{Kind: audit.KindHeartbeat},
			want: false,
		},
		{
			name: "job_complete skips (channel hook covers it)",
			e:    audit.Event{Kind: audit.KindJobComplete, AgentID: "worker-una"},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsMessagingKind(c.e); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestFormatter_ApprovalMessage(t *testing.T) {
	f := MessageFormatter{
		TeamID:           "foo",
		DashboardBaseURL: "https://dash.example",
		TaskTitle: func(id string) string {
			if id == "t-3a2fbc01" {
				return "PM design doc"
			}
			return ""
		},
	}
	msg, ok := f.Format(audit.Event{
		Kind:    audit.KindTaskStageChanged,
		AgentID: "worker-una",
		Meta:    map[string]any{"task_id": "t-3a2fbc01", "to": "awaiting_approval"},
	})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Severity != SeverityAction {
		t.Errorf("severity = %s, want action", msg.Severity)
	}
	if msg.Title != "[foo] Approval needed" {
		t.Errorf("title = %q", msg.Title)
	}
	if !strings.Contains(msg.Summary, "Coder Una") {
		t.Errorf("summary missing persona: %q", msg.Summary)
	}
	if !strings.Contains(msg.Summary, "PM design doc") {
		t.Errorf("summary missing task title: %q", msg.Summary)
	}
	if msg.Link != "https://dash.example/teams/foo/#task-t-3a2fbc01" {
		t.Errorf("link = %q", msg.Link)
	}
	if msg.TaskID != "t-3a2fbc01" || msg.TeamID != "foo" || msg.AgentID != "worker-una" {
		t.Errorf("identity fields wrong: %+v", msg)
	}
}

func TestFormatter_DecisionQuestion(t *testing.T) {
	f := MessageFormatter{TeamID: "foo", DashboardBaseURL: "https://d"}
	msg, ok := f.Format(audit.Event{
		Kind:    audit.KindDecisionNote,
		AgentID: "leader",
		Message: "Keep old auth around or rip it?",
		Meta:    map[string]any{"task_id": "t-9bc1aa", "severity": "question"},
	})
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.Severity != SeverityDecision {
		t.Errorf("severity = %s", msg.Severity)
	}
	if !strings.Contains(msg.Title, "Question from") {
		t.Errorf("title = %q", msg.Title)
	}
	if !strings.Contains(msg.Summary, "Keep old auth") {
		t.Errorf("summary = %q", msg.Summary)
	}
}

func TestFormatter_DecisionInfoSkipped(t *testing.T) {
	f := MessageFormatter{TeamID: "foo"}
	if _, ok := f.Format(audit.Event{
		Kind:    audit.KindDecisionNote,
		AgentID: "leader",
		Message: "Decided to use X.",
		Meta:    map[string]any{"task_id": "t-1", "severity": "info"},
	}); ok {
		t.Fatal("info-severity decision should not produce a message")
	}
}

func TestFormatter_Blocker(t *testing.T) {
	f := MessageFormatter{TeamID: "foo", DashboardBaseURL: "https://d"}
	msg, ok := f.Format(audit.Event{
		Kind:    audit.KindBlockerNote,
		AgentID: "reviewer-blake",
		Message: "Need prod db creds.",
		Meta:    map[string]any{"task_id": "t-7d43"},
	})
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.Severity != SeverityWarning {
		t.Errorf("severity = %s", msg.Severity)
	}
	if !strings.Contains(msg.Summary, "Reviewer Blake") {
		t.Errorf("summary missing persona: %q", msg.Summary)
	}
	if !strings.Contains(msg.Summary, "moved to blocked") {
		t.Errorf("summary missing tail: %q", msg.Summary)
	}
}

func TestFormatter_LeaderError(t *testing.T) {
	f := MessageFormatter{TeamID: "foo", DashboardBaseURL: "https://d"}
	msg, ok := f.Format(audit.Event{
		Kind:    audit.KindJobError,
		AgentID: "leader",
		JobID:   "j-9ac1",
		Message: "API Error: 529 Overloaded.",
	})
	if !ok {
		t.Fatal("expected ok")
	}
	if !strings.Contains(msg.Title, "Leader error") {
		t.Errorf("title = %q", msg.Title)
	}
	if !strings.Contains(msg.Summary, "529") {
		t.Errorf("summary = %q", msg.Summary)
	}

	if _, ok := f.Format(audit.Event{
		Kind:    audit.KindJobError,
		AgentID: "worker-una",
		Message: "boom",
	}); ok {
		t.Fatal("worker error should not produce a message")
	}
}
