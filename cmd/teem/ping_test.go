package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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
	if !strings.Contains(body, "Pinged — leader is taking a turn now") {
		t.Errorf("flash banner missing or stale for flash=pinged; body excerpt:\n%s", flashExcerpt(body))
	}
	if strings.Contains(body, "Ping queued") || strings.Contains(body, "next tick") {
		t.Errorf("flash still uses stale 'queued'/'next tick' wording")
	}

	// Unknown flash values are dropped (whitelist-only). The page legitimately
	// embeds a same-origin inline <script> for sessionStorage expand-state
	// persistence, so probe for the hostile payload itself rather than the
	// generic tag.
	req2 := httptest.NewRequest(http.MethodGet, "/teams/alpha?flash=%3Cscript%3Ealert(1)%3C/script%3E", nil)
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req2)
	if strings.Contains(w2.Body.String(), "alert(1)") {
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
	got := w.Header().Get("Location")
	if !strings.HasPrefix(got, "/teams/alpha?flash=pinged&ping_ts=") {
		t.Errorf("Location=%q want prefix /teams/alpha?flash=pinged&ping_ts=", got)
	}
	// ping_ts must be a recent unix-seconds value the team page can parse.
	const prefix = "/teams/alpha?flash=pinged&ping_ts="
	tsStr := strings.TrimPrefix(got, prefix)
	n, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		t.Fatalf("ping_ts=%q not parseable: %v", tsStr, err)
	}
	if delta := time.Since(time.Unix(n, 0)); delta < 0 || delta > 5*time.Second {
		t.Errorf("ping_ts %d is %v away from now; want recent", n, delta)
	}
}

// TestTeamPage_FlashUpgradesToTickOK verifies the "pinged" banner upgrades
// to "Leader turn done (Xs)" once a successful leader pulse_tick has
// been written to the audit log since the redirect's ping_ts.
func TestTeamPage_FlashUpgradesToTickOK(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	pingTS := time.Now().Add(-2 * time.Second)
	if err := rt.auditSink.Write(audit.Event{
		Timestamp: pingTS.Add(time.Second),
		AgentID:   "leader",
		Kind:      audit.Kind("pulse_tick"),
		Message:   "ok",
		Meta:      map[string]any{"trigger": "manual", "duration_ms": 1234, "tool_calls": 0},
	}); err != nil {
		t.Fatalf("write audit: %v", err)
	}

	url := "/teams/alpha?flash=pinged&ping_ts=" + strconv.FormatInt(pingTS.Unix(), 10)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Leader turn done") || !strings.Contains(body, "1.2s") {
		t.Errorf("expected tick_ok flash with duration; body excerpt:\n%s", flashExcerpt(body))
	}
	if strings.Contains(body, "leader is taking a turn now") {
		t.Errorf("flash should have upgraded out of 'pinged' once the tick landed")
	}
}

// TestTeamPage_FlashUpgradesToTickFailed verifies the banner becomes
// "Leader turn FAILED: <msg>" when the leader pulse_tick carries
// error=true.
func TestTeamPage_FlashUpgradesToTickFailed(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	pingTS := time.Now().Add(-2 * time.Second)
	if err := rt.auditSink.Write(audit.Event{
		Timestamp: pingTS.Add(time.Second),
		AgentID:   "leader",
		Kind:      audit.Kind("pulse_tick"),
		Message:   "claude exec: context deadline exceeded",
		Meta:      map[string]any{"trigger": "manual", "duration_ms": 500, "error": true},
	}); err != nil {
		t.Fatalf("write audit: %v", err)
	}

	url := "/teams/alpha?flash=pinged&ping_ts=" + strconv.FormatInt(pingTS.Unix(), 10)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Leader turn FAILED") {
		t.Errorf("expected tick_failed flash; body excerpt:\n%s", flashExcerpt(body))
	}
	if !strings.Contains(body, "context deadline exceeded") {
		t.Errorf("expected leader error message in flash; body excerpt:\n%s", flashExcerpt(body))
	}
}

// TestTeamPage_FlashStaysPingedUntilOutcome covers the pre-resolution
// window: ping_ts has been redirected with, but no leader pulse_tick has
// landed yet. Banner must remain the "taking a turn" wording so the 10s
// meta-refresh keeps the operator in the loop.
func TestTeamPage_FlashStaysPingedUntilOutcome(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	pingTS := time.Now().Add(-1 * time.Second).Unix()
	url := "/teams/alpha?flash=pinged&ping_ts=" + strconv.FormatInt(pingTS, 10)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Pinged — leader is taking a turn now") {
		t.Errorf("expected unresolved 'pinged' flash; body excerpt:\n%s", flashExcerpt(body))
	}
	if strings.Contains(body, "Leader turn done") || strings.Contains(body, "FAILED") {
		t.Errorf("flash upgraded without any matching audit event")
	}
}

// TestPing_RefusedWhenChannelsLive verifies that the manual-ping
// handler returns 409 with the prescribed message when an operator
// chat session is active. Two writers on the leader's session file
// would race; the chat is already driving the leader anyway.
func TestPing_RefusedWhenChannelsLive(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt
	rt.channelsLive = true

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d want %d body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "operator chat session is active") {
		t.Errorf("body=%q want operator-chat-active message", w.Body.String())
	}
	// No pulse_tick / operator audit should have been written.
	events, _ := rt.auditSink.Query("", time.Now().Add(-time.Minute), 16)
	for _, e := range events {
		if e.Kind == audit.Kind("pulse_tick") && e.AgentID == "operator" {
			t.Errorf("refused ping should not write a manual pulse_tick audit event; got %+v", e)
		}
	}
}

// TestPing_HTMLRefusedWhenChannelsLive verifies the form-POST variant:
// Accept: text/html clients get a 303 redirect with the
// ping_skipped_chat_active flash tag, so the team page can render a
// friendly banner instead of dumping a JSON body in the browser.
func TestPing_HTMLRefusedWhenChannelsLive(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt
	rt.channelsLive = true

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d want 303 body=%s", w.Code, w.Body.String())
	}
	got := w.Header().Get("Location")
	if !strings.Contains(got, "flash=ping_skipped_chat_active") {
		t.Errorf("Location=%q want flash=ping_skipped_chat_active", got)
	}
}

// TestPing_ResumesWhenChannelsCleared verifies the path returns to
// normal (200 / ping queued) once the operator's chat disconnects and
// the channels-live flag is cleared.
func TestPing_ResumesWhenChannelsCleared(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt
	rt.channelsLive = false

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ping queued") {
		t.Errorf("body=%q want 'ping queued'", w.Body.String())
	}
}

// TestTeamPage_RendersPingSkippedChatActiveFlash verifies the dashboard
// renders the friendly banner when redirected with the new flash tag.
func TestTeamPage_RendersPingSkippedChatActiveFlash(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha?flash=ping_skipped_chat_active", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "leader is already awake via your chat session") {
		t.Errorf("expected chat-active banner; body excerpt:\n%s", flashExcerpt(w.Body.String()))
	}
}

// flashExcerpt slices out the area around the dashboard's flash div so
// test failure output stays small and readable.
func flashExcerpt(body string) string {
	i := strings.Index(body, `class="flash`)
	if i < 0 {
		return "(no flash div rendered)"
	}
	end := i + 200
	if end > len(body) {
		end = len(body)
	}
	return body[i:end]
}
