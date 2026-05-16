package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleAPITeamRoute dispatches /api/teams/<id>/... routes. Phase 1
// exposes one endpoint:
//
//	GET /api/teams/<id>/state  → minimal {team:{id,name}} envelope
//
// Auth model matches the dashboard's tailnet boundary: no bearer
// required. The richer endpoints documented in docs/dashboard-spa.md §6
// land in Phase 2+.
func (d *daemon) handleAPITeamRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/teams/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.NotFound(w, r)
		return
	}
	id, suffix := rest[:slash], rest[slash:]
	rt := d.resolveTeam(id)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	switch suffix {
	case "/state":
		d.handleAPITeamState(w, r, rt)
	default:
		http.NotFound(w, r)
	}
}

type apiTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type apiTeamStateResponse struct {
	Team apiTeam `json:"team"`
}

func (d *daemon) handleAPITeamState(w http.ResponseWriter, _ *http.Request, rt *registeredTeam) {
	tv := d.snapshotTeam(rt)
	id := ""
	if rt.team != nil {
		id = rt.team.ID
	}
	resp := apiTeamStateResponse{Team: apiTeam{ID: id, Name: tv.Name}}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}
