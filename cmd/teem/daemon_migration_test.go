package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frasergraham/teem/internal/team"
)

// migrationFixture sets up a temp HOME and a ~/.teem tree, then returns
// the .teem base dir migrateLegacyTeamDirsIn operates against. All
// downstream helpers (writeTeamRegistration, defaultStateDir) read HOME
// too, so they land in the same tree.
func migrationFixture(t *testing.T) (home, base string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	base = filepath.Join(home, ".teem")
	for _, sub := range []string{"state", "audit", "worktrees"} {
		if err := os.MkdirAll(filepath.Join(base, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home, base
}

// seedLegacyTeam materialises a pre-T33 layout: a slug-keyed state
// dir with a registration.json holding the supplied yaml body, plus
// matching audit/worktrees dirs (each containing a sentinel file so we
// can confirm the rename moved the contents, not just an empty dir).
func seedLegacyTeam(t *testing.T, base, slug, yamlBody string) {
	t.Helper()
	state := filepath.Join(base, "state", slug)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := teamRegistration{TeamYAML: yamlBody}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "registration.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	audit := filepath.Join(base, "audit", slug)
	if err := os.MkdirAll(audit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(audit, "audit.jsonl"), []byte("{\"e\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(base, "worktrees", slug, "worker-x")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "marker"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateLegacyTeamDirs_SkipsCanonical confirms idempotency: a
// state dir whose name is already a canonical t-<hex> id is left
// alone. Without this, a daemon restart would try to re-mint each
// boot and silently churn the tree.
func TestMigrateLegacyTeamDirs_SkipsCanonical(t *testing.T) {
	_, base := migrationFixture(t)
	id := "t-deadbeef00112233"
	canonicalDir := filepath.Join(base, "state", id)
	if err := os.MkdirAll(canonicalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalDir, "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	migrateLegacyTeamDirsIn(base)

	if _, err := os.Stat(canonicalDir); err != nil {
		t.Errorf("canonical state dir disappeared: %v", err)
	}
}

// TestMigrateLegacyTeamDirs_MintsForLegacy is the happy path: a slug
// dir with a registration whose YAML has no `id:` gets an id minted,
// state/audit/worktree dirs are renamed under it, and the new
// registration.json holds yaml carrying the same id back-filled.
func TestMigrateLegacyTeamDirs_MintsForLegacy(t *testing.T) {
	_, base := migrationFixture(t)
	yamlBody := `team:
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	seedLegacyTeam(t, base, "alpha", yamlBody)

	migrateLegacyTeamDirsIn(base)

	// Locate the new id by scanning state/.
	entries, err := os.ReadDir(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	var newID string
	for _, e := range entries {
		if team.IsCanonicalID(e.Name()) {
			newID = e.Name()
		}
	}
	if newID == "" {
		t.Fatalf("no canonical-id state dir after migration: %v", entries)
	}

	// Legacy state dir gone, new dir present.
	if _, err := os.Stat(filepath.Join(base, "state", "alpha")); !os.IsNotExist(err) {
		t.Errorf("legacy state dir still present: %v", err)
	}

	// Audit + worktrees moved.
	if _, err := os.Stat(filepath.Join(base, "audit", newID, "audit.jsonl")); err != nil {
		t.Errorf("audit not migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "worktrees", newID, "worker-x", "marker")); err != nil {
		t.Errorf("worktree not migrated: %v", err)
	}

	// Registration.json under the new id carries id-bearing yaml.
	regBody, err := os.ReadFile(filepath.Join(base, "state", newID, "registration.json"))
	if err != nil {
		t.Fatalf("read new registration: %v", err)
	}
	var reg teamRegistration
	if err := json.Unmarshal(regBody, &reg); err != nil {
		t.Fatalf("parse new registration: %v", err)
	}
	if !strings.Contains(reg.TeamYAML, "id: "+newID) {
		t.Errorf("migrated registration.json YAML missing id: %s", reg.TeamYAML)
	}
}

// TestMigrateLegacyTeamDirs_UsesExistingID confirms that when the
// stored yaml ALREADY carries an `id:`, the migration reuses it
// instead of minting a fresh one. (EnsureIDFile is the line that
// guarantees this; the test exists so a future "always mint" tweak
// can't sneak in.)
func TestMigrateLegacyTeamDirs_UsesExistingID(t *testing.T) {
	_, base := migrationFixture(t)
	existing := "t-cafebabecafebabe"
	yamlBody := `team:
  id: ` + existing + `
  name: beta
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	seedLegacyTeam(t, base, "beta", yamlBody)

	migrateLegacyTeamDirsIn(base)

	if _, err := os.Stat(filepath.Join(base, "state", existing)); err != nil {
		t.Errorf("state dir not renamed to existing id %q: %v", existing, err)
	}
	if _, err := os.Stat(filepath.Join(base, "state", "beta")); !os.IsNotExist(err) {
		t.Errorf("legacy state dir lingered: %v", err)
	}
}

// TestMigrateLegacyTeamDirs_SkipsMissingRegistration covers the
// orphan case: a state dir without a registration.json must not crash
// the daemon — it's likely leftover scratch and the migration should
// log + continue.
func TestMigrateLegacyTeamDirs_SkipsMissingRegistration(t *testing.T) {
	_, base := migrationFixture(t)
	orphan := filepath.Join(base, "state", "orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}

	// Must not panic.
	migrateLegacyTeamDirsIn(base)

	// Dir is left as-is for the operator to clean up.
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("orphan dir disappeared: %v", err)
	}
}

// TestMigrateLegacyTeamDirs_HandlesPartialOnRerun is the
// state-as-marker check. We migrate once happily, then re-run; the
// canonical id dir is now present and migration must skip without
// touching it. This is the regression guard for "migration re-mints
// on every boot."
func TestMigrateLegacyTeamDirs_HandlesPartialOnRerun(t *testing.T) {
	_, base := migrationFixture(t)
	yamlBody := `team:
  name: gamma
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	seedLegacyTeam(t, base, "gamma", yamlBody)

	migrateLegacyTeamDirsIn(base)
	migrateLegacyTeamDirsIn(base) // second call must be a no-op

	entries, err := os.ReadDir(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	canonical := 0
	for _, e := range entries {
		if team.IsCanonicalID(e.Name()) {
			canonical++
		}
		if e.Name() == "gamma" {
			t.Errorf("legacy state dir came back on re-run")
		}
	}
	if canonical != 1 {
		t.Errorf("want 1 canonical state dir, got %d: %v", canonical, entries)
	}
}
