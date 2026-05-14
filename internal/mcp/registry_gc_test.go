package mcp

import (
	"testing"
	"time"
)

func TestRegistry_GCStopped_TTLZeroIsNoop(t *testing.T) {
	r := NewRegistry()
	r.Add(AgentEntry{ID: "worker-ada", Role: "worker", State: StateStopped, LastSeen: time.Now().Add(-72 * time.Hour)})
	removed := r.GCStopped(time.Now(), 0)
	if removed != 0 {
		t.Errorf("ttl=0 should be no-op, removed=%d", removed)
	}
	if got := r.List(); len(got) != 1 {
		t.Errorf("entry should still be present, got %d", len(got))
	}
}

func TestRegistry_GCStopped_RemovesOldStopped(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(AgentEntry{ID: "old", Role: "worker", State: StateStopped, LastSeen: now.Add(-48 * time.Hour)})
	r.Add(AgentEntry{ID: "young", Role: "worker", State: StateStopped, LastSeen: now.Add(-30 * time.Minute)})
	r.Add(AgentEntry{ID: "live", Role: "worker", State: StateRunning, LastSeen: now.Add(-48 * time.Hour)})

	removed := r.GCStopped(now, 24*time.Hour)
	if removed != 1 {
		t.Errorf("expected 1 removal, got %d", removed)
	}
	if _, ok := r.Get("old"); ok {
		t.Error("old stopped entry should have been removed")
	}
	if _, ok := r.Get("young"); !ok {
		t.Error("young stopped entry should remain")
	}
	if _, ok := r.Get("live"); !ok {
		t.Error("running entry should always remain regardless of age")
	}
}
