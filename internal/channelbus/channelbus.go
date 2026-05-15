// Package channelbus is a tiny in-process broker for Claude Code
// channel events. The daemon's per-team MCP server PushChannel call
// publishes Events here; the daemon's SSE endpoint
// (/teams/<name>/channel-events) opens one subscription per connected
// teem-channel stdio shim and forwards every Event.
//
// Why a bus at all: Claude Code only registers channel listeners on
// stdio MCP servers it spawned itself. Our orchestrator MCP server
// runs over HTTP, so its notifications/claude/channel emissions go
// into the void. The shim binary opens a stdio MCP transport that
// claude does listen on, then re-emits Events it receives from this
// bus over that transport. The bus is the seam.
//
// Delivery is best-effort fan-out with per-subscriber buffering. A
// slow subscriber drops events rather than back-pressuring the
// publisher — channels are wake-up nudges, not a durable log.
package channelbus

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// dropLogInterval throttles the per-subscriber "dropped event" log so
// a wedged listener doesn't flood stderr. One line per subscriber per
// interval is enough to make the symptom visible without drowning the
// signal.
const dropLogInterval = 30 * time.Second

// dropLogSink is where slow-listener drops are reported. Overridable
// for tests; defaults to stderr.
var dropLogSink io.Writer = os.Stderr

// Event is a single channel notification: the body claude will render
// inside <channel source="teem">…</channel> plus the flat string
// metadata that becomes attributes on that block.
type Event struct {
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// Bus fans Events out to every active subscriber. Safe for concurrent
// use; the zero value is unusable — call New.
type Bus struct {
	mu          sync.Mutex
	subscribers map[int]chan Event
	lastDropLog map[int]time.Time
	nextID      int
	bufSize     int
	now         func() time.Time
}

// New returns a Bus whose subscriber channels are buffered to
// bufSize. A reasonable default is 64: bursty events from a job
// completing pile up briefly while the SSE writer is mid-flush.
func New(bufSize int) *Bus {
	if bufSize <= 0 {
		bufSize = 64
	}
	return &Bus{
		subscribers: map[int]chan Event{},
		lastDropLog: map[int]time.Time{},
		bufSize:     bufSize,
		now:         time.Now,
	}
}

// Subscribe registers a new listener. The returned channel receives
// every Event Publish'd while the subscription is open. Call cancel
// to drop the subscription; the channel is closed at that point.
//
// A slow listener whose buffer fills up will start dropping events
// silently — that's intentional. We optimise for the leader's wake
// signal, not for replay completeness.
func (b *Bus) Subscribe() (id int, ch <-chan Event, cancel func()) {
	id, ch, _, c := b.SubscribeAndCount()
	return id, ch, func() { c() }
}

// SubscribeAndCount is the TOCTOU-free variant of Subscribe: it
// registers the listener AND returns the post-subscribe subscriber
// count observed under the same internal lock, so callers can decide
// "am I the first subscriber?" without a separate Len() call (which
// could see an inflated/deflated count between Subscribe and Len).
// The returned cancel func unsubscribes and returns the post-cancel
// count, symmetrically — useful for "am I the last subscriber?".
func (b *Bus) SubscribeAndCount() (id int, ch <-chan Event, count int, cancel func() int) {
	if b == nil {
		closed := make(chan Event)
		close(closed)
		return 0, closed, 0, func() int { return 0 }
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id = b.nextID
	c := make(chan Event, b.bufSize)
	b.subscribers[id] = c
	count = len(b.subscribers)
	cancel = func() int {
		b.mu.Lock()
		defer b.mu.Unlock()
		if ex, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			delete(b.lastDropLog, id)
			close(ex)
		}
		return len(b.subscribers)
	}
	return id, c, count, cancel
}

// Publish delivers e to every current subscriber. Non-blocking: if a
// subscriber's buffer is full the event is dropped for that listener
// only and a rate-limited warning is logged so a wedged listener is
// visible. Safe to call with no subscribers.
func (b *Bus) Publish(e Event) {
	if b == nil {
		return
	}
	type entry struct {
		id int
		c  chan Event
	}
	b.mu.Lock()
	entries := make([]entry, 0, len(b.subscribers))
	for id, c := range b.subscribers {
		entries = append(entries, entry{id: id, c: c})
	}
	b.mu.Unlock()
	var dropped []int
	for _, en := range entries {
		select {
		case en.c <- e:
		default:
			dropped = append(dropped, en.id)
		}
	}
	if len(dropped) == 0 {
		return
	}
	now := b.now()
	b.mu.Lock()
	toLog := make([]int, 0, len(dropped))
	for _, id := range dropped {
		if _, still := b.subscribers[id]; !still {
			continue
		}
		if last, ok := b.lastDropLog[id]; ok && now.Sub(last) < dropLogInterval {
			continue
		}
		b.lastDropLog[id] = now
		toLog = append(toLog, id)
	}
	b.mu.Unlock()
	for _, id := range toLog {
		fmt.Fprintf(dropLogSink, "[channelbus] dropped event for slow subscriber id=%d\n", id)
	}
}

// Len returns the current subscriber count. Useful for tests.
func (b *Bus) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}
