package mcp

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/messaging"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/team"
)

// testHookSink mirrors the production hookedSink in cmd/teem: every
// successful Write fans through a callback. The regression contract
// these tests cover is "leader-originated audit Writes (record_decision,
// record_blocker) go through the configured Audit Sink, so a
// hook-firing Sink delivers them to downstream consumers (messaging,
// channels, etc.)". Before t-da4c381c, mcp's tools wrote straight to
// the FileSink and skipped the hook chain entirely.
type testHookSink struct {
	inner audit.Sink
	hook  func([]audit.Event)
}

func (s *testHookSink) Write(e audit.Event) error {
	if err := s.inner.Write(e); err != nil {
		return err
	}
	if s.hook != nil {
		s.hook([]audit.Event{e})
	}
	return nil
}

func (s *testHookSink) Query(agentID string, since time.Time, limit int) ([]audit.Event, error) {
	return s.inner.Query(agentID, since, limit)
}

func (s *testHookSink) Close() error { return s.inner.Close() }

type recordingNotifier struct {
	mu    sync.Mutex
	calls []messaging.Message
}

func (r *recordingNotifier) Notify(_ context.Context, msg messaging.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, msg)
	return nil
}

func (r *recordingNotifier) snapshot() []messaging.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]messaging.Message, len(r.calls))
	copy(out, r.calls)
	return out
}

// newMessagingHookForTest builds a hook that runs every event through
// messaging.MessageFormatter and forwards rendered Messages to the
// recording notifier. Mirrors makeMessagingHook in cmd/teem without
// dedup so each event in a test produces exactly one delivery.
func newMessagingHookForTest(fmtr messaging.MessageFormatter, n *recordingNotifier) func([]audit.Event) {
	return func(events []audit.Event) {
		for _, e := range events {
			msg, ok := fmtr.Format(e)
			if !ok {
				continue
			}
			_ = n.Notify(context.Background(), msg)
		}
	}
}

// newHookedTestServer builds a Server whose Audit sink fans every
// Write through `hook`. Mirrors newTestServerFull but lets the test
// observe the hook firing.
func newHookedTestServer(t *testing.T, hook func([]audit.Event)) (*Server, *plan.Plan) {
	t.Helper()
	dir := t.TempDir()
	p, err := plan.Open(filepath.Join(dir, "plan.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	a, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	sink := &testHookSink{inner: a, hook: hook}
	tm := &team.Team{
		Name:       "t",
		Leader:     team.LeaderSpec{SystemPrompt: "p"},
		Archetypes: []team.ArchetypeSpec{{Role: "worker", Placement: "local", MaxConcurrent: 1}},
	}
	srv, err := New(Config{
		Bus:      bus.NewMemBus(),
		Team:     tm,
		Registry: NewRegistry(),
		Spawner:  &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}},
		Plan:     p,
		Audit:    sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, p
}

// TestRecordDecisionQuestionFiresMessagingHook is the regression test
// for t-da4c381c. record_decision with severity=question must reach a
// messaging notifier when the Audit sink fires hooks on Write — the
// previous bug was that MCP tools wrote straight to the FileSink and
// skipped the hook chain.
func TestRecordDecisionQuestionFiresMessagingHook(t *testing.T) {
	n := &recordingNotifier{}
	fmtr := messaging.MessageFormatter{TeamID: "alpha", DashboardBaseURL: "https://d"}
	srv, p := newHookedTestServer(t, newMessagingHookForTest(fmtr, n))

	task, err := p.AddTask(plan.NewTaskInput{Title: "Pick a library"})
	if err != nil {
		t.Fatal(err)
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "record_decision"
	req.Params.Arguments = map[string]any{
		"task_id":  task.ID,
		"text":     "should we use library A or B?",
		"severity": "question",
	}
	res, err := srv.handleRecordDecision(context.Background(), req)
	if err != nil {
		t.Fatalf("record_decision: %v", err)
	}
	if res.IsError {
		t.Fatalf("record_decision returned IsError: %s", textOf(t, res))
	}

	got := n.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 messaging notification, got %d: %+v", len(got), got)
	}
	if got[0].Severity != messaging.SeverityDecision {
		t.Errorf("severity = %q, want %q", got[0].Severity, messaging.SeverityDecision)
	}
	if got[0].TaskID != task.ID {
		t.Errorf("task_id = %q, want %q", got[0].TaskID, task.ID)
	}
	if got[0].AgentID != "leader" {
		t.Errorf("agent_id = %q, want %q", got[0].AgentID, "leader")
	}
}

// TestRecordDecisionInfoSkipsMessagingHook locks the negative case:
// severity=info decisions are journal entries, not operator pages. The
// hook still fires (the Sink runs every Write), but the messaging
// formatter filters them out before the notifier sees anything.
func TestRecordDecisionInfoSkipsMessagingHook(t *testing.T) {
	n := &recordingNotifier{}
	fmtr := messaging.MessageFormatter{TeamID: "alpha"}
	srv, p := newHookedTestServer(t, newMessagingHookForTest(fmtr, n))

	task, _ := p.AddTask(plan.NewTaskInput{Title: "X"})
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "record_decision"
	req.Params.Arguments = map[string]any{
		"task_id": task.ID,
		"text":    "going with library A",
		// severity unset → defaults to "info"
	}
	if _, err := srv.handleRecordDecision(context.Background(), req); err != nil {
		t.Fatalf("record_decision: %v", err)
	}
	if got := n.snapshot(); len(got) != 0 {
		t.Fatalf("expected 0 notifications for severity=info, got %d", len(got))
	}
}

// TestRecordBlockerFiresMessagingHook covers the parallel gap on
// record_blocker. Blockers are always operator-must-see so the hook
// always delivers when the task isn't already in a blocked state.
func TestRecordBlockerFiresMessagingHook(t *testing.T) {
	n := &recordingNotifier{}
	fmtr := messaging.MessageFormatter{TeamID: "alpha", DashboardBaseURL: "https://d"}
	srv, p := newHookedTestServer(t, newMessagingHookForTest(fmtr, n))

	task, _ := p.AddTask(plan.NewTaskInput{Title: "Ship X"})
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "record_blocker"
	req.Params.Arguments = map[string]any{
		"task_id": task.ID,
		"text":    "missing API key in env",
	}
	res, err := srv.handleRecordBlocker(context.Background(), req)
	if err != nil {
		t.Fatalf("record_blocker: %v", err)
	}
	if res.IsError {
		t.Fatalf("record_blocker returned IsError: %s", textOf(t, res))
	}

	got := n.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 messaging notification, got %d: %+v", len(got), got)
	}
	if got[0].Severity != messaging.SeverityWarning {
		t.Errorf("severity = %q, want %q", got[0].Severity, messaging.SeverityWarning)
	}
	if got[0].TaskID != task.ID {
		t.Errorf("task_id = %q, want %q", got[0].TaskID, task.ID)
	}
}
