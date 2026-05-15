package team

import "strings"

// personaDisplayRoles maps an agent_id's role prefix to the friendly
// display word the leader uses in operator-facing prose. Kept in sync
// with the role mapping in StatusMessageGuidance.
var personaDisplayRoles = map[string]string{
	"worker":          "Coder",
	"reviewer":        "Reviewer",
	"integrator":      "Integrator",
	"project_manager": "PM",
}

// PersonaName returns the friendly form of an agent_id for use in
// operator-facing prose. "worker-uma" → "Coder Uma". Unknown role
// prefixes or malformed agent_ids (no "-", empty name) are returned
// unchanged so the caller never silently rewrites a string it doesn't
// recognise.
func PersonaName(agentID string) string {
	idx := strings.Index(agentID, "-")
	if idx <= 0 || idx == len(agentID)-1 {
		return agentID
	}
	role := agentID[:idx]
	name := agentID[idx+1:]
	display, ok := personaDisplayRoles[role]
	if !ok {
		return agentID
	}
	return display + " " + strings.ToUpper(name[:1]) + name[1:]
}
