package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/messaging"
	"github.com/frasergraham/teem/internal/usage"
)

// telegramIdleTimeout caps how long a single /reply turn (Claude
// subprocess lifetime) may run before the daemon kills it. Operator
// /done inside the same Telegram thread can cancel earlier.
const telegramIdleTimeout = 10 * time.Minute

// telegramChatSessions tracks in-flight `/reply <token>` subprocesses
// keyed by reply token so `/done` can cancel them mid-flight.
//
// Each /reply turn registers itself on entry and unregisters on exit.
// Because claude -p is a one-shot subprocess (no persistent session),
// the session is at most one outstanding turn per replyToken at a
// time; subsequent /reply calls with the same token replace the
// cancel-fn after the prior turn finishes.
type telegramChatSessions struct {
	mu       sync.Mutex
	sessions map[string]context.CancelFunc
}

func newTelegramChatSessions() *telegramChatSessions {
	return &telegramChatSessions{sessions: map[string]context.CancelFunc{}}
}

// register installs cancel under token. Any prior cancel for the same
// token is replaced (and the caller is responsible for having already
// finished that turn — Register is called from the same goroutine that
// will Wait on the subprocess).
func (s *telegramChatSessions) register(token string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = cancel
}

// unregister removes the entry, but only when it still points at the
// supplied cancel — guards against a concurrent /done racing the
// normal-exit path.
func (s *telegramChatSessions) unregister(token string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sessions[token]
	if !ok {
		return
	}
	// pointer-style identity check via formatting; CancelFunc is not
	// comparable directly across captures.
	if fmt.Sprintf("%p", cur) == fmt.Sprintf("%p", cancel) {
		delete(s.sessions, token)
	}
}

// cancel invokes the registered cancel-fn for token, if any. Returns
// true when a session was running.
func (s *telegramChatSessions) cancel(token string) bool {
	s.mu.Lock()
	c, ok := s.sessions[token]
	if ok {
		delete(s.sessions, token)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	c()
	return true
}

// telegramUpdate is the slice of the Telegram bot update payload we
// care about. message.text carries the operator's body; message.chat.id
// is where we post the leader's reply back to.
type telegramUpdate struct {
	Message *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

// telegramReplier abstracts the chat-id-aware sendMessage call so tests
// can record outbound posts without a real Telegram server.
type telegramReplier interface {
	SendText(ctx context.Context, chatID int64, text string) error
}

// effectiveWebhookPort decides which TCP port the dedicated Telegram
// webhook listener should bind. It returns:
//
//   - port=0 when telegram is disabled (no listener at all),
//   - (cfg.WebhookPort, defaulted=false) when the operator configured a
//     port explicitly, or
//   - (<main listener port>+1, defaulted=true) when telegram is enabled
//     and webhook_port is unset — the operator-friendly default so the
//     listener "just works" once messaging.telegram.enabled flips on.
//
// listenAddr is the daemon's --listen flag (":7777" by default). When
// it doesn't parse as a port number we return (0, false) so the
// caller can keep the existing main-port behaviour rather than binding
// something arbitrary.
func effectiveWebhookPort(cfg messaging.TelegramConfig, listenAddr string) (port int, defaulted bool) {
	if !cfg.Enabled {
		return 0, false
	}
	if cfg.WebhookPort > 0 {
		return cfg.WebhookPort, false
	}
	main := parsePortNumber(listenAddr)
	if main <= 0 {
		return 0, false
	}
	return main + 1, true
}

// parsePortNumber extracts the numeric port from a Go listen address
// like ":7777" or "0.0.0.0:7777". Returns 0 when the address isn't
// recognisable.
func parsePortNumber(addr string) int {
	norm := normalizePort(addr)
	if !strings.HasPrefix(norm, ":") {
		return 0
	}
	n, err := strconv.Atoi(norm[1:])
	if err != nil {
		return 0
	}
	return n
}

// newWebhookHandler builds an http.Handler that serves ONLY
// /messaging/telegram/webhook (delegating to handleTelegramWebhook) and
// returns 404 for every other path. Used by the daemon when the operator
// configures telegram.webhook_port — the dedicated port should expose
// nothing else, so Tailscale Funnel (or any reverse proxy) limits public
// reach to the webhook endpoint alone.
func newWebhookHandler(d *daemon) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messaging/telegram/webhook" {
			http.NotFound(w, r)
			return
		}
		d.handleTelegramWebhook(w, r)
	})
}

// handleTelegramWebhook serves POST /messaging/telegram/webhook?token=…
//
// Auth: the ?token URL parameter must match the daemon's current
// d.messagingWebhookToken (rotated on every daemon start). This is
// weak — the tailnet boundary is the load-bearing security layer.
// Operators should funnel the URL through Tailscale Funnel or an
// equivalent reverse-proxy that limits exposure.
//
// Body shape: standard Telegram bot update JSON. We only act on
// message updates whose text starts with `/reply <token> …` or `/done`.
func (d *daemon) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.messagingWebhookToken == "" {
		http.Error(w, "messaging disabled", http.StatusNotFound)
		return
	}
	if got := r.URL.Query().Get("token"); subtle.ConstantTimeCompare([]byte(got), []byte(d.messagingWebhookToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var upd telegramUpdate
	if err := json.NewDecoder(r.Body).Decode(&upd); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if upd.Message == nil {
		// Non-message updates (edits, callback queries) — accept and
		// ignore so Telegram doesn't retry.
		w.WriteHeader(http.StatusOK)
		return
	}
	text := strings.TrimSpace(upd.Message.Text)
	chatID := upd.Message.Chat.ID

	switch {
	case strings.HasPrefix(text, "/done"):
		d.handleTelegramDone(w, r.Context(), chatID, strings.TrimSpace(strings.TrimPrefix(text, "/done")))
		return
	case strings.HasPrefix(text, "/reply"):
		d.handleTelegramReply(w, r.Context(), chatID, strings.TrimSpace(strings.TrimPrefix(text, "/reply")))
		return
	}

	// Unrecognised text — accept (Telegram retries on non-2xx) but
	// nudge the operator on what command shape we expect.
	if rep := d.telegramReplier(); rep != nil && chatID != 0 {
		_ = rep.SendText(r.Context(), chatID, "Send `/reply <token> <message>` to chat with the leader, or `/done` to end the current thread.")
	}
	w.WriteHeader(http.StatusOK)
}

// handleTelegramDone parses an optional token argument and cancels the
// matching session. With no argument we cancel every session — useful
// when the operator has lost track of which token belongs to which
// thread.
func (d *daemon) handleTelegramDone(w http.ResponseWriter, ctx context.Context, chatID int64, arg string) {
	if d.messagingChatSessions == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	rep := d.telegramReplier()
	if arg == "" {
		// Cancel every live session.
		d.messagingChatSessions.mu.Lock()
		cancels := make([]context.CancelFunc, 0, len(d.messagingChatSessions.sessions))
		for tok, c := range d.messagingChatSessions.sessions {
			cancels = append(cancels, c)
			delete(d.messagingChatSessions.sessions, tok)
		}
		d.messagingChatSessions.mu.Unlock()
		for _, c := range cancels {
			c()
		}
		if rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, fmt.Sprintf("Done. Cancelled %d active session(s).", len(cancels)))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	ok := d.messagingChatSessions.cancel(arg)
	if rep != nil && chatID != 0 {
		if ok {
			_ = rep.SendText(ctx, chatID, "Done. Leader session cancelled.")
		} else {
			_ = rep.SendText(ctx, chatID, "No active session for that token.")
		}
	}
	w.WriteHeader(http.StatusOK)
}

// handleTelegramReply parses `<token> <message>`, looks the token up in
// the store, and spawns a one-shot leader chat subprocess scoped to the
// task that originally fired the outbound ping. The subprocess output
// is collected and posted back as a single Telegram sendMessage.
func (d *daemon) handleTelegramReply(w http.ResponseWriter, ctx context.Context, chatID int64, arg string) {
	rep := d.telegramReplier()

	token, body := splitFirstWord(arg)
	if token == "" {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, "Usage: `/reply <token> <message>`")
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if d.messagingReplyTokens == nil {
		http.Error(w, "messaging disabled", http.StatusNotFound)
		return
	}
	rctx, ok := d.messagingReplyTokens.Lookup(token)
	if !ok {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, "Token unknown or expired. Reply to a fresh ping.")
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.TrimSpace(body) == "" {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, "Add a message after the token, e.g. `/reply "+token+" can you describe the change?`")
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	rt := d.resolveTeam(rctx.TeamID)
	if rt == nil {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, "Team for that token is no longer registered.")
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// Acknowledge fast — running claude takes time and Telegram will
	// retry on slow webhooks. The actual chat turn runs in the
	// background and posts its result via sendMessage when ready.
	w.WriteHeader(http.StatusOK)
	go d.runTelegramTurn(token, body, chatID, rctx, rt)
}

// runTelegramTurn is the long-running half of handleTelegramReply: it
// builds a context body, spawns the leader subprocess, collects all
// assistant text, and posts the rendered turn back to Telegram. Runs
// in its own goroutine so Telegram's webhook timeout (a few seconds)
// is not blocked by claude.
func (d *daemon) runTelegramTurn(token, userMessage string, chatID int64, rctx messaging.ReplyContext, rt *registeredTeam) {
	rep := d.telegramReplier()

	turnCtx, cancel := context.WithTimeout(d.baseCtx, telegramIdleTimeout)
	defer cancel()
	if d.messagingChatSessions != nil {
		d.messagingChatSessions.register(token, cancel)
		defer d.messagingChatSessions.unregister(token, cancel)
	}

	runner := d.chatRunner
	if runner == nil {
		runner = defaultChatRunner
	}
	mcpConfig := filepath.Join(defaultStateDir(rt.team.ID), "pulse-mcp.json")
	contextBody := fmt.Sprintf(
		"You are responding to a Telegram message from the operator about task %s (originally surfaced by %s).\n"+
			"Take one turn — be concise; this lands on the operator's phone.\n"+
			"Use list_tasks / query_audit if you need state.\n"+
			"Sent at: %s\n",
		rctx.TaskID, rctx.AgentID, time.Now().UTC().Format(time.RFC3339),
	)

	startedAt := time.Now().UTC()
	stdout, wait, err := runner(turnCtx, mcpConfig, rt.repoRoot, contextBody, userMessage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[messaging-webhook] start: %v\n", err)
		if rep != nil && chatID != 0 {
			_ = rep.SendText(d.baseCtx, chatID, "Leader subprocess failed to start: "+err.Error())
		}
		return
	}

	cap := usage.NewCapture(startedAt)
	text, parseErr := collectChatTurn(stdout, cap)
	waitErr := wait()
	d.recordChatUsage(rt, cap.Summary(), "leader-telegram-chat")

	if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(d.baseCtx, chatID, "Leader turn timed out after "+telegramIdleTimeout.String()+".")
		}
		return
	}
	if errors.Is(turnCtx.Err(), context.Canceled) {
		// /done — already replied to operator in handleTelegramDone.
		return
	}
	if waitErr != nil && parseErr == nil {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(d.baseCtx, chatID, "Leader subprocess errored: "+waitErr.Error())
		}
		return
	}
	if parseErr != nil {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(d.baseCtx, chatID, "Leader output parse error: "+parseErr.Error())
		}
		return
	}
	if strings.TrimSpace(text) == "" {
		text = "(leader returned no text)"
	}
	if rep != nil && chatID != 0 {
		_ = rep.SendText(d.baseCtx, chatID, text)
	}
}

// collectChatTurn drains Claude Code's stream-json and returns the
// accumulated assistant text plus any parse error. Each raw line is
// also fed through the supplied usage.Capture so Telegram chat spend
// flows through the same usage extractor as dashboard chat and pulse
// ticks. cap may be nil.
func collectChatTurn(r io.Reader, cap *usage.Capture) (string, error) {
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type assistantMsg struct {
		Content []contentBlock `json:"content"`
	}
	type ev struct {
		Type    string       `json:"type"`
		Result  string       `json:"result"`
		Message assistantMsg `json:"message"`
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var (
		parts      []string
		resultText string
	)
	for sc.Scan() {
		line := sc.Bytes()
		if cap != nil {
			cap.Feed(line)
		}
		var e ev
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			for _, c := range e.Message.Content {
				if c.Type == "text" && c.Text != "" {
					parts = append(parts, c.Text)
				}
			}
		case "result":
			if e.Result != "" {
				resultText = e.Result
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read stream: %w", err)
	}
	if len(parts) == 0 && resultText != "" {
		return resultText, nil
	}
	return strings.Join(parts, "\n"), nil
}

// telegramReplier returns the daemon's outbound-text helper, or nil
// when messaging is disabled / non-Telegram. Tests inject a fake by
// overwriting d.messagingReplierOverride.
func (d *daemon) telegramReplier() telegramReplier {
	if d.messagingReplierOverride != nil {
		return d.messagingReplierOverride
	}
	if d.messagingTelegram != nil {
		return d.messagingTelegram
	}
	return nil
}

// splitFirstWord returns the first whitespace-delimited word and the
// trimmed remainder of s. Used to peel `<token>` off `/reply <token>
// <message>` arguments.
func splitFirstWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	idx := strings.IndexAny(s, " \t\n")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], strings.TrimSpace(s[idx:])
}
