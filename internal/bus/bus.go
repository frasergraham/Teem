package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

type Kind string

const (
	KindJob    Kind = "job"
	KindResult Kind = "result"
	KindStatus Kind = "status"
	KindLog    Kind = "log"
)

type Message struct {
	ID        string    `json:"id"`
	Topic     string    `json:"topic"`
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"`
	Kind      Kind      `json:"kind"`
	Payload   []byte    `json:"payload,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Bus is the abstract message bus.
type Bus interface {
	// Publish delivers msg to every active subscriber of msg.Topic. It
	// never blocks on slow subscribers; if a subscriber's buffer is full
	// the message is dropped for that subscriber and logged via the bus's
	// own logger.
	Publish(ctx context.Context, msg Message) error
	// Subscribe returns a channel that receives every message published on
	// topic until ctx is cancelled. The channel is closed when the
	// subscription ends.
	Subscribe(ctx context.Context, topic string) (<-chan Message, error)
	// Recent returns up to n most-recently-published messages on topic.
	// Used by the query_bus MCP tool.
	Recent(topic string, n int) []Message
	// Close releases all subscriptions and stops accepting publishes.
	Close() error
}

// NewID returns a short hex id suitable for messages or jobs.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
