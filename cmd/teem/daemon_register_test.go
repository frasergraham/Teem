package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/team"
)

// reregisterFixture sets HOME so writeTeamRegistration / persistStateSnapshot
// land under a temp dir, then returns a daemon pre-seeded with one
// registered team. Only the .team field of registeredTeam is populated —
// the early-return branch in handleRegister doesn't touch the per-team
// services, so spawner/audit/plan/etc. stay nil.
func reregisterFixture(t *testing.T, initialYAML string) (*daemon, *team.Team, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	tmp := filepath.Join(home, "initial.yaml")
	if err := os.WriteFile(tmp, []byte(initialYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	tt, err := team.Load(tmp)
	if err != nil {
		t.Fatalf("load initial: %v", err)
	}
	if tt.ID == "" {
		id, err := team.EnsureIDFile(tmp)
		if err != nil {
			t.Fatalf("ensure id: %v", err)
		}
		tt.ID = id
		// Re-read so the canonical YAML body carries the minted id.
		body, _ := os.ReadFile(tmp)
		initialYAML = string(body)
	}

	d := &daemon{
		teams:    map[string]*registeredTeam{},
		endpoint: "http://test.local:0",
	}
	d.teams[tt.ID] = &registeredTeam{
		team:       tt,
		registered: time.Now(),
	}
	// Persist the initial registration so on-disk state mirrors the
	// in-memory team, matching what handleRegister would have done on
	// first registration.
	if err := writeTeamRegistration(tt.ID, teamRegistration{
		TeamYAML:     initialYAML,
		RegisteredAt: d.teams[tt.ID].registered,
	}); err != nil {
		t.Fatalf("seed registration: %v", err)
	}
	return d, tt, initialYAML
}

// reregister POSTs a register request via the handler and returns the
// recorder so the caller can assert on status / body.
func reregister(t *testing.T, d *daemon, yamlBody string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(registerRequest{TeamYAML: yamlBody})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/control/teams", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	d.handleRegister(rr, req)
	return rr
}

func readRegistration(t *testing.T, teamID string) teamRegistration {
	t.Helper()
	body, err := os.ReadFile(defaultRegistrationPath(teamID))
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	var reg teamRegistration
	if err := json.Unmarshal(body, &reg); err != nil {
		t.Fatalf("parse registration: %v", err)
	}
	return reg
}

// TestHandleRegister_RefreshesNameOnRereg verifies the headline bug:
// the operator edits `name:` and re-registers; the in-memory team and
// on-disk YAML both reflect the new name.
func TestHandleRegister_RefreshesNameOnRereg(t *testing.T) {
	id := "t-aaaaaaaaaaaaaaaa"
	initial := `team:
  id: ` + id + `
  name: example-team
  leader: {system_prompt: hello}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	d, tt, _ := reregisterFixture(t, initial)

	updated := strings.Replace(initial, "name: example-team", "name: Teem", 1)
	rr := reregister(t, d, updated)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := d.teams[tt.ID].team.Name; got != "Teem" {
		t.Errorf("in-memory Name = %q, want %q", got, "Teem")
	}
	reg := readRegistration(t, tt.ID)
	if !strings.Contains(reg.TeamYAML, "name: Teem") {
		t.Errorf("registration.json missing new name:\n%s", reg.TeamYAML)
	}
}

// TestHandleRegister_LeaderPromptRefresh confirms Leader.SystemPrompt
// is part of the safe-display set and refreshes the same way Name does.
func TestHandleRegister_LeaderPromptRefresh(t *testing.T) {
	id := "t-bbbbbbbbbbbbbbbb"
	initial := `team:
  id: ` + id + `
  name: alpha
  leader: {system_prompt: old}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	d, tt, _ := reregisterFixture(t, initial)

	updated := strings.Replace(initial, "system_prompt: old", "system_prompt: new", 1)
	rr := reregister(t, d, updated)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := d.teams[tt.ID].team.Leader.SystemPrompt; got != "new" {
		t.Errorf("in-memory Leader.SystemPrompt = %q, want %q", got, "new")
	}
	reg := readRegistration(t, tt.ID)
	if !strings.Contains(reg.TeamYAML, "system_prompt: new") {
		t.Errorf("registration.json missing new prompt:\n%s", reg.TeamYAML)
	}
}

// TestHandleRegister_StructuralChangeWarnsButDoesNotApply seeds two
// archetypes, re-registers with a third added, and asserts:
//   - stderr carries the structural-change warning
//   - in-memory archetype list is unchanged (restart required)
//   - registration.json reflects the operator's new YAML so the next
//     daemon bounce picks it up
func TestHandleRegister_StructuralChangeWarnsButDoesNotApply(t *testing.T) {
	id := "t-cccccccccccccccc"
	initial := `team:
  id: ` + id + `
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
    - {role: reviewer, placement: local, max_concurrent: 1}
`
	d, tt, _ := reregisterFixture(t, initial)
	beforeArchCount := len(d.teams[tt.ID].team.Archetypes)

	updated := `team:
  id: ` + id + `
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
    - {role: reviewer, placement: local, max_concurrent: 1}
    - {role: integrator, placement: local, max_concurrent: 1}
`
	stderr := captureStderr(t, func() {
		rr := reregister(t, d, updated)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	})
	if !strings.Contains(stderr, "structural changes detected") {
		t.Errorf("stderr missing warning:\n%s", stderr)
	}
	if !strings.Contains(stderr, "integrator") {
		t.Errorf("stderr missing added role name:\n%s", stderr)
	}
	if got := len(d.teams[tt.ID].team.Archetypes); got != beforeArchCount {
		t.Errorf("in-memory archetype count = %d, want unchanged %d", got, beforeArchCount)
	}
	reg := readRegistration(t, tt.ID)
	if !strings.Contains(reg.TeamYAML, "role: integrator") {
		t.Errorf("registration.json missing the added archetype:\n%s", reg.TeamYAML)
	}
}

// TestHandleRegister_NoChangesIsStillIdempotent posts the same YAML
// twice (already-registered + identical re-register) and asserts no
// warning fires and the in-memory team is bit-identical.
func TestHandleRegister_NoChangesIsStillIdempotent(t *testing.T) {
	id := "t-dddddddddddddddd"
	initial := `team:
  id: ` + id + `
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	d, tt, normalised := reregisterFixture(t, initial)
	beforeName := d.teams[tt.ID].team.Name
	beforePrompt := d.teams[tt.ID].team.Leader.SystemPrompt
	beforeArchCount := len(d.teams[tt.ID].team.Archetypes)

	stderr := captureStderr(t, func() {
		rr := reregister(t, d, normalised)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	})
	if strings.Contains(stderr, "structural changes detected") {
		t.Errorf("unexpected warning on no-op re-register:\n%s", stderr)
	}
	if got := d.teams[tt.ID].team.Name; got != beforeName {
		t.Errorf("Name drifted: %q → %q", beforeName, got)
	}
	if got := d.teams[tt.ID].team.Leader.SystemPrompt; got != beforePrompt {
		t.Errorf("Leader.SystemPrompt drifted: %q → %q", beforePrompt, got)
	}
	if got := len(d.teams[tt.ID].team.Archetypes); got != beforeArchCount {
		t.Errorf("archetype count drifted: %d → %d", beforeArchCount, got)
	}
}

func TestSafeReregisterDelta_TableDriven(t *testing.T) {
	mkTeam := func(name, prompt string, archs []team.ArchetypeSpec, tracker *team.TrackerConfig, tnet team.TailnetSpec) *team.Team {
		return &team.Team{
			ID:         "t-0000000000000001",
			Name:       name,
			Leader:     team.LeaderSpec{SystemPrompt: prompt},
			Archetypes: archs,
			Tracker:    tracker,
			Tailnet:    tnet,
		}
	}
	baseArchs := []team.ArchetypeSpec{
		{Role: "worker", Placement: "local", MaxConcurrent: 1},
	}
	twoArchs := []team.ArchetypeSpec{
		{Role: "worker", Placement: "local", MaxConcurrent: 1},
		{Role: "reviewer", Placement: "local", MaxConcurrent: 1},
	}
	bumpedArchs := []team.ArchetypeSpec{
		{Role: "worker", Placement: "local", MaxConcurrent: 3},
	}

	cases := []struct {
		name           string
		existing       *team.Team
		fresh          *team.Team
		wantDisplay    bool
		wantStructural []string // substrings that MUST appear in the joined output
	}{
		{
			name:        "identical",
			existing:    mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			fresh:       mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			wantDisplay: false,
		},
		{
			name:        "name only",
			existing:    mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			fresh:       mkTeam("b", "p", baseArchs, nil, team.TailnetSpec{}),
			wantDisplay: true,
		},
		{
			name:        "prompt only",
			existing:    mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			fresh:       mkTeam("a", "q", baseArchs, nil, team.TailnetSpec{}),
			wantDisplay: true,
		},
		{
			name:           "archetype add",
			existing:       mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			fresh:          mkTeam("a", "p", twoArchs, nil, team.TailnetSpec{}),
			wantStructural: []string{"archetypes changed", "added reviewer"},
		},
		{
			name:           "archetype remove",
			existing:       mkTeam("a", "p", twoArchs, nil, team.TailnetSpec{}),
			fresh:          mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			wantStructural: []string{"archetypes changed", "removed reviewer"},
		},
		{
			name:           "max_concurrent change",
			existing:       mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			fresh:          mkTeam("a", "p", bumpedArchs, nil, team.TailnetSpec{}),
			wantStructural: []string{"archetypes changed", "worker.max_concurrent 1→3"},
		},
		{
			name:           "tracker add",
			existing:       mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			fresh:          mkTeam("a", "p", baseArchs, &team.TrackerConfig{Type: "linear", TeamID: "ENG"}, team.TailnetSpec{}),
			wantStructural: []string{"tracker added"},
		},
		{
			name:           "tracker field change",
			existing:       mkTeam("a", "p", baseArchs, &team.TrackerConfig{Type: "linear", TeamID: "ENG"}, team.TailnetSpec{}),
			fresh:          mkTeam("a", "p", baseArchs, &team.TrackerConfig{Type: "linear", TeamID: "BUG"}, team.TailnetSpec{}),
			wantStructural: []string{"tracker changed", "team_id"},
		},
		{
			name:           "tailnet auth_key_env change",
			existing:       mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{AuthKeyEnv: "OLD_KEY"}),
			fresh:          mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{AuthKeyEnv: "NEW_KEY"}),
			wantStructural: []string{"tailnet changed"},
		},
		{
			name:        "tailnet hostname auto-derived ignored",
			existing:    mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{Hostname: "a"}),
			fresh:       mkTeam("b", "p", baseArchs, nil, team.TailnetSpec{Hostname: "b"}),
			wantDisplay: true,
			// no structural — Hostname diff alone is not flagged
		},
		{
			name:           "combination safe + structural",
			existing:       mkTeam("a", "p", baseArchs, nil, team.TailnetSpec{}),
			fresh:          mkTeam("b", "q", twoArchs, nil, team.TailnetSpec{}),
			wantDisplay:    true,
			wantStructural: []string{"archetypes changed", "added reviewer"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			display, structural := safeReregisterDelta(c.existing, c.fresh)
			if display != c.wantDisplay {
				t.Errorf("displayChanged = %v, want %v", display, c.wantDisplay)
			}
			joined := strings.Join(structural, "; ")
			if len(c.wantStructural) == 0 {
				if len(structural) != 0 {
					t.Errorf("structural = %v, want empty", structural)
				}
				return
			}
			for _, want := range c.wantStructural {
				if !strings.Contains(joined, want) {
					t.Errorf("structural missing %q: %s", want, joined)
				}
			}
		})
	}
}
