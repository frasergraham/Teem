package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleTeamRoute_TranscriptPathServesSPA asserts that
// GET /teams/<id>/transcripts/<agent>/<job> returns the SPA shell
// (so the React-side <TranscriptPage> can take over) rather than the
// bearer-auth NDJSON download. POSTs on the same path remain wired to
// the legacy mirror handler — that's the writer the worker uses to
// upload its transcript and we must not break it.
func TestHandleTeamRoute_TranscriptPathServesSPA(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	t.Run("happy path serves SPA", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/teams/alpha/transcripts/worker-ada/j-1", nil)
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type=%q want text/html", ct)
		}
		if !strings.Contains(w.Body.String(), `<div id="root">`) {
			t.Errorf("SPA shell missing root div; body excerpt:\n%s", excerpt(w.Body.String()))
		}
	})

	t.Run("malformed agent id falls through to mirror handler", func(t *testing.T) {
		// `..` is rejected by isSafeID, so the SPA branch declines and
		// the request falls into handleTranscripts — which 401s without
		// a bearer. The test asserts the request did NOT serve the SPA;
		// the legacy handler is free to 400/401 depending on auth.
		req := httptest.NewRequest(http.MethodGet, "/teams/alpha/transcripts/../etc/passwd", nil)
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, req)
		body := w.Body.String()
		if strings.Contains(body, `<div id="root">`) {
			t.Errorf("malformed id served SPA shell (should have fallen through); body excerpt:\n%s", excerpt(body))
		}
		// Whatever the legacy handler returns, it must not be 200 OK
		// with the SPA — accept anything in the 4xx range.
		if w.Code < 400 || w.Code >= 500 {
			t.Errorf("code=%d want 4xx", w.Code)
		}
	})

	t.Run("post still routes to bearer-auth mirror", func(t *testing.T) {
		// POST without a bearer must 401 — same as before. The SPA
		// branch is GET-only.
		req := httptest.NewRequest(http.MethodPost, "/teams/alpha/transcripts/worker-ada/j-1", strings.NewReader(""))
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("POST without bearer: code=%d want 401", w.Code)
		}
	})

	t.Run("unknown team 404s", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/teams/zzz/transcripts/worker-ada/j-1", nil)
		w := httptest.NewRecorder()
		d.handler().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("unknown team: code=%d want 404", w.Code)
		}
	})
}
