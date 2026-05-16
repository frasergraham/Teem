package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}

	// Top-level keys promised in docs/dashboard-spa.md §6.
	wantKeys := []string{
		"now", "etag", "team", "hero", "agents", "workers", "tasks",
		"decisions", "leader_status", "pulse", "branches",
		"channels_state",
	}
	for _, k := range wantKeys {
		if _, ok := resp[k]; !ok {
			t.Errorf("response missing top-level key %q (keys=%v)", k, sortedKeys(resp))
		}
	}

	// team.id / team.name must round-trip the canonical id.
	var meta apiTeamMeta
	if err := json.Unmarshal(resp["team"], &meta); err != nil {
		t.Fatalf("team subobject: %v", err)
	}
	if meta.ID != "alpha" {
		t.Errorf("team.id=%q want alpha", meta.ID)
	}
	if meta.Name != "alpha" {
		t.Errorf("team.name=%q want alpha", meta.Name)
	}

	// tasks must carry the two SPA-required buckets.
	var tasks map[string]json.RawMessage
	if err := json.Unmarshal(resp["tasks"], &tasks); err != nil {
		t.Fatalf("tasks subobject: %v", err)
	}
	for _, k := range []string{"open", "awaiting_approval"} {
		if _, ok := tasks[k]; !ok {
			t.Errorf("tasks missing %q (keys=%v)", k, sortedKeys(tasks))
		}
	}

	// channels_state must be a sentinel string.
	var channels string
	if err := json.Unmarshal(resp["channels_state"], &channels); err != nil {
		t.Fatalf("channels_state: %v", err)
	}
	if channels != "live" && channels != "fallback" {
		t.Errorf("channels_state=%q want live|fallback", channels)
	}

	// etag is the sha256 of the rest of the body; non-empty and
	// prefixed with "sha256:".
	var etag string
	if err := json.Unmarshal(resp["etag"], &etag); err != nil {
		t.Fatalf("etag: %v", err)
	}
	if !strings.HasPrefix(etag, "sha256:") || len(etag) <= len("sha256:") {
		t.Errorf("etag=%q want sha256:<hex>", etag)
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

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
