package messaging

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTelegramNotifier_SendsHTTP(t *testing.T) {
	var gotPath string
	var gotBody []byte
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET123", 99887766, srv.URL, srv.Client())
	err := n.Notify(context.Background(), Message{
		Title:    "[teem-foo] Approval needed",
		Summary:  "Coder Una finished t-3a2f.",
		Severity: SeverityAction,
		Link:     "https://dash.example/teams/foo/#task-t-3a2f",
		TaskID:   "t-3a2f",
		TeamID:   "foo",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/botSECRET123/sendMessage" {
		t.Errorf("path = %s, want /botSECRET123/sendMessage", gotPath)
	}

	var payload sendMessagePayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body json: %v\nbody=%s", err, string(gotBody))
	}
	if payload.ChatID != 99887766 {
		t.Errorf("chat_id = %d, want 99887766", payload.ChatID)
	}
	if payload.ParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", payload.ParseMode)
	}
	if !strings.Contains(payload.Text, "<b>[teem-foo] Approval needed</b>") {
		t.Errorf("text missing bold title: %q", payload.Text)
	}
	if !strings.Contains(payload.Text, "Coder Una finished t-3a2f.") {
		t.Errorf("text missing summary: %q", payload.Text)
	}
	if !strings.Contains(payload.Text, `<a href="https://dash.example/teams/foo/#task-t-3a2f">Open task</a>`) {
		t.Errorf("text missing link: %q", payload.Text)
	}
}

func TestTelegramNotifier_NonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"bad chat_id"}`))
	}))
	defer srv.Close()
	n := NewTelegramNotifierWithBase("TOK", 1, srv.URL, srv.Client())
	err := n.Notify(context.Background(), Message{Title: "x"})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want it to mention 400", err)
	}
}

func TestTelegramNotifier_TransportErrorDoesNotLeakToken(t *testing.T) {
	// Point the notifier at a server that immediately closes the
	// connection so client.Do returns a *url.Error. The default
	// (*url.Error).Error() string includes the request URL, which for
	// Telegram embeds the bot token in /bot<TOKEN>/sendMessage — that
	// would leak the token to operator stderr / log files.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijacker unsupported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	const token = "123456:ABC-fake-token"
	n := NewTelegramNotifierWithBase(token, 1, srv.URL, srv.Client())
	err := n.Notify(context.Background(), Message{Title: "x"})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("bot token leaked in error message: %v", err)
	}
}

func TestTelegramNotifier_NotifyRecordsMessageID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("TOK", 1, srv.URL, srv.Client())
	err := n.Notify(context.Background(), Message{
		Title:      "Approval needed",
		Summary:    "x",
		ReplyToken: "abcd1234",
		TeamID:     "alpha",
		TaskID:     "t-3a2f",
		AgentID:    "worker-una",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	tok, ok := n.LookupByMessageID(42)
	if !ok {
		t.Fatal("LookupByMessageID(42) ok=false; expected the message_id to have been recorded")
	}
	if tok != "abcd1234" {
		t.Errorf("token=%q want abcd1234", tok)
	}
}

func TestTelegramNotifier_NotifyWithoutReplyTokenSkipsRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("TOK", 1, srv.URL, srv.Client())
	if err := n.Notify(context.Background(), Message{Title: "no token"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if _, ok := n.LookupByMessageID(7); ok {
		t.Fatal("expected LookupByMessageID(7) to return ok=false when no ReplyToken was attached")
	}
}

func TestTelegramNotifier_LookupExpired(t *testing.T) {
	n := NewTelegramNotifierWithBase("TOK", 1, "http://unused", nil)
	n.recordMessageID(99, "tok-99")

	// Skew the clock past the TTL window so the next lookup evicts.
	n.nowFn = func() time.Time { return time.Now().Add(messageIDTTL + time.Hour) }

	if _, ok := n.LookupByMessageID(99); ok {
		t.Fatal("expected expired entry to return ok=false")
	}
	// And it should have been evicted.
	if _, ok := n.messageIDIdx[99]; ok {
		t.Fatal("expired entry was not evicted from the index")
	}
}

func TestTelegramNotifier_LRUCap(t *testing.T) {
	n := NewTelegramNotifierWithBase("TOK", 1, "http://unused", nil)
	// Insert one more than the cap. Oldest (id=1) must be evicted;
	// newest (id=cap+1) must still be present.
	for i := int64(1); i <= int64(messageIDCap)+1; i++ {
		n.recordMessageID(i, "t")
	}
	if _, ok := n.LookupByMessageID(1); ok {
		t.Fatalf("oldest entry (id=1) should have been LRU-evicted after %d inserts", messageIDCap+1)
	}
	if _, ok := n.LookupByMessageID(int64(messageIDCap) + 1); !ok {
		t.Fatalf("newest entry (id=%d) should still be present", messageIDCap+1)
	}
	if got := n.messageIDLst.Len(); got != messageIDCap {
		t.Errorf("list length=%d want %d", got, messageIDCap)
	}
}

func TestTelegramNotifier_EscapesHTMLInSubstitutions(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(200)
	}))
	defer srv.Close()
	n := NewTelegramNotifierWithBase("TOK", 1, srv.URL, srv.Client())
	if err := n.Notify(context.Background(), Message{
		Title:   "<script>alert(1)</script>",
		Summary: "1 < 2 && 3 > 2",
	}); err != nil {
		t.Fatal(err)
	}
	var payload sendMessagePayload
	_ = json.Unmarshal(gotBody, &payload)
	if strings.Contains(payload.Text, "<script>") {
		t.Errorf("title not escaped: %q", payload.Text)
	}
	if !strings.Contains(payload.Text, "1 &lt; 2 &amp;&amp; 3 &gt; 2") {
		t.Errorf("summary not escaped: %q", payload.Text)
	}
}
