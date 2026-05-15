package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/frasergraham/teem/internal/team"
)

// safeReregisterDelta classifies the diff between an already-registered
// team and a freshly-parsed YAML body submitted via POST /register.
//
// displayChanged is true when one of the "safe" display-only fields
// differs (Name, Leader.SystemPrompt). These can be mutated in place
// because they're only read at the next worker spawn / dashboard render
// — no goroutine or provisioner state hangs off them.
//
// structuralChanges is a human-readable list of diffs that wire
// goroutines, provisioners, or tracker pollers and so cannot be safely
// applied mid-flight. The handler logs the list and tells the operator
// to restart the daemon; the new YAML is still written to
// registration.json so the next bounce picks it up. Empty list means
// nothing structural changed.
func safeReregisterDelta(existing, fresh *team.Team) (displayChanged bool, structuralChanges []string) {
	if existing == nil || fresh == nil {
		return false, nil
	}
	displayChanged = existing.Name != fresh.Name ||
		existing.Leader.SystemPrompt != fresh.Leader.SystemPrompt

	if diff := diffArchetypes(existing, fresh); diff != "" {
		structuralChanges = append(structuralChanges, diff)
	}
	if diff := diffTracker(existing.Tracker, fresh.Tracker); diff != "" {
		structuralChanges = append(structuralChanges, diff)
	}
	if diff := diffTailnet(existing, fresh); diff != "" {
		structuralChanges = append(structuralChanges, diff)
	}
	return displayChanged, structuralChanges
}

// augmentedArchetypes returns the team's declared archetypes plus the
// synthesised project_manager (if Tracker.Type is set). The existing
// team has had MaybePMArchetype appended at first-register; a fresh
// team has not. Augmenting both sides keeps the diff symmetric so a
// Tracker-driven PM doesn't masquerade as a bare archetype add.
func augmentedArchetypes(t *team.Team) []team.ArchetypeSpec {
	archs := t.SnapshotArchetypes()
	pm := team.MaybePMArchetype(t)
	if pm == nil {
		return archs
	}
	for _, a := range archs {
		if a.Role == pm.Role {
			return archs
		}
	}
	return append(archs, *pm)
}

func diffArchetypes(existing, fresh *team.Team) string {
	existArchs := augmentedArchetypes(existing)
	freshArchs := augmentedArchetypes(fresh)
	existByRole := make(map[string]team.ArchetypeSpec, len(existArchs))
	for _, a := range existArchs {
		existByRole[a.Role] = a
	}
	freshByRole := make(map[string]team.ArchetypeSpec, len(freshArchs))
	for _, a := range freshArchs {
		freshByRole[a.Role] = a
	}
	var added, removed, changed []string
	for role := range freshByRole {
		if _, ok := existByRole[role]; !ok {
			added = append(added, role)
		}
	}
	for role, e := range existByRole {
		f, ok := freshByRole[role]
		if !ok {
			removed = append(removed, role)
			continue
		}
		if e.MaxConcurrent != f.MaxConcurrent {
			changed = append(changed, fmt.Sprintf("%s.max_concurrent %d→%d", role, e.MaxConcurrent, f.MaxConcurrent))
		}
	}
	if len(added) == 0 && len(removed) == 0 && len(changed) == 0 {
		return ""
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ","))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ","))
	}
	if len(changed) > 0 {
		parts = append(parts, strings.Join(changed, ","))
	}
	return "archetypes changed (" + strings.Join(parts, "; ") + ")"
}

// diffTailnet reports tailnet structural changes. AuthKeyEnv is the
// only field this checks: it's operator-explicit and gates which env
// var the spawner reads at provision time. Hostname is intentionally
// skipped — team.Load defaults it to sanitizeHostname(Name), so a
// Name-only edit would otherwise show up as a structural tailnet diff
// (false positive). Operator-set hostname overrides are uncommon and
// still get persisted to registration.json, so a daemon bounce picks
// them up regardless of whether we log a warning.
func diffTailnet(existing, fresh *team.Team) string {
	if existing.Tailnet.AuthKeyEnv != fresh.Tailnet.AuthKeyEnv {
		return "tailnet changed"
	}
	return ""
}

func diffTracker(existing, fresh *team.TrackerConfig) string {
	switch {
	case existing == nil && fresh == nil:
		return ""
	case existing == nil && fresh != nil:
		return "tracker added"
	case existing != nil && fresh == nil:
		return "tracker removed"
	}
	var diffs []string
	if existing.Type != fresh.Type {
		diffs = append(diffs, fmt.Sprintf("type %q→%q", existing.Type, fresh.Type))
	}
	if existing.TeamID != fresh.TeamID {
		diffs = append(diffs, fmt.Sprintf("team_id %q→%q", existing.TeamID, fresh.TeamID))
	}
	if existing.AuthEnv != fresh.AuthEnv {
		diffs = append(diffs, fmt.Sprintf("auth_env %q→%q", existing.AuthEnv, fresh.AuthEnv))
	}
	if existing.AuthFile != fresh.AuthFile {
		diffs = append(diffs, fmt.Sprintf("auth_file %q→%q", existing.AuthFile, fresh.AuthFile))
	}
	if existing.PollInterval != fresh.PollInterval {
		diffs = append(diffs, fmt.Sprintf("poll_interval %s→%s", existing.PollInterval, fresh.PollInterval))
	}
	if len(diffs) == 0 {
		return ""
	}
	return "tracker changed (" + strings.Join(diffs, ", ") + ")"
}
