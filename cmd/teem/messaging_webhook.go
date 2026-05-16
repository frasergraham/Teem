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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

// tryRegister installs cancel under key only when no entry already
// exists. Returns true on success and false if the key was already
// taken — the caller treats that as a "session in flight" signal.
func (s *telegramChatSessions) tryRegister(key string, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[key]; ok {
		return false
	}
	s.sessions[key] = cancel
	return true
}

// leaderSessionKey is the session-map key used for bare-text leader
// chat turns. Distinct from the raw reply-token strings used by the
// task-scoped /reply path so the two namespaces can coexist in the
// same map.
func leaderSessionKey(chatID int64) string {
	return "leader:" + strconv.FormatInt(chatID, 10)
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
// is where we post the leader's reply back to. ReplyToMessage is set
// when the operator used Telegram's native long-press → Reply gesture
// against one of the bot's prior outbound messages — its message_id
// lets us resolve the correct reply token without the operator typing
// /reply.
type telegramUpdate struct {
	Message *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID      int64            `json:"message_id"`
	Chat           telegramChat     `json:"chat"`
	Text           string           `json:"text"`
	ReplyToMessage *telegramMessage `json:"reply_to_message"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

// telegramReplier abstracts the chat-id-aware sendMessage call so tests
// can record outbound posts without a real Telegram server.
type telegramReplier interface {
	SendText(ctx context.Context, chatID int64, text string) error
}

// messageIDLookuper resolves an outbound Telegram message_id back to
// the reply token + context the bot stamped on it. Implemented by
// *messaging.TelegramNotifier; tests inject a fake.
type messageIDLookuper interface {
	LookupByMessageID(int64) (string, messaging.ReplyContext, bool)
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
		if r.URL.Path != messaging.WebhookPath {
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

	// Native reply gesture: the operator long-pressed one of the bot's
	// outbound messages and tapped Reply. The webhook update carries
	// reply_to_message.message_id; we resolve it back to the same
	// token+context the operator would otherwise have typed. Checked
	// BEFORE prefix dispatch so a native reply whose body happens to
	// start with /reply or /done is still treated as a chat turn.
	if upd.Message.ReplyToMessage != nil && upd.Message.ReplyToMessage.MessageID != 0 {
		token, _, ok := d.lookupMessageID(upd.Message.ReplyToMessage.MessageID)
		if !ok {
			if rep := d.telegramReplier(); rep != nil && chatID != 0 {
				_ = rep.SendText(r.Context(), chatID, "This thread expired — tap a recent notification to start a new one.")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		d.dispatchTelegramReply(w, r.Context(), chatID, token, text)
		return
	}

	switch {
	case strings.HasPrefix(text, "/done"):
		d.handleTelegramDone(w, r.Context(), chatID, strings.TrimSpace(strings.TrimPrefix(text, "/done")))
		return
	case strings.HasPrefix(text, "/reply"):
		d.handleTelegramReply(w, r.Context(), chatID, strings.TrimSpace(strings.TrimPrefix(text, "/reply")))
		return
	case strings.HasPrefix(text, "/help"), strings.HasPrefix(text, "/start"):
		if rep := d.telegramReplier(); rep != nil && chatID != 0 {
			_ = rep.SendText(r.Context(), chatID, "Hi! Talk to me in plain text — I'm the Teem leader. `/reply <token> <msg>` for task-scoped replies, `/done` to cancel the current turn.")
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// Bare text: treat as a direct leader-chat turn (parallel to the
	// dashboard's /control/teams/<id>/chat panel). No task scope, no
	// token — the operator is initiating a fresh conversation.
	d.handleTelegramLeaderChat(w, r.Context(), chatID, text)
}

// handleTelegramLeaderChat spawns a leader chat subprocess scoped to
// the daemon's first registered team. The bot's response is the
// leader's response, chunked to Telegram's 4096-char per-message cap.
//
// Auth: when messagingCfg.ChatID is set, incoming chatID must match;
// otherwise the operator is told they're not authorised and no chat is
// spawned. ChatID==0 (test / pre-config) skips the check.
//
// In-flight guard: at most one leader turn per chatID. A new bare
// message that arrives mid-turn gets a polite "still responding" reply
// instead of being queued — the operator can retry.
func (d *daemon) handleTelegramLeaderChat(w http.ResponseWriter, ctx context.Context, chatID int64, text string) {
	rep := d.telegramReplier()

	if d.messagingCfg.ChatID != 0 && d.messagingCfg.ChatID != chatID {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, "Not authorised.")
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if strings.TrimSpace(text) == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	rt := d.firstRegisteredTeam()
	if rt == nil {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, "No team is registered with the daemon yet.")
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	turnCtx, cancel := context.WithTimeout(d.baseCtx, telegramIdleTimeout)
	key := leaderSessionKey(chatID)
	if d.messagingChatSessions != nil {
		if !d.messagingChatSessions.tryRegister(key, cancel) {
			cancel()
			if rep != nil && chatID != 0 {
				_ = rep.SendText(ctx, chatID, "Leader is still responding to your previous message — try again in a sec.")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	go d.runTelegramLeaderTurn(turnCtx, cancel, key, text, chatID, rt)
}

// firstRegisteredTeam returns the lexicographically-first team by id,
// or nil when no team is registered. Telegram bare-text chat is
// daemon-global so it needs to land somewhere; in the single-team
// usage model this is just "the team". Deterministic ordering keeps
// behaviour stable across daemon restarts and across test runs.
func (d *daemon) firstRegisteredTeam() *registeredTeam {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.teams) == 0 {
		return nil
	}
	ids := make([]string, 0, len(d.teams))
	for id := range d.teams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return d.teams[ids[0]]
}

// runTelegramLeaderTurn is the long-running half of
// handleTelegramLeaderChat: builds the chat context body, spawns the
// leader subprocess via the shared chatRunner seam, collects all
// assistant text, and posts the response back to the operator's chat
// (chunked to Telegram's 4096-char message limit).
//
// usage is captured under agent_id="leader-telegram" — distinct from
// the /reply path's "leader-telegram-chat" and from the dashboard's
// "leader-chat" — so the daily budget gate can break down spend per
// surface.
func (d *daemon) runTelegramLeaderTurn(turnCtx context.Context, cancel context.CancelFunc, sessionKey, userMessage string, chatID int64, rt *registeredTeam) {
	rep := d.telegramReplier()
	defer cancel()
	if d.messagingChatSessions != nil {
		defer d.messagingChatSessions.unregister(sessionKey, cancel)
	}

	runner := d.chatRunner
	if runner == nil {
		runner = defaultChatRunner
	}
	mcpConfig := filepath.Join(defaultStateDir(rt.team.ID), "pulse-mcp.json")
	contextBody := fmt.Sprintf(
		"You are responding to a direct chat message from the operator over Telegram.\n"+
			"Take one turn — be concise; this lands on the operator's phone.\n"+
			"Use list_tasks / list_agents / query_audit if you need state.\n"+
			"Sent at: %s\n",
		time.Now().UTC().Format(time.RFC3339),
	)

	startedAt := time.Now().UTC()
	stdout, wait, err := runner(turnCtx, mcpConfig, rt.repoRoot, contextBody, userMessage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[messaging-webhook] leader chat start: %v\n", err)
		if rep != nil && chatID != 0 {
			_ = rep.SendText(d.baseCtx, chatID, "Leader subprocess failed to start: "+err.Error())
		}
		return
	}

	cap := usage.NewCapture(startedAt)
	text, parseErr := collectChatTurn(stdout, cap)
	waitErr := wait()
	d.recordChatUsage(rt, cap.Summary(), "leader-telegram")

	if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		if rep != nil && chatID != 0 {
			_ = rep.SendText(d.baseCtx, chatID, "Leader turn timed out after "+telegramIdleTimeout.String()+".")
		}
		return
	}
	if errors.Is(turnCtx.Err(), context.Canceled) {
		// /done — handleTelegramDone already acknowledged.
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
	if rep == nil || chatID == 0 {
		return
	}
	for _, chunk := range chunkForTelegram(text) {
		_ = rep.SendText(d.baseCtx, chatID, chunk)
	}
}

// telegramMaxMessageBytes is Telegram's documented per-message size
// limit (sendMessage rejects payloads beyond 4096 UTF-8 bytes).
const telegramMaxMessageBytes = 4096

// chunkForTelegram splits text into ≤4096-byte slices so a long
// leader response can be delivered as multiple consecutive bot
// messages. Splits on the last newline within the window when
// available, otherwise hard-cuts on a byte boundary — Telegram
// renders the broken edges as monospace anyway, and the operator
// can still read the run-on text. Short inputs return as a
// single-element slice.
func chunkForTelegram(text string) []string {
	if len(text) <= telegramMaxMessageBytes {
		return []string{text}
	}
	var out []string
	for len(text) > telegramMaxMessageBytes {
		cut := telegramMaxMessageBytes
		if nl := strings.LastIndexByte(text[:cut], '\n'); nl > telegramMaxMessageBytes/2 {
			cut = nl
		} else {
			// No newline in the upper half of the window: hard-cut, but
			// back off to the nearest rune start so we never emit a
			// chunk that ends mid-multibyte-rune (Telegram rejects
			// invalid UTF-8).
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, text[:cut])
		text = strings.TrimPrefix(text[cut:], "\n")
	}
	if len(text) > 0 {
		out = append(out, text)
	}
	return out
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

// handleTelegramReply parses `<token> <message>` from a /reply command
// and hands the resolved (token, body) to dispatchTelegramReply.
func (d *daemon) handleTelegramReply(w http.ResponseWriter, ctx context.Context, chatID int64, arg string) {
	token, body := splitFirstWord(arg)
	if token == "" {
		if rep := d.telegramReplier(); rep != nil && chatID != 0 {
			_ = rep.SendText(ctx, chatID, "Usage: `/reply <token> <message>`")
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	d.dispatchTelegramReply(w, ctx, chatID, token, body)
}

// dispatchTelegramReply takes an already-resolved reply token plus the
// operator's body, looks the token up in the store, and spawns a
// one-shot leader chat subprocess scoped to the task that originally
// fired the outbound ping. Shared by both the /reply text path and the
// native long-press → Reply gesture path.
func (d *daemon) dispatchTelegramReply(w http.ResponseWriter, ctx context.Context, chatID int64, token, body string) {
	rep := d.telegramReplier()

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

// lookupMessageID resolves a Telegram outbound message_id to its
// stored reply token + context. Tests inject via
// d.messagingMessageIDLookup so they can assert the native-reply path
// without a real notifier.
func (d *daemon) lookupMessageID(id int64) (string, messaging.ReplyContext, bool) {
	if d.messagingMessageIDLookup != nil {
		return d.messagingMessageIDLookup(id)
	}
	if d.messagingTelegram != nil {
		return d.messagingTelegram.LookupByMessageID(id)
	}
	return "", messaging.ReplyContext{}, false
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
