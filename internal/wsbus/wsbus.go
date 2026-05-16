// Package wsbus is the per-team in-process broker for WebSocket-pushed
// SPA events. The daemon publishes audit and snapshot-invalidate
// envelopes here; the /api/teams/<id>/events WebSocket handler opens
// one subscription per connected SPA client and writes every Envelope
// over the wire.
//
// Shape mirrors internal/channelbus: best-effort fan-out with
// per-subscriber buffering; a slow subscriber drops events rather than
// back-pressuring publishers. Each bus also keeps a small ring buffer
// of recent envelopes so a freshly-connecting client can fill in the
// gap between last-known seq and "now" without re-fetching state.
package wsbus

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// dropLogInterval throttles the per-subscriber "dropped envelope" log so
// a wedged listener doesn't flood stderr. One line per subscriber per
// interval is enough to make the symptom visible without drowning the
// signal.
const dropLogInterval = 30 * time.Second

// dropLogSink is where slow-listener drops are reported. Overridable
// for tests; defaults to stderr.
var dropLogSink io.Writer = os.Stderr

// Per-listener buffer. 32 absorbs short bursts (e.g. an
// integrator-driven flurry of audit events) without forcing the bus
// into drop mode.
const defaultListenerBuffer = 32

// defaultRingSize is what callers get when they pass <= 0 to New.
// 2000 envelopes is roughly an hour of activity on a busy team, which
// is more than enough headroom to bridge transient network blips
// without forcing a snapshot_invalidate.
const defaultRingSize = 2000

// Envelope is the unit of fan-out. Exactly one of Event/Reason is set
// per Kind:
//
//   - "audit"               → Event populated
//   - "snapshot_invalidate" → Reason populated
//   - "ping"                → both empty
type Envelope struct {
	Kind   string       `json:"kind"`
	Seq    uint64       `json:"seq"`
	TS     time.Time    `json:"ts"`
	Event  *audit.Event `json:"event,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

// Bus fans Envelopes out to subscribers and retains a ring buffer of
// the most-recent N for backfill. Safe for concurrent use; the zero
// value is unusable — call New.
type Bus struct {
	mu          sync.Mutex
	subscribers map[int]chan Envelope
	lastDropLog map[int]time.Time
	nextID      int
	bufSize     int

	ring     []Envelope
	ringSize int
	ringHead int // index of next write slot
	ringLen  int // current count (≤ ringSize)

	seq uint64
	now func() time.Time
}

// New returns a Bus whose ring buffer holds the last ringSize
// envelopes. ringSize <= 0 means use the default (2000).
func New(ringSize int) *Bus {
	if ringSize <= 0 {
		ringSize = defaultRingSize
	}
	return &Bus{
		subscribers: map[int]chan Envelope{},
		lastDropLog: map[int]time.Time{},
		bufSize:     defaultListenerBuffer,
		ring:        make([]Envelope, ringSize),
		ringSize:    ringSize,
		now:         time.Now,
	}
}

// Subscribe registers a new listener. The returned channel receives
// every Envelope Publish'd while the subscription is open. Call cancel
// to drop the subscription; the channel is closed at that point.
//
// A slow listener whose buffer fills up will start dropping envelopes
// silently — the client is expected to notice gaps in Seq and
// reconnect with the last-good seq if it cares about continuity.
func (b *Bus) Subscribe() (id int, ch <-chan Envelope, cancel func()) {
	if b == nil {
		closed := make(chan Envelope)
		close(closed)
		return 0, closed, func() {}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id = b.nextID
	c := make(chan Envelope, b.bufSize)
	b.subscribers[id] = c
	cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if ex, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			delete(b.lastDropLog, id)
			close(ex)
		}
	}
	return id, c, cancel
}

// Publish appends e to the ring buffer and delivers it to every
// current subscriber. Non-blocking: a full subscriber buffer drops the
// envelope for that listener only and logs once per dropLogInterval.
//
// Sends are performed under b.mu (alongside cancel's close) so a
// concurrent cancel cannot race with the send and panic on a closed
// channel. Sends are non-blocking via select-default, so a wedged
// listener cannot stall other publishers.
func (b *Bus) Publish(e Envelope) {
	if b == nil {
		return
	}
	now := b.now()
	b.mu.Lock()
	b.ring[b.ringHead] = e
	b.ringHead = (b.ringHead + 1) % b.ringSize
	if b.ringLen < b.ringSize {
		b.ringLen++
	}
	var toLog []int
	for id, c := range b.subscribers {
		select {
		case c <- e:
		default:
			if last, ok := b.lastDropLog[id]; ok && now.Sub(last) < dropLogInterval {
				continue
			}
			b.lastDropLog[id] = now
			toLog = append(toLog, id)
		}
	}
	b.mu.Unlock()
	for _, id := range toLog {
		fmt.Fprintf(dropLogSink, "[wsbus] dropped envelope for slow subscriber id=%d\n", id)
	}
}

// Recent returns up to n most-recent envelopes in publish order
// (oldest first). n <= 0 or n > current ring length returns all
// retained envelopes.
func (b *Bus) Recent(n int) []Envelope {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ringLen == 0 {
		return nil
	}
	if n <= 0 || n > b.ringLen {
		n = b.ringLen
	}
	out := make([]Envelope, n)
	// Oldest entry lives at (ringHead - ringLen) mod ringSize. We want
	// the last n: start at (ringHead - n) mod ringSize.
	start := (b.ringHead - n + b.ringSize) % b.ringSize
	for i := 0; i < n; i++ {
		out[i] = b.ring[(start+i)%b.ringSize]
	}
	return out
}

// NextSeq returns a fresh monotonic sequence number and advances the
// counter. Callers stamp this on the Envelope they're about to Publish
// so clients can detect gaps and request a snapshot_invalidate.
func (b *Bus) NextSeq() uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	return b.seq
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
