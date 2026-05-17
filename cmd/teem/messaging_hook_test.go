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
	hook := makeMessagingHook(n, fmtr, newTestDedup(t), nil)
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
	if h := makeMessagingHook(nil, messaging.MessageFormatter{}, newTestDedup(t), nil); h != nil {
		t.Fatal("nil notifier should yield nil hook")
	}
}

func TestMessagingHook_SuppressesConsecutiveIdlePulses(t *testing.T) {
	idle := func() audit.Event {
		return audit.Event{Kind: audit.KindPulseTick, AgentID: "leader", Meta: map[string]any{"tool_calls": 0}}
	}
	busy := func() audit.Event {
		return audit.Event{Kind: audit.KindPulseTick, AgentID: "leader", Meta: map[string]any{"tool_calls": 3}}
	}
	blocker := func() audit.Event {
		return audit.Event{Kind: audit.KindBlockerNote, AgentID: "reviewer-blake", Message: "creds missing", Meta: map[string]any{"task_id": "t-1"}}
	}
	// Pulse_tick dedup keys collapse to "team//info"; with a fixed clock
	// they'd swallow back-to-back distinct events in these tests. Drive
	// the dedup clock forward 1h per read so consecutive events always
	// escape the window, leaving the idle-suppression logic as the only
	// gate we're exercising.
	newOpenDedup := func() *messaging.Dedup {
		d, err := messaging.NewDedup(filepath.Join(t.TempDir(), "ded.json"), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		base := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
		var ticks int64
		d.SetClock(func() time.Time {
			ticks++
			return base.Add(time.Duration(ticks) * time.Hour)
		})
		return d
	}

	// Two idles in a row: first goes through, second is dropped.
	t.Run("idle then idle drops the second", func(t *testing.T) {
		n := &recNotifier{}
		fmtr := messaging.MessageFormatter{TeamID: "foo"}
		hook := makeMessagingHook(n, fmtr, newOpenDedup(), nil)
		hook([]audit.Event{idle()})
		hook([]audit.Event{idle()})
		if got := len(n.snapshot()); got != 1 {
			t.Fatalf("expected 1 notification (second idle suppressed), got %d", got)
		}
	})

	// Busy tick resets the marker so the next idle is delivered.
	t.Run("busy tick resets the streak", func(t *testing.T) {
		n := &recNotifier{}
		fmtr := messaging.MessageFormatter{TeamID: "foo"}
		hook := makeMessagingHook(n, fmtr, newOpenDedup(), nil)
		hook([]audit.Event{idle(), busy(), idle()})
		if got := len(n.snapshot()); got != 3 {
			t.Fatalf("expected 3 notifications (busy clears suppression), got %d", got)
		}
	})

	// Any non-pulse operator-must-see event ("something happened") also
	// resets the marker, so the next idle is informative again.
	t.Run("non-pulse event resets the streak", func(t *testing.T) {
		n := &recNotifier{}
		fmtr := messaging.MessageFormatter{TeamID: "foo"}
		hook := makeMessagingHook(n, fmtr, newOpenDedup(), nil)
		hook([]audit.Event{idle(), blocker(), idle()})
		if got := len(n.snapshot()); got != 3 {
			t.Fatalf("expected 3 notifications (non-pulse event clears suppression), got %d", got)
		}
	})

	// Many idles in a row: only the first one is forwarded.
	t.Run("long idle streak collapses to one", func(t *testing.T) {
		n := &recNotifier{}
		fmtr := messaging.MessageFormatter{TeamID: "foo"}
		hook := makeMessagingHook(n, fmtr, newOpenDedup(), nil)
		hook([]audit.Event{idle(), idle(), idle(), idle()})
		if got := len(n.snapshot()); got != 1 {
			t.Fatalf("expected 1 notification (rest suppressed), got %d", got)
		}
	})
}

func TestMessagingHook_DedupBlocksRepeats(t *testing.T) {
	n := &recNotifier{}
	fmtr := messaging.MessageFormatter{TeamID: "foo"}
	hook := makeMessagingHook(n, fmtr, newTestDedup(t), nil)
	ev := audit.Event{Kind: audit.KindBlockerNote, AgentID: "leader", Meta: map[string]any{"task_id": "t-1"}}
	hook([]audit.Event{ev, ev, ev})
	if got := len(n.snapshot()); got != 1 {
		t.Fatalf("expected 1 (deduped), got %d", got)
	}
}
