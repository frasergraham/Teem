package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/messaging"
)

type recNotifier struct {
	mu    sync.Mutex
	calls []messaging.Message
}

func (r *recNotifier) Notify(_ context.Context, msg messaging.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, msg)
	return nil
}

func (r *recNotifier) snapshot() []messaging.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]messaging.Message, len(r.calls))
	copy(out, r.calls)
	return out
}

func newTestDedup(t *testing.T) *messaging.Dedup {
	t.Helper()
	d, err := messaging.NewDedup(filepath.Join(t.TempDir(), "ded.json"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d.SetClock(func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) })
	return d
}

func TestMessagingHook_FiltersOperatorMustSeeSubset(t *testing.T) {
	n := &recNotifier{}
	fmtr := messaging.MessageFormatter{TeamID: "foo", DashboardBaseURL: "https://d"}
	hook := makeMessagingHook(n, fmtr, newTestDedup(t))
	hook([]audit.Event{
		// fires
		{Kind: audit.KindBlockerNote, AgentID: "reviewer-blake", Message: "creds missing", Meta: map[string]any{"task_id": "t-1"}},
		// skips (channel-only)
		{Kind: audit.KindJobComplete, AgentID: "worker-una"},
		// skips (worker error)
		{Kind: audit.KindJobError, AgentID: "worker-una", Message: "boom"},
		// fires (leader error)
		{Kind: audit.KindJobError, AgentID: "leader", Message: "boom"},
		// skips (ordinary stage change)
		{Kind: audit.KindTaskStageChanged, Meta: map[string]any{"to": "reviewing", "task_id": "t-2"}},
		// fires (awaiting_approval)
		{Kind: audit.KindTaskStageChanged, AgentID: "worker-una", Meta: map[string]any{"to": "awaiting_approval", "task_id": "t-3"}},
		// skips (info decision)
		{Kind: audit.KindDecisionNote, AgentID: "leader", Meta: map[string]any{"task_id": "t-4", "severity": "info"}},
		// fires (question decision)
		{Kind: audit.KindDecisionNote, AgentID: "leader", Message: "?", Meta: map[string]any{"task_id": "t-5", "severity": "question"}},
	})
	got := n.snapshot()
	if len(got) != 4 {
		t.Fatalf("expected 4 notifications, got %d: %+v", len(got), got)
	}
	wantTasks := []string{"t-1", "", "t-3", "t-5"}
	for i, want := range wantTasks {
		if got[i].TaskID != want {
			t.Errorf("call[%d].TaskID = %q, want %q", i, got[i].TaskID, want)
		}
	}
}

func TestMessagingHook_NilNotifierReturnsNil(t *testing.T) {
	if h := makeMessagingHook(nil, messaging.MessageFormatter{}, newTestDedup(t)); h != nil {
		t.Fatal("nil notifier should yield nil hook")
	}
}

func TestMessagingHook_DedupBlocksRepeats(t *testing.T) {
	n := &recNotifier{}
	fmtr := messaging.MessageFormatter{TeamID: "foo"}
	hook := makeMessagingHook(n, fmtr, newTestDedup(t))
	ev := audit.Event{Kind: audit.KindBlockerNote, AgentID: "leader", Meta: map[string]any{"task_id": "t-1"}}
	hook([]audit.Event{ev, ev, ev})
	if got := len(n.snapshot()); got != 1 {
		t.Fatalf("expected 1 (deduped), got %d", got)
	}
}
