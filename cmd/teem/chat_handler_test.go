package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
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

// TestChatEndpoint_TimeoutAfter5Minutes asserts the chat handler
// imposes a per-request deadline on the subprocess context. We use a
// 50ms artificial timeout (chatTimeout) so the test runs in real time;
// the runner blocks on ctx.Done and reports back via a channel whether
// the cancel reason was DeadlineExceeded.
func TestChatEndpoint_TimeoutAfter5Minutes(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.chatTimeout = 50 * time.Millisecond

	cancelReason := make(chan error, 1)
	d.chatRunner = func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			cancelReason <- ctx.Err()
			_ = pw.Close()
		}()
		wait := func() error {
			<-ctx.Done()
			return ctx.Err()
		}
		return pr, wait, nil
	}

	body := strings.NewReader(`{"message":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	start := time.Now()
	d.handler().ServeHTTP(w, req)
	elapsed := time.Since(start)

	select {
	case got := <-cancelReason:
		if !errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("ctx err=%v want DeadlineExceeded", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner ctx never cancelled")
	}
	if elapsed > 2*time.Second {
		t.Errorf("handler ran %v (expected ~50ms — handler not honouring chatTimeout?)", elapsed)
	}
}

// TestChatEndpoint_EmitsUsageEvent asserts the chat handler writes a
// KindUsageEvent into the team's audit sink after the subprocess
// finishes, with agent_id="leader-chat" and the token counts pulled
// from the stream's result rollup. Without this, operator chat spend
// silently bypasses the daily-token budget gate.
func TestChatEndpoint_EmitsUsageEvent(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.chatRunner = fakeChatRunner(
		`{"type":"system","subtype":"init","model":"claude-opus-4-7"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hi"}]}}`,
		`{"type":"result","result":"Hi","usage":{"input_tokens":120,"output_tokens":45,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`,
	)

	body := strings.NewReader(`{"message":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	events, err := rt.auditSink.Query("", time.Time{}, 100)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	var usageEvent *audit.Event
	for i, e := range events {
		if e.Kind == audit.KindUsageEvent {
			usageEvent = &events[i]
			break
		}
	}
	if usageEvent == nil {
		t.Fatalf("no KindUsageEvent in audit; got %d events", len(events))
	}
	if usageEvent.AgentID != "leader-chat" {
		t.Errorf("AgentID=%q want leader-chat", usageEvent.AgentID)
	}
	// JSON round-trip turns numeric Meta values into float64.
	if got, _ := usageEvent.Meta["input_tokens"].(float64); got != 120 {
		t.Errorf("Meta input_tokens=%v want 120 (meta=%v)", got, usageEvent.Meta)
	}
	if got, _ := usageEvent.Meta["output_tokens"].(float64); got != 45 {
		t.Errorf("Meta output_tokens=%v want 45", got)
	}
	if got, _ := usageEvent.Meta["model"].(string); got != "claude-opus-4-7" {
		t.Errorf("Meta model=%q want claude-opus-4-7", got)
	}
	if got, _ := usageEvent.Meta["agent_id"].(string); got != "leader-chat" {
		t.Errorf("Meta agent_id=%q want leader-chat", got)
	}
}

// TestChatEndpoint_BodySizeCapEnforced asserts an oversized POST body
// is rejected by http.MaxBytesReader before the JSON decoder can
// allocate the full payload. We expect 413; 400 is also accepted
// because Go versions before 1.20 wrap MaxBytesError differently.
func TestChatEndpoint_BodySizeCapEnforced(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// 100 KiB > 64 KiB cap. Build a valid-shape JSON so failure is
	// unambiguously the size cap, not a parser error.
	big := strings.Repeat("x", 100*1024)
	body := strings.NewReader(`{"message":"` + big + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 413 (or 400 fallback) for oversized body; body=%s", w.Code, w.Body.String())
	}
}

func TestTeamPage_ChatPanel_RendersWithSessionStorageScript(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	req := httptest.NewRequest(http.MethodGet, "/teams/alpha/legacy", nil)
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
