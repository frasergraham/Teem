package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// telegramAPIBase is the production endpoint. Overridable by tests via
// NewTelegramNotifierWithBase.
const telegramAPIBase = "https://api.telegram.org"

// TelegramNotifier sends Message payloads via the Telegram Bot API's
// sendMessage endpoint, parse_mode=HTML.
type TelegramNotifier struct {
	token   string
	chatID  int64
	baseURL string
	client  *http.Client
}

// NewTelegramNotifier returns a notifier targeting api.telegram.org.
// Pass a nil client to get a 10-second-timeout default.
func NewTelegramNotifier(token string, chatID int64, client *http.Client) *TelegramNotifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TelegramNotifier{
		token:   token,
		chatID:  chatID,
		baseURL: telegramAPIBase,
		client:  client,
	}
}

// NewTelegramNotifierWithBase is the test seam — same as
// NewTelegramNotifier but lets the caller redirect the API to a local
// httptest server.
func NewTelegramNotifierWithBase(token string, chatID int64, baseURL string, client *http.Client) *TelegramNotifier {
	n := NewTelegramNotifier(token, chatID, client)
	n.baseURL = baseURL
	return n
}

// sendMessagePayload is the wire shape we POST to sendMessage. Only the
// fields we set are listed.
type sendMessagePayload struct {
	ChatID                int64  `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

func (n *TelegramNotifier) Notify(ctx context.Context, msg Message) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", n.baseURL, n.token)
	body, err := json.Marshal(sendMessagePayload{
		ChatID:                n.chatID,
		Text:                  renderHTMLBody(msg),
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("telegram: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: new request: %s", sanitizeURLError(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: post: %s", sanitizeURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return nil
}

// SendText posts a plain-text sendMessage to the supplied chat_id.
// Used by the inbound webhook handler to ferry leader chat output back
// to the operator's Telegram thread. parse_mode is left blank so the
// text is delivered as-is (no HTML rendering, no escaping required).
func (n *TelegramNotifier) SendText(ctx context.Context, chatID int64, text string) error {
	if text == "" {
		return nil
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", n.baseURL, n.token)
	body, err := json.Marshal(sendMessagePayload{
		ChatID:                chatID,
		Text:                  text,
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("telegram: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: new request: %s", sanitizeURLError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: post: %s", sanitizeURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return nil
}

// SetWebhook calls Telegram's setWebhook endpoint pointing the bot at
// url. Used by the `teem messaging telegram register-webhook` CLI.
func (n *TelegramNotifier) SetWebhook(ctx context.Context, url string) error {
	endpoint := fmt.Sprintf("%s/bot%s/setWebhook", n.baseURL, n.token)
	body, err := json.Marshal(map[string]any{
		"url":             url,
		"allowed_updates": []string{"message"},
	})
	if err != nil {
		return fmt.Errorf("telegram: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: new request: %s", sanitizeURLError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: post: %s", sanitizeURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram: setWebhook status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return nil
}

// sanitizeURLError formats an error from net/http without leaking the
// request URL — for Telegram the URL embeds the bot token in its path
// (/bot<TOKEN>/sendMessage), so the default (*url.Error).Error() would
// surface the token to operator stderr and log files. We emit just the
// Op + inner error, which carries the useful diagnostic info.
func sanitizeURLError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("%s: %v", urlErr.Op, urlErr.Err)
	}
	return err.Error()
}

// renderHTMLBody assembles the parse_mode=HTML body Telegram renders.
// Three characters need escaping (& < >); we escape exactly the
// substituted bits and leave the literal tags alone.
func renderHTMLBody(msg Message) string {
	var b strings.Builder
	if msg.Title != "" {
		b.WriteString("<b>")
		b.WriteString(html.EscapeString(msg.Title))
		b.WriteString("</b>\n")
	}
	if msg.Summary != "" {
		b.WriteString(html.EscapeString(msg.Summary))
	}
	if msg.Link != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(msg.Link))
		b.WriteString(`">Open task</a>`)
	}
	if msg.ReplyToken != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Reply <code>/reply ")
		b.WriteString(html.EscapeString(msg.ReplyToken))
		b.WriteString(" your message</code> to chat with the leader.")
	}
	return b.String()
}
