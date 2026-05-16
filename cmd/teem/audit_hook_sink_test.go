package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// hookedSink is the single source of truth that makes leader-originated
// audit events (record_decision, record_blocker, chat usage events, …)
// fan through the same hook chain as worker HTTP POSTs. These tests
// pin the contract: Write delegates to the inner sink AND fires the
// hook with the event; Query/Close pass through; SetHook is the only
// supported way to install or replace the chain.

func TestHookedSink_WriteFiresHook(t *testing.T) {
	dir := t.TempDir()
	inner, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	var (
		mu  sync.Mutex
		got []audit.Event
	)
	sink := newHookedSink(inner)
	sink.SetHook(func(events []audit.Event) {
		mu.Lock()
		got = append(got, events...)
		mu.Unlock()
	})

	ev := audit.Event{
		Timestamp: time.Now().UTC(),
		AgentID:   "leader",
		Kind:      audit.KindDecisionNote,
		Message:   "?",
		Meta:      map[string]any{"task_id": "t-1", "severity": "question"},
	}
	if err := sink.Write(ev); err != nil {
		t.Fatalf("Write: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(got))
	}
	if got[0].Kind != audit.KindDecisionNote || got[0].AgentID != "leader" {
		t.Errorf("hook saw unexpected event: %+v", got[0])
	}

	// Inner sink also got the write.
	stored, err := inner.Query("", time.Time{}, 10)
	if err != nil {
		t.Fatalf("inner Query: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("inner sink has %d events, want 1", len(stored))
	}
}

func TestHookedSink_NoHookYet_StillWrites(t *testing.T) {
	// Buildup order in buildTeamServices means the sink is constructed
	// before the hook chain is wired (the chain depends on objects that
	// take the sink in their config). A Write that races that wiring
	// must still land on disk; the hook is a best-effort fan-out, not a
	// gate.
	dir := t.TempDir()
	inner, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	sink := newHookedSink(inner)
	if err := sink.Write(audit.Event{AgentID: "leader", Kind: audit.KindBlockerNote}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stored, _ := inner.Query("", time.Time{}, 10)
	if len(stored) != 1 {
		t.Fatalf("expected 1 stored event, got %d", len(stored))
	}
}

func TestHookedSink_QueryDelegates(t *testing.T) {
	dir := t.TempDir()
	inner, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	sink := newHookedSink(inner)
	_ = sink.Write(audit.Event{AgentID: "worker-ada", Kind: audit.KindJobComplete, JobID: "j1"})
	_ = sink.Write(audit.Event{AgentID: "leader", Kind: audit.KindDecisionNote})

	got, err := sink.Query("leader", time.Time{}, 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].AgentID != "leader" {
		t.Errorf("Query filter not delegated: %+v", got)
	}
}
