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

// TestSubscribeAndCount_AtomicWithLock verifies that the post-subscribe
// count is observed under the same lock that registered the listener:
// the count must equal exactly one more than what was visible before
// (no other goroutine can wedge a subscribe between insert and count).
func TestSubscribeAndCount_AtomicWithLock(t *testing.T) {
	bus := New(4)

	_, ch1, count1, cancel1 := bus.SubscribeAndCount()
	if count1 != 1 {
		t.Errorf("first subscribe count = %d, want 1", count1)
	}
	_, ch2, count2, cancel2 := bus.SubscribeAndCount()
	if count2 != 2 {
		t.Errorf("second subscribe count = %d, want 2", count2)
	}

	if post := cancel1(); post != 1 {
		t.Errorf("cancel after second subscribe: post=%d, want 1", post)
	}
	if post := cancel2(); post != 0 {
		t.Errorf("cancel after last subscribe: post=%d, want 0", post)
	}

	// Both channels closed by cancel — draining should return promptly.
	drainClosed := func(c <-chan Event) {
		for range c {
		}
	}
	drainClosed(ch1)
	drainClosed(ch2)
}

// TestSubscribeAndCount_ConcurrentFirstSubscriber: under N concurrent
// SubscribeAndCount calls, exactly one observer should see count==1.
// This is the property the daemon's channels-live state machine relies
// on to emit exactly one live transition.
func TestSubscribeAndCount_ConcurrentFirstSubscriber(t *testing.T) {
	const N = 16
	bus := New(4)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		firsts  int
		cancels []func() int
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _, count, cancel := bus.SubscribeAndCount()
			mu.Lock()
			if count == 1 {
				firsts++
			}
			cancels = append(cancels, cancel)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firsts != 1 {
		t.Errorf("expected exactly one subscriber to see count=1, got %d", firsts)
	}
	if got := bus.Len(); got != N {
		t.Errorf("Len after %d subscribes = %d, want %d", N, got, N)
	}
	// Tear down: last cancel must return 0.
	var lastZero int
	for _, c := range cancels {
		post := c()
		if post == 0 {
			lastZero++
		}
	}
	if lastZero != 1 {
		t.Errorf("expected exactly one cancel to return 0, got %d", lastZero)
	}
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
