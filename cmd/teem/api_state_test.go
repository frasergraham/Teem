package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPITeamState_ReturnsJSON(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/api/teams/alpha/state", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q want application/json", ct)
	}

	var resp apiTeamStateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp.Team.ID != "alpha" {
		t.Errorf("team.id=%q want alpha", resp.Team.ID)
	}
	if resp.Team.Name != "alpha" {
		t.Errorf("team.name=%q want alpha", resp.Team.Name)
	}
}

func TestAPITeamState_UnknownTeam(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}

	req := httptest.NewRequest(http.MethodGet, "/api/teams/nonexistent/state", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", w.Code, w.Body.String())
	}
}
