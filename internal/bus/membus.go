package bus

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	defaultSubBuffer  = 64
	defaultHistorySize = 128
)

type subscription struct {
	ch     chan Message
	cancel context.CancelFunc
}

// MemBus is an in-process implementation of Bus backed by Go channels.
type MemBus struct {
	mu          sync.RWMutex
	subscribers map[string][]*subscription
	history     map[string][]Message
	closed      bool
}

func NewMemBus() *MemBus {
	return &MemBus{
		subscribers: map[string][]*subscription{},
		history:     map[string][]Message{},
	}
}

var ErrBusClosed = errors.New("bus: closed")

func (b *MemBus) Publish(ctx context.Context, msg Message) error {
	if msg.ID == "" {
		msg.ID = NewID()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBusClosed
	}
	h := b.history[msg.Topic]
	h = append(h, msg)
	if len(h) > defaultHistorySize {
		h = h[len(h)-defaultHistorySize:]
	}
	b.history[msg.Topic] = h
	subs := append([]*subscription(nil), b.subscribers[msg.Topic]...)
	b.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- msg:
		default:
			// drop on full buffer rather than block publisher
		}
	}
	return nil
}

func (b *MemBus) Subscribe(ctx context.Context, topic string) (<-chan Message, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBusClosed
	}
	subCtx, cancel := context.WithCancel(ctx)
	sub := &subscription{
		ch:     make(chan Message, defaultSubBuffer),
		cancel: cancel,
	}
	b.subscribers[topic] = append(b.subscribers[topic], sub)
	b.mu.Unlock()

	go func() {
		<-subCtx.Done()
		b.removeSub(topic, sub)
	}()
	return sub.ch, nil
}

func (b *MemBus) removeSub(topic string, target *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers[topic]
	for i, s := range subs {
		if s == target {
			b.subscribers[topic] = append(subs[:i], subs[i+1:]...)
			close(s.ch)
			return
		}
	}
}

func (b *MemBus) Recent(topic string, n int) []Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	h := b.history[topic]
	if n <= 0 || n >= len(h) {
		out := make([]Message, len(h))
		copy(out, h)
		return out
	}
	out := make([]Message, n)
	copy(out, h[len(h)-n:])
	return out
}

func (b *MemBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for _, subs := range b.subscribers {
		for _, s := range subs {
			s.cancel()
		}
	}
	b.subscribers = map[string][]*subscription{}
	return nil
}
