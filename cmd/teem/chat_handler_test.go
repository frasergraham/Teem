package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeChatRunner returns a chatRunner that emits a canned stream-json
// payload on stdout, mirroring what `claude -p --output-format stream-json`
// would print for a multi-chunk assistant turn.
func fakeChatRunner(lines ...string) chatRunner {
	body := strings.Join(lines, "\n") + "\n"
	return func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		return io.NopCloser(strings.NewReader(body)), func() error { return nil }, nil
	}
}

func TestChatEndpoint_StreamsResponse(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.chatRunner = fakeChatRunner(
		`{"type":"system","subtype":"init","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":" operator."}]}}`,
		`{"type":"result","result":"Hello operator."}`,
	)

	body := strings.NewReader(`{"message":"What's blocking Pax?"}`)
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type=%q want text/event-stream", ct)
	}
	got := w.Body.String()
	if !strings.Contains(got, "data: Hello\n") {
		t.Errorf("missing first chunk SSE frame; body=\n%s", got)
	}
	if !strings.Contains(got, "data:  operator.\n") {
		t.Errorf("missing second chunk SSE frame; body=\n%s", got)
	}
	if !strings.Contains(got, "event: done") {
		t.Errorf("missing terminal done frame; body=\n%s", got)
	}
}

// TestChatEndpoint_AuthGated documents the boundary: /control/teams/<id>/chat
// shares the dashboard's tailnet-only model (same as /ping). There is
// no per-user bearer auth — an unknown team falls through to 404 just
// like ping does. If we ever add bearer auth this test should flip to
// asserting 401/403 for missing tokens.
func TestChatEndpoint_AuthGated(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}

	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/control/teams/missing/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 for unknown team body=%s", w.Code, w.Body.String())
	}
}

func TestChatEndpoint_RejectsEmptyMessage(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	cases := []struct {
		name string
		body string
	}{
		{"empty string", `{"message":""}`},
		{"whitespace only", `{"message":"   \n\t"}`},
		{"missing field", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/chat", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			d.handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("code=%d want 400 body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestChatEndpoint_WrongMethodReturns405(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/control/teams/alpha/chat", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405 body=%s", w.Code, w.Body.String())
	}
}

func TestTeamPage_ChatPanel_RendersWithSessionStorageScript(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="chat-panel"`) {
		t.Errorf("chat-panel section missing from team detail page")
	}
	if !strings.Contains(body, `data-team-id="alpha"`) {
		t.Errorf("chat-panel data-team-id attribute missing or wrong team id")
	}
	if !strings.Contains(body, `id="chat-input-alpha"`) || !strings.Contains(body, `id="chat-log-alpha"`) {
		t.Errorf("chat-panel input/log ids are not team-scoped")
	}
	if !strings.Contains(body, `'teem.chat.' + teamID`) {
		t.Errorf("inline SSE client missing sessionStorage key namespacing")
	}
	if !strings.Contains(body, `/control/teams/' + teamID + '/chat'`) {
		t.Errorf("inline SSE client does not POST to the chat endpoint")
	}
	// The bridge-console "operator action" panels live in this order:
	// workers-panel → chat-panel → decisions-section. Verify the chat
	// panel sits between them so a future template edit doesn't quietly
	// move it elsewhere.
	wIdx := strings.Index(body, `class="workers-panel"`)
	cIdx := strings.Index(body, `class="chat-panel"`)
	dIdx := strings.Index(body, `class="decisions-section"`)
	if wIdx < 0 || cIdx < 0 || dIdx < 0 || !(wIdx < cIdx && cIdx < dIdx) {
		t.Errorf("chat-panel not positioned between workers-panel and decisions-section (workers=%d chat=%d decisions=%d)", wIdx, cIdx, dIdx)
	}
}
