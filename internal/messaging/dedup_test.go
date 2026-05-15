package messaging

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDedup_WithinWindow(t *testing.T) {
	d, err := NewDedup(filepath.Join(t.TempDir(), "ded.json"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return now })

	msg := Message{TeamID: "foo", TaskID: "t-1", Severity: SeverityAction}
	if !d.Allow(msg) {
		t.Fatal("first fire should be allowed")
	}
	// Same key 5 minutes later: still inside window.
	d.SetClock(func() time.Time { return now.Add(5 * time.Minute) })
	if d.Allow(msg) {
		t.Fatal("second fire inside window should be blocked")
	}
}

func TestDedup_OutsideWindow(t *testing.T) {
	d, err := NewDedup(filepath.Join(t.TempDir(), "ded.json"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return now })
	msg := Message{TeamID: "foo", TaskID: "t-1", Severity: SeverityAction}
	if !d.Allow(msg) {
		t.Fatal("first fire allowed")
	}
	d.SetClock(func() time.Time { return now.Add(11 * time.Minute) })
	if !d.Allow(msg) {
		t.Fatal("re-fire outside window should pass")
	}
}

func TestDedup_SeverityEscalation(t *testing.T) {
	d, _ := NewDedup(filepath.Join(t.TempDir(), "ded.json"), 10*time.Minute)
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return now })

	// decision then action for the same task within the window — both
	// must fire because severity is part of the dedup key.
	dec := Message{TeamID: "foo", TaskID: "t-1", Severity: SeverityDecision}
	act := Message{TeamID: "foo", TaskID: "t-1", Severity: SeverityAction}
	if !d.Allow(dec) {
		t.Fatal("decision fire")
	}
	if !d.Allow(act) {
		t.Fatal("action fire after decision should pass")
	}
}

func TestDedup_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ded.json")
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

	d1, _ := NewDedup(path, 10*time.Minute)
	d1.SetClock(func() time.Time { return now })
	msg := Message{TeamID: "foo", TaskID: "t-1", Severity: SeverityAction}
	if !d1.Allow(msg) {
		t.Fatal("first fire")
	}

	// Re-open: state should be loaded from disk; same key in-window
	// should still be blocked.
	d2, err := NewDedup(path, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d2.SetClock(func() time.Time { return now.Add(2 * time.Minute) })
	if d2.Allow(msg) {
		t.Fatal("after reopen, in-window same key should be blocked")
	}
}
