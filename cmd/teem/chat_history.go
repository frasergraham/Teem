package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// chatTurn is one operator↔leader exchange surfaced by recentChatBurst.
// Timestamps are UTC; UserMessage / AssistantText are unredacted but
// trimmed of surrounding whitespace so the rendered burst stays tight.
type chatTurn struct {
	Timestamp     time.Time
	UserMessage   string
	AssistantText string
}

// burstParams tunes recentChatBurst. Zero-value parameters are
// substituted with the package defaults below.
type burstParams struct {
	FloorTurns int           // guaranteed-included recent turns
	GapWindow  time.Duration // max gap between included turns beyond the floor
	MaxTurns   int           // hard cap on returned turn count
	MaxChars   int           // hard cap on total user+assistant chars in returned slice
}

// defaultBurstParams is the production tuning: always surface the last
// 10 turns; extend the burst backwards while consecutive turns sit
// within 10 minutes of each other; cap at 30 turns or 12K chars
// (whichever bites first). Out of scope for operator-tunable knobs.
var defaultBurstParams = burstParams{
	FloorTurns: 10,
	GapWindow:  10 * time.Minute,
	MaxTurns:   30,
	MaxChars:   12_000,
}

// recentChatBurst is the pure floor-then-burst windowing helper. Input
// turns are sorted oldest→newest; the returned slice is the trailing
// window in the same order.
//
// Algorithm:
//
//  1. Always include the last FloorTurns turns (or all turns if fewer
//     exist).
//  2. Walking backwards from the floor, include each earlier turn only
//     if the gap to the next-newer included turn is ≤ GapWindow.
//  3. Stop when MaxTurns is reached OR the total user+assistant char
//     count exceeds MaxChars (we trim from the oldest end to satisfy
//     MaxChars).
//
// Caller is responsible for filtering by scope (team_id / chat_id /
// agent_id) before passing turns in — recentChatBurst makes no scoping
// decisions of its own.
func recentChatBurst(turns []chatTurn, p burstParams) []chatTurn {
	if p.FloorTurns <= 0 {
		p.FloorTurns = defaultBurstParams.FloorTurns
	}
	if p.GapWindow <= 0 {
		p.GapWindow = defaultBurstParams.GapWindow
	}
	if p.MaxTurns <= 0 {
		p.MaxTurns = defaultBurstParams.MaxTurns
	}
	if p.MaxChars <= 0 {
		p.MaxChars = defaultBurstParams.MaxChars
	}
	if len(turns) == 0 {
		return nil
	}

	// Index of the earliest turn we plan to include. Start at the
	// floor (or 0 if fewer turns exist).
	start := len(turns) - p.FloorTurns
	if start < 0 {
		start = 0
	}

	// Extend backwards while gap to the next-newer turn ≤ GapWindow
	// and we have not exceeded MaxTurns.
	for start > 0 {
		gap := turns[start].Timestamp.Sub(turns[start-1].Timestamp)
		if gap < 0 {
			gap = -gap
		}
		if gap > p.GapWindow {
			break
		}
		if len(turns)-(start-1) > p.MaxTurns {
			break
		}
		start--
	}

	out := turns[start:]

	// MaxChars: trim oldest entries until under the cap.
	for len(out) > 0 && burstChars(out) > p.MaxChars {
		out = out[1:]
	}
	// MaxTurns: if floor itself exceeds the cap, trim oldest.
	for len(out) > p.MaxTurns {
		out = out[1:]
	}
	return out
}

func burstChars(turns []chatTurn) int {
	n := 0
	for _, t := range turns {
		n += len(t.UserMessage) + len(t.AssistantText)
	}
	return n
}

// renderChatBurst formats turns into a plain-text block suitable for
// inclusion in the leader subprocess's --append-system-prompt context
// body. Returns "" when turns is empty so callers can append
// unconditionally without inserting a header for no rows.
func renderChatBurst(turns []chatTurn) string {
	if len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Recent conversation (oldest first):\n")
	for _, t := range turns {
		ts := t.Timestamp.UTC().Format(time.RFC3339)
		if u := singleLine(t.UserMessage); u != "" {
			fmt.Fprintf(&b, "[%s] operator: %s\n", ts, u)
		}
		if a := singleLine(t.AssistantText); a != "" {
			fmt.Fprintf(&b, "[%s] you: %s\n", ts, a)
		}
	}
	return b.String()
}

// singleLine collapses internal newlines so each rendered turn occupies
// at most one line in the burst block. Returns "" for empty input so
// the caller can skip the line entirely — a tool-only turn shouldn't
// render as `you: (empty)`, and a spawn-error row (assistant_text="")
// should still surface the operator's question without a fake empty
// reply alongside it.
func singleLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
}

// auditEventsToTurns converts a list of KindLeaderChatTurn events into
// the chatTurn slice recentChatBurst consumes. Events lacking
// user_message AND assistant_text are skipped. Output is in the same
// order as input — caller is responsible for sort order.
func auditEventsToTurns(events []audit.Event) []chatTurn {
	out := make([]chatTurn, 0, len(events))
	for _, e := range events {
		if e.Kind != audit.KindLeaderChatTurn {
			continue
		}
		user, _ := e.Meta["user_message"].(string)
		assistant, _ := e.Meta["assistant_text"].(string)
		if user == "" && assistant == "" {
			continue
		}
		out = append(out, chatTurn{
			Timestamp:     e.Timestamp,
			UserMessage:   user,
			AssistantText: assistant,
		})
	}
	return out
}

// filterByChatID drops events whose Meta.chat_id does not match want.
// Telegram bare-chat scope: turns are stored on the team's audit sink
// alongside dashboard turns, so the chat_id field distinguishes the
// Telegram surface from the dashboard one (which omits chat_id).
// want=0 disables the filter.
func filterByChatID(events []audit.Event, want int64) []audit.Event {
	if want == 0 {
		return events
	}
	// Fresh allocation: don't alias the input slice. audit.FileSink.Query
	// happens to return a fresh slice today, but a future sink might
	// return shared backing storage, and an aliased `out` would mutate
	// it in place.
	out := make([]audit.Event, 0, len(events))
	for _, e := range events {
		if got, ok := chatIDFromMeta(e.Meta); ok && got == want {
			out = append(out, e)
		}
	}
	return out
}

// chatIDFromMeta extracts the chat_id from an audit Event's Meta map.
// JSON round-trips deliver numeric Meta values as float64 — both shapes
// are accepted. Returns (0,false) when the key is missing or not
// numeric (e.g. dashboard chat-turn events that omit chat_id).
func chatIDFromMeta(meta map[string]any) (int64, bool) {
	if meta == nil {
		return 0, false
	}
	v, ok := meta["chat_id"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// loadChatBurst queries the audit sink for past leader-chat turns
// matching agentID (and, for Telegram, chatID) and returns the rendered
// burst block. since bounds the lookback window — we never scan beyond
// 24 hours of history regardless of the burst params.
//
// Errors are swallowed: a missing or unreadable audit sink should not
// abort a chat turn, just degrade to no-history. Caller appends the
// returned string to the context body.
func loadChatBurst(sink audit.Sink, agentID string, chatID int64, p burstParams) string {
	if sink == nil || agentID == "" {
		return ""
	}
	since := time.Now().Add(-24 * time.Hour)
	events, err := sink.Query(agentID, since, 0)
	if err != nil {
		return ""
	}
	events = filterByChatID(events, chatID)
	turns := auditEventsToTurns(events)
	turns = recentChatBurst(turns, p)
	return renderChatBurst(turns)
}
