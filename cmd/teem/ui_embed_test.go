package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// SPA serving requires cmd/teem/ui/dist to have been built (make ui).
// Each test asserts on the index.html shell rather than asset hashes so
// rebuilds don't churn the test.

func TestSPAEmbedded_IndexAtV2(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/v2/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `<div id="root">`) {
		t.Errorf("SPA index.html missing root div; body excerpt:\n%s", excerpt(body))
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type=%q want text/html", ct)
	}
}

func TestSPAEmbedded_TeamRootServesSPA(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<div id="root">`) {
		t.Errorf("/teams/<id> did not serve SPA shell; body excerpt:\n%s", excerpt(w.Body.String()))
	}
}

func TestSPAEmbedded_UnknownTeam404(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/nonexistent", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", w.Code, w.Body.String())
	}
}

func TestSPAEmbedded_HistoryFallback(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/v2/some/sub/path", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<div id="root">`) {
		t.Errorf("history fallback did not serve index.html for deep route")
	}
}

func TestSPAEmbedded_NotFoundForRealAssets(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/v2/nonexistent.png", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", w.Code, w.Body.String())
	}
}

func excerpt(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[:400]
}
