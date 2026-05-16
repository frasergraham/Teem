package main

import "testing"

// TestResolveTeam_IDAndNameAlias verifies the URL key resolver accepts
// both the canonical team id and the team's display Name. Long-lived
// clients (Claude Code's MCP transport, the teem-channel SSE shim)
// captured a `/teams/<name>/...` URL before TI1 minted a separate id;
// the alias keeps that handshake alive across daemon restart.
func TestResolveTeam_IDAndNameAlias(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	rt := newFullTestTeam(t, "ignored-by-test")
	rt.team.ID = "t-abc1234567890def"
	rt.team.Name = "example-team"
	d.teams[rt.team.ID] = rt

	if got := d.resolveTeam("t-abc1234567890def"); got != rt {
		t.Errorf("id lookup: got %p want %p", got, rt)
	}
	if got := d.resolveTeam("example-team"); got != rt {
		t.Errorf("name alias: got %p want %p", got, rt)
	}
	if got := d.resolveTeam("nonesuch"); got != nil {
		t.Errorf("unknown key should return nil, got %p", got)
	}
}

// TestResolveTeam_IDTakesPrecedence verifies that when one team's Name
// happens to match another team's canonical id, the id-keyed team
// wins. Pathological case; documented in resolveTeam's comment.
func TestResolveTeam_IDTakesPrecedence(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}}
	idTeam := newFullTestTeam(t, "id-team")
	idTeam.team.ID = "t-collidewithname"
	idTeam.team.Name = "id-team"
	d.teams[idTeam.team.ID] = idTeam

	nameTeam := newFullTestTeam(t, "other-id")
	nameTeam.team.ID = "other-id"
	nameTeam.team.Name = "t-collidewithname"
	d.teams[nameTeam.team.ID] = nameTeam

	if got := d.resolveTeam("t-collidewithname"); got != idTeam {
		t.Errorf("id match must win over name alias: got %p want %p", got, idTeam)
	}
}
