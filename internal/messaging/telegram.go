package messaging

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// telegramAPIBase is the production endpoint. Overridable by tests via
// NewTelegramNotifierWithBase.
const telegramAPIBase = "https://api.telegram.org"

// messageIDTTL bounds how long an outbound message_id stays
// resolvable via LookupByMessageID. Matches the reply-token TTL so a
// native reply gesture behaves the same as typing /reply <token>.
const messageIDTTL = 24 * time.Hour

// messageIDCap caps the in-memory message_id → reply-token map so a
// long-running daemon can't grow unbounded. LRU eviction keeps the
// most-recent entries.
const messageIDCap = 1000

// messageIDEntry pairs an outbound Telegram message_id with the
// reply-token the operator would otherwise have typed. The reply
// context is re-resolved from the token at lookup time, so we don't
// store it here.
type messageIDEntry struct {
	MessageID int64
	Token     string
	At        time.Time
}

// TelegramNotifier sends Message payloads via the Telegram Bot API's
// sendMessage endpoint, parse_mode=HTML.
type TelegramNotifier struct {
	token   string
	chatID  int64
	baseURL string
	client  *http.Client

	// messageIDs is an LRU+TTL map from outbound Telegram message_id to
	// the reply token / context the operator would otherwise type as
	// `/reply <token> ...`. Populated on every successful Notify whose
	// Message carries a ReplyToken; consulted by the inbound webhook
	// handler when an update has reply_to_message set.
	messageIDMu  sync.Mutex
	messageIDLst *list.List              // front = newest, back = oldest
	messageIDIdx map[int64]*list.Element // message_id → element of *messageIDEntry
	nowFn        func() time.Time
}

// NewTelegramNotifier returns a notifier targeting api.telegram.org.
// Pass a nil client to get a 10-second-timeout default.
func NewTelegramNotifier(token string, chatID int64, client *http.Client) *TelegramNotifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TelegramNotifier{
		token:        token,
		chatID:       chatID,
		baseURL:      telegramAPIBase,
		client:       client,
		messageIDLst: list.New(),
		messageIDIdx: map[int64]*list.Element{},
		nowFn:        time.Now,
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
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	if msg.ReplyToken != "" {
		var parsed struct {
			OK     bool `json:"ok"`
			Result struct {
				MessageID int64 `json:"message_id"`
			} `json:"result"`
		}
		if err := json.Unmarshal(buf, &parsed); err == nil && parsed.OK && parsed.Result.MessageID != 0 {
			n.recordMessageID(parsed.Result.MessageID, msg.ReplyToken)
		}
	}
	return nil
}

// recordMessageID stores (or refreshes) an entry mapping a Telegram
// outbound message_id to the operator's reply token. LRU-evicts the
// oldest entry once messageIDCap is exceeded.
func (n *TelegramNotifier) recordMessageID(id int64, token string) {
	n.messageIDMu.Lock()
	defer n.messageIDMu.Unlock()
	if elem, ok := n.messageIDIdx[id]; ok {
		// Refresh in place — keeps memory bounded and resets TTL.
		entry := elem.Value.(*messageIDEntry)
		entry.Token = token
		entry.At = n.nowFn()
		n.messageIDLst.MoveToFront(elem)
		return
	}
	entry := &messageIDEntry{MessageID: id, Token: token, At: n.nowFn()}
	elem := n.messageIDLst.PushFront(entry)
	n.messageIDIdx[id] = elem
	for n.messageIDLst.Len() > messageIDCap {
		back := n.messageIDLst.Back()
		if back == nil {
			break
		}
		evicted := back.Value.(*messageIDEntry)
		n.messageIDLst.Remove(back)
		delete(n.messageIDIdx, evicted.MessageID)
	}
}

// LookupByMessageID returns the reply token recorded for a
// previously-sent outbound Telegram message. ok=false when the
// message_id is unknown or its TTL has elapsed (in which case the
// entry is also evicted). The reply context is re-resolved from the
// token by the caller via ReplyTokenStore.Lookup.
func (n *TelegramNotifier) LookupByMessageID(id int64) (string, bool) {
	n.messageIDMu.Lock()
	defer n.messageIDMu.Unlock()
	elem, ok := n.messageIDIdx[id]
	if !ok {
		return "", false
	}
	entry := elem.Value.(*messageIDEntry)
	if n.nowFn().Sub(entry.At) > messageIDTTL {
		n.messageIDLst.Remove(elem)
		delete(n.messageIDIdx, id)
		return "", false
	}
	return entry.Token, true
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

// GetWebhookInfo asks Telegram for the bot's currently-registered
// webhook URL. Returns "" when no webhook is set. The Telegram API
// shape is `{"ok":true,"result":{"url":"...","has_custom_certificate":...}}`;
// a non-ok response is surfaced as an error carrying the API's
// description so the operator can act on it.
func (n *TelegramNotifier) GetWebhookInfo(ctx context.Context) (string, error) {
	endpoint := fmt.Sprintf("%s/bot%s/getWebhookInfo", n.baseURL, n.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("telegram: new request: %s", sanitizeURLError(err))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("telegram: get: %s", sanitizeURLError(err))
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("telegram: getWebhookInfo status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			URL string `json:"url"`
		} `json:"result"`
	}
	if err := json.Unmarshal(buf, &parsed); err != nil {
		return "", fmt.Errorf("telegram: getWebhookInfo decode: %w", err)
	}
	if !parsed.OK {
		return "", fmt.Errorf("telegram: getWebhookInfo: %s", parsed.Description)
	}
	return parsed.Result.URL, nil
}

// EnsureWebhook makes the Telegram bot's registered webhook match
// hookURL: it calls GetWebhookInfo first and only POSTs setWebhook when
// the URL differs. Returns changed=true when SetWebhook was called,
// changed=false when the bot was already pointed at hookURL. Any API
// error short-circuits and is returned to the caller — the daemon's
// goroutine wrapper logs and swallows so a transient Telegram outage
// doesn't fail startup.
func (n *TelegramNotifier) EnsureWebhook(ctx context.Context, hookURL string) (changed bool, err error) {
	current, err := n.GetWebhookInfo(ctx)
	if err != nil {
		return false, err
	}
	if current == hookURL {
		return false, nil
	}
	if err := n.SetWebhook(ctx, hookURL); err != nil {
		return false, err
	}
	return true, nil
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
