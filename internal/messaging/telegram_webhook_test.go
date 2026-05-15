package messaging

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestTelegramNotifier_GetWebhookInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botSECRET/getWebhookInfo" {
			t.Errorf("path = %s, want /botSECRET/getWebhookInfo", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"url":"https://teem.example/messaging/telegram/webhook?token=xyz","has_custom_certificate":false,"pending_update_count":0}}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET", 99, srv.URL, srv.Client())
	got, err := n.GetWebhookInfo(context.Background())
	if err != nil {
		t.Fatalf("GetWebhookInfo: %v", err)
	}
	if got != "https://teem.example/messaging/telegram/webhook?token=xyz" {
		t.Errorf("url = %q want telegram.example webhook URL", got)
	}
}

func TestTelegramNotifier_GetWebhookInfo_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET", 99, srv.URL, srv.Client())
	_, err := n.GetWebhookInfo(context.Background())
	if err == nil {
		t.Fatal("expected error for ok=false response")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error should carry the API description; got %v", err)
	}
}

// TestAutoRegister_SetsWebhookWhenURLDiffers verifies EnsureWebhook
// posts setWebhook when the bot's currently-registered URL is stale.
func TestAutoRegister_SetsWebhookWhenURLDiffers(t *testing.T) {
	var setCalled bool
	var setBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"url":"https://old.example/webhook"}}`))
		case strings.HasSuffix(r.URL.Path, "/setWebhook"):
			setCalled = true
			setBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET", 99, srv.URL, srv.Client())
	hookURL := "https://new.example/messaging/telegram/webhook?token=abc"
	changed, err := n.EnsureWebhook(context.Background(), hookURL)
	if err != nil {
		t.Fatalf("EnsureWebhook: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when URL differs")
	}
	if !setCalled {
		t.Fatal("setWebhook was not called")
	}
	var payload map[string]any
	if err := json.Unmarshal(setBody, &payload); err != nil {
		t.Fatalf("setWebhook body: %v", err)
	}
	if payload["url"] != hookURL {
		t.Errorf("setWebhook url=%v want %q", payload["url"], hookURL)
	}
}

// TestAutoRegister_SkipsWhenURLMatches verifies EnsureWebhook does NOT
// post setWebhook when the bot is already registered at hookURL.
func TestAutoRegister_SkipsWhenURLMatches(t *testing.T) {
	hookURL := "https://teem.example/messaging/telegram/webhook?token=abc"
	var setCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"url":"` + hookURL + `"}}`))
		case strings.HasSuffix(r.URL.Path, "/setWebhook"):
			setCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET", 99, srv.URL, srv.Client())
	changed, err := n.EnsureWebhook(context.Background(), hookURL)
	if err != nil {
		t.Fatalf("EnsureWebhook: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when URL already matches")
	}
	if setCalled {
		t.Fatal("setWebhook should not have been called")
	}
}

// TestAutoRegister_LogsButDoesNotFailOnAPIErrors confirms that a
// failing GetWebhookInfo surfaces as an error from EnsureWebhook (which
// the daemon's goroutine wrapper catches and logs without aborting
// startup).
func TestAutoRegister_LogsButDoesNotFailOnAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"description":"telegram is sad"}`))
	}))
	defer srv.Close()

	n := NewTelegramNotifierWithBase("SECRET", 99, srv.URL, srv.Client())
	changed, err := n.EnsureWebhook(context.Background(), "https://teem.example/messaging/telegram/webhook?token=abc")
	if err == nil {
		t.Fatal("expected an error from a 500 getWebhookInfo response")
	}
	if changed {
		t.Fatal("changed should be false on error")
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
