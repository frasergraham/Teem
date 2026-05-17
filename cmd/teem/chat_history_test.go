package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func TestRecentChatBurst_FloorAlwaysIncluded(t *testing.T) {
	base := mustTime(t, "2026-05-16T12:00:00Z")
	// 12 turns, every 30 minutes (gaps far exceed the 10-minute gap
	// window). Floor=10 must still surface the last 10 turns.
	turns := make([]chatTurn, 12)
	for i := range turns {
		turns[i] = chatTurn{
			Timestamp:     base.Add(time.Duration(i) * 30 * time.Minute),
			UserMessage:   "u",
			AssistantText: "a",
		}
	}
	got := recentChatBurst(turns, burstParams{})
	if len(got) != 10 {
		t.Fatalf("len=%d want 10 (floor)", len(got))
	}
	// Floor should be the *last* 10 turns.
	if !got[0].Timestamp.Equal(turns[2].Timestamp) {
		t.Errorf("first kept ts=%s want %s", got[0].Timestamp, turns[2].Timestamp)
	}
	if !got[len(got)-1].Timestamp.Equal(turns[len(turns)-1].Timestamp) {
		t.Errorf("last kept != newest")
	}
}

func TestRecentChatBurst_GapExtendsBackwards(t *testing.T) {
	base := mustTime(t, "2026-05-16T12:00:00Z")
	// 15 tightly-spaced turns (2 min apart) — within the 10-minute
	// gap. With floor=10 and a continuous burst, we should extend back
	// to include all 15 because every gap ≤10min and we haven't hit
	// max-turns=30 or max-chars=12K.
	turns := make([]chatTurn, 15)
	for i := range turns {
		turns[i] = chatTurn{
			Timestamp:     base.Add(time.Duration(i) * 2 * time.Minute),
			UserMessage:   "u",
			AssistantText: "a",
		}
	}
	got := recentChatBurst(turns, burstParams{})
	if len(got) != 15 {
		t.Fatalf("len=%d want 15 (full burst), turns=%d", len(got), len(turns))
	}
}

func TestRecentChatBurst_StopsAtLongGap(t *testing.T) {
	base := mustTime(t, "2026-05-16T12:00:00Z")
	// 12 turns. First 2 are far away (separated from #2 by 1h); the
	// remaining 10 are tightly clustered. Floor=10 keeps the cluster;
	// the long gap to turn #1 should stop further backward extension.
	var turns []chatTurn
	turns = append(turns, chatTurn{Timestamp: base, UserMessage: "old1", AssistantText: "a"})
	turns = append(turns, chatTurn{Timestamp: base.Add(5 * time.Minute), UserMessage: "old2", AssistantText: "a"})
	// 1 hour later, 10 quick turns 1m apart.
	cluster := base.Add(time.Hour + 5*time.Minute)
	for i := 0; i < 10; i++ {
		turns = append(turns, chatTurn{
			Timestamp:     cluster.Add(time.Duration(i) * time.Minute),
			UserMessage:   "u",
			AssistantText: "a",
		})
	}
	got := recentChatBurst(turns, burstParams{})
	if len(got) != 10 {
		t.Fatalf("len=%d want 10 (gap should block extension)", len(got))
	}
	if !got[0].Timestamp.Equal(cluster) {
		t.Errorf("first kept ts=%s want cluster start %s", got[0].Timestamp, cluster)
	}
}

func TestRecentChatBurst_MaxTurnsCap(t *testing.T) {
	base := mustTime(t, "2026-05-16T12:00:00Z")
	// 50 turns 1 minute apart — all within the gap window. MaxTurns=30
	// should clamp the returned slice.
	turns := make([]chatTurn, 50)
	for i := range turns {
		turns[i] = chatTurn{
			Timestamp:     base.Add(time.Duration(i) * time.Minute),
			UserMessage:   "u",
			AssistantText: "a",
		}
	}
	got := recentChatBurst(turns, burstParams{})
	if len(got) != 30 {
		t.Fatalf("len=%d want 30 (max-turns cap)", len(got))
	}
	// The returned slice should be the trailing 30.
	if !got[0].Timestamp.Equal(turns[20].Timestamp) {
		t.Errorf("first kept ts=%s want %s (trailing 30)", got[0].Timestamp, turns[20].Timestamp)
	}
}

func TestRecentChatBurst_MaxCharsCap(t *testing.T) {
	base := mustTime(t, "2026-05-16T12:00:00Z")
	// 12 turns of ~2K chars each → 24K total. MaxChars=12K should trim
	// from the oldest end until we're under the cap.
	big := strings.Repeat("x", 1000)
	turns := make([]chatTurn, 12)
	for i := range turns {
		turns[i] = chatTurn{
			Timestamp:     base.Add(time.Duration(i) * time.Minute),
			UserMessage:   big,
			AssistantText: big,
		}
	}
	got := recentChatBurst(turns, burstParams{})
	total := 0
	for _, tt := range got {
		total += len(tt.UserMessage) + len(tt.AssistantText)
	}
	if total > 12_000 {
		t.Fatalf("total chars=%d want ≤12000", total)
	}
	if len(got) == 0 {
		t.Fatal("burst was completely emptied; expected at least 6 trailing turns to fit")
	}
}

func TestRecentChatBurst_Empty(t *testing.T) {
	if got := recentChatBurst(nil, burstParams{}); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

func TestRecentChatBurst_FewerThanFloor(t *testing.T) {
	base := mustTime(t, "2026-05-16T12:00:00Z")
	turns := []chatTurn{
		{Timestamp: base, UserMessage: "u", AssistantText: "a"},
		{Timestamp: base.Add(time.Minute), UserMessage: "u", AssistantText: "a"},
	}
	got := recentChatBurst(turns, burstParams{})
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (all-of-input when below floor)", len(got))
	}
}

func TestRenderChatBurst_EmptyReturnsEmpty(t *testing.T) {
	if got := renderChatBurst(nil); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

func TestRenderChatBurst_FormatsTurns(t *testing.T) {
	turns := []chatTurn{
		{
			Timestamp:     mustTime(t, "2026-05-16T12:00:00Z"),
			UserMessage:   "Hi there",
			AssistantText: "Hello!\nHow can I help?",
		},
		{
			Timestamp:     mustTime(t, "2026-05-16T12:01:00Z"),
			UserMessage:   "Status?",
			AssistantText: "All green.",
		},
	}
	got := renderChatBurst(turns)
	if !strings.Contains(got, "Recent conversation") {
		t.Errorf("missing header; got=%q", got)
	}
	if !strings.Contains(got, "operator: Hi there") {
		t.Errorf("missing first user line; got=%q", got)
	}
	if !strings.Contains(got, "you: Hello! How can I help?") {
		t.Errorf("missing collapsed assistant line; got=%q", got)
	}
	if !strings.Contains(got, "operator: Status?") {
		t.Errorf("missing second user line; got=%q", got)
	}
}

// TestRenderChatBurst_SkipsEmptySides asserts that a turn with an
// empty user_message or assistant_text emits only the populated side
// rather than rendering a "(empty)" placeholder. Covers the spawn-error
// case (assistant_text="") and tool-only turns (no assistant prose).
func TestRenderChatBurst_SkipsEmptySides(t *testing.T) {
	turns := []chatTurn{
		{
			Timestamp:     mustTime(t, "2026-05-16T12:00:00Z"),
			UserMessage:   "spawn-failed-question",
			AssistantText: "",
		},
		{
			Timestamp:     mustTime(t, "2026-05-16T12:01:00Z"),
			UserMessage:   "",
			AssistantText: "tool-only-reply",
		},
	}
	got := renderChatBurst(turns)
	if !strings.Contains(got, "operator: spawn-failed-question") {
		t.Errorf("missing operator line for spawn-error turn; got=%q", got)
	}
	if !strings.Contains(got, "you: tool-only-reply") {
		t.Errorf("missing you line for tool-only turn; got=%q", got)
	}
	if strings.Contains(got, "(empty)") {
		t.Errorf("rendered placeholder leaked into burst; got=%q", got)
	}
}

// TestLoadChatBurst_AgentIDFilter ensures the loader scopes by
// agent_id: a dashboard turn (leader-chat) must not surface in a
// telegram burst, and vice versa.
func TestLoadChatBurst_AgentIDFilter(t *testing.T) {
	dir := t.TempDir()
	sink, err := audit.OpenFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	writeTurn := func(agentID string, chatID int64, user, assistant string) {
		meta := map[string]any{
			"agent_id":       agentID,
			"team_id":        "alpha",
			"user_message":   user,
			"assistant_text": assistant,
		}
		if chatID != 0 {
			meta["chat_id"] = chatID
		}
		if err := sink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   agentID,
			Kind:      audit.KindLeaderChatTurn,
			Meta:      meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeTurn("leader-chat", 0, "dash-q", "dash-a")
	writeTurn("leader-telegram", 42, "tg-q", "tg-a")
	writeTurn("leader-telegram", 99, "other-q", "other-a")

	dash := loadChatBurst(sink, "leader-chat", 0, defaultBurstParams)
	if !strings.Contains(dash, "dash-q") {
		t.Errorf("dashboard burst missing own turn; got=%q", dash)
	}
	if strings.Contains(dash, "tg-q") || strings.Contains(dash, "other-q") {
		t.Errorf("dashboard burst leaked telegram turns; got=%q", dash)
	}

	tg := loadChatBurst(sink, "leader-telegram", 42, defaultBurstParams)
	if !strings.Contains(tg, "tg-q") {
		t.Errorf("telegram burst missing own turn; got=%q", tg)
	}
	if strings.Contains(tg, "other-q") {
		t.Errorf("telegram burst leaked chat_id=99 turn; got=%q", tg)
	}
	if strings.Contains(tg, "dash-q") {
		t.Errorf("telegram burst leaked dashboard turn; got=%q", tg)
	}
}

// TestChatEndpoint_BurstIncludedInContextBody asserts the dashboard
// chat handler queries the team's audit log for prior leader-chat
// turns and prepends them to the subprocess's context body.
func TestChatEndpoint_BurstIncludedInContextBody(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Seed two prior turns so the burst has something to render.
	d.recordChatTurn(rt, "leader-chat", 0, "earlier-question", "earlier-answer")
	d.recordChatTurn(rt, "leader-chat", 0, "follow-up", "follow-up-answer")

	var seenContext string
	d.chatRunner = func(ctx context.Context, mcpConfig, model, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		seenContext = contextBody
		return io.NopCloser(strings.NewReader(`{"type":"result","result":"ok"}` + "\n")), func() error { return nil }, nil
	}

	body := strings.NewReader(`{"message":"new question"}`)
	req := httptest.NewRequest(http.MethodPost, "/control/teams/alpha/chat", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(seenContext, "Recent conversation") {
		t.Errorf("context body missing burst header; got:\n%s", seenContext)
	}
	if !strings.Contains(seenContext, "earlier-question") {
		t.Errorf("context body missing earlier user message; got:\n%s", seenContext)
	}
	if !strings.Contains(seenContext, "follow-up-answer") {
		t.Errorf("context body missing follow-up assistant text; got:\n%s", seenContext)
	}
}

// TestChatEndpoint_RecordsChatTurn asserts the dashboard handler writes
// a KindLeaderChatTurn event after a successful turn so the next
// request's burst will include it.
func TestChatEndpoint_RecordsChatTurn(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.chatRunner = fakeChatRunner(
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hi"}]}}`,
		`{"type":"result","result":"Hi"}`,
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
	var turn *audit.Event
	for i, e := range events {
		if e.Kind == audit.KindLeaderChatTurn {
			turn = &events[i]
			break
		}
	}
	if turn == nil {
		t.Fatal("no KindLeaderChatTurn in audit")
	}
	if turn.AgentID != "leader-chat" {
		t.Errorf("AgentID=%q want leader-chat", turn.AgentID)
	}
	if got, _ := turn.Meta["user_message"].(string); got != "hello" {
		t.Errorf("Meta user_message=%q want hello", got)
	}
	if got, _ := turn.Meta["assistant_text"].(string); got != "Hi" {
		t.Errorf("Meta assistant_text=%q want Hi", got)
	}
	if got, _ := turn.Meta["team_id"].(string); got != "alpha" {
		t.Errorf("Meta team_id=%q want alpha", got)
	}
	if _, ok := turn.Meta["chat_id"]; ok {
		t.Errorf("dashboard turn must not set chat_id; got %v", turn.Meta["chat_id"])
	}
}

// TestTelegramBareChat_BurstIncludedInContextBody asserts the Telegram
// bare-chat path includes prior leader-telegram turns (scoped by
// chat_id) in the context body of the next subprocess.
func TestTelegramBareChat_BurstIncludedInContextBody(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt

	// Seed a Telegram turn on chat 42 and a noise turn on chat 99.
	d.recordChatTurn(rt, "leader-telegram", 42, "yesterday", "yesterday-answer")
	d.recordChatTurn(rt, "leader-telegram", 99, "other-chat", "other-chat-answer")

	gotContext := make(chan string, 1)
	d.chatRunner = func(ctx context.Context, mcpConfig, model, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		gotContext <- contextBody
		return io.NopCloser(strings.NewReader(`{"type":"result","result":"ok"}` + "\n")), func() error { return nil }, nil
	}

	body := strings.NewReader(`{"message":{"chat":{"id":42},"text":"hello again"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var contextBody string
	select {
	case contextBody = <-gotContext:
	case <-time.After(2 * time.Second):
		t.Fatal("runner not called")
	}
	if !strings.Contains(contextBody, "yesterday") {
		t.Errorf("context body missing same-chat history; got:\n%s", contextBody)
	}
	if strings.Contains(contextBody, "other-chat") {
		t.Errorf("context body leaked turns from chat_id=99; got:\n%s", contextBody)
	}
}

// TestTelegramBareChat_RecordsChatTurn asserts the bare-chat path
// writes a KindLeaderChatTurn event tagged with the originating
// chat_id.
func TestTelegramBareChat_RecordsChatTurn(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.chatRunner = func(ctx context.Context, mcpConfig, model, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		stream := strings.Join([]string{
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Roger"}]}}`,
			`{"type":"result","result":"Roger"}`,
		}, "\n") + "\n"
		return io.NopCloser(strings.NewReader(stream)), func() error { return nil }, nil
	}

	body := strings.NewReader(`{"message":{"chat":{"id":7},"text":"ping"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var turn *audit.Event
	waitFor(t, func() bool {
		events, err := rt.auditSink.Query("", time.Time{}, 100)
		if err != nil {
			return false
		}
		for i, e := range events {
			if e.Kind == audit.KindLeaderChatTurn {
				turn = &events[i]
				return true
			}
		}
		return false
	})

	if turn.AgentID != "leader-telegram" {
		t.Errorf("AgentID=%q want leader-telegram", turn.AgentID)
	}
	if got, _ := turn.Meta["user_message"].(string); got != "ping" {
		t.Errorf("Meta user_message=%q want ping", got)
	}
	if got, _ := turn.Meta["assistant_text"].(string); got != "Roger" {
		t.Errorf("Meta assistant_text=%q want Roger", got)
	}
	if got, _ := chatIDFromMeta(turn.Meta); got != 7 {
		t.Errorf("Meta chat_id=%d want 7", got)
	}
}

// TestChatEndpoint_RecordsTurnOnSpawnError asserts the dashboard chat
// handler still emits a KindLeaderChatTurn event when the subprocess
// fails to start — otherwise the operator's last question would be
// silently dropped from the next turn's burst context.
func TestChatEndpoint_RecordsTurnOnSpawnError(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.chatRunner = func(ctx context.Context, mcpConfig, model, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		return nil, nil, errors.New("claude CLI not on PATH")
	}

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
	var turns []audit.Event
	for _, e := range events {
		if e.Kind == audit.KindLeaderChatTurn {
			turns = append(turns, e)
		}
	}
	if len(turns) != 1 {
		t.Fatalf("got %d KindLeaderChatTurn events, want 1", len(turns))
	}
	turn := turns[0]
	if turn.AgentID != "leader-chat" {
		t.Errorf("AgentID=%q want leader-chat", turn.AgentID)
	}
	if got, _ := turn.Meta["user_message"].(string); got != "hello" {
		t.Errorf("Meta user_message=%q want hello", got)
	}
	if got, _ := turn.Meta["assistant_text"].(string); got != "" {
		t.Errorf("Meta assistant_text=%q want empty", got)
	}
}

// TestTelegramBareChat_RecordsTurnOnSpawnError mirrors the dashboard
// test for the Telegram bare-text surface. The recorded event must keep
// chat_id meta so the next turn on the same Telegram thread still
// scopes its burst correctly.
func TestTelegramBareChat_RecordsTurnOnSpawnError(t *testing.T) {
	d, _ := newWebhookTestDaemon(t)
	rt := newFullTestTeam(t, "alpha")
	d.teams["alpha"] = rt
	d.chatRunner = func(ctx context.Context, mcpConfig, model, repoRoot, contextBody, userMessage string) (io.ReadCloser, func() error, error) {
		return nil, nil, errors.New("claude CLI not on PATH")
	}

	body := strings.NewReader(`{"message":{"chat":{"id":7},"text":"ping"}}`)
	req := httptest.NewRequest(http.MethodPost, "/messaging/telegram/webhook?token=webhooksecret", body)
	w := httptest.NewRecorder()
	d.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var turn *audit.Event
	waitFor(t, func() bool {
		events, err := rt.auditSink.Query("", time.Time{}, 100)
		if err != nil {
			return false
		}
		for i, e := range events {
			if e.Kind == audit.KindLeaderChatTurn {
				turn = &events[i]
				return true
			}
		}
		return false
	})

	if turn.AgentID != "leader-telegram" {
		t.Errorf("AgentID=%q want leader-telegram", turn.AgentID)
	}
	if got, _ := turn.Meta["user_message"].(string); got != "ping" {
		t.Errorf("Meta user_message=%q want ping", got)
	}
	if got, _ := turn.Meta["assistant_text"].(string); got != "" {
		t.Errorf("Meta assistant_text=%q want empty", got)
	}
	if got, _ := chatIDFromMeta(turn.Meta); got != 7 {
		t.Errorf("Meta chat_id=%d want 7", got)
	}
}
