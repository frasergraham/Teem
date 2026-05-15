package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// seedRestoreCandidate writes a state dir at <stateRoot>/<dirName> with
// a registration.json carrying the supplied yaml body. mtime sets the
// registration.json mtime so dedup-by-most-recent is testable.
func seedRestoreCandidate(t *testing.T, stateRoot, dirName, yamlBody string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(stateRoot, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := teamRegistration{TeamYAML: yamlBody, RegisteredAt: mtime}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(dir, "registration.json")
	if err := os.WriteFile(regPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(regPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return regPath
}

// TestPlanTeamRestores_DedupsSameID is the simplest dedup case: two
// state dirs whose registrations resolve to the same id. Can happen
// when migration found `state/<id>` already present and left the
// legacy slug dir alone — both then get walked at restore.
func TestPlanTeamRestores_DedupsSameID(t *testing.T) {
	_, base := migrationFixture(t)
	stateRoot := filepath.Join(base, "state")
	id := "t-deadbeefcafef00d"
	yaml := `team:
  id: ` + id + `
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	now := time.Now()
	// Canonical dir, newer mtime — should win.
	seedRestoreCandidate(t, stateRoot, id, yaml, now)
	// Stale slug dir with the SAME id in its yaml, older mtime.
	seedRestoreCandidate(t, stateRoot, "alpha", yaml, now.Add(-time.Hour))

	got := planTeamRestores(stateRoot)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	if got[0].team.ID != id {
		t.Errorf("winner id = %q, want %q", got[0].team.ID, id)
	}
	if got[0].dirName != id {
		t.Errorf("winner dirName = %q, want %q (newer reg.mtime should win)", got[0].dirName, id)
	}
}

// TestPlanTeamRestores_DedupsSameNameDistinctIDs is the phantom case:
// two state dirs, distinct ids, same Name. A past partial migration
// minted a phantom while the canonical id-dir already existed; we keep
// only one (most recent reg.mtime) so the daemon doesn't end up with
// two *team.Team entries for what the operator considers one team.
func TestPlanTeamRestores_DedupsSameNameDistinctIDs(t *testing.T) {
	_, base := migrationFixture(t)
	stateRoot := filepath.Join(base, "state")
	canonical := "t-857d9294a3ea3c68"
	phantom := "t-708a353a82401d7c"
	yamlFor := func(id string) string {
		return `team:
  id: ` + id + `
  name: example-team
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	}
	now := time.Now()
	// Canonical: newer reg.mtime — should win.
	seedRestoreCandidate(t, stateRoot, canonical, yamlFor(canonical), now)
	// Phantom: older reg.mtime — should be dropped with a WARN.
	seedRestoreCandidate(t, stateRoot, phantom, yamlFor(phantom), now.Add(-2*time.Hour))

	got := planTeamRestores(stateRoot)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate (phantom must be dropped), got %d: %+v", len(got), got)
	}
	if got[0].team.ID != canonical {
		t.Errorf("winner id = %q, want canonical %q", got[0].team.ID, canonical)
	}
	if got[0].team.Name != "example-team" {
		t.Errorf("winner name = %q, want example-team", got[0].team.Name)
	}
}

// TestPlanTeamRestores_PartialMigrationState is the operator scenario
// from t-8a632a46 verbatim: a canonical id-dir, a phantom id-dir for
// the same Name, AND a legacy slug dir whose yaml carries the canonical
// id (the migration-target-already-exists leftover). Restore must
// collapse all three into a single canonical entry.
func TestPlanTeamRestores_PartialMigrationState(t *testing.T) {
	_, base := migrationFixture(t)
	stateRoot := filepath.Join(base, "state")
	canonical := "t-857d9294a3ea3c68"
	phantom := "t-708a353a82401d7c"
	canonicalYAML := `team:
  id: ` + canonical + `
  name: example-team
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	phantomYAML := `team:
  id: ` + phantom + `
  name: example-team
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	now := time.Now()
	seedRestoreCandidate(t, stateRoot, canonical, canonicalYAML, now)
	seedRestoreCandidate(t, stateRoot, phantom, phantomYAML, now.Add(-time.Hour))
	seedRestoreCandidate(t, stateRoot, "example-team", canonicalYAML, now.Add(-30*time.Minute))

	got := planTeamRestores(stateRoot)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	if got[0].team.ID != canonical {
		t.Errorf("winner id = %q, want canonical %q", got[0].team.ID, canonical)
	}
}

// seedRestoreCandidateWithRepo is seedRestoreCandidate plus a repo_root
// in the registration so candidateAnchored can find the operator's
// teem.yaml. The teem.yaml itself must be written separately by the
// caller (typically with a specific id-or-no-id).
func seedRestoreCandidateWithRepo(t *testing.T, stateRoot, dirName, yamlBody, repoRoot string, mtime time.Time) {
	t.Helper()
	dir := filepath.Join(stateRoot, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := teamRegistration{TeamYAML: yamlBody, RepoRoot: repoRoot, RegisteredAt: mtime}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(dir, "registration.json")
	if err := os.WriteFile(regPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(regPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestPlanTeamRestores_PrefersTeamYamlAnchoredID is the round-2
// regression: in the phantom case, the operator's `<repo>/teem.yaml`
// is the source of truth for which id is canonical — NOT mtime. This
// test reverses round-1's convenient mtime arrangement (phantom is
// NEWER on disk) and asserts canonical still wins because teem.yaml
// names it.
func TestPlanTeamRestores_PrefersTeamYamlAnchoredID(t *testing.T) {
	_, base := migrationFixture(t)
	stateRoot := filepath.Join(base, "state")
	canonical := "t-857d9294a3ea3c68"
	phantom := "t-708a353a82401d7c"
	yamlFor := func(id string) string {
		return `team:
  id: ` + id + `
  name: example-team
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	}
	// Operator's working tree carries the canonical id.
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "teem.yaml"), []byte(yamlFor(canonical)), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Canonical: OLDER mtime — would lose under round-1's pure-mtime rule.
	seedRestoreCandidateWithRepo(t, stateRoot, canonical, yamlFor(canonical), repoRoot, now.Add(-2*time.Hour))
	// Phantom: NEWER mtime + same RepoRoot (typical for a fresh mint
	// triggered by the operator's own teem.yaml). The teem.yaml does
	// NOT carry phantom's id, so it is not anchored.
	seedRestoreCandidateWithRepo(t, stateRoot, phantom, yamlFor(phantom), repoRoot, now)

	got := planTeamRestores(stateRoot)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	if got[0].team.ID != canonical {
		t.Errorf("winner id = %q, want canonical %q (teem.yaml anchor must beat newer mtime)", got[0].team.ID, canonical)
	}
}

// TestPlanTeamRestores_IsIdempotent asserts that calling planTeamRestores
// twice on the same state dir produces identical results AND leaves the
// on-disk state unchanged between calls. The brief explicitly called
// this out as a requirement (running daemon boot twice in a row must
// be a no-op).
func TestPlanTeamRestores_IsIdempotent(t *testing.T) {
	_, base := migrationFixture(t)
	stateRoot := filepath.Join(base, "state")
	yaml := `team:
  id: t-1111111111111111
  name: alpha
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
	now := time.Now()
	seedRestoreCandidate(t, stateRoot, "t-1111111111111111", yaml, now)

	snapshotState := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		entries, err := os.ReadDir(stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p := filepath.Join(stateRoot, e.Name(), "registration.json")
			body, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			out[e.Name()] = string(body)
		}
		return out
	}

	before := snapshotState()
	first := planTeamRestores(stateRoot)
	mid := snapshotState()
	second := planTeamRestores(stateRoot)
	after := snapshotState()

	if len(first) != len(second) {
		t.Fatalf("candidate count drifted: first=%d second=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].team.ID != second[i].team.ID || first[i].dirName != second[i].dirName {
			t.Errorf("candidate[%d] drifted: first=%+v second=%+v", i, first[i], second[i])
		}
	}
	if !reflect.DeepEqual(before, mid) {
		t.Errorf("first call mutated disk state:\nbefore=%v\nafter=%v", before, mid)
	}
	if !reflect.DeepEqual(mid, after) {
		t.Errorf("second call mutated disk state:\nbefore=%v\nafter=%v", mid, after)
	}
}

// TestPlanTeamRestores_DistinctTeamsBothKept is the negative-control:
// two state dirs with distinct ids AND distinct Names must both come
// through. Without this, an over-eager dedup (e.g. by id-prefix or
// dir-name fuzziness) would silently drop real teams.
func TestPlanTeamRestores_DistinctTeamsBothKept(t *testing.T) {
	_, base := migrationFixture(t)
	stateRoot := filepath.Join(base, "state")
	ids := []string{"t-1111111111111111", "t-2222222222222222"}
	names := []string{"alpha", "beta"}
	now := time.Now()
	for i, id := range ids {
		yaml := `team:
  id: ` + id + `
  name: ` + names[i] + `
  leader: {system_prompt: p}
  archetypes:
    - {role: worker, placement: local, max_concurrent: 1}
`
		seedRestoreCandidate(t, stateRoot, id, yaml, now)
	}
	got := planTeamRestores(stateRoot)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(got), got)
	}
	gotIDs := map[string]bool{got[0].team.ID: true, got[1].team.ID: true}
	for _, id := range ids {
		if !gotIDs[id] {
			t.Errorf("missing id %q in restored set %v", id, gotIDs)
		}
	}
}
