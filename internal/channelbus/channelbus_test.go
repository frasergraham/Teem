package channelbus

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPublish_RateLimitsDropLog verifies that when a slow subscriber's
// buffer overflows, the per-subscriber drop-log warning fires at most
// once per dropLogInterval — saturating with 1000+ drops in quick
// succession must collapse to a single stderr line.
func TestPublish_RateLimitsDropLog(t *testing.T) {
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	prev := dropLogSink
	dropLogSink = lockedWriter{w: &buf, mu: &mu}
	t.Cleanup(func() { dropLogSink = prev })

	bus := New(1)
	bus.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	_, ch, cancel := bus.Subscribe()
	t.Cleanup(cancel)

	for i := 0; i < 1001; i++ {
		bus.Publish(Event{Content: "x"})
	}

	// Drain ch in the background so cancel() doesn't deadlock on a
	// closed channel and the test goroutine exits cleanly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()

	mu.Lock()
	got := strings.Count(buf.String(), "[channelbus] dropped event")
	mu.Unlock()
	if got != 1 {
		t.Fatalf("rate-limited drop log should fire exactly once per interval, got %d lines:\n%s", got, buf.String())
	}

	cancel()
	<-done
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (lw lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}
