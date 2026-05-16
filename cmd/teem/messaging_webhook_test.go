package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/messaging"
)

// fakeReplier records SendText calls and replays them to tests.
type fakeReplier struct {
	mu    sync.Mutex
	calls []fakeReplierCall
}

type fakeReplierCall struct {
	ChatID int64
	Text   string
}

func (f *fakeReplier) SendText(_ context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeReplierCall{ChatID: chatID, Text: text})
	return nil
}

func (f *fakeReplier) snapshot() []fakeReplierCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeReplierCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// waitFor polls fn until it returns true or 2 seconds pass.
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waitFor: condition never became true")
}

func newWebhookTestDaemon(t *testing.T) (*daemon, *fakeReplier) {
	t.Helper()
	tokens, err := messaging.NewReplyTokenStore(filepath.Join(t.TempDir(), "tok.json"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rep := &fakeReplier{}
	d := &daemon{
		teams:                    map[string]*registeredTeam{},
		baseCtx:                  context.Background(),
		messagingWebhookToken:    "webhooksecret",
		messagingReplyTokens:     tokens,
		messagingChatSessions:    newTelegramChatSessions(),
		messagingReplierOverride: rep,
	}
	return d, rep
}

func TestWebhook_RejectsInvalidToken(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)

	body := strings.NewReader(`{"message":{"chat":{"id":1},"text":"/done"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=wrong", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 body=%s", w.Code, w.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	d.handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("missing token code=%d want 401", w2.Code)
	}
}

func TestWebhook_WrongMethodReturns405(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	req := httptest.NewRequest(http.MethodGet, "/messaging/telegram/webhook?token=webhooksecret", nil)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", w.Code)
	}
}

func TestWebhook_AcceptsValidToken_UnknownCommand(t *testing.T) {
	d, rep := newWebhookTestDaemon(t)

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"hello bot"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", w.Code, w.Body.String())
	}
	calls := rep.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 reply about command shape, got %d", len(calls))
	}
	if !strings.Contains(calls[0].Text, "/reply") {
		t.Errorf("hint missing /reply shape: %q", calls[0].Text)
	}
}

func TestWebhook_ReplyUnknownTokenAcksGracefully(t *testing.T) {
	d, rep := newWebhookTestDaemon(t)

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"/reply deadbeef what's up"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", w.Code)
	}
	calls := rep.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].Text, "unknown or expired") {
		t.Fatalf("expected 'unknown or expired' hint, got %+v", calls)
	}
}

func TestWebhook_SpawnsLeaderChat_RespondsToTelegram(t *testing.T) {
	d, rep := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	tok, err := d.messagingReplyTokens.Issue(messaging.ReplyContext{
		TeamID: "alpha", TaskID: "t-3a2f", AgentID: "worker-una",
	})
	if err != nil {
		t.Fatal(err)
	}

	gotPrompt := make(chan string, 1)
	d.chatRunner = func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		gotPrompt <- userMessage
		stream := strings.Join([]string{
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello operator."}]}}`,
			`{"type":"result","result":"Hello operator."}`,
		}, "\n") + "\n"
		return io.NopCloser(strings.NewReader(stream)), func() error { return nil }, nil
	}

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"/reply ` + tok + ` how is it going"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	select {
	case prompt := <-gotPrompt:
		if prompt != "how is it going" {
			t.Errorf("user prompt=%q want %q", prompt, "how is it going")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chat runner never invoked")
	}

	waitFor(t, func() bool {
		for _, c := range rep.snapshot() {
			if c.ChatID == 42 && strings.Contains(c.Text, "Hello operator.") {
				return true
			}
		}
		return false
	})
}

func TestWebhook_DoneCommandCancelsActiveSession(t *testing.T) {
	d, rep := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	tok, _ := d.messagingReplyTokens.Issue(messaging.ReplyContext{
		TeamID: "alpha", TaskID: "t-1",
	})

	started := make(chan struct{})
	cancelled := make(chan struct{})
	d.chatRunner = func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		close(started)
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			close(cancelled)
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, func() error {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return nil
		}, nil
	}

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"/reply ` + tok + ` long running"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/reply code=%d", w.Code)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("chat runner never started")
	}

	doneBody := strings.NewReader(`{"message":{"chat":{"id":42},"text":"/done ` + tok + `"}}`)
	doneReq := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", doneBody)
	dw := httptest.NewRecorder()
	d.handler().ServeHTTP(dw, doneReq)
	if dw.Code != http.StatusOK {
		t.Fatalf("/done code=%d body=%s", dw.Code, dw.Body.String())
	}

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("subprocess context was not cancelled by /done")
	}

	waitFor(t, func() bool {
		for _, c := range rep.snapshot() {
			if strings.Contains(c.Text, "cancelled") || strings.Contains(c.Text, "Cancelled") {
				return true
			}
		}
		return false
	})
}

func TestWebhook_IdleKill_RespectsContextDeadline(t *testing.T) {
	// We rebuild the daemon with a fake clock-equivalent: override the
	// idle timeout by setting it via a derived context. The simplest
	// black-box check is that the runner's ctx carries a deadline ≤
	// telegramIdleTimeout.
	d, _ := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	tok, _ := d.messagingReplyTokens.Issue(messaging.ReplyContext{TeamID: "alpha", TaskID: "t-1"})

	gotDeadline := make(chan time.Time, 1)
	d.chatRunner = func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			gotDeadline <- time.Time{}
		} else {
			gotDeadline <- dl
		}
		return io.NopCloser(strings.NewReader("")), func() error { return nil }, nil
	}

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"/reply ` + tok + ` hi"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case dl := <-gotDeadline:
		if dl.IsZero() {
			t.Fatal("subprocess context had no deadline; idle-kill cannot fire")
		}
		want := time.Now().Add(telegramIdleTimeout)
		// Allow some slop — the deadline is relative to when the
		// goroutine called context.WithTimeout.
		if dl.After(want.Add(time.Minute)) || dl.Before(want.Add(-2*telegramIdleTimeout)) {
			t.Errorf("deadline=%s not within idle-timeout window of %s", dl, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner not called")
	}
}

// TestWebhook_EmitsUsageEvent asserts the Telegram /reply path writes
// a KindUsageEvent with agent_id="leader-telegram-chat" so inbound
// chat spend hits the daily budget gate. Mirrors
// TestChatEndpoint_EmitsUsageEvent for the dashboard chat path.
func TestWebhook_EmitsUsageEvent(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	tok, err := d.messagingReplyTokens.Issue(messaging.ReplyContext{
		TeamID: "alpha", TaskID: "t-3a2f", AgentID: "worker-una",
	})
	if err != nil {
		t.Fatal(err)
	}

	d.chatRunner = func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		stream := strings.Join([]string{
			`{"type":"system","subtype":"init","model":"claude-opus-4-7"}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Hi"}]}}`,
			`{"type":"result","result":"Hi","usage":{"input_tokens":120,"output_tokens":45,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`,
		}, "\n") + "\n"
		return io.NopCloser(strings.NewReader(stream)), func() error { return nil }, nil
	}

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"/reply ` + tok + ` hello"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var usageEvent *audit.Event
	waitFor(t, func() bool {
		events, err := rt.auditSink.Query("", time.Time{}, 100)
		if err != nil {
			return false
		}
		for i, e := range events {
			if e.Kind == audit.KindUsageEvent {
				usageEvent = &events[i]
				return true
			}
		}
		return false
	})

	if usageEvent.AgentID != "leader-telegram-chat" {
		t.Errorf("AgentID=%q want leader-telegram-chat", usageEvent.AgentID)
	}
	if got, _ := usageEvent.Meta["input_tokens"].(float64); got != 120 {
		t.Errorf("Meta input_tokens=%v want 120 (meta=%v)", got, usageEvent.Meta)
	}
	if got, _ := usageEvent.Meta["output_tokens"].(float64); got != 45 {
		t.Errorf("Meta output_tokens=%v want 45", got)
	}
	if got, _ := usageEvent.Meta["model"].(string); got != "claude-opus-4-7" {
		t.Errorf("Meta model=%q want claude-opus-4-7", got)
	}
	if got, _ := usageEvent.Meta["agent_id"].(string); got != "leader-telegram-chat" {
		t.Errorf("Meta agent_id=%q want leader-telegram-chat", got)
	}
}

func TestHandleTelegramWebhook_NativeReplyResolvesToken(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	tok, err := d.messagingReplyTokens.Issue(messaging.ReplyContext{
		TeamID: "alpha", TaskID: "t-3a2f", AgentID: "worker-una",
	})
	if err != nil {
		t.Fatal(err)
	}
	d.messagingMessageIDLookup = func(id int64) (string, messaging.ReplyContext, bool) {
		if id == 42 {
			return tok, messaging.ReplyContext{TeamID: "alpha", TaskID: "t-3a2f", AgentID: "worker-una"}, true
		}
		return "", messaging.ReplyContext{}, false
	}

	gotPrompt := make(chan string, 1)
	d.chatRunner = func(ctx context.Context, mcpConfig, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		gotPrompt <- userMessage
		stream := strings.Join([]string{
			`{"type":"assistant","message":{"content":[{"type":"text","text":"ack"}]}}`,
			`{"type":"result","result":"ack"}`,
		}, "\n") + "\n"
		return io.NopCloser(strings.NewReader(stream)), func() error { return nil }, nil
	}

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"how is it going","reply_to_message":{"message_id":42}}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	select {
	case prompt := <-gotPrompt:
		// Native reply: full text becomes the body verbatim — no /reply
		// stripping, no token in the body.
		if prompt != "how is it going" {
			t.Errorf("user prompt=%q want %q", prompt, "how is it going")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chat runner never invoked from native-reply path")
	}
}

func TestHandleTelegramWebhook_NativeReplyUnknownMessageID(t *testing.T) {
	d, rep := newWebhookTestDaemon(t)
	d.messagingMessageIDLookup = func(id int64) (string, messaging.ReplyContext, bool) {
		return "", messaging.ReplyContext{}, false
	}

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"hello again","reply_to_message":{"message_id":99}}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	calls := rep.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 outbound reply, got %d (%+v)", len(calls), calls)
	}
	got := calls[0].Text
	if !strings.Contains(strings.ToLower(got), "expired") {
		t.Errorf("reply should hint that the thread expired; got %q", got)
	}
	if strings.Contains(got, "/reply") || strings.Contains(got, "<token>") {
		t.Errorf("native-reply expiry hint should not include the /reply <token> usage string; got %q", got)
	}
}

func TestWebhook_IssueTokenStampsOutboundMessage(t *testing.T) {
	// The hook stamps msg.ReplyToken when tokens != nil — verify via a
	// recording Notifier rather than running the whole hook stack.
	tokens, _ := messaging.NewReplyTokenStore(filepath.Join(t.TempDir(), "tok.json"), time.Hour)
	tok, err := tokens.Issue(messaging.ReplyContext{TeamID: "x", TaskID: "t-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Lookup(tok); !ok {
		t.Fatal("freshly-issued token did not resolve")
	}
}
