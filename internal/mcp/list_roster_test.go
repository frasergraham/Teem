package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/team"
)

// rosterWire mirrors the inline wire struct in handleListRoster so
// tests can assert the JSON shape exactly.
type rosterWire struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	FirstSeen time.Time `json:"first_seen,omitempty"`
	LastSeen  time.Time `json:"last_seen"`
	InUse     bool      `json:"in_use"`
	Source    string    `json:"source,omitempty"`
}

func TestListRoster_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	entries := []roster.Entry{
		{ID: "worker-ada", Role: "worker", InUse: true, FirstSeen: now, LastUsedAt: now, Source: roster.SourceWordlist},
		{ID: "worker-blake", Role: "worker", InUse: false, FirstSeen: now, LastUsedAt: now, Source: roster.SourceWordlist},
		{ID: "reviewer-cleo", Role: "reviewer", InUse: false, FirstSeen: now, LastUsedAt: now, Source: roster.SourceWordlist},
		{ID: "bob", Role: "reviewer", InUse: false, FirstSeen: now, LastUsedAt: now, Source: roster.SourceNamed},
	}
	sp := &fakeSpawner{
		running: map[string]bool{"worker-ada": true},
		roles:   map[string]string{"worker-ada": "worker"},
		roster:  entries,
	}
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	srv := newTestServer(t, tm, sp)
	// Mark worker-ada running in the live registry so handleListRoster
	// derives in_use=true. The fake's IsRunning isn't consulted —
	// list_roster reads s.registry directly.
	srv.registry.Add(AgentEntry{ID: "worker-ada", Role: "worker", State: StateRunning})

	res := callTool(t, srv, "list_roster", map[string]any{})
	if resultIsError(res) {
		t.Fatalf("list_roster failed: %s", resultText(t, res))
	}
	var got []rosterWire
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d: %+v", len(got), got)
	}
	byName := map[string]rosterWire{}
	for _, e := range got {
		byName[e.Name] = e
	}
	if e := byName["ada"]; e.Role != "worker" || !e.InUse || e.Source != roster.SourceWordlist {
		t.Errorf("ada entry wrong: %+v", e)
	}
	if e := byName["blake"]; e.Role != "worker" || e.InUse {
		t.Errorf("blake entry wrong: %+v", e)
	}
	if e := byName["cleo"]; e.Role != "reviewer" || e.InUse {
		t.Errorf("cleo entry wrong: %+v", e)
	}
	// bob is operator-named; the role prefix isn't stripped because
	// the id doesn't start with "<role>-".
	if e := byName["bob"]; e.Role != "reviewer" || e.Source != roster.SourceNamed {
		t.Errorf("bob entry wrong: %+v", e)
	}
}

func TestListRoster_FilterByRole(t *testing.T) {
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	entries := []roster.Entry{
		{ID: "worker-ada", Role: "worker", FirstSeen: now, LastUsedAt: now, Source: roster.SourceWordlist},
		{ID: "worker-blake", Role: "worker", FirstSeen: now, LastUsedAt: now, Source: roster.SourceWordlist},
		{ID: "reviewer-cleo", Role: "reviewer", FirstSeen: now, LastUsedAt: now, Source: roster.SourceWordlist},
		{ID: "reviewer-dale", Role: "reviewer", FirstSeen: now, LastUsedAt: now, Source: roster.SourceWordlist},
	}
	sp := &fakeSpawner{
		running: map[string]bool{},
		roles:   map[string]string{},
		roster:  entries,
	}
	tm := &team.Team{Name: "t", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	srv := newTestServer(t, tm, sp)

	res := callTool(t, srv, "list_roster", map[string]any{"role": "worker"})
	if resultIsError(res) {
		t.Fatalf("list_roster failed: %s", resultText(t, res))
	}
	var got []rosterWire
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 worker entries, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Role != "worker" {
			t.Errorf("filter leaked non-worker entry: %+v", e)
		}
	}
}
