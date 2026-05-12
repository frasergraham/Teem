package bus

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMemBus_FanOut(t *testing.T) {
	b := NewMemBus()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	results := make([][]Message, 3)
	for i := range results {
		ch, err := b.Subscribe(ctx, "topic.a")
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		wg.Add(1)
		go func(i int, ch <-chan Message) {
			defer wg.Done()
			for msg := range ch {
				results[i] = append(results[i], msg)
				if len(results[i]) == 3 {
					return
				}
			}
		}(i, ch)
	}

	for i := 0; i < 3; i++ {
		if err := b.Publish(ctx, Message{Topic: "topic.a", Kind: KindJob, Payload: []byte("x")}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscribers did not receive all messages")
	}

	for i, got := range results {
		if len(got) != 3 {
			t.Errorf("subscriber %d got %d msgs, want 3", i, len(got))
		}
	}
}

func TestMemBus_CancelSubscription(t *testing.T) {
	b := NewMemBus()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Subscribe(ctx, "t")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// drain until closed
			for range ch {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestMemBus_Recent(t *testing.T) {
	b := NewMemBus()
	defer b.Close()
	for i := 0; i < 5; i++ {
		_ = b.Publish(context.Background(), Message{Topic: "t", Kind: KindLog})
	}
	got := b.Recent("t", 3)
	if len(got) != 3 {
		t.Errorf("Recent: got %d, want 3", len(got))
	}
}

func TestMemBus_PublishAfterClose(t *testing.T) {
	b := NewMemBus()
	_ = b.Close()
	err := b.Publish(context.Background(), Message{Topic: "t"})
	if err != ErrBusClosed {
		t.Errorf("Publish after close: got %v, want ErrBusClosed", err)
	}
}
