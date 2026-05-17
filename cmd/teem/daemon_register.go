package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/team"
)

// --- /control/teams handlers ----------------------------------------------

type registerRequest struct {
	TeamYAML     string `json:"team_yaml"`
	RepoRoot     string `json:"repo_root,omitempty"`
	WorktreeBase string `json:"worktree_base,omitempty"`
}

// teamRegistration is the on-disk snapshot the daemon uses to rebuild
// a team after a restart. Lives at ~/.teem/state/<team-id>/registration.json.
type teamRegistration struct {
	TeamYAML     string    `json:"team_yaml"`
	RepoRoot     string    `json:"repo_root,omitempty"`
	WorktreeBase string    `json:"worktree_base,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
}

func writeTeamRegistration(teamID string, reg teamRegistration) error {
	path := defaultRegistrationPath(teamID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func removeTeamRegistration(teamID string) {
	_ = os.Remove(defaultRegistrationPath(teamID))
}

// migrateLegacyTeamDirs is the pre-T33 → T33 migration: walk every
// per-team state dir under ~/.teem/state and, when it doesn't already
// look like a `t-<hex>` id directory, mint an id and rename the state
// / audit / worktree dirs to use it. Idempotent: a re-run skips any
// dir already in the canonical form.
//
// The mint also writes the new id back into the registration.json
// TeamYAML body so the next daemon load picks it up cleanly, and
// (best-effort) into the operator's teem.yaml at repo_root if it
// exists and is writable. A failure on the operator's yaml is logged
// and the migration continues — the in-memory id still works for the
// current run.
func migrateLegacyTeamDirs(home string) {
	migrateLegacyTeamDirsIn(filepath.Join(home, ".teem"))
}

// migrateLegacyTeamDirsIn is the testable form of migrateLegacyTeamDirs:
// it walks state/audit/worktrees under an explicit base dir. The home
// shim above just calls this with `<home>/.teem`.
//
// Partial-failure recovery: audit and worktrees rename first
// (best-effort, log on failure but continue). State renames last and
// is the canonical marker — if `state/<id>` exists, the team is
// considered migrated. A failed audit/worktree rename strands those
// dirs under the legacy slug, but the consumer paths (defaultAuditPath,
// defaultWorktreeBase) are keyed by ID; the strand just means audit
// history / worktrees aren't visible at the new id. We log a warning
// rather than crash, so the daemon still boots; a re-run of the
// migration will see `state/<id>` already canonical and skip.
func migrateLegacyTeamDirsIn(base string) {
	stateRoot := filepath.Join(base, "state")
	auditRoot := filepath.Join(base, "audit")
	worktreesRoot := filepath.Join(base, "worktrees")

	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		oldName := e.Name()
		if team.IsCanonicalID(oldName) {
			continue // already migrated
		}
		regPath := filepath.Join(stateRoot, oldName, "registration.json")
		body, err := os.ReadFile(regPath)
		if err != nil {
			// State dir without a registration — likely orphaned. Skip.
			continue
		}
		var reg teamRegistration
		if err := json.Unmarshal(body, &reg); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (bad registration.json: %v)\n", oldName, err)
			continue
		}

		// Mint via EnsureIDFile against a temp copy so we can both
		// (a) get the id, and (b) capture the rewritten YAML body to
		// re-persist into registration.json. EnsureIDFile reuses an
		// existing id in the YAML if present, so a yaml that already
		// has `id:` doesn't get a fresh one.
		tmpFile, err := writeTempYAML(reg.TeamYAML)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (temp yaml: %v)\n", oldName, err)
			continue
		}
		newID, err := team.EnsureIDFile(tmpFile)
		if err != nil {
			_ = os.Remove(tmpFile)
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (mint id: %v)\n", oldName, err)
			continue
		}
		updated, _ := os.ReadFile(tmpFile)
		_ = os.Remove(tmpFile)
		reg.TeamYAML = string(updated)

		newStateDir := filepath.Join(stateRoot, newID)
		if _, err := os.Stat(newStateDir); err == nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: skip %s (target %s already exists)\n", oldName, newID)
			continue
		}

		// Rename audit + worktrees FIRST (each best-effort: a missing
		// source is fine; a failed rename logs a warning and continues
		// rather than aborting — state's rename is the canonical
		// migration marker). This ordering means a crash between the
		// first two and the state rename leaves both legacy and the
		// in-progress state dir intact, so a re-run can complete.
		oldAudit := filepath.Join(auditRoot, oldName)
		newAudit := filepath.Join(auditRoot, newID)
		if _, err := os.Stat(oldAudit); err == nil {
			if rerr := os.Rename(oldAudit, newAudit); rerr != nil {
				fmt.Fprintf(os.Stderr, "[teemd] migration: rename audit %s -> %s: %v (stranded under legacy slug; not fatal)\n", oldName, newID, rerr)
			} else {
				fmt.Fprintf(os.Stderr, "[teemd] migrated audit dir: %s -> %s\n", oldAudit, newAudit)
			}
		}

		oldWT := filepath.Join(worktreesRoot, oldName)
		newWT := filepath.Join(worktreesRoot, newID)
		if _, err := os.Stat(oldWT); err == nil {
			if rerr := os.Rename(oldWT, newWT); rerr != nil {
				fmt.Fprintf(os.Stderr, "[teemd] migration: rename worktrees %s -> %s: %v (stranded under legacy slug; not fatal)\n", oldName, newID, rerr)
			} else {
				fmt.Fprintf(os.Stderr, "[teemd] migrated worktree dir: %s -> %s\n", oldWT, newWT)
			}
		}

		// State LAST: this is the canonical marker — if this rename
		// succeeds, the team is considered migrated and a re-run skips
		// it. If it fails, audit/worktrees are still under the legacy
		// slug AND state is too, so a future re-run starts fresh.
		if err := os.Rename(filepath.Join(stateRoot, oldName), newStateDir); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: rename state %s -> %s: %v (migration aborted for this team)\n", oldName, newID, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[teemd] migrated team to id %s: %s -> %s\n", newID, filepath.Join(stateRoot, oldName), newStateDir)

		// Write the id-bearing YAML back into the new registration.json.
		if werr := writeTeamRegistration(newID, reg); werr != nil {
			fmt.Fprintf(os.Stderr, "[teemd] migration: write new registration.json for %s: %v\n", newID, werr)
		}

		// Best-effort: also back-fill the SAME minted id into the
		// operator's teem.yaml at repo_root so the next `teem chat`
		// from that working tree reuses the migrated state instead of
		// minting a fresh id and stranding it.
		if reg.RepoRoot != "" {
			candidate := filepath.Join(reg.RepoRoot, "teem.yaml")
			if _, err := os.Stat(candidate); err == nil {
				if werr := team.SetIDFile(candidate, newID); werr != nil {
					fmt.Fprintf(os.Stderr, "[teemd] migration: could not back-fill %s: %v (id-only state dir migrated)\n", candidate, werr)
				}
			}
		}
	}
}

// restoreTeams rebuilds every team that has a registration.json on
// disk. Best-effort: a corrupt file or a YAML that no longer parses
// logs and continues — we'd rather serve N-1 teams than refuse to
// start. Called once at daemon boot, before serving HTTP.
//
// Runs the pre-T33 slug→id migration first so the rest of this
// function only sees id-keyed directories.
func (d *daemon) restoreTeams() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	migrateLegacyTeamDirs(home)
	stateRoot := filepath.Join(home, ".teem", "state")
	for _, c := range planTeamRestores(stateRoot) {
		// Append the synthesised project_manager archetype if the
		// team is wired to a tracker. Best-effort: if the role
		// already exists in the YAML (operator added it manually)
		// AddArchetype returns ErrArchetypeExists and we skip.
		if pm := team.MaybePMArchetype(c.team); pm != nil {
			if err := c.team.AddArchetype(*pm); err != nil && !errors.Is(err, team.ErrArchetypeExists) {
				fmt.Fprintf(os.Stderr, "[teemd] %s: append project_manager: %v\n", c.team.Name, err)
			}
		}
		rt, err := d.buildTeamServices(c.team, c.reg.RepoRoot, c.reg.WorktreeBase)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: build services: %v\n", c.dirName, err)
			continue
		}
		// Preserve the original registration time so the dashboard
		// doesn't show "registered just now" after every restart.
		if !c.reg.RegisteredAt.IsZero() {
			rt.registered = c.reg.RegisteredAt
		}
		d.mu.Lock()
		d.teams[c.team.ID] = rt
		d.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[teemd] restored team %q (id %s, pulse %s)\n", c.team.Name, c.team.ID, pulseStateLabel(rt))
		// Reconcile workers and persistent agents asynchronously so a
		// slow Fargate API call doesn't block boot.
		rtRef := rt
		safeGo("reconcile.restored:"+rtRef.team.ID, func() {
			if n := rtRef.spawner.ReconcileLocalSockets(context.Background()); n > 0 {
				fmt.Fprintf(os.Stderr, "[teemd] %s: reattached %d local worker(s)\n", rtRef.team.Name, n)
			}
			if n := rtRef.spawner.Reconcile(context.Background()); n > 0 {
				fmt.Fprintf(os.Stderr, "[teemd] %s: reconciled %d persistent agent(s)\n", rtRef.team.Name, n)
			}
		})
	}
	d.persistStateSnapshot()
}

// restoreCandidate is one fully-loaded team ready to wire into d.teams.
// Returned by planTeamRestores after dedup so callers can register
// without re-checking for collisions.
type restoreCandidate struct {
	dirName string // basename of the state dir the candidate came from
	team    *team.Team
	reg     teamRegistration
}

// planTeamRestores scans state dirs under stateRoot, parses each
// registration.json, and returns one candidate per restored team. It
// guarantees idempotency in two dimensions:
//
//  1. Same id from multiple state dirs (typical when a legacy slug dir
//     survived migration because its target id-dir already existed):
//     the dir whose registration.json was modified most recently wins;
//     the others are logged and skipped.
//  2. Distinct ids for the same Name (typical phantom — a past
//     partial migration minted a fresh id while the operator's
//     canonical state dir already existed): prefer the candidate whose
//     id is the one referenced by `<reg.RepoRoot>/teem.yaml` (the
//     operator's source of truth — same signal migrateLegacyTeamDirs
//     uses when back-filling); fall back to most-recent-mtime only if
//     no candidate is teem.yaml-anchored (or both are). A loud WARN
//     names both ids so the operator can remove the stale dir.
//
// Side-effects despite the "plan" framing: when a registration's YAML
// lacks an id, this function mints one via EnsureIDFile and writes the
// back-filled YAML into the registration.json (mirroring the
// pre-refactor inline mint path in restoreTeams). The mint is
// idempotent — once written, a second call observes the id and skips
// the mint, so running daemon boot twice in a row leaves disk state
// unchanged.
func planTeamRestores(stateRoot string) []restoreCandidate {
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		return nil
	}
	var all []loadedTeam
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		regPath := filepath.Join(stateRoot, e.Name(), "registration.json")
		info, err := os.Stat(regPath)
		if err != nil {
			continue
		}
		body, err := os.ReadFile(regPath)
		if err != nil {
			continue
		}
		var reg teamRegistration
		if err := json.Unmarshal(body, &reg); err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: bad registration.json: %v\n", e.Name(), err)
			continue
		}
		tmpFile, err := writeTempYAML(reg.TeamYAML)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: temp yaml: %v\n", e.Name(), err)
			continue
		}
		t, err := team.Load(tmpFile)
		if err != nil {
			_ = os.Remove(tmpFile)
			fmt.Fprintf(os.Stderr, "[teemd] skip %s: invalid yaml: %v\n", e.Name(), err)
			continue
		}
		// Load is pure-read since the team-id refactor: mint+persist
		// here so restored teams without an id (legacy registrations
		// that escaped migrateLegacyTeamDirs) still get one written
		// back into the saved YAML body.
		if t.ID == "" {
			id, werr := team.EnsureIDFile(tmpFile)
			if werr != nil {
				_ = os.Remove(tmpFile)
				fmt.Fprintf(os.Stderr, "[teemd] skip %s: mint id: %v\n", e.Name(), werr)
				continue
			}
			t.ID = id
		}
		if updated, rerr := os.ReadFile(tmpFile); rerr == nil && string(updated) != reg.TeamYAML {
			reg.TeamYAML = string(updated)
			_ = writeTeamRegistration(t.ID, reg)
		}
		_ = os.Remove(tmpFile)
		all = append(all, loadedTeam{
			dirName:  e.Name(),
			regMTime: info.ModTime(),
			reg:      reg,
			t:        t,
		})
	}
	// Pass 1: collapse same-id duplicates. The teem.yaml anchor is
	// irrelevant here (both candidates share an id by definition) so
	// pickWinner falls through to mtime — most recent reg.json mtime
	// wins, ties broken by lexicographic dirName.
	byID := make(map[string]loadedTeam, len(all))
	for _, l := range all {
		prev, dup := byID[l.t.ID]
		if !dup {
			byID[l.t.ID] = l
			continue
		}
		winner, loser := pickWinner(prev, l)
		fmt.Fprintf(os.Stderr, "[teemd] skip %s: id %s already restored from %s (idempotent dedup; consider removing the stale state dir under ~/.teem/state/)\n",
			loser.dirName, loser.t.ID, winner.dirName)
		byID[l.t.ID] = winner
	}
	// Pass 2: collapse same-name distinct-id collisions (the phantom
	// case). Iterate byID in sorted key order so log output is
	// deterministic when ≥3 candidates share a Name. WARN names both
	// ids so the operator can investigate.
	idKeys := make([]string, 0, len(byID))
	for k := range byID {
		idKeys = append(idKeys, k)
	}
	sort.Strings(idKeys)
	byName := make(map[string]loadedTeam, len(byID))
	for _, k := range idKeys {
		l := byID[k]
		prev, dup := byName[l.t.Name]
		if !dup {
			byName[l.t.Name] = l
			continue
		}
		winner, loser := pickWinner(prev, l)
		fmt.Fprintf(os.Stderr, "[teemd] WARN: team %q has two state dirs with different ids — keeping %s (from %s); skipping %s (from %s). Operator should remove the stale dir under ~/.teem/state/.\n",
			l.t.Name, winner.t.ID, winner.dirName, loser.t.ID, loser.dirName)
		byName[l.t.Name] = winner
	}
	out := make([]restoreCandidate, 0, len(byName))
	for _, l := range byName {
		out = append(out, restoreCandidate{
			dirName: l.dirName,
			team:    l.t,
			reg:     l.reg,
		})
	}
	// Stable order for callers/tests: by id.
	sort.Slice(out, func(i, j int) bool { return out[i].team.ID < out[j].team.ID })
	return out
}

// loadedTeam is one parsed state-dir entry mid-flight inside
// planTeamRestores. Hoisted to package scope only so pickWinner can
// reference it; not part of any external contract.
type loadedTeam struct {
	dirName  string
	regMTime time.Time
	reg      teamRegistration
	t        *team.Team
}

// pickWinner returns (winner, loser) for two candidates that collide on
// id (pass 1) or Name (pass 2). The teem.yaml anchor at
// `<reg.RepoRoot>/teem.yaml` is the operator's source of truth — the
// id it carries was either chosen by the operator or back-filled by
// migrateLegacyTeamDirs — so if exactly one candidate is anchored
// there, it wins regardless of mtime. This is the round-2 fix for the
// phantom case where the phantom dir was created *after* the canonical
// dir and so had a newer mtime under the round-1 pure-mtime rule.
//
// When neither (or both) candidate(s) are anchored, fall through to the
// mtime rule: most recent reg.json mtime wins; ties broken by
// lexicographic dirName for deterministic output.
func pickWinner(a, b loadedTeam) (winner, loser loadedTeam) {
	aAnchored := candidateAnchored(a)
	bAnchored := candidateAnchored(b)
	if aAnchored && !bAnchored {
		return a, b
	}
	if bAnchored && !aAnchored {
		return b, a
	}
	if a.regMTime.After(b.regMTime) {
		return a, b
	}
	if b.regMTime.After(a.regMTime) {
		return b, a
	}
	if a.dirName < b.dirName {
		return a, b
	}
	return b, a
}

// candidateAnchored reports whether the candidate's id is the one
// recorded in `<reg.RepoRoot>/teem.yaml`. Empty RepoRoot, missing /
// unreadable file, or unparseable YAML all read as "not anchored" —
// callers fall back to the mtime tiebreaker.
func candidateAnchored(l loadedTeam) bool {
	if l.reg.RepoRoot == "" {
		return false
	}
	yamlPath := filepath.Join(l.reg.RepoRoot, "teem.yaml")
	t, err := team.Load(yamlPath)
	if err != nil {
		return false
	}
	return t.ID != "" && t.ID == l.t.ID
}

func pulseStateLabel(rt *registeredTeam) string {
	if rt.pulse == nil {
		return "—"
	}
	if rt.pulse.Running() {
		if rt.pulse.Paused() {
			return "paused"
		}
		return "running"
	}
	return "off"
}

type registerResponse struct {
	Team     string `json:"team"`
	MCPURL   string `json:"mcp_url"`
	AuditURL string `json:"audit_url"`
}

func (d *daemon) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TeamYAML == "" {
		http.Error(w, "team_yaml is required", http.StatusBadRequest)
		return
	}
	// Parse the YAML by writing to a temp file and using team.Load
	// (pure-read since the team-id refactor). When the submitted YAML
	// lacks an `id:`, EnsureIDFile mints one into the temp file; we
	// re-read so the id-bearing YAML is what gets persisted into
	// registration.json.
	tmpFile, err := writeTempYAML(req.TeamYAML)
	if err != nil {
		http.Error(w, fmt.Sprintf("write yaml: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpFile)
	t, err := team.Load(tmpFile)
	if err != nil {
		http.Error(w, fmt.Sprintf("validate team: %v", err), http.StatusBadRequest)
		return
	}
	if t.ID == "" {
		id, werr := team.EnsureIDFile(tmpFile)
		if werr != nil {
			http.Error(w, fmt.Sprintf("mint team id: %v", werr), http.StatusInternalServerError)
			return
		}
		t.ID = id
	}
	if updated, rerr := os.ReadFile(tmpFile); rerr == nil {
		req.TeamYAML = string(updated)
	}

	// Re-register: refresh safe display fields in place, warn on
	// structural diffs, always rewrite registration.json so the next
	// daemon bounce picks up the operator's edits. Structural changes
	// (archetype add/remove, tracker config, tailnet) wire goroutines
	// and provisioners that can't be torn down mid-flight; the warning
	// tells the operator to restart to apply them.
	d.mu.Lock()
	existing, ok := d.teams[t.ID]
	d.mu.Unlock()
	if ok {
		displayChanged, structural := safeReregisterDelta(existing.team, t)
		if displayChanged {
			d.mu.Lock()
			existing.team.Name = t.Name
			existing.team.Leader.SystemPrompt = t.Leader.SystemPrompt
			d.mu.Unlock()
		}
		if len(structural) > 0 {
			fmt.Fprintf(os.Stderr,
				"[teemd] re-register %s: structural changes detected, restart daemon to apply: %s\n",
				t.ID, strings.Join(structural, "; "))
		}
		if werr := writeTeamRegistration(t.ID, teamRegistration{
			TeamYAML:     req.TeamYAML,
			RepoRoot:     req.RepoRoot,
			WorktreeBase: req.WorktreeBase,
			RegisteredAt: existing.registered,
		}); werr != nil {
			fmt.Fprintf(os.Stderr, "[teemd] warning: persist registration for %q: %v\n", t.Name, werr)
		}
		if displayChanged {
			d.persistStateSnapshot()
		}
		writeJSON(w, http.StatusOK, registerResponse{
			Team:     t.Name,
			MCPURL:   d.endpoint + "/teams/" + t.ID + "/mcp",
			AuditURL: d.endpoint + "/teams/" + t.ID + "/audit",
		})
		return
	}

	// Append the synthesised project_manager archetype if the team
	// is wired to a tracker. See restoreTeams for the symmetric
	// call; both paths must wire it so registrations and daemon
	// restarts present the same roster.
	if pm := team.MaybePMArchetype(t); pm != nil {
		if err := t.AddArchetype(*pm); err != nil && !errors.Is(err, team.ErrArchetypeExists) {
			fmt.Fprintf(os.Stderr, "[teemd] %s: append project_manager: %v\n", t.Name, err)
		}
	}
	rt, err := d.buildTeamServices(t, req.RepoRoot, req.WorktreeBase)
	if err != nil {
		http.Error(w, fmt.Sprintf("build team services: %v", err), http.StatusInternalServerError)
		return
	}
	d.mu.Lock()
	d.teams[t.ID] = rt
	d.mu.Unlock()
	if err := writeTeamRegistration(t.ID, teamRegistration{
		TeamYAML:     req.TeamYAML,
		RepoRoot:     req.RepoRoot,
		WorktreeBase: req.WorktreeBase,
		RegisteredAt: rt.registered,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] warning: persist registration for %q: %v\n", t.Name, err)
	}
	d.persistStateSnapshot()

	// Best-effort reconcile in two passes:
	//
	// 1. Local subprocess workers from the previous daemon run. Their
	//    sockets are still on disk; probe each, register live ones,
	//    sweep stale.
	// 2. Persistent agents from the team YAML (tailnet-hosted; either
	//    operator-managed local or Fargate).
	safeGo("reconcile.registered:"+t.ID, func() {
		if n := rt.spawner.ReconcileLocalSockets(context.Background()); n > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] reattached %d local worker(s) for %s\n", n, t.Name)
		}
		if n := rt.spawner.Reconcile(context.Background()); n > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] reconciled %d persistent agent(s) for %s\n", n, t.Name)
		}
	})

	writeJSON(w, http.StatusCreated, registerResponse{
		Team:     t.Name,
		MCPURL:   rt.leaderURL + "/mcp",
		AuditURL: rt.leaderURL + "/audit",
	})
}
func writeTempYAML(body string) (string, error) {
	f, err := os.CreateTemp("", "teem-register-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		return "", err
	}
	return f.Name(), nil
}
