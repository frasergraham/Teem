package audit

import (
	"sync"
	"time"
)

// TailCache is an in-memory ring buffer of recent audit Events that
// short-circuits FileSink.Query for since-anchored queries that fall
// inside its window. Bounded by capacity (events) and max age. Filled
// by FileSink.Write after a successful disk append, and bootstrapped
// once at FileSink open from the tail of the on-disk log.
//
// A query whose since predicate is older than the cache's floor falls
// through to a full disk scan; the RWMutex guarding the ring is NOT
// held during that fallback.
type TailCache struct {
	mu     sync.RWMutex
	cap    int
	maxAge time.Duration
	buf    []Event
	head   int // index of the oldest event in buf
	size   int // number of events currently in buf
	// floor is the oldest timestamp the cache promises to cover. A
	// query with since < floor cannot be served from the cache.
	floor time.Time
}

// NewTailCache returns a cache with the given capacity (number of
// events) and max age. capacity must be > 0; maxAge <= 0 disables age
// eviction.
func NewTailCache(capacity int, maxAge time.Duration) *TailCache {
	if capacity <= 0 {
		capacity = 1
	}
	return &TailCache{
		cap:    capacity,
		maxAge: maxAge,
		buf:    make([]Event, capacity),
	}
}

// Append records e in the ring. Events older than maxAge are evicted
// from the head first; if the ring is then full the oldest event is
// overwritten and floor advances. now is wired through for tests;
// production callers pass time.Now().UTC().
//
// Floor only moves forward. The bootstrap floor (set via
// SetBootstrapFloor) and any later age-eviction can only widen the
// disk-fallback range, never narrow it.
func (c *TailCache) Append(e Event, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictOldLocked(now)
	if c.size == c.cap {
		c.buf[c.head] = e
		c.head = (c.head + 1) % c.cap
		if t := c.buf[c.head].Timestamp; t.After(c.floor) {
			c.floor = t
		}
		return
	}
	idx := (c.head + c.size) % c.cap
	c.buf[idx] = e
	c.size++
	// Only seed floor on the very first event when bootstrap was
	// skipped. Backdated events do NOT widen the cache's coverage
	// claim — only an explicit SetBootstrapFloor or further-back
	// eviction can move floor.
	if c.floor.IsZero() {
		c.floor = e.Timestamp
	}
}

func (c *TailCache) evictOldLocked(now time.Time) {
	if c.maxAge <= 0 {
		return
	}
	cutoff := now.Add(-c.maxAge)
	evicted := false
	for c.size > 0 && c.buf[c.head].Timestamp.Before(cutoff) {
		c.head = (c.head + 1) % c.cap
		c.size--
		evicted = true
	}
	if !evicted {
		return
	}
	if c.size > 0 {
		if t := c.buf[c.head].Timestamp; t.After(c.floor) {
			c.floor = t
		}
		return
	}
	// Ring drained by age. Floor advances to cutoff (it never goes
	// backward).
	if cutoff.After(c.floor) {
		c.floor = cutoff
	}
}

// SetBootstrapFloor lowers the cache's floor to t if the cache did
// NOT have to evict events to fit during bootstrap (i.e. all loaded
// disk events fit in the ring). When the caller has scanned every
// disk event since t and appended them, the cache is provably complete
// from t onward and can serve queries that far back.
func (c *TailCache) SetBootstrapFloor(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.size < c.cap {
		c.floor = t
	}
}

// QueryFromCache returns events matching the filter and a bool
// indicating whether the cache could fully serve the query. If covered
// is false, the caller must fall through to disk. since must be
// non-zero; a zero since means "all history" which a bounded cache
// can never promise to cover.
func (c *TailCache) QueryFromCache(agentID string, since time.Time, limit int) (events []Event, covered bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if since.IsZero() {
		return nil, false
	}
	if since.Before(c.floor) {
		return nil, false
	}
	out := make([]Event, 0, c.size)
	for i := 0; i < c.size; i++ {
		idx := (c.head + i) % c.cap
		e := c.buf[idx]
		if e.Timestamp.Before(since) {
			continue
		}
		if agentID != "" && e.AgentID != agentID {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, true
}

// Floor returns the oldest timestamp the cache currently covers.
func (c *TailCache) Floor() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.floor
}

// Size returns the number of events currently in the ring.
func (c *TailCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.size
}
