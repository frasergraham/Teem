package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frasergraham/teem/internal/messaging"
)

func TestWebhookListener_DefaultsToMainPortPlus1WhenEnabled(t *testing.T) {
	cfg := messaging.TelegramConfig{Enabled: true}
	port, defaulted := effectiveWebhookPort(cfg, ":7777")
	if port != 7778 {
		t.Errorf("port = %d, want 7778", port)
	}
	if !defaulted {
		t.Error("defaulted should be true when webhook_port is unset")
	}
}

func TestWebhookListener_ExplicitPortOverridesDefault(t *testing.T) {
	cfg := messaging.TelegramConfig{Enabled: true, WebhookPort: 9001}
	port, defaulted := effectiveWebhookPort(cfg, ":7777")
	if port != 9001 {
		t.Errorf("port = %d, want 9001", port)
	}
	if defaulted {
		t.Error("defaulted should be false when operator pinned a port")
	}
}

func TestWebhookListener_DisabledWhenTelegramDisabled(t *testing.T) {
	cfg := messaging.TelegramConfig{Enabled: false, WebhookPort: 9001}
	port, defaulted := effectiveWebhookPort(cfg, ":7777")
	if port != 0 {
		t.Errorf("port = %d, want 0 when telegram is disabled", port)
	}
	if defaulted {
		t.Error("defaulted should be false when telegram is disabled")
	}
}

func TestWebhookListener_DefaultGivesUpWhenListenAddrUnparseable(t *testing.T) {
	cfg := messaging.TelegramConfig{Enabled: true}
	port, defaulted := effectiveWebhookPort(cfg, "garbage")
	if port != 0 {
		t.Errorf("port = %d, want 0 when listen addr can't be parsed", port)
	}
	if defaulted {
		t.Error("defaulted should be false when we can't derive a port")
	}
}

// TestWebhookHandler_404OnOtherPaths confirms the dedicated webhook
// handler only serves /messaging/telegram/webhook and 404s everything
// else — including dashboard / MCP / control paths that DO exist on
// the main listener.
func TestWebhookHandler_404OnOtherPaths(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	h := newWebhookHandler(d)

	paths := []string{
		"/",
		"/dashboard",
		"/ui",
		"/healthz",
		"/control/teams",
		"/teams/alpha/audit",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d want 404 body=%s", p, w.Code, w.Body.String())
		}
	}
}

// TestWebhookHandler_AllowsWebhookPath verifies that POSTing to the
// real webhook path on the dedicated handler still flows through the
// shared handleTelegramWebhook (token check + 200 ack on a
// non-message update).
func TestWebhookHandler_AllowsWebhookPath(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	h := newWebhookHandler(d)

	// Empty message body -> handler decodes the (empty) JSON, sees no
	// Message, and 200s. Token must still be valid.
	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", w.Code, w.Body.String())
	}

	// Wrong token still 401s on the dedicated port.
	req2 := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=wrong", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token code=%d want 401", w2.Code)
	}

	// GET on the webhook path is 405 (handler's method check).
	req3 := httptest.NewRequest(http.MethodGet, "/messaging/telegram/webhook?token=webhooksecret", nil)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req3)
	if w3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code=%d want 405", w3.Code)
	}
}

// TestWebhookHandler_LiveListener spins a real TCP listener bound to
// 127.0.0.1:0, serves the dedicated handler on it, and round-trips one
// 404 (other path) + one 200 (webhook path) over the network. This is
// the closest a unit test gets to verifying the daemon's startup glue
// without booting a full daemon.
func TestWebhookHandler_LiveListener(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: newWebhookHandler(d)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	base := "http://" + ln.Addr().String()

	// 1. Root -> 404
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET / code=%d want 404", resp.StatusCode)
	}

	// 2. Dashboard -> 404
	resp2, err := http.Get(base + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET /dashboard code=%d want 404", resp2.StatusCode)
	}

	// 3. POST /messaging/telegram/webhook?token=… -> 200
	resp3, err := http.Post(base+"/messaging/telegram/webhook?token=webhooksecret",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("POST webhook code=%d want 200", resp3.StatusCode)
	}
}

// TestMainHandler_WebhookMountedOnMainPort is the no-regression
// guard: when WebhookPort is 0 (the default), the main daemon handler
// still mounts /messaging/telegram/webhook directly. This shadows the
// older TestWebhook_RejectsInvalidToken intent but pins the property
// explicitly so anyone refactoring the listener wiring doesn't quietly
// move the route off the main port.
func TestMainHandler_WebhookMountedOnMainPort(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	if d.messagingCfg.WebhookPort != 0 {
		t.Fatalf("test scaffolding: WebhookPort=%d, want 0 (this test pins behavior at the default)", d.messagingCfg.WebhookPort)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/messaging/telegram/webhook?token=webhooksecret",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("main-port webhook code=%d want 200 body=%s", w.Code, w.Body.String())
	}
}
