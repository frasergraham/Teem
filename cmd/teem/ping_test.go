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
	"github.com/frasergraham/teem/internal/channelbus"
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

// TestPing_NudgesChannelWhenChannelsLive verifies the t-d753f950 ping
// branch: with an operator chat session active, the manual ping is
// delivered as a channel block (one channelbus.Event) and an
// operator-attributed pulse_tick audit event records the redirect.
// 200 + ping_nudged flash is the user-visible outcome.
func TestPing_NudgesChannelWhenChannelsLive(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	rt.channelBus = channelbus.New(4)
	_, busCh, cancel := rt.channelBus.Subscribe()
	defer cancel()
	d.teams["alpha"] = rt
	rt.channelsLive.Store(true)

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/ping", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "channel nudge") {
		t.Errorf("body=%q want channel-nudge message", w.Body.String())
	}

	select {
	case ev := <-busCh:
		if ev.Meta["kind"] != "pulse_tick" || ev.Meta["source"] != "teem" {
			t.Errorf("channel event meta = %+v, want kind=pulse_tick source=teem", ev.Meta)
		}
		if !strings.Contains(ev.Content, "Pulse tick") {
			t.Errorf("channel event content = %q, want pulse_tick body", ev.Content)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected one channelbus.Event published")
	}

	events, _ := rt.auditSink.Query("", time.Now().Add(-time.Minute), 16)
	var operatorTick *audit.Event
	for i := range events {
		e := events[i]
		if e.Kind == audit.Kind("pulse_tick") && e.AgentID == "operator" {
			operatorTick = &e
			break
		}
	}
	if operatorTick == nil {
		t.Fatalf("expected operator pulse_tick audit; events=%+v", events)
	}
	if route, _ := operatorTick.Meta["route"].(string); route != "channel" {
		t.Errorf("audit meta.route=%v want \"channel\"", operatorTick.Meta["route"])
	}
}

// TestPing_ResumesWhenChannelsCleared verifies the path returns to
// normal (200 / ping queued) once the operator's chat disconnects and
// the channels-live flag is cleared.
func TestPing_ResumesWhenChannelsCleared(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newPingTeam(t, "alpha")
	d.teams["alpha"] = rt
	rt.channelsLive.Store(false)

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
