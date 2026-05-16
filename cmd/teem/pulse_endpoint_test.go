package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/pulse"
)

// newPulsePanelTeam wires a registeredTeam with a working Pulse
// (LoadSession=ok-false so Tick is a fast no-op) and a per-team
// pulse-wake.txt file under a temp dir, so the /control/teams/<id>/pulse
// endpoints have somewhere durable to write.
func newPulsePanelTeam(t *testing.T, id string) (*registeredTeam, string) {
	t.Helper()
	rt := newFullTestTeam(t, id)
	dir := t.TempDir()
	wakeFile := filepath.Join(dir, "pulse-wake.txt")
	rt.pulse = pulse.New(pulse.Config{
		TeamName:       id,
		TeamID:         id,
		LoadSession:    func() (string, bool, error) { return "", false, nil },
		PauseFile:      filepath.Join(dir, "pulse.paused"),
		RunningFile:    filepath.Join(dir, "pulse.running"),
		WakePromptFile: wakeFile,
		Audit:          rt.auditSink,
		Interval:       5 * time.Minute,
	})
	return rt, wakeFile
}

// TestPulseEndpoint_Start_AcceptsCustomWakePrompt covers the
// /control/teams/<id>/pulse/start sub-path: a custom wake_prompt in
// the JSON body must persist to the per-team pulse-wake.txt file and
// take effect on the running pulse.
func TestPulseEndpoint_Start_AcceptsCustomWakePrompt(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt, wakeFile := newPulsePanelTeam(t, "alpha")
	d.teams["alpha"] = rt
	t.Cleanup(rt.pulse.Stop)

	custom := "Bridge: scan ops board, then reply."
	body, _ := json.Marshal(pulseCommand{Interval: "1m", WakePrompt: &custom})
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/pulse/start", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var got pulseStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.Running {
		t.Errorf("response says Running=false after start")
	}
	if got.WakePrompt != custom {
		t.Errorf("response WakePrompt = %q, want %q", got.WakePrompt, custom)
	}
	if got.UseDefaultWakePrompt {
		t.Errorf("UseDefaultWakePrompt should be false after override")
	}

	// File on disk must carry the custom value so a daemon restart
	// re-reads it.
	persisted, err := os.ReadFile(wakeFile)
	if err != nil {
		t.Fatalf("read wake file: %v", err)
	}
	if got := strings.TrimSpace(string(persisted)); got != custom {
		t.Errorf("wake file body = %q, want %q", got, custom)
	}
	if rt.pulse.Interval() != time.Minute {
		t.Errorf("interval after start = %s, want 1m", rt.pulse.Interval())
	}
}

// TestPulseEndpoint_Stop verifies POST /control/teams/<id>/pulse/stop
// stops the pulse and clears the persisted running flag (operator-
// explicit Stop, not the daemon-shutdown variant).
func TestPulseEndpoint_Stop(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt, _ := newPulsePanelTeam(t, "alpha")
	d.teams["alpha"] = rt
	rt.pulse.Start(context.Background())
	if !rt.pulse.Running() {
		t.Fatal("precondition: pulse should be running before Stop")
	}

	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/pulse/stop", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if rt.pulse.Running() {
		t.Errorf("pulse should be stopped after /pulse/stop")
	}
	if rt.pulse.WasRunning() {
		t.Errorf("operator-explicit stop must clear the running flag")
	}
}

// TestPulseEndpoint_Config_UpdatesIntervalAndPrompt verifies the
// /pulse/config sub-path applies an in-memory interval change AND
// persists a wake-prompt update without bouncing the pulse.
func TestPulseEndpoint_Config_UpdatesIntervalAndPrompt(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt, wakeFile := newPulsePanelTeam(t, "alpha")
	d.teams["alpha"] = rt
	rt.pulse.Start(context.Background())
	t.Cleanup(rt.pulse.Stop)

	updated := "Config-updated wake prompt."
	body, _ := json.Marshal(pulseCommand{Interval: "45s", WakePrompt: &updated})
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/pulse/config", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if got := rt.pulse.Interval(); got != 45*time.Second {
		t.Errorf("Interval after config = %s, want 45s", got)
	}
	if got := rt.pulse.WakePrompt(); got != updated {
		t.Errorf("WakePrompt after config = %q, want %q", got, updated)
	}
	persisted, err := os.ReadFile(wakeFile)
	if err != nil {
		t.Fatalf("read wake file: %v", err)
	}
	if got := strings.TrimSpace(string(persisted)); got != updated {
		t.Errorf("wake file body = %q, want %q", got, updated)
	}
	// Pulse should still be running — config is a hot-update.
	if !rt.pulse.Running() {
		t.Errorf("Running should remain true after /pulse/config")
	}
}

// TestPulseEndpoint_Config_FormPost mirrors the dashboard form post
// shape (interval_value + interval_unit + wake_prompt as form fields,
// Accept: text/html). Verifies the daemon stitches the unit/value back
// together and 303-redirects to the team page.
func TestPulseEndpoint_Config_FormPost(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt, _ := newPulsePanelTeam(t, "alpha")
	d.teams["alpha"] = rt
	rt.pulse.Start(context.Background())
	t.Cleanup(rt.pulse.Stop)

	form := url.Values{
		"interval_value": []string{"2"},
		"interval_unit":  []string{"m"},
		"wake_prompt":    []string{"From the form."},
	}
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/pulse/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code=%d want 303 body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/teams/alpha/legacy" {
		t.Errorf("Location=%q want /teams/alpha/legacy", loc)
	}
	if got := rt.pulse.Interval(); got != 2*time.Minute {
		t.Errorf("Interval after form-post = %s, want 2m", got)
	}
	if got := rt.pulse.WakePrompt(); got != "From the form." {
		t.Errorf("WakePrompt after form-post = %q", got)
	}
}

// TestPulseEndpoint_GetIncludesWakePrompt verifies the GET response
// shape carries the wake-prompt fields the dashboard needs.
func TestPulseEndpoint_GetIncludesWakePrompt(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background(), token: "tok"}
	rt, _ := newPulsePanelTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/control/teams/alpha/pulse", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got pulseStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WakePrompt == "" {
		t.Errorf("WakePrompt empty in GET response: %+v", got)
	}
	if got.DefaultWakePrompt != pulse.DefaultWakePrompt() {
		t.Errorf("DefaultWakePrompt mismatch")
	}
	if !got.UseDefaultWakePrompt {
		t.Errorf("UseDefaultWakePrompt should be true on a fresh team with no override")
	}
}

// TestTeamPage_PulsePanel_RendersCurrentState verifies the dashboard
// template renders the pulse-management panel with the current
// running/interval/prompt state, and toggles the start vs stop URL on
// the lamp form based on whether pulse is running.
func TestTeamPage_PulsePanel_RendersCurrentState(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt, _ := newPulsePanelTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Off state.
	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="pulse-panel"`,
		`name="interval_value"`,
		`name="interval_unit"`,
		`name="wake_prompt"`,
		`action="/control/teams/alpha/pulse/start"`,
		`action="/control/teams/alpha/pulse/config"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
	if strings.Contains(body, `action="/control/teams/alpha/pulse/stop"`) {
		t.Errorf("stop URL should not appear when pulse is off")
	}

	// On + custom override flips the toggle URL to /stop and renders
	// the override in the textarea body.
	rt.pulse.Start(context.Background())
	defer rt.pulse.Stop()
	override := "Operator-supplied wake message."
	if err := rt.pulse.SetWakePrompt(override); err != nil {
		t.Fatal(err)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req2)
	body = w2.Body.String()
	if !strings.Contains(body, `action="/control/teams/alpha/pulse/stop"`) {
		t.Errorf("stop URL missing when pulse is on")
	}
	if !strings.Contains(body, override) {
		t.Errorf("custom wake-prompt override not rendered in textarea")
	}
}
