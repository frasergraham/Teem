package wsbus

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

func TestBus_PublishAndReceive(t *testing.T) {
	b := New(16)
	_, ch, cancel := b.Subscribe()
	t.Cleanup(cancel)

	want := Envelope{Kind: "audit", Seq: b.NextSeq(), TS: time.Unix(1, 0).UTC()}
	b.Publish(want)

	select {
	case got := <-ch:
		if got.Kind != want.Kind || got.Seq != want.Seq {
			t.Fatalf("envelope mismatch: got %+v want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive published envelope")
	}
}

func TestBus_FanOutToMultipleSubscribers(t *testing.T) {
	b := New(16)
	_, ch1, cancel1 := b.Subscribe()
	t.Cleanup(cancel1)
	_, ch2, cancel2 := b.Subscribe()
	t.Cleanup(cancel2)

	env := Envelope{Kind: "audit", Seq: b.NextSeq()}
	b.Publish(env)

	for i, ch := range []<-chan Envelope{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Seq != env.Seq {
				t.Fatalf("subscriber %d: got seq %d want %d", i, got.Seq, env.Seq)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive envelope", i)
		}
	}
}

func TestBus_SlowSubscriberDropsWithoutBlocking(t *testing.T) {
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	prev := dropLogSink
	dropLogSink = lockedWriter{w: &buf, mu: &mu}
	t.Cleanup(func() { dropLogSink = prev })

	b := New(64)
	b.bufSize = 4
	b.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	// Slow subscriber: never drains.
	_, _, cancelSlow := b.Subscribe()
	t.Cleanup(cancelSlow)

	// Fast subscriber: drains everything.
	_, fastCh, cancelFast := b.Subscribe()
	t.Cleanup(cancelFast)

	var fastSeen int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range fastCh {
			atomic.AddInt32(&fastSeen, 1)
		}
	}()

	// Pace publishes so the fast subscriber stays drained while the
	// slow subscriber's buf=4 fills and then drops on every subsequent
	// envelope.
	const N = 50
	for i := 0; i < N; i++ {
		b.Publish(Envelope{Kind: "audit", Seq: uint64(i + 1)})
		time.Sleep(time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fastSeen) >= N {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&fastSeen); got != N {
		t.Fatalf("fast subscriber should have received all %d envelopes, got %d", N, got)
	}

	mu.Lock()
	lines := strings.Count(buf.String(), "[wsbus] dropped envelope")
	mu.Unlock()
	if lines != 1 {
		t.Fatalf("rate-limited drop log should fire exactly once per interval, got %d lines:\n%s", lines, buf.String())
	}

	cancelFast()
	<-done
}

func TestBus_RingBuffer_RecentReturnsLastN(t *testing.T) {
	b := New(4)
	for i := 1; i <= 10; i++ {
		b.Publish(Envelope{Kind: "audit", Seq: uint64(i)})
	}

	got := b.Recent(0) // 0 → all retained = ringSize
	if len(got) != 4 {
		t.Fatalf("Recent(0) returned %d envelopes, want 4", len(got))
	}
	want := []uint64{7, 8, 9, 10}
	for i, env := range got {
		if env.Seq != want[i] {
			t.Errorf("ring[%d].Seq = %d, want %d", i, env.Seq, want[i])
		}
	}

	got3 := b.Recent(3)
	if len(got3) != 3 {
		t.Fatalf("Recent(3) returned %d, want 3", len(got3))
	}
	want3 := []uint64{8, 9, 10}
	for i, env := range got3 {
		if env.Seq != want3[i] {
			t.Errorf("ring[%d].Seq = %d, want %d", i, env.Seq, want3[i])
		}
	}

	// Asking for more than retained also returns everything.
	gotMore := b.Recent(100)
	if len(gotMore) != 4 {
		t.Fatalf("Recent(100) returned %d, want 4 (all retained)", len(gotMore))
	}
}

func TestBus_RingBuffer_RecentBelowCapacity(t *testing.T) {
	b := New(16)
	for i := 1; i <= 3; i++ {
		b.Publish(Envelope{Kind: "audit", Seq: uint64(i)})
	}
	got := b.Recent(10)
	if len(got) != 3 {
		t.Fatalf("Recent before fill: got %d, want 3", len(got))
	}
	if got[0].Seq != 1 || got[2].Seq != 3 {
		t.Errorf("Recent before fill: seqs=%v, want [1,2,3]", []uint64{got[0].Seq, got[1].Seq, got[2].Seq})
	}
}

func TestBus_CancelRemovesSubscription(t *testing.T) {
	b := New(16)
	id, ch, cancel := b.Subscribe()
	_ = id

	cancel()
	if b.Len() != 0 {
		t.Fatalf("Len after cancel = %d, want 0", b.Len())
	}

	// Channel must be closed.
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("channel still open after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("read from cancelled channel blocked")
	}

	// Subsequent Publishes must not panic (would happen if we still
	// held a reference and tried to send on a closed channel).
	b.Publish(Envelope{Kind: "audit", Seq: b.NextSeq()})
}

func TestBus_NextSeqIsMonotonic(t *testing.T) {
	b := New(16)
	last := uint64(0)
	for i := 0; i < 100; i++ {
		got := b.NextSeq()
		if got != last+1 {
			t.Fatalf("seq %d: got %d, want %d", i, got, last+1)
		}
		last = got
	}
}

func TestBus_ConcurrentPublishSubscribeRace(t *testing.T) {
	b := New(64)
	stop := make(chan struct{})

	var pubWg sync.WaitGroup
	for i := 0; i < 4; i++ {
		pubWg.Add(1)
		go func() {
			defer pubWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b.Publish(Envelope{Kind: "audit", Seq: b.NextSeq(), Event: &audit.Event{}})
			}
		}()
	}

	var subWg sync.WaitGroup
	for i := 0; i < 4; i++ {
		subWg.Add(1)
		go func() {
			defer subWg.Done()
			_, ch, cancel := b.Subscribe()
			defer cancel()
			deadline := time.Now().Add(50 * time.Millisecond)
			for time.Now().Before(deadline) {
				select {
				case <-ch:
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()
	}

	subWg.Wait()
	close(stop)
	pubWg.Wait()

	// Sanity: Recent must not panic and must return at most ringSize.
	r := b.Recent(0)
	if len(r) > 64 {
		t.Fatalf("Recent returned %d > ringSize 64", len(r))
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
