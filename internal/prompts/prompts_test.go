package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/team"
)

func newTestTeam(t *testing.T) *team.Team {
	t.Helper()
	tm := &team.Team{
		Name:   "alpha",
		Leader: team.LeaderSpec{SystemPrompt: "Ship the MVP."},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Description: "Implements features.", Placement: "local", MaxConcurrent: 3},
			{Role: "reviewer", Description: "Reads diffs.", Placement: "local", MaxConcurrent: 2},
		},
	}
	if err := tm.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return tm
}

func TestBuilder_LeaderWithoutOverride(t *testing.T) {
	b := New(newTestTeam(t), t.TempDir())
	got := b.Leader()
	for _, want := range []string{"You are the Leader", "worker", "reviewer", "Ship the MVP."} {
		if !strings.Contains(got, want) {
			t.Errorf("Leader prompt missing %q\n--- got ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "Operator overrides") {
		t.Errorf("Leader prompt should not include override banner when no override file exists")
	}
}

func TestBuilder_LeaderWithOverride(t *testing.T) {
	dir := t.TempDir()
	b := New(newTestTeam(t), dir)
	if err := b.AppendOverride("leader", "Always run go vet before commit."); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := b.Leader()
	for _, want := range []string{"Ship the MVP.", "Operator overrides", "Always run go vet"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestBuilder_ArchetypePromptsCarryDescription(t *testing.T) {
	b := New(newTestTeam(t), t.TempDir())
	got, ok := b.Archetype("worker")
	if !ok {
		t.Fatalf("Archetype(worker): ok=false on a declared role")
	}
	for _, want := range []string{"worker worker", "Implements features.", "Placement: local"} {
		if !strings.Contains(got, want) {
			t.Errorf("Archetype prompt missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestArchetypePrompt_IntegratorIncludesContract asserts that
// spawning an integrator carries the contract + forbidden-ops block.
// The Archetype builder gates this on role == "integrator" — see
// baseArchetype in prompts.go. Regressing the gate (e.g. dropping the
// branch, or moving the block out of baseArchetype) breaks here.
func TestArchetypePrompt_IntegratorIncludesContract(t *testing.T) {
	tm := &team.Team{
		Name:   "alpha",
		Leader: team.LeaderSpec{SystemPrompt: "Ship the MVP."},
		Archetypes: []team.ArchetypeSpec{
			{Role: "worker", Description: "Implements features.", Placement: "local", MaxConcurrent: 3},
			{Role: "integrator", Description: "Merges branches.", Placement: "local", MaxConcurrent: 1},
		},
	}
	if err := tm.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	b := New(tm, t.TempDir())
	got, ok := b.Archetype("integrator")
	if !ok {
		t.Fatalf("Archetype(integrator): ok=false on a declared role")
	}
	for _, want := range []string{
		"Integrator contract",
		"Forbidden git operations",
		"git update-ref",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("integrator prompt missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestArchetypePrompt_WorkerExcludesIntegratorContract is the
// negative guard: the contract + forbidden-ops block must NOT be
// folded into non-integrator archetypes. If baseArchetype's role gate
// regresses (e.g. someone moves the block above the `if role ==
// "integrator"` check), this test catches it.
func TestArchetypePrompt_WorkerExcludesIntegratorContract(t *testing.T) {
	b := New(newTestTeam(t), t.TempDir())
	got, ok := b.Archetype("worker")
	if !ok {
		t.Fatalf("Archetype(worker): ok=false on a declared role")
	}
	for _, banned := range []string{
		"Integrator contract",
		"Forbidden git operations",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("worker prompt unexpectedly contains %q\n--- got ---\n%s", banned, got)
		}
	}
}

func TestBuilder_ArchetypeUnknownRoleSignalsMiss(t *testing.T) {
	b := New(newTestTeam(t), t.TempDir())
	got, ok := b.Archetype("nonexistent")
	if ok {
		t.Errorf("Archetype(nonexistent): ok=true, want false")
	}
	if got != "" {
		t.Errorf("Archetype(nonexistent): got %q, want \"\"", got)
	}
}

func TestBuilder_AppendOverride_AtomicAndHistoric(t *testing.T) {
	dir := t.TempDir()
	b := New(newTestTeam(t), dir)
	if err := b.AppendOverride("worker", "first note"); err != nil {
		t.Fatalf("append1: %v", err)
	}
	if err := b.AppendOverride("worker", "second note"); err != nil {
		t.Fatalf("append2: %v", err)
	}
	body, ok, err := b.Override("worker")
	if err != nil || !ok {
		t.Fatalf("override: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(body, "first note") || !strings.Contains(body, "second note") {
		t.Errorf("override body should preserve both appends; got:\n%s", body)
	}
	headers := strings.Count(body, "## Appended ")
	if headers != 2 {
		t.Errorf("expected 2 timestamped headers, got %d", headers)
	}
	// No stale tmp left behind.
	if _, err := os.Stat(filepath.Join(dir, "worker.md.tmp")); !os.IsNotExist(err) {
		t.Errorf("tmp file should not exist after successful append: %v", err)
	}
}

func TestBuilder_OverridePath_Shape(t *testing.T) {
	dir := t.TempDir()
	b := New(newTestTeam(t), dir)
	got := b.OverridePath("reviewer")
	want := filepath.Join(dir, "reviewer.md")
	if got != want {
		t.Errorf("OverridePath: got %q want %q", got, want)
	}
	// Path traversal must be rejected.
	if p := b.OverridePath("../etc/passwd"); p != "" {
		t.Errorf("OverridePath should reject traversal; got %q", p)
	}
	if p := b.OverridePath("with/slash"); p != "" {
		t.Errorf("OverridePath should reject slash; got %q", p)
	}
}

func TestBuilder_LeaderRoleIsDistinctFromArchetypes(t *testing.T) {
	dir := t.TempDir()
	b := New(newTestTeam(t), dir)
	if err := b.AppendOverride("leader", "leader-only note"); err != nil {
		t.Fatalf("leader append: %v", err)
	}
	if err := b.AppendOverride("worker", "worker-only note"); err != nil {
		t.Fatalf("worker append: %v", err)
	}
	lead := b.Leader()
	wkr, ok := b.Archetype("worker")
	if !ok {
		t.Fatalf("Archetype(worker): ok=false")
	}
	if !strings.Contains(lead, "leader-only note") || strings.Contains(lead, "worker-only note") {
		t.Errorf("leader prompt should contain only the leader override; got:\n%s", lead)
	}
	if !strings.Contains(wkr, "worker-only note") || strings.Contains(wkr, "leader-only note") {
		t.Errorf("worker prompt should contain only the worker override; got:\n%s", wkr)
	}
}

func TestBuilder_AppendOverride_RejectsBadRole(t *testing.T) {
	b := New(newTestTeam(t), t.TempDir())
	if err := b.AppendOverride("../oops", "x"); err == nil {
		t.Errorf("expected error for traversal role")
	}
	if err := b.AppendOverride("", "x"); err == nil {
		t.Errorf("expected error for empty role")
	}
	if err := b.AppendOverride("worker", "   "); err == nil {
		t.Errorf("expected error for whitespace-only text")
	}
}

func TestBuilder_Override_MissingFileIsNotAnError(t *testing.T) {
	b := New(newTestTeam(t), t.TempDir())
	body, ok, err := b.Override("worker")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok || body != "" {
		t.Errorf("expected (\"\", false, nil); got (%q, %v)", body, ok)
	}
}

func TestValidateRole_Grammar(t *testing.T) {
	good := []string{"worker", "reviewer", "worker-pm", "leader", "a", "a1", "a_b", "x-y-z"}
	bad := []string{"", "Worker", "1abc", "with space", "a/b", "-leading-dash", "_leading-underscore", "a..b"}
	for _, s := range good {
		if err := ValidateRole(s); err != nil {
			t.Errorf("ValidateRole(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateRole(s); err == nil {
			t.Errorf("ValidateRole(%q) = nil, want error", s)
		}
	}
}

// TestValidateRole_AgreesWithArchmem locks the prompts and archmem
// role-slug grammars together: any role accepted by one must be
// accepted by the other, and any rejected by one must be rejected by
// the other. Future drift in either package's regex breaks here.
func TestValidateRole_AgreesWithArchmem(t *testing.T) {
	cases := []string{
		"worker", "reviewer", "worker-pm", "leader", "a", "a1", "a_b", "x-y-z",
		"", "Worker", "1abc", "with space", "a/b", "-leading-dash",
		"_leading-underscore", "a..b", "WORKER", "worker pm",
	}
	for _, s := range cases {
		promptsOK := ValidateRole(s) == nil
		archmemOK := archmem.IsValidRoleName(s)
		if promptsOK != archmemOK {
			t.Errorf("role %q: prompts=%v archmem=%v — validators have diverged", s, promptsOK, archmemOK)
		}
	}
}

func TestBuilder_NoOverrideDir(t *testing.T) {
	b := New(newTestTeam(t), "")
	got := b.Leader()
	if !strings.Contains(got, "Ship the MVP.") {
		t.Errorf("Leader still works without override dir: %s", got)
	}
	if err := b.AppendOverride("worker", "x"); err == nil {
		t.Errorf("AppendOverride should error when overrideDir is unset")
	}
}
