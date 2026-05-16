package main

import (
	"context"
	"io/fs"
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

func TestSPAEmbedded_TeamRootRedirectsToTrailingSlash(t *testing.T) {
	// Vite emits relative ./assets/... refs in index.html; without a
	// trailing slash the browser resolves them against /teams/ instead
	// of /teams/<id>/, so the bare form must redirect.
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d want 303 body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/teams/alpha/" {
		t.Errorf("Location=%q want /teams/alpha/", loc)
	}
}

func TestSPAEmbedded_TeamRootSlashServesSPA(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<div id="root">`) {
		t.Errorf("/teams/<id>/ did not serve SPA shell; body excerpt:\n%s", excerpt(w.Body.String()))
	}
}

func TestSPAEmbedded_AssetsUnderTeamPath(t *testing.T) {
	// The SPA's relative ./assets/<hash>.js refs resolve to
	// /teams/<id>/assets/<hash>.js once the page is at the trailing-
	// slash form. Those must hit the embedded bundle, not 404.
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	dist, err := spaDistFS()
	if err != nil {
		t.Fatalf("spaDistFS: %v", err)
	}
	entries, err := fs.ReadDir(dist, "assets")
	if err != nil {
		t.Fatalf("read assets/: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no built assets — run `make ui`")
	}
	asset := entries[0].Name()

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/assets/"+asset, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("asset %q: code=%d body=%s", asset, w.Code, w.Body.String())
	}
	if w.Body.Len() == 0 {
		t.Errorf("asset %q served empty body", asset)
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
