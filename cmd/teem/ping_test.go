package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/pulse"
)

// newPingTeam returns a registeredTeam with a working Pulse and audit
// sink suitable for exercising the /control/teams/<id>/ping handler.
// LoadSession returns ok=false so Tick is a fast no-op (it bumps
// counters and returns before invoking claude).
func newPingTeam(t *testing.T, id string) *registeredTeam {
	t.Helper()
	rt := newFullTestTeam(t, id)
	dir := t.TempDir()
	rt.pulse = pulse.New(pulse.Config{
		TeamName:    id,
		TeamID:      id,
		LoadSession: func() (string, bool, error) { return "", false, nil },
		PauseFile:   filepath.Join(dir, "pulse.paused"),
		Audit:       rt.auditSink,
	})
	return rt
}

func TestPing_OKQueuesTickAndAuditEvent(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, "ping queued") {
		t.Errorf("body=%q want 'ping queued'", got)
	}

	// The handler writes the audit event synchronously (the Tick
	// goroutine may still be racing). Read it back.
	events, err := rt.auditSink.Query("", time.Now().Add(-time.Minute), 16)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	var found *audit.Event
	for i := range events {
		e := events[i]
		if e.Kind == audit.Kind("pulse_tick") && e.AgentID == "operator" {
			found = &e
			break
		}
	}
	if found == nil {
		t.Fatalf("no operator pulse_tick event written; events=%+v", events)
	}
	if got := found.Meta["trigger"]; got != "manual" {
		t.Errorf("meta.trigger=%v want manual", got)
	}
}

func TestPing_PausedReturns409(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt
	if err := rt.pulse.Pause("test"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d want %d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pulse paused") {
		t.Errorf("body=%q want 'pulse paused'", w.Body.String())
	}
}

// busyPulse is a tiny fake registeredTeam that returns true from
// Busy() so the handler reports tick-in-progress. We reuse the real
// Pulse but grab its tick mutex from a goroutine before calling the
// handler; this is the same code path the handler peeks.
func TestPing_TickInProgressReturns202(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Block a tick mid-flight by holding the mutex via a slow
	// LoadSession callback. Rebuild Pulse with the slow loader.
	hold := make(chan struct{})
	release := make(chan struct{})
	rt.pulse = pulse.New(pulse.Config{
		TeamName: "alpha",
		TeamID:   "alpha",
		LoadSession: func() (string, bool, error) {
			close(hold)
			<-release
			return "", false, nil
		},
		PauseFile: filepath.Join(t.TempDir(), "pulse.paused"),
		Audit:     rt.auditSink,
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = rt.pulse.Tick(context.Background(), "test")
	}()
	<-hold // tick is now inside the mutex, blocked on LoadSession

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	close(release)
	wg.Wait()

	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d want %d body=%s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tick already in progress") {
		t.Errorf("body=%q want 'tick already in progress'", w.Body.String())
	}
}

func TestPing_UnknownTeamReturns404(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}

	req := httptest.NewRequest(http.MethodPost, "/control/teams/missing/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", w.Code, w.Body.String())
	}
}

func TestPing_WrongMethodReturns405(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/control/teams/alpha/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405 body=%s", w.Code, w.Body.String())
	}
}

func TestTeamPage_RendersPingButtonAndFlash(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha?flash=pinged", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `action="/control/teams/alpha/ping"`) {
		t.Errorf("ping form missing from team-detail page")
	}
	if !strings.Contains(body, "Ping leader") {
		t.Errorf("ping button label missing")
	}
	if !strings.Contains(body, "Ping queued") {
		t.Errorf("flash banner missing for flash=pinged")
	}

	// Unknown flash values are dropped (whitelist-only).
	req2 := httptest.NewRequest(http.MethodGet, "/teams/alpha?flash=%3Cscript%3Ealert(1)%3C/script%3E", nil)
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req2)
	if strings.Contains(w2.Body.String(), "<script>") {
		t.Errorf("flash whitelist failed to drop hostile value")
	}
}

func TestPing_HTMLAcceptRedirectsWithFlash(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d want 303 body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/teams/alpha?flash=pinged" {
		t.Errorf("Location=%q", got)
	}
}
