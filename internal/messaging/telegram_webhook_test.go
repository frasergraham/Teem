package messaging

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramNotifier_SetWebhook(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET", 99, srv.URL, srv.Client())
	err := n.SetWebhook(context.Background(), "https://teem.example.test/messaging/telegram/webhook?token=abcd")
	if err != nil {
		t.Fatalf("SetWebhook: %v", err)
	}
	if gotPath != "/botSECRET/setWebhook" {
		t.Errorf("path = %s, want /botSECRET/setWebhook", gotPath)
	}
	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body: %v\n%s", err, string(gotBody))
	}
	if payload["url"] != "https://teem.example.test/messaging/telegram/webhook?token=abcd" {
		t.Errorf("url = %v, want the hook URL", payload["url"])
	}
}

func TestTelegramNotifier_SendText(t *testing.T) {
	var payload sendMessagePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET", 99, srv.URL, srv.Client())
	if err := n.SendText(context.Background(), 7777, "Hello back."); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if payload.ChatID != 7777 {
		t.Errorf("chat_id=%d want 7777", payload.ChatID)
	}
	if payload.Text != "Hello back." {
		t.Errorf("text=%q want %q", payload.Text, "Hello back.")
	}
}
